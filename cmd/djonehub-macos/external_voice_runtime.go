package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
	"github.com/iniwex5/vohive/internal/sipwebrtc"
)

var (
	errExternalVoiceClosed      = errors.New("external voice is closed")
	errExternalVoiceInvalid     = errors.New("external voice request is invalid")
	errExternalVoiceConflict    = errors.New("external voice ownership conflict")
	errExternalVoiceStale       = errors.New("external voice call changed")
	errExternalVoiceNotReady    = errors.New("external voice media is not ready")
	errExternalVoiceUnavailable = errors.New("external voice provider is unavailable")
)

var externalVoiceClientNoncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

const externalVoiceOfferTokenBytes = 32

// sipVoiceRuntimeConfig contains only process-local policy and a validated
// adapter. Provider credentials and private handles never enter this object or
// any remote response.
type sipVoiceRuntimeConfig struct {
	GatewayID        string
	Adapter          sipgateway.Adapter
	Journal          sipgateway.MutationJournal
	Evidence         func(sipgateway.CommandKind, string, [sha256.Size]byte) (sipgateway.MutationEvidence, error)
	Network          remotevoice.Config
	AllowAnswer      bool
	AllowReject      bool
	AllowDTMF        bool
	AllowEnd         bool
	AllowDial        bool
	Freshness        time.Duration
	IDReader         io.Reader
	TokenReader      io.Reader
	Now              func() time.Time
	WebRTCAnswerer   remotevoice.MediaAnswerFunc
	IncomingNotifier func(sipgateway.PublicCallRef)
}

func (sipVoiceRuntimeConfig) String() string   { return "sipVoiceRuntimeConfig{redacted}" }
func (sipVoiceRuntimeConfig) GoString() string { return "sipVoiceRuntimeConfig{redacted}" }

type externalVoiceOfferRequest struct {
	Version          int                      `json:"version"`
	Call             sipgateway.PublicCallRef `json:"call"`
	ExpectedRevision uint64                   `json:"expected_revision"`
	ClientNonce      string                   `json:"client_nonce"`
	SDPOffer         string                   `json:"sdp_offer"`
	// turnRelay is injected only by the authenticated public-Web handler. It is
	// never accepted from, or serialized back to, the browser.
	turnRelay *remotevoice.TURNRelayConfig
}

func (externalVoiceOfferRequest) String() string   { return "externalVoiceOfferRequest{redacted}" }
func (externalVoiceOfferRequest) GoString() string { return "externalVoiceOfferRequest{redacted}" }

type externalVoiceCallRequest struct {
	Version          int                      `json:"version"`
	Call             sipgateway.PublicCallRef `json:"call"`
	ExpectedRevision uint64                   `json:"expected_revision"`
	MediaLeaseID     string                   `json:"media_lease_id"`
}

type externalVoiceDialRequest struct {
	Version int    `json:"version"`
	Number  string `json:"number"`
}

func (externalVoiceCallRequest) String() string   { return "externalVoiceCallRequest{redacted}" }
func (externalVoiceCallRequest) GoString() string { return "externalVoiceCallRequest{redacted}" }

type externalVoiceRejectRequest struct {
	Version          int                      `json:"version"`
	Call             sipgateway.PublicCallRef `json:"call"`
	ExpectedRevision uint64                   `json:"expected_revision"`
}

func (externalVoiceRejectRequest) String() string   { return "externalVoiceRejectRequest{redacted}" }
func (externalVoiceRejectRequest) GoString() string { return "externalVoiceRejectRequest{redacted}" }

type externalVoiceDTMFRequest struct {
	Version          int                      `json:"version"`
	Call             sipgateway.PublicCallRef `json:"call"`
	ExpectedRevision uint64                   `json:"expected_revision"`
	MediaLeaseID     string                   `json:"media_lease_id"`
	Digits           string                   `json:"digits"`
}

func (externalVoiceDTMFRequest) String() string   { return "externalVoiceDTMFRequest{redacted}" }
func (externalVoiceDTMFRequest) GoString() string { return "externalVoiceDTMFRequest{redacted}" }

type externalVoiceOfferResult struct {
	Call      sipgateway.PublicCallSnapshot `json:"call"`
	AnswerSDP string                        `json:"answer_sdp"`
}

func (externalVoiceOfferResult) String() string   { return "externalVoiceOfferResult{redacted}" }
func (externalVoiceOfferResult) GoString() string { return "externalVoiceOfferResult{redacted}" }

type externalVoiceStatus struct {
	Enabled          bool                           `json:"enabled"`
	Health           string                         `json:"health"`
	ObservedAt       time.Time                      `json:"observed_at,omitempty"`
	Call             *sipgateway.PublicCallSnapshot `json:"call"`
	OwnedByRequester bool                           `json:"owned_by_requester"`
	AnswerEnabled    bool                           `json:"answer_enabled"`
	RejectEnabled    bool                           `json:"reject_enabled"`
	DTMFEnabled      bool                           `json:"dtmf_enabled"`
	EndEnabled       bool                           `json:"end_enabled"`
	DialEnabled      bool                           `json:"dial_enabled"`
	RecoveryRequired bool                           `json:"recovery_required"`
}

type externalVoiceOfferReplay struct {
	bodyHash  [sha256.Size]byte
	call      sipgateway.PublicCallRef
	revision  uint64
	leaseID   string
	answerSDP string
}

type externalVoiceDialReplay struct {
	bodyHash [sha256.Size]byte
	call     sipgateway.PublicCallSnapshot
}

func (externalVoiceOfferReplay) String() string   { return "externalVoiceOfferReplay{redacted}" }
func (externalVoiceOfferReplay) GoString() string { return "externalVoiceOfferReplay{redacted}" }

type sipVoiceRuntime struct {
	operationMu sync.Mutex
	mu          sync.Mutex

	coordinator      *sipgateway.Coordinator
	network          remotevoice.Config
	allowAnswer      bool
	allowReject      bool
	allowDTMF        bool
	allowEnd         bool
	allowDial        bool
	idReader         io.Reader
	idMu             sync.Mutex
	ownerKey         [sha256.Size]byte
	evidence         func(sipgateway.CommandKind, string, [sha256.Size]byte) (sipgateway.MutationEvidence, error)
	now              func() time.Time
	incomingNotifier func(sipgateway.PublicCallRef)

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	closed           bool
	health           string
	observedAt       time.Time
	current          *sipgateway.PublicCallSnapshot
	owner            string
	uncertainKind    sipgateway.CommandKind
	recoveryRequired bool
	offers           map[string]externalVoiceOfferReplay
	dials            map[string]externalVoiceDialReplay
	closeOnce        sync.Once
	closeErr         error
}

func (*sipVoiceRuntime) String() string   { return "sipVoiceRuntime{redacted}" }
func (*sipVoiceRuntime) GoString() string { return "sipVoiceRuntime{redacted}" }

func newExternalVoiceRuntime(parent context.Context, cfg sipVoiceRuntimeConfig) (*sipVoiceRuntime, error) {
	if parent == nil || cfg.Adapter == nil || cfg.GatewayID == "" ||
		!validExternalVoiceNetworkBase(cfg.Network) {
		return nil, errExternalVoiceInvalid
	}
	if (cfg.AllowEnd || cfg.AllowDial) && !cfg.AllowAnswer {
		return nil, errExternalVoiceInvalid
	}
	if cfg.AllowDial {
		if _, ok := cfg.Adapter.(sipgateway.CallDialer); !ok || !cfg.Adapter.Capabilities().Dial {
			return nil, errExternalVoiceInvalid
		}
	}
	if cfg.AllowReject || cfg.AllowDTMF {
		if _, ok := cfg.Adapter.(sipgateway.CallController); !ok {
			return nil, errExternalVoiceInvalid
		}
	}
	if (cfg.AllowAnswer || cfg.AllowEnd) && (cfg.Journal == nil || cfg.Evidence == nil) {
		_ = cfg.Adapter.Close()
		return nil, errExternalVoiceInvalid
	}
	if cfg.IDReader == nil {
		cfg.IDReader = rand.Reader
	}
	if cfg.TokenReader == nil {
		cfg.TokenReader = rand.Reader
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	var ownerKey [sha256.Size]byte
	if _, err := io.ReadFull(cfg.TokenReader, ownerKey[:]); err != nil {
		_ = cfg.Adapter.Close()
		return nil, errExternalVoiceUnavailable
	}
	defer zeroExternalVoiceRecoverySecret(&ownerKey)
	ctx, cancel := context.WithCancel(parent)
	runtime := &sipVoiceRuntime{
		network: cloneExternalVoiceNetwork(cfg.Network), allowAnswer: cfg.AllowAnswer,
		allowReject: cfg.AllowReject, allowDTMF: cfg.AllowDTMF, allowEnd: cfg.AllowEnd,
		allowDial: cfg.AllowDial,
		idReader:  cfg.TokenReader, ownerKey: ownerKey, evidence: cfg.Evidence,
		now: cfg.Now, ctx: ctx, cancel: cancel,
		incomingNotifier: cfg.IncomingNotifier,
		done:             make(chan struct{}), health: "starting",
		offers: make(map[string]externalVoiceOfferReplay), dials: make(map[string]externalVoiceDialReplay),
	}
	coordinator, err := sipgateway.NewCoordinator(sipgateway.CoordinatorConfig{
		GatewayID: cfg.GatewayID,
		Adapter:   cfg.Adapter,
		Journal:   cfg.Journal,
		Policy: sipgateway.MutationPolicy{
			AllowAnswerIncoming: cfg.AllowAnswer,
			AllowEndActive:      cfg.AllowEnd,
		},
		FreshnessWindow: cfg.Freshness,
		IDReader:        cfg.IDReader,
		Now:             cfg.Now,
		WrapMedia: func(source sipgateway.MediaSession) (sipgateway.MediaSession, error) {
			return sipwebrtc.New(sipwebrtc.Config{
				Source: source, Now: cfg.Now, Answerer: cfg.WebRTCAnswerer,
			})
		},
	})
	if err != nil {
		cancel()
		zeroExternalVoiceRecoverySecret(&runtime.ownerKey)
		_ = cfg.Adapter.Close()
		return nil, err
	}
	runtime.coordinator = coordinator
	go runtime.observeLoop()
	return runtime, nil
}

func (r *sipVoiceRuntime) observeLoop() {
	defer close(r.done)
	for {
		observation, err := r.coordinator.Observe(r.ctx)
		if err != nil {
			r.mu.Lock()
			if !r.closed {
				r.health = "disconnected"
				if errors.Is(err, sipgateway.ErrRecoveryRequired) ||
					errors.Is(err, sipgateway.ErrReconcileRequired) {
					r.recoveryRequired = true
				}
			}
			r.mu.Unlock()
			return
		}
		r.applyObservation(observation)
	}
}

func (r *sipVoiceRuntime) applyObservation(observation sipgateway.Observation) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.health = "connected"
	r.observedAt = r.now()
	if observation.Kind == sipgateway.EventGatewayRestart && observation.Call == nil && !observation.Ignored {
		r.current = nil
		r.owner = ""
		r.uncertainKind = ""
		clear(r.offers)
		r.mu.Unlock()
		return
	}
	if observation.Call == nil {
		r.mu.Unlock()
		return
	}
	call := cloneExternalVoiceCall(*observation.Call)
	if r.current == nil || r.current.Call != call.Call {
		r.owner = ""
		r.uncertainKind = ""
		clear(r.offers)
	}
	if call.Phase == sipgateway.PhaseEnded {
		r.owner = ""
		r.uncertainKind = ""
		clear(r.offers)
	}
	r.current = &call
	notifier := r.incomingNotifier
	shouldNotify := !observation.Ignored && call.Phase == sipgateway.PhaseIncomingRinging
	r.mu.Unlock()
	if shouldNotify && notifier != nil {
		// The callback is the bounded local Web Push enqueue seam. It publishes
		// no provider handle and must never wait on a push-service network call.
		notifier(call.Call)
	}
}

func (r *sipVoiceRuntime) Status(identity string) externalVoiceStatus {
	identity = r.ownerIdentity(identity)
	r.mu.Lock()
	if r.closed {
		status := externalVoiceStatus{Enabled: true, Health: "closed"}
		r.mu.Unlock()
		return status
	}
	current := cloneExternalVoiceCallPointer(r.current)
	health := r.health
	observedAt := r.observedAt
	owner := r.owner
	answerEnabled := r.allowAnswer
	rejectEnabled := r.allowReject
	dtmfEnabled := r.allowDTMF
	endEnabled := r.allowEnd
	dialEnabled := r.allowDial
	r.mu.Unlock()

	if current != nil {
		if refreshed, err := r.coordinator.Snapshot(current.Call); err == nil {
			copy := cloneExternalVoiceCall(refreshed)
			current = &copy
			r.publishCurrent(copy)
		}
	}
	r.mu.Lock()
	owner = r.owner
	r.mu.Unlock()
	return externalVoiceStatus{
		Enabled: true, Health: health, ObservedAt: observedAt, Call: current,
		OwnedByRequester: owner != "" && owner == identity,
		AnswerEnabled:    answerEnabled, RejectEnabled: rejectEnabled, DTMFEnabled: dtmfEnabled,
		EndEnabled: endEnabled, DialEnabled: dialEnabled,
	}
}

func (r *sipVoiceRuntime) Dial(
	ctx context.Context, identity, commandID string, request externalVoiceDialRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.PublicCallSnapshot, error) {
	canonicalIdentity := strings.ToLower(strings.TrimSpace(identity))
	identity = r.ownerIdentity(canonicalIdentity)
	if ctx == nil || identity == "" || !validExternalVoiceDialRequest(request) ||
		commandID == "" || bodyHash == ([sha256.Size]byte{}) {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if err := r.requireOpen(); err != nil {
		return sipgateway.PublicCallSnapshot{}, err
	}
	if !r.allowDial {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceNotReady
	}
	r.mu.Lock()
	if replay, ok := r.dials[commandID]; ok {
		if replay.bodyHash != bodyHash {
			r.mu.Unlock()
			return sipgateway.PublicCallSnapshot{}, errExternalVoiceConflict
		}
		call := cloneExternalVoiceCall(replay.call)
		r.mu.Unlock()
		return call, nil
	}
	if r.current != nil && r.current.Phase != sipgateway.PhaseEnded {
		r.mu.Unlock()
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceConflict
	}
	r.mu.Unlock()
	call, err := r.coordinator.Dial(ctx, request.Number)
	if err != nil {
		return sipgateway.PublicCallSnapshot{}, classifyExternalVoiceError(err)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceClosed
	}
	r.owner = identity
	copy := cloneExternalVoiceCall(call)
	r.current = &copy
	r.dials[commandID] = externalVoiceDialReplay{bodyHash: bodyHash, call: copy}
	r.mu.Unlock()
	return call, nil
}

func (r *sipVoiceRuntime) Offer(
	ctx context.Context,
	identity string,
	request externalVoiceOfferRequest,
	bodyHash [sha256.Size]byte,
) (externalVoiceOfferResult, error) {
	identity = r.ownerIdentity(identity)
	if ctx == nil || identity == "" || request.Version != 2 ||
		request.Call.PublicCallID == "" || request.Call.Generation == 0 ||
		request.ExpectedRevision == 0 || !externalVoiceClientNoncePattern.MatchString(request.ClientNonce) ||
		len(request.SDPOffer) == 0 || len(request.SDPOffer) > remotevoice.MaxOfferBytes {
		return externalVoiceOfferResult{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if err := r.requireOpen(); err != nil {
		return externalVoiceOfferResult{}, err
	}
	if !r.allowAnswer {
		return externalVoiceOfferResult{}, errExternalVoiceNotReady
	}
	replayKey := identity + "\x00" + request.ClientNonce
	if replay, ok := r.lookupOfferReplay(replayKey); ok {
		if replay.bodyHash != bodyHash {
			return externalVoiceOfferResult{}, errExternalVoiceConflict
		}
		current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, false)
		if err != nil || current.Media == nil || current.Media.LeaseID != replay.leaseID ||
			current.Call != replay.call {
			return externalVoiceOfferResult{}, errExternalVoiceStale
		}
		return externalVoiceOfferResult{Call: current, AnswerSDP: replay.answerSDP}, nil
	}
	current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, true)
	if err != nil {
		return externalVoiceOfferResult{}, err
	}
	if current.Phase != sipgateway.PhaseIncomingRinging &&
		current.Phase != sipgateway.PhaseMediaPreparing && current.Phase != sipgateway.PhaseMediaReady {
		return externalVoiceOfferResult{}, errExternalVoiceNotReady
	}
	// Exact response-loss replay was handled above. A different nonce must not
	// renegotiate an already-owned peer; it waits for the ringing bridge watcher
	// to release the prior lease first.
	if current.Media != nil {
		return externalVoiceOfferResult{}, errExternalVoiceConflict
	}
	prepared, err := r.coordinator.PrepareMedia(ctx, sipgateway.PrepareRequest{
		Call: request.Call, ExpectedRevision: request.ExpectedRevision,
		Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
	})
	if err != nil || prepared.Media == nil {
		return externalVoiceOfferResult{}, classifyExternalVoiceError(err)
	}
	media, err := r.coordinator.MediaSession(prepared.Call, prepared.Media.LeaseID)
	if err != nil {
		return externalVoiceOfferResult{}, classifyExternalVoiceError(err)
	}
	bridge, ok := media.(*sipwebrtc.Bridge)
	if !ok {
		r.releasePreparedMedia(prepared)
		return externalVoiceOfferResult{}, errExternalVoiceUnavailable
	}
	network := cloneExternalVoiceNetwork(r.network)
	if request.turnRelay != nil {
		relay := *request.turnRelay
		relay.URLs = append([]string(nil), request.turnRelay.URLs...)
		network.AllowedInterfaces = nil
		network.AllowedLocalCIDRs = nil
		network.AllowedRemoteCIDRs = nil
		network.TURNRelay = &relay
	}
	network.Token, err = r.newOfferToken()
	if err != nil {
		r.releasePreparedMedia(prepared)
		return externalVoiceOfferResult{}, errExternalVoiceUnavailable
	}
	network.Generation = prepared.Call.Generation
	answer, err := bridge.Answer(ctx, []byte(request.SDPOffer), network)
	if err != nil {
		r.releasePreparedMedia(prepared)
		return externalVoiceOfferResult{}, classifyExternalVoiceError(err)
	}
	latest, err := r.coordinator.Snapshot(prepared.Call)
	if err != nil || latest.Media == nil || latest.Media.LeaseID != prepared.Media.LeaseID ||
		latest.Phase == sipgateway.PhaseEnded {
		r.releasePreparedMedia(prepared)
		return externalVoiceOfferResult{}, errExternalVoiceStale
	}
	r.mu.Lock()
	if r.closed || (r.owner != "" && r.owner != identity) ||
		r.current == nil || r.current.Call != latest.Call ||
		r.current.Revision != latest.Revision || r.current.Phase == sipgateway.PhaseEnded {
		r.mu.Unlock()
		r.releasePreparedMedia(latest)
		return externalVoiceOfferResult{}, errExternalVoiceStale
	}
	r.owner = identity
	replay := externalVoiceOfferReplay{
		bodyHash: bodyHash, call: latest.Call, revision: latest.Revision,
		leaseID: latest.Media.LeaseID, answerSDP: string(answer),
	}
	r.offers[replayKey] = replay
	copy := cloneExternalVoiceCall(latest)
	r.current = &copy
	r.mu.Unlock()
	go r.watchBridge(latest.Call, latest.Revision, latest.Media.LeaseID, bridge)
	return externalVoiceOfferResult{Call: latest, AnswerSDP: string(answer)}, nil
}

func (r *sipVoiceRuntime) Answer(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceCallRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	canonicalIdentity := strings.ToLower(strings.TrimSpace(identity))
	identity = r.ownerIdentity(canonicalIdentity)
	if !validExternalVoiceCallRequest(request) || commandID == "" || bodyHash == ([sha256.Size]byte{}) {
		return sipgateway.MutationResult{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if !r.allowAnswer {
		return sipgateway.MutationResult{}, errExternalVoiceNotReady
	}
	current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, false)
	if err != nil {
		return sipgateway.MutationResult{}, classifyExternalVoiceError(err)
	}
	if current.Media == nil || current.Media.LeaseID != request.MediaLeaseID {
		return sipgateway.MutationResult{}, errExternalVoiceConflict
	}
	evidence, err := r.evidence(sipgateway.CommandAnswerIncoming, canonicalIdentity, bodyHash)
	if err != nil {
		return sipgateway.MutationResult{}, classifyExternalVoiceError(err)
	}
	result, err := r.coordinator.AnswerIncoming(ctx, sipgateway.AnswerRequest{
		Call: request.Call, ExpectedRevision: request.ExpectedRevision,
		CommandID: commandID, MediaLeaseID: request.MediaLeaseID,
		Evidence: evidence,
	})
	r.publishCurrent(result.Call)
	if errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		r.setUncertainKind(result.Call, sipgateway.CommandAnswerIncoming)
		return result, sipgateway.ErrCommandOutcomeUnknown
	}
	if err == nil && result.Outcome == sipgateway.CommandRejected {
		return result, errExternalVoiceConflict
	}
	return result, classifyExternalVoiceError(err)
}

func (r *sipVoiceRuntime) Reject(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceRejectRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	canonicalIdentity := strings.ToLower(strings.TrimSpace(identity))
	identity = r.ownerIdentity(canonicalIdentity)
	if request.Version != externalVoiceAPISchemaVersion || request.Call.PublicCallID == "" ||
		request.Call.Generation == 0 || request.ExpectedRevision == 0 ||
		commandID == "" || bodyHash == ([sha256.Size]byte{}) {
		return sipgateway.MutationResult{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if !r.allowReject {
		return sipgateway.MutationResult{}, errExternalVoiceNotReady
	}
	current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, false)
	if err != nil {
		return sipgateway.MutationResult{}, classifyExternalVoiceError(err)
	}
	if current.Phase != sipgateway.PhaseIncomingRinging {
		return sipgateway.MutationResult{}, errExternalVoiceConflict
	}
	result, err := r.coordinator.RejectIncoming(ctx, sipgateway.RejectIncomingRequest{
		Call: request.Call, ExpectedRevision: request.ExpectedRevision, CommandID: commandID,
	})
	r.publishCurrent(result.Call)
	if errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		return result, sipgateway.ErrCommandOutcomeUnknown
	}
	if err == nil && result.Outcome == sipgateway.CommandRejected {
		return result, errExternalVoiceConflict
	}
	return result, classifyExternalVoiceError(err)
}

func (r *sipVoiceRuntime) DTMF(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceDTMFRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	canonicalIdentity := strings.ToLower(strings.TrimSpace(identity))
	identity = r.ownerIdentity(canonicalIdentity)
	if request.Version != externalVoiceAPISchemaVersion || request.Call.PublicCallID == "" ||
		request.Call.Generation == 0 || request.ExpectedRevision == 0 ||
		len(request.MediaLeaseID) < 8 || len(request.MediaLeaseID) > 128 ||
		bodyHash == ([sha256.Size]byte{}) || !validDTMFDigits(request.Digits) {
		return sipgateway.MutationResult{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if !r.allowDTMF {
		return sipgateway.MutationResult{}, errExternalVoiceNotReady
	}
	current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, false)
	if err != nil {
		return sipgateway.MutationResult{}, classifyExternalVoiceError(err)
	}
	if current.Media == nil || current.Media.LeaseID != request.MediaLeaseID ||
		(current.Phase != sipgateway.PhaseActiveUnverified && current.Phase != sipgateway.PhaseActiveTransportVerified) {
		return sipgateway.MutationResult{}, errExternalVoiceConflict
	}
	result, err := r.coordinator.SendDTMF(ctx, sipgateway.DTMFRequest{
		Call: request.Call, ExpectedRevision: request.ExpectedRevision,
		MediaLeaseID: request.MediaLeaseID, CommandID: commandID, Digits: request.Digits,
	})
	r.publishCurrent(result.Call)
	if errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		return result, sipgateway.ErrCommandOutcomeUnknown
	}
	return result, classifyExternalVoiceError(err)
}

func (r *sipVoiceRuntime) End(
	ctx context.Context,
	identity, commandID string,
	request externalVoiceCallRequest,
	bodyHash [sha256.Size]byte,
) (sipgateway.MutationResult, error) {
	canonicalIdentity := strings.ToLower(strings.TrimSpace(identity))
	identity = r.ownerIdentity(canonicalIdentity)
	if !validExternalVoiceCallRequest(request) || commandID == "" || bodyHash == ([sha256.Size]byte{}) {
		return sipgateway.MutationResult{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	if !r.allowEnd {
		return sipgateway.MutationResult{}, errExternalVoiceNotReady
	}
	current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, false)
	if err != nil {
		return sipgateway.MutationResult{}, classifyExternalVoiceError(err)
	}
	if current.Media == nil || current.Media.LeaseID != request.MediaLeaseID {
		return sipgateway.MutationResult{}, errExternalVoiceConflict
	}
	evidence, err := r.evidence(sipgateway.CommandEndActive, canonicalIdentity, bodyHash)
	if err != nil {
		return sipgateway.MutationResult{}, classifyExternalVoiceError(err)
	}
	result, err := r.coordinator.EndActive(ctx, sipgateway.EndRequest{
		Call: request.Call, ExpectedRevision: request.ExpectedRevision, CommandID: commandID,
		MediaLeaseID: request.MediaLeaseID, Evidence: evidence,
	})
	r.publishCurrent(result.Call)
	if errors.Is(err, sipgateway.ErrCommandOutcomeUnknown) {
		r.setUncertainKind(result.Call, sipgateway.CommandEndActive)
		return result, sipgateway.ErrCommandOutcomeUnknown
	}
	if err == nil && result.Outcome == sipgateway.CommandRejected {
		return result, errExternalVoiceConflict
	}
	return result, classifyExternalVoiceError(err)
}

func (r *sipVoiceRuntime) Reconcile(
	ctx context.Context,
	identity string,
	request externalVoiceCallRequest,
	allowControl, allowHangup bool,
) (sipgateway.PublicCallSnapshot, error) {
	identity = r.ownerIdentity(identity)
	if !validExternalVoiceCallRequest(request) {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceInvalid
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	current, err := r.requireCurrent(identity, request.Call, request.ExpectedRevision, false)
	if err != nil {
		return sipgateway.PublicCallSnapshot{}, classifyExternalVoiceError(err)
	}
	if current.Media == nil || current.Media.LeaseID != request.MediaLeaseID || !current.ReconcileRequired {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceConflict
	}
	r.mu.Lock()
	kind := r.uncertainKind
	r.mu.Unlock()
	if (kind == sipgateway.CommandAnswerIncoming && !allowControl) ||
		(kind == sipgateway.CommandEndActive && !allowHangup) || kind == "" {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceConflict
	}
	result, err := r.coordinator.Reconcile(ctx, request.Call)
	if err != nil {
		return sipgateway.PublicCallSnapshot{}, classifyExternalVoiceError(err)
	}
	r.publishCurrent(result)
	if !result.ReconcileRequired {
		r.setUncertainKind(result, "")
	}
	return result, nil
}

func (r *sipVoiceRuntime) watchBridge(
	call sipgateway.PublicCallRef,
	revision uint64,
	leaseID string,
	bridge *sipwebrtc.Bridge,
) {
	_ = bridge.Wait(r.ctx)
	if r.ctx.Err() != nil {
		return
	}
	r.operationMu.Lock()
	defer r.operationMu.Unlock()
	current, err := r.coordinator.Snapshot(call)
	if err == nil && current.Revision == revision && current.Media != nil &&
		current.Media.LeaseID == leaseID &&
		(current.Phase == sipgateway.PhaseMediaPreparing || current.Phase == sipgateway.PhaseMediaReady) {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if released, releaseErr := r.coordinator.ReleaseMedia(releaseCtx, sipgateway.ReleaseRequest{
			Call: call, ExpectedRevision: current.Revision, MediaLeaseID: leaseID,
		}); releaseErr == nil {
			r.publishCurrent(released)
		}
		cancel()
	}
	r.mu.Lock()
	for key, replay := range r.offers {
		if replay.call == call && replay.leaseID == leaseID {
			delete(r.offers, key)
		}
	}
	r.mu.Unlock()
}

func (r *sipVoiceRuntime) releasePreparedMedia(snapshot sipgateway.PublicCallSnapshot) {
	if snapshot.Media == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = r.coordinator.ReleaseMedia(ctx, sipgateway.ReleaseRequest{
		Call: snapshot.Call, ExpectedRevision: snapshot.Revision, MediaLeaseID: snapshot.Media.LeaseID,
	})
}

func (r *sipVoiceRuntime) requireCurrent(
	identity string,
	call sipgateway.PublicCallRef,
	revision uint64,
	claim bool,
) (sipgateway.PublicCallSnapshot, error) {
	r.mu.Lock()
	if r.closed || r.ctx.Err() != nil || r.health == "disconnected" || r.current == nil {
		r.mu.Unlock()
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceUnavailable
	}
	current := cloneExternalVoiceCall(*r.current)
	owner := r.owner
	r.mu.Unlock()
	if current.Call != call || current.Revision != revision {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceStale
	}
	if owner != "" && owner != identity {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceConflict
	}
	if !claim && owner != identity {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceConflict
	}
	refreshed, err := r.coordinator.Snapshot(call)
	if err != nil || refreshed.Revision != revision {
		return sipgateway.PublicCallSnapshot{}, errExternalVoiceStale
	}
	return refreshed, nil
}

func (r *sipVoiceRuntime) lookupOfferReplay(key string) (externalVoiceOfferReplay, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	replay, ok := r.offers[key]
	return replay, ok
}

func (r *sipVoiceRuntime) publishCurrent(call sipgateway.PublicCallSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Only the observation loop may establish a call after startup/restart. A
	// result or status refresh already in flight when a restart clears current
	// must never resurrect the old public identity. Likewise, a slow operation
	// cannot roll an exact call back to an older provider revision.
	if r.closed || r.current == nil || r.current.Call != call.Call ||
		call.Revision < r.current.Revision {
		return
	}
	copy := cloneExternalVoiceCall(call)
	r.current = &copy
	if call.Phase == sipgateway.PhaseEnded {
		r.owner = ""
		r.uncertainKind = ""
		clear(r.offers)
	}
}

func (r *sipVoiceRuntime) setUncertainKind(call sipgateway.PublicCallSnapshot, kind sipgateway.CommandKind) {
	r.mu.Lock()
	if !r.closed && r.current != nil && r.current.Call == call.Call &&
		r.current.Revision == call.Revision {
		r.uncertainKind = kind
	}
	r.mu.Unlock()
}

func (r *sipVoiceRuntime) newOfferToken() (string, error) {
	buffer := make([]byte, externalVoiceOfferTokenBytes)
	r.idMu.Lock()
	_, err := io.ReadFull(r.idReader, buffer)
	r.idMu.Unlock()
	if err != nil {
		return "", err
	}
	return "svt_" + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (r *sipVoiceRuntime) ownerIdentity(identity string) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if identity == "" {
		return ""
	}
	r.idMu.Lock()
	defer r.idMu.Unlock()
	mac := hmac.New(sha256.New, r.ownerKey[:])
	_, _ = mac.Write([]byte("external-voice-v2-owner\x00"))
	_, _ = mac.Write([]byte(identity))
	return "svo_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (r *sipVoiceRuntime) requireOpen() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errExternalVoiceClosed
	}
	if r.health == "disconnected" {
		return errExternalVoiceUnavailable
	}
	return nil
}

func (r *sipVoiceRuntime) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.health = "closed"
		r.current = nil
		r.owner = ""
		clear(r.offers)
		r.mu.Unlock()
		r.cancel()
		r.closeErr = r.coordinator.Close()
		<-r.done
		r.idMu.Lock()
		for index := range r.ownerKey {
			r.ownerKey[index] = 0
		}
		r.idMu.Unlock()
	})
	return r.closeErr
}

func validExternalVoiceCallRequest(request externalVoiceCallRequest) bool {
	return request.Version == 2 && request.Call.PublicCallID != "" && request.Call.Generation != 0 &&
		request.ExpectedRevision != 0 && len(request.MediaLeaseID) >= 8 && len(request.MediaLeaseID) <= 128
}

func validExternalVoiceDialRequest(request externalVoiceDialRequest) bool {
	if request.Version != externalVoiceAPISchemaVersion || request.Number == "" ||
		len(request.Number) > 32 || request.Number != strings.TrimSpace(request.Number) || request.Number == "+" {
		return false
	}
	for index, char := range request.Number {
		if index == 0 && char == '+' {
			continue
		}
		if !(char >= '0' && char <= '9') && char != '*' && char != '#' {
			return false
		}
	}
	return true
}

func validDTMFDigits(value string) bool {
	if len(value) == 0 || len(value) > 32 || value != strings.TrimSpace(value) {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && char != '*' && char != '#' &&
			!(char >= 'A' && char <= 'D') && !(char >= 'a' && char <= 'd') {
			return false
		}
	}
	return true
}

func cloneExternalVoiceCall(value sipgateway.PublicCallSnapshot) sipgateway.PublicCallSnapshot {
	copy := value
	if value.Media != nil {
		media := *value.Media
		copy.Media = &media
	}
	return copy
}

func cloneExternalVoiceCallPointer(value *sipgateway.PublicCallSnapshot) *sipgateway.PublicCallSnapshot {
	if value == nil {
		return nil
	}
	copy := cloneExternalVoiceCall(*value)
	return &copy
}

func cloneExternalVoiceNetwork(value remotevoice.Config) remotevoice.Config {
	copy := value
	copy.AllowedInterfaces = append([]string(nil), value.AllowedInterfaces...)
	copy.AllowedLocalCIDRs = append([]string(nil), value.AllowedLocalCIDRs...)
	copy.AllowedRemoteCIDRs = append([]string(nil), value.AllowedRemoteCIDRs...)
	if value.TURNRelay != nil {
		relay := *value.TURNRelay
		relay.URLs = append([]string(nil), value.TURNRelay.URLs...)
		copy.TURNRelay = &relay
	}
	return copy
}

func validExternalVoiceNetworkBase(value remotevoice.Config) bool {
	if value.UDPMin == 0 || value.UDPMax < value.UDPMin {
		return false
	}
	tailnet := len(value.AllowedInterfaces) == 1 && len(value.AllowedLocalCIDRs) > 0 &&
		len(value.AllowedRemoteCIDRs) == 1
	publicRelayBase := len(value.AllowedInterfaces) == 0 && len(value.AllowedLocalCIDRs) == 0 &&
		len(value.AllowedRemoteCIDRs) == 0
	return tailnet || publicRelayBase
}

func classifyExternalVoiceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, sipgateway.ErrClosed):
		return errExternalVoiceUnavailable
	case errors.Is(err, sipgateway.ErrCallNotFound), errors.Is(err, sipgateway.ErrStaleRevision),
		errors.Is(err, sipgateway.ErrBootEpochMismatch):
		return errExternalVoiceStale
	case errors.Is(err, sipgateway.ErrCommandConflict), errors.Is(err, sipgateway.ErrMediaLeaseMismatch),
		errors.Is(err, sipgateway.ErrConcurrentCall):
		return errExternalVoiceConflict
	case errors.Is(err, sipgateway.ErrMediaNotPrepared), errors.Is(err, sipgateway.ErrWrongCallPhase),
		errors.Is(err, sipgateway.ErrMutationDisabled), errors.Is(err, sipgateway.ErrCapabilityUnavailable),
		errors.Is(err, sipwebrtc.ErrNetworkNotPrepared), errors.Is(err, sipwebrtc.ErrOfferConflict):
		return errExternalVoiceNotReady
	case errors.Is(err, sipgateway.ErrCommandOutcomeUnknown):
		return sipgateway.ErrCommandOutcomeUnknown
	default:
		return fmt.Errorf("%w", errExternalVoiceUnavailable)
	}
}
