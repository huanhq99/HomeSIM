package sipgateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	defaultFreshnessWindow = 2 * time.Second
	defaultMaxCommands     = 4096
	defaultMaxBootEpochs   = 4096
	publicCallIDBytes      = 32
	publicMediaIDBytes     = 32
	publicMediaIDPrefix    = "pml_"
)

// MutationPolicy is independent of adapter capabilities. Its zero value
// denies every call mutation.
type MutationPolicy struct {
	AllowAnswerIncoming bool
	AllowEndActive      bool
}

// CoordinatorConfig supplies the exact expected gateway and local policy.
type CoordinatorConfig struct {
	GatewayID string
	Adapter   Adapter
	// Journal is mandatory whenever either provider mutation is enabled. Its
	// Arm and Resolve calls are the synchronous durable boundary around every
	// adapter mutation.
	Journal MutationJournal
	// WrapMedia may attach a service-owned network bridge to an adapter media
	// session before the coordinator publishes its public lease. On success the
	// returned session owns the source and must preserve its private ID, exact
	// call reference, and codec. On error the source remains coordinator-owned
	// and is closed by PrepareMedia.
	WrapMedia       func(MediaSession) (MediaSession, error)
	Policy          MutationPolicy
	FreshnessWindow time.Duration
	MaxCommands     int
	MaxBootEpochs   int
	IDReader        io.Reader
	Now             func() time.Time
}

// Dial asks an explicitly configured provider to create one outbound dialog.
// The provider returns an incoming-shaped ringing observation so the existing
// media/answer/end lifecycle can be reused. This is intentionally not part of
// the default mutation policy: callers must opt into a CallDialer and must
// serialize/idempotency-wrap the request at their authenticated boundary.
func (c *Coordinator) Dial(ctx context.Context, number string) (PublicCallSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return PublicCallSnapshot{}, err
	}
	dialer, ok := c.adapter.(CallDialer)
	if !ok || !c.capabilities.Dial {
		return PublicCallSnapshot{}, ErrCapabilityUnavailable
	}
	if !validDialNumber(number) {
		return PublicCallSnapshot{}, ErrInvalidCommand
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.isClosed() {
		return PublicCallSnapshot{}, ErrClosed
	}
	c.mu.Lock()
	if c.call != nil && c.call.provider.State != ProviderCallEnded {
		c.mu.Unlock()
		return PublicCallSnapshot{}, ErrConcurrentCall
	}
	c.mu.Unlock()
	snapshot, err := dialer.Dial(ctx, number)
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	if snapshot.Direction != CallDirectionOutgoing || snapshot.State != ProviderCallIncoming ||
		snapshot.Validate() != nil {
		return PublicCallSnapshot{}, ErrInvalidEvent
	}
	created, oldMedia, err := c.createCall(snapshot)
	closeMedia(oldMedia)
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	return created, nil
}

// CallPhase is the server-owned state exposed to remote clients.
type CallPhase string

const (
	PhaseIncomingRinging  CallPhase = "incoming_ringing"
	PhaseMediaPreparing   CallPhase = "media_preparing"
	PhaseMediaReady       CallPhase = "media_ready"
	PhaseAnswerPending    CallPhase = "answer_pending"
	PhaseReconciling      CallPhase = "reconciling"
	PhaseActiveUnverified CallPhase = "active_unverified"
	// PhaseActiveTransportVerified proves fresh frames across the gateway-to-Mac
	// and Mac-to-client transports in both directions. It deliberately does not
	// claim that a human at the cellular far end heard or produced intelligible
	// speech; that still requires a real-call acoustic acceptance test.
	PhaseActiveTransportVerified CallPhase = "active_transport_verified"
	PhaseActiveUnmanaged         CallPhase = "active_unmanaged"
	PhaseEnding                  CallPhase = "ending"
	PhaseEnded                   CallPhase = "ended"
)

// PublicMediaSnapshot exposes proof state without provider handles, endpoint
// addresses, payloads, or credentials.
type PublicMediaSnapshot struct {
	LeaseID                 string `json:"lease_id"`
	Codec                   Codec  `json:"codec"`
	Prepared                bool   `json:"prepared"`
	Activated               bool   `json:"activated"`
	ActivationEpoch         uint64 `json:"activation_epoch,omitempty"`
	GatewayToClientFresh    bool   `json:"gateway_to_client_fresh"`
	ClientToGatewayFresh    bool   `json:"client_to_gateway_fresh"`
	BidirectionalFresh      bool   `json:"bidirectional_fresh"`
	GatewayToClientFrames   uint64 `json:"gateway_to_client_frames"`
	ClientToGatewayFrames   uint64 `json:"client_to_gateway_frames"`
	GatewayToClientBaseline uint64 `json:"gateway_to_client_baseline"`
	ClientToGatewayBaseline uint64 `json:"client_to_gateway_baseline"`
	ActivationFailed        bool   `json:"activation_failed"`
}

// PublicCallSnapshot contains only server-owned opaque identity and redacted
// state. The adapter's provider handle and boot epoch never cross this boundary.
type PublicCallSnapshot struct {
	Call              PublicCallRef        `json:"call"`
	Revision          uint64               `json:"revision"`
	Phase             CallPhase            `json:"phase"`
	Direction         CallDirection        `json:"direction,omitempty"`
	ControllerOwned   bool                 `json:"controller_owned"`
	ReconcileRequired bool                 `json:"reconcile_required"`
	Media             *PublicMediaSnapshot `json:"media,omitempty"`
}

// Observation is one applied or ignored adapter event.
type Observation struct {
	Kind    EventKind           `json:"kind"`
	Ignored bool                `json:"ignored"`
	Call    *PublicCallSnapshot `json:"call,omitempty"`
}

// PrepareRequest binds media negotiation to a public call and exact provider
// revision without exposing provider identity to the caller.
type PrepareRequest struct {
	Call             PublicCallRef
	ExpectedRevision uint64
	Codecs           []Codec
}

// ReleaseRequest detaches one exact public media lease while its call is
// still ringing. It is a host-resource operation and never mutates the
// provider call.
type ReleaseRequest struct {
	Call             PublicCallRef
	ExpectedRevision uint64
	MediaLeaseID     string
}

// AnswerRequest binds a one-shot answer to exact call, revision, command,
// media lease, owner evidence, and immutable request evidence.
type AnswerRequest struct {
	Call             PublicCallRef
	ExpectedRevision uint64
	CommandID        string
	MediaLeaseID     string
	Evidence         MutationEvidence
}

func (AnswerRequest) String() string   { return "sipgateway.AnswerRequest{redacted}" }
func (AnswerRequest) GoString() string { return "sipgateway.AnswerRequest{redacted}" }

// EndRequest binds a one-shot end to an exact active call revision and its
// still-owned public media lease and durable evidence.
type EndRequest struct {
	Call             PublicCallRef
	ExpectedRevision uint64
	CommandID        string
	MediaLeaseID     string
	Evidence         MutationEvidence
}

func (EndRequest) String() string   { return "sipgateway.EndRequest{redacted}" }
func (EndRequest) GoString() string { return "sipgateway.EndRequest{redacted}" }

// MutationResult reports explicit adapter outcome alongside current redacted
// state. Unknown is always returned with ErrCommandOutcomeUnknown.
type MutationResult struct {
	CommandID string             `json:"command_id"`
	Outcome   CommandOutcome     `json:"outcome"`
	Call      PublicCallSnapshot `json:"call"`
}

// Coordinator serializes provider transitions and owns public identities. Its
// bounded command map prevents duplicate execution during one process lifetime,
// while MutationJournal durably arms every enabled provider mutation before it
// executes and keeps ambiguous outcomes available for recovery across restart.
type Coordinator struct {
	observeGate chan struct{}
	opMu        sync.Mutex
	mu          sync.Mutex

	gatewayID      string
	adapter        Adapter
	journal        MutationJournal
	wrapMedia      func(MediaSession) (MediaSession, error)
	capabilities   Capabilities
	policy         MutationPolicy
	freshness      time.Duration
	maxCommands    int
	maxBootEpochs  int
	idReader       io.Reader
	now            func() time.Time
	bootEpoch      string
	seenBootEpochs map[string]struct{}
	call           *coordinatorCall
	nextGeneration uint64
	nextActivation uint64
	commands       map[string]coordinatorCommandRecord
	closed         bool
	closeOnce      sync.Once
	closeDone      chan struct{}
	closeErr       error
}

type coordinatorCall struct {
	public                PublicCallRef
	provider              CallSnapshot
	phase                 CallPhase
	media                 MediaSession
	publicMediaLeaseID    string
	mediaActivationEpoch  uint64
	controllerOwned       bool
	uncertainCommandID    string
	uncertainCommandKind  CommandKind
	uncertainRecoveryID   string
	mediaActivationFailed bool
}

type coordinatorCommandFingerprint struct {
	kind             CommandKind
	call             PublicCallRef
	expectedRevision uint64
	mediaLeaseID     string
	evidence         MutationEvidence
}

type coordinatorCommandRecord struct {
	fingerprint coordinatorCommandFingerprint
	outcome     CommandOutcome
}

// NewCoordinator validates capability and policy before accepting events.
func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	if !validToken(cfg.GatewayID, 1, 64) || cfg.Adapter == nil {
		return nil, ErrInvalidIdentity
	}
	capabilities := cfg.Adapter.Capabilities()
	if err := capabilities.Validate(); err != nil {
		return nil, err
	}
	if cfg.Policy.AllowAnswerIncoming && !capabilities.AnswerIncoming {
		return nil, ErrCapabilityUnavailable
	}
	if cfg.Policy.AllowEndActive && !capabilities.EndActive {
		return nil, ErrCapabilityUnavailable
	}
	if cfg.Policy.AllowAnswerIncoming || cfg.Policy.AllowEndActive {
		if cfg.Journal == nil {
			return nil, ErrMutationJournalRequired
		}
		if _, ok := cfg.Adapter.(RecoveryAdapter); !ok {
			return nil, ErrCapabilityUnavailable
		}
	}
	if cfg.FreshnessWindow == 0 {
		cfg.FreshnessWindow = defaultFreshnessWindow
	}
	if cfg.FreshnessWindow < 40*time.Millisecond || cfg.FreshnessWindow > 30*time.Second {
		return nil, ErrMediaNotPrepared
	}
	if cfg.MaxCommands == 0 {
		cfg.MaxCommands = defaultMaxCommands
	}
	if cfg.MaxCommands < 1 || cfg.MaxCommands > 65536 {
		return nil, ErrCommandStoreFull
	}
	if cfg.MaxBootEpochs == 0 {
		cfg.MaxBootEpochs = defaultMaxBootEpochs
	}
	if cfg.MaxBootEpochs < 1 || cfg.MaxBootEpochs > 65536 {
		return nil, ErrBootEpochStoreFull
	}
	if cfg.IDReader == nil {
		cfg.IDReader = rand.Reader
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	capabilities.Codecs = cloneCodecs(capabilities.Codecs)
	observeGate := make(chan struct{}, 1)
	observeGate <- struct{}{}
	return &Coordinator{
		gatewayID: cfg.GatewayID, adapter: cfg.Adapter, capabilities: capabilities,
		journal:   cfg.Journal,
		wrapMedia: cfg.WrapMedia,
		policy:    cfg.Policy, freshness: cfg.FreshnessWindow, maxCommands: cfg.MaxCommands,
		maxBootEpochs: cfg.MaxBootEpochs,
		idReader:      cfg.IDReader, now: cfg.Now,
		commands:       make(map[string]coordinatorCommandRecord),
		seenBootEpochs: make(map[string]struct{}),
		observeGate:    observeGate,
		closeDone:      make(chan struct{}),
	}, nil
}

// Observe waits for one adapter event, then applies revision and epoch checks.
// Waiting does not hold the coordinator operation lock.
func (c *Coordinator) Observe(ctx context.Context) (Observation, error) {
	if err := contextError(ctx); err != nil {
		return Observation{}, err
	}
	select {
	case <-ctx.Done():
		return Observation{}, ctx.Err()
	case <-c.observeGate:
	}
	defer func() { c.observeGate <- struct{}{} }()
	if c.isClosed() {
		return Observation{}, ErrClosed
	}
	event, err := c.adapter.Observe(ctx)
	if err != nil {
		return Observation{}, err
	}
	if err := event.Validate(); err != nil {
		return Observation{}, err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.isClosed() {
		return Observation{}, ErrClosed
	}
	return c.applyEvent(event)
}

func (c *Coordinator) applyEvent(event Event) (Observation, error) {
	if event.Kind == EventGatewayRestart {
		if event.GatewayID != c.gatewayID {
			return Observation{}, ErrInvalidIdentity
		}
		c.mu.Lock()
		if c.bootEpoch == event.BootEpoch {
			c.mu.Unlock()
			return Observation{Kind: event.Kind, Ignored: true}, nil
		}
		if _, seen := c.seenBootEpochs[event.BootEpoch]; seen {
			c.mu.Unlock()
			return Observation{}, ErrBootEpochMismatch
		}
		if len(c.seenBootEpochs) >= c.maxBootEpochs {
			c.mu.Unlock()
			return Observation{}, ErrBootEpochStoreFull
		}
		oldMedia := mediaOf(c.call)
		c.seenBootEpochs[event.BootEpoch] = struct{}{}
		c.bootEpoch = event.BootEpoch
		c.call = nil
		c.mu.Unlock()
		closeMedia(oldMedia)
		return Observation{Kind: event.Kind}, nil
	}
	if event.Call == nil || event.Call.Ref.GatewayID != c.gatewayID {
		return Observation{}, ErrInvalidIdentity
	}
	return c.applyObservedCall(*event.Call)
}

func (c *Coordinator) applyObservedCall(snapshot CallSnapshot) (Observation, error) {
	c.mu.Lock()
	if c.bootEpoch == "" {
		if snapshot.State == ProviderCallEnded {
			c.mu.Unlock()
			return Observation{Kind: EventCallChanged, Ignored: true}, nil
		}
		c.bootEpoch = snapshot.Ref.BootEpoch
		c.seenBootEpochs[snapshot.Ref.BootEpoch] = struct{}{}
	}
	if snapshot.Ref.BootEpoch != c.bootEpoch {
		c.mu.Unlock()
		return Observation{}, ErrBootEpochMismatch
	}
	current := c.call
	if current == nil || (current.provider.State == ProviderCallEnded &&
		!current.provider.Ref.SameDialog(snapshot.Ref)) {
		if snapshot.State == ProviderCallEnded {
			c.mu.Unlock()
			return Observation{Kind: EventCallChanged, Ignored: true}, nil
		}
		c.mu.Unlock()
		created, oldMedia, err := c.createCall(snapshot)
		closeMedia(oldMedia)
		if err != nil {
			return Observation{}, err
		}
		return Observation{Kind: EventCallChanged, Call: &created}, nil
	}
	if !current.provider.Ref.SameDialog(snapshot.Ref) {
		c.mu.Unlock()
		return Observation{}, ErrConcurrentCall
	}
	if snapshot.Ref.Revision < current.provider.Ref.Revision {
		c.mu.Unlock()
		call, err := c.Snapshot(current.public)
		return Observation{Kind: EventCallChanged, Ignored: true, Call: pointerCall(call)}, err
	}
	if snapshot.Ref.Revision == current.provider.Ref.Revision {
		if snapshot.State != current.provider.State {
			c.mu.Unlock()
			return Observation{}, ErrInvalidEvent
		}
		public := current.public
		c.mu.Unlock()
		call, err := c.Snapshot(public)
		return Observation{Kind: EventCallChanged, Ignored: true, Call: pointerCall(call)}, err
	}
	if !validProviderTransition(current.provider.State, snapshot.State) {
		c.mu.Unlock()
		return Observation{}, ErrInvalidEvent
	}
	public := current.public
	activate, closeSession := c.applyProviderStateLocked(current, snapshot)
	c.mu.Unlock()
	closeMedia(closeSession)
	if activate != nil {
		c.activateMedia(public, activate)
	}
	call, err := c.Snapshot(public)
	return Observation{Kind: EventCallChanged, Call: pointerCall(call)}, err
}

func (c *Coordinator) createCall(snapshot CallSnapshot) (PublicCallSnapshot, MediaSession, error) {
	publicID, err := c.newPublicCallID()
	if err != nil {
		return PublicCallSnapshot{}, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	oldMedia := mediaOf(c.call)
	c.nextGeneration++
	if c.nextGeneration == 0 {
		return PublicCallSnapshot{}, oldMedia, ErrInvalidIdentity
	}
	phase := PhaseIncomingRinging
	if snapshot.State == ProviderCallActive {
		phase = PhaseActiveUnmanaged
	}
	c.call = &coordinatorCall{
		public:   PublicCallRef{PublicCallID: publicID, Generation: c.nextGeneration},
		provider: snapshot, phase: phase,
	}
	state := cloneCoordinatorCall(c.call)
	return c.renderCall(state), oldMedia, nil
}

// PrepareMedia reserves one adapter media session for an exact ringing call.
func (c *Coordinator) PrepareMedia(ctx context.Context, request PrepareRequest) (PublicCallSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return PublicCallSnapshot{}, err
	}
	if request.ExpectedRevision == 0 || validateRequestedCodecs(request.Codecs) != nil {
		return PublicCallSnapshot{}, ErrMediaNotPrepared
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	current, err := c.requireCurrent(request.Call, request.ExpectedRevision)
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	if hasUncertainMutation(current) {
		return PublicCallSnapshot{}, ErrReconcileRequired
	}
	if current.provider.State != ProviderCallIncoming ||
		(current.phase != PhaseIncomingRinging && current.phase != PhaseMediaPreparing &&
			current.phase != PhaseMediaReady) {
		return PublicCallSnapshot{}, ErrWrongCallPhase
	}
	if current.media != nil {
		return c.Snapshot(request.Call)
	}
	if !c.capabilities.PrepareMedia {
		return PublicCallSnapshot{}, ErrCapabilityUnavailable
	}
	media, err := c.adapter.PrepareMedia(ctx, PrepareMediaRequest{
		Call: current.provider.Ref, Codecs: cloneCodecs(request.Codecs),
	})
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	if !validPreparedMedia(media, current.provider.Ref, request.Codecs) {
		closeMedia(media)
		return PublicCallSnapshot{}, ErrMediaNotPrepared
	}
	if c.wrapMedia != nil {
		source := media
		media, err = c.wrapMedia(source)
		if err != nil {
			closeMedia(media)
			closeMedia(source)
			// A wrapper may contain transport, SDP, endpoint, or credential
			// details in its error. Keep the coordinator boundary stable and
			// redacted; the caller only needs to know preparation failed.
			return PublicCallSnapshot{}, ErrMediaNotPrepared
		}
		if !sameMediaIdentity(media, source) {
			closeMedia(media)
			// Do not rely on a malformed wrapper to have adopted/closed source.
			// MediaSession.Close is required to be idempotent.
			closeMedia(source)
			return PublicCallSnapshot{}, ErrMediaLeaseMismatch
		}
	}
	publicMediaLeaseID, err := c.newPublicMediaLeaseID()
	if err != nil {
		closeMedia(media)
		return PublicCallSnapshot{}, err
	}
	c.mu.Lock()
	if c.closed || c.call == nil || c.call.public != request.Call ||
		c.call.provider.Ref != current.provider.Ref || c.call.media != nil {
		c.mu.Unlock()
		closeMedia(media)
		return PublicCallSnapshot{}, ErrStaleRevision
	}
	c.call.media = media
	c.call.publicMediaLeaseID = publicMediaLeaseID
	c.call.mediaActivationEpoch = 0
	c.call.mediaActivationFailed = false
	// An adapter session is ready for the service-owned network wrapper, but the
	// public media lease is not ready until its current Snapshot proves the
	// browser transport as well. renderCall promotes this phase dynamically.
	c.call.phase = PhaseMediaPreparing
	c.mu.Unlock()
	return c.Snapshot(request.Call)
}

// MediaSession returns the current in-process media owner after exact public
// call and lease checks. It never serializes provider identity and callers must
// not close the returned session; the coordinator retains lifetime ownership.
// The session may become stale immediately after this method returns, so every
// later call mutation must still pass the coordinator's exact revision gates.
func (c *Coordinator) MediaSession(public PublicCallRef, publicLeaseID string) (MediaSession, error) {
	if !validPublicCallRef(public) || !validOpaque(publicLeaseID, 1, 128) {
		return nil, ErrMediaLeaseMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.call == nil || c.call.public != public || c.call.media == nil ||
		c.call.publicMediaLeaseID != publicLeaseID {
		return nil, ErrMediaLeaseMismatch
	}
	return c.call.media, nil
}

// ReleaseMedia lets a disconnected browser discard a ringing WebRTC/media
// lease and negotiate a fresh one for the same call. It cannot release media
// after answer has begun, during reconciliation, or from a stale public lease.
func (c *Coordinator) ReleaseMedia(ctx context.Context, request ReleaseRequest) (PublicCallSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return PublicCallSnapshot{}, err
	}
	if !validPublicCallRef(request.Call) || request.ExpectedRevision == 0 ||
		!validOpaque(request.MediaLeaseID, 1, 128) {
		return PublicCallSnapshot{}, ErrMediaLeaseMismatch
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	current, err := c.requireCurrent(request.Call, request.ExpectedRevision)
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	if hasUncertainMutation(current) || current.provider.State != ProviderCallIncoming ||
		(current.phase != PhaseMediaPreparing && current.phase != PhaseMediaReady) {
		return PublicCallSnapshot{}, ErrWrongCallPhase
	}
	if current.media == nil || current.publicMediaLeaseID != request.MediaLeaseID {
		return PublicCallSnapshot{}, ErrMediaLeaseMismatch
	}
	c.mu.Lock()
	if c.closed || c.call == nil || c.call.public != current.public ||
		c.call.provider.Ref != current.provider.Ref || c.call.media != current.media ||
		c.call.publicMediaLeaseID != request.MediaLeaseID {
		c.mu.Unlock()
		return PublicCallSnapshot{}, ErrMediaLeaseMismatch
	}
	media := c.call.media
	c.call.media = nil
	c.call.publicMediaLeaseID = ""
	c.call.mediaActivationEpoch = 0
	c.call.mediaActivationFailed = false
	c.call.phase = PhaseIncomingRinging
	state := cloneCoordinatorCall(c.call)
	c.mu.Unlock()
	// Provider cleanup errors may contain endpoint details. The authoritative
	// public lease is already detached, so close best-effort and keep the
	// remote boundary stable and redacted.
	closeMedia(media)
	return c.renderCall(state), nil
}

// AnswerIncoming executes at most one adapter mutation for a command ID.
func (c *Coordinator) AnswerIncoming(ctx context.Context, request AnswerRequest) (MutationResult, error) {
	fingerprint := coordinatorCommandFingerprint{
		kind: CommandAnswerIncoming, call: request.Call,
		expectedRevision: request.ExpectedRevision, mediaLeaseID: request.MediaLeaseID,
		evidence: request.Evidence,
	}
	return c.executeMutation(ctx, request.CommandID, fingerprint)
}

// EndActive executes at most one adapter mutation for a command ID.
func (c *Coordinator) EndActive(ctx context.Context, request EndRequest) (MutationResult, error) {
	fingerprint := coordinatorCommandFingerprint{
		kind: CommandEndActive, call: request.Call,
		expectedRevision: request.ExpectedRevision, mediaLeaseID: request.MediaLeaseID,
		evidence: request.Evidence,
	}
	return c.executeMutation(ctx, request.CommandID, fingerprint)
}

func (c *Coordinator) executeMutation(
	ctx context.Context,
	commandID string,
	fingerprint coordinatorCommandFingerprint,
) (MutationResult, error) {
	if err := contextError(ctx); err != nil {
		return MutationResult{}, err
	}
	if !validPublicCallRef(fingerprint.call) || fingerprint.expectedRevision == 0 ||
		!validCommandID(commandID) || fingerprint.evidence.Validate() != nil {
		return MutationResult{}, ErrInvalidCommand
	}
	if !validOpaque(fingerprint.mediaLeaseID, 1, 128) {
		return MutationResult{}, ErrInvalidCommand
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.isClosed() {
		return MutationResult{}, ErrClosed
	}
	if existing, ok := c.commands[commandID]; ok {
		if existing.fingerprint != fingerprint {
			return MutationResult{}, ErrCommandConflict
		}
		call, err := c.Snapshot(fingerprint.call)
		if err != nil {
			return MutationResult{}, err
		}
		result := MutationResult{CommandID: commandID, Outcome: existing.outcome, Call: call}
		if existing.outcome == CommandUnknown {
			return result, ErrCommandOutcomeUnknown
		}
		return result, nil
	}
	if len(c.commands) >= c.maxCommands {
		return MutationResult{}, ErrCommandStoreFull
	}
	current, err := c.requireCurrent(fingerprint.call, fingerprint.expectedRevision)
	if err != nil {
		return MutationResult{}, err
	}
	if hasUncertainMutation(current) {
		return MutationResult{}, ErrReconcileRequired
	}
	switch fingerprint.kind {
	case CommandAnswerIncoming:
		if !c.policy.AllowAnswerIncoming {
			return MutationResult{}, ErrMutationDisabled
		}
		if !c.capabilities.AnswerIncoming {
			return MutationResult{}, ErrCapabilityUnavailable
		}
		if current.provider.State != ProviderCallIncoming ||
			(current.phase != PhaseMediaPreparing && current.phase != PhaseMediaReady) || current.media == nil {
			return MutationResult{}, ErrMediaNotPrepared
		}
		media := current.media.Snapshot()
		if current.publicMediaLeaseID != fingerprint.mediaLeaseID ||
			!validPreparedMediaSnapshot(current.media, current.provider.Ref, media) {
			return MutationResult{}, ErrMediaLeaseMismatch
		}
	case CommandEndActive:
		if !c.policy.AllowEndActive {
			return MutationResult{}, ErrMutationDisabled
		}
		if !c.capabilities.EndActive {
			return MutationResult{}, ErrCapabilityUnavailable
		}
		if current.provider.State != ProviderCallActive || !current.controllerOwned ||
			current.phase != PhaseActiveUnverified {
			return MutationResult{}, ErrWrongCallPhase
		}
		if current.media == nil || current.publicMediaLeaseID != fingerprint.mediaLeaseID {
			return MutationResult{}, ErrMediaLeaseMismatch
		}
	default:
		return MutationResult{}, ErrInvalidCommand
	}
	recoveryAdapter, ok := c.adapter.(RecoveryAdapter)
	if !ok {
		return MutationResult{}, ErrCapabilityUnavailable
	}
	providerToken, err := recoveryAdapter.RecoveryToken(current.provider.Ref)
	if err != nil {
		return MutationResult{}, err
	}
	pending := PendingMutation{
		Kind: fingerprint.kind, CommandID: commandID, Call: current.public,
		ExpectedRevision:   fingerprint.expectedRevision,
		PublicMediaLeaseID: fingerprint.mediaLeaseID,
		ProviderToken:      providerToken, Evidence: fingerprint.evidence,
	}
	if err := pending.Validate(); err != nil {
		return MutationResult{}, err
	}
	recoveryID, err := c.journal.Arm(pending)
	if err != nil {
		return MutationResult{}, err
	}

	// Arm is durable before any in-memory or provider transition. From this
	// point until a durable Resolve succeeds, every observable local outcome is
	// unknown and every later mutation is blocked behind reconciliation.
	c.commands[commandID] = coordinatorCommandRecord{fingerprint: fingerprint, outcome: CommandUnknown}
	c.mu.Lock()
	if c.closed || c.call == nil || c.call.public != current.public ||
		c.call.provider.Ref != current.provider.Ref {
		c.mu.Unlock()
		return c.markUnknown(commandID, fingerprint, recoveryID, current.public, ErrClosed)
	}
	c.call.uncertainCommandID = commandID
	c.call.uncertainCommandKind = fingerprint.kind
	c.call.uncertainRecoveryID = recoveryID
	if fingerprint.kind == CommandAnswerIncoming {
		c.call.phase = PhaseAnswerPending
	} else {
		c.call.phase = PhaseEnding
	}
	c.mu.Unlock()
	if !validOpaque(recoveryID, 1, MaxMutationRecoveryIDBytes) {
		return c.markUnknown(commandID, fingerprint, recoveryID, current.public,
			ErrInvalidMutationJournalRecord)
	}

	var adapterResult CommandResult
	if fingerprint.kind == CommandAnswerIncoming {
		adapterResult, err = c.adapter.AnswerIncoming(ctx, AnswerIncomingRequest{
			Call: current.provider.Ref, CommandID: commandID, MediaLeaseID: current.media.ID(),
		})
	} else {
		adapterResult, err = c.adapter.EndActive(ctx, EndActiveRequest{
			Call: current.provider.Ref, CommandID: commandID,
		})
	}
	if err != nil {
		return c.markUnknown(commandID, fingerprint, recoveryID, current.public, nil)
	}
	return c.applyCommandResult(commandID, fingerprint, recoveryID, current, adapterResult)
}

func (c *Coordinator) applyCommandResult(
	commandID string,
	fingerprint coordinatorCommandFingerprint,
	recoveryID string,
	before coordinatorCall,
	result CommandResult,
) (MutationResult, error) {
	if result.CommandID != commandID ||
		(result.Outcome != CommandApplied && result.Outcome != CommandRejected && result.Outcome != CommandUnknown) {
		return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
	}
	if result.Outcome == CommandUnknown {
		return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
	}
	if result.Current == nil || result.Current.Validate() != nil ||
		!result.Current.Ref.SameDialog(before.provider.Ref) ||
		result.Current.Ref.Revision < before.provider.Ref.Revision {
		return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
	}
	current := *result.Current
	if result.Outcome == CommandApplied {
		wanted := ProviderCallActive
		if fingerprint.kind == CommandEndActive {
			wanted = ProviderCallEnded
		}
		if current.State != wanted || current.Ref.Revision <= before.provider.Ref.Revision {
			return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
		}
	} else {
		wanted := ProviderCallIncoming
		if fingerprint.kind == CommandEndActive {
			wanted = ProviderCallActive
		}
		if current.State != wanted {
			return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
		}
	}

	c.mu.Lock()
	callMatches := !c.closed && c.call != nil && c.call.public == before.public &&
		c.call.provider.Ref.SameDialog(before.provider.Ref) &&
		c.call.uncertainCommandID == commandID && c.call.uncertainRecoveryID == recoveryID
	c.mu.Unlock()
	if !callMatches {
		return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
	}
	resolution := MutationResolution{Outcome: result.Outcome, State: current.State}
	if err := resolution.Validate(); err != nil {
		return c.markUnknown(commandID, fingerprint, recoveryID, before.public, nil)
	}
	if err := c.journal.Resolve(recoveryID, resolution); err != nil {
		return c.markUnknown(commandID, fingerprint, recoveryID, before.public, err)
	}

	var activate MediaSession
	var closeSession MediaSession
	c.mu.Lock()
	if c.closed || c.call == nil || c.call.public != before.public ||
		!c.call.provider.Ref.SameDialog(before.provider.Ref) ||
		c.call.uncertainCommandID != commandID || c.call.uncertainRecoveryID != recoveryID {
		c.mu.Unlock()
		return MutationResult{}, ErrClosed
	}
	c.commands[commandID] = coordinatorCommandRecord{fingerprint: fingerprint, outcome: result.Outcome}
	c.call.provider = current
	c.call.uncertainCommandID = ""
	c.call.uncertainCommandKind = ""
	c.call.uncertainRecoveryID = ""
	if result.Outcome == CommandRejected {
		if fingerprint.kind == CommandAnswerIncoming {
			if current.Ref != before.provider.Ref {
				closeSession = c.call.media
				c.call.media = nil
				c.call.publicMediaLeaseID = ""
				c.call.mediaActivationEpoch = 0
				c.call.phase = PhaseIncomingRinging
			} else {
				c.call.phase = PhaseMediaPreparing
			}
		} else {
			c.call.phase = PhaseActiveUnverified
		}
	} else if fingerprint.kind == CommandAnswerIncoming {
		c.call.phase = PhaseActiveUnverified
		c.call.controllerOwned = true
		activate = c.call.media
	} else {
		c.call.phase = PhaseEnded
		closeSession = c.call.media
		c.call.media = nil
		c.call.publicMediaLeaseID = ""
		c.call.mediaActivationEpoch = 0
	}
	c.mu.Unlock()
	closeMedia(closeSession)
	if activate != nil {
		c.activateMedia(before.public, activate)
	}
	call, err := c.Snapshot(before.public)
	if err != nil {
		return MutationResult{}, err
	}
	return MutationResult{CommandID: commandID, Outcome: result.Outcome, Call: call}, nil
}

func (c *Coordinator) markUnknown(
	commandID string,
	fingerprint coordinatorCommandFingerprint,
	recoveryID string,
	public PublicCallRef,
	cause error,
) (MutationResult, error) {
	c.commands[commandID] = coordinatorCommandRecord{fingerprint: fingerprint, outcome: CommandUnknown}
	c.mu.Lock()
	if c.call != nil && c.call.public == public {
		c.call.phase = PhaseReconciling
		c.call.uncertainCommandID = commandID
		c.call.uncertainCommandKind = fingerprint.kind
		c.call.uncertainRecoveryID = recoveryID
	}
	c.mu.Unlock()
	call, err := c.Snapshot(public)
	if err != nil {
		return MutationResult{}, errors.Join(ErrCommandOutcomeUnknown, cause, err)
	}
	return MutationResult{CommandID: commandID, Outcome: CommandUnknown, Call: call},
		errors.Join(ErrCommandOutcomeUnknown, cause)
}

// Reconcile is the only state-query path after an ambiguous command. It never
// executes or retries a mutation.
func (c *Coordinator) Reconcile(ctx context.Context, public PublicCallRef) (PublicCallSnapshot, error) {
	if err := contextError(ctx); err != nil {
		return PublicCallSnapshot{}, err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	current, err := c.requireCurrent(public, 0)
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	inspected, err := c.adapter.Inspect(ctx, current.provider.Ref)
	if err != nil {
		return PublicCallSnapshot{}, err
	}
	if err := inspected.Validate(); err != nil {
		return PublicCallSnapshot{}, err
	}
	if !inspected.Ref.SameDialog(current.provider.Ref) || inspected.Ref.Revision < current.provider.Ref.Revision {
		return PublicCallSnapshot{}, ErrStaleRevision
	}
	if inspected.Ref.Revision == current.provider.Ref.Revision && inspected.State != current.provider.State {
		return PublicCallSnapshot{}, ErrInvalidEvent
	}
	if inspected.Ref.Revision > current.provider.Ref.Revision &&
		!validProviderTransition(current.provider.State, inspected.State) {
		return PublicCallSnapshot{}, ErrInvalidEvent
	}
	if c.isClosed() {
		return PublicCallSnapshot{}, ErrClosed
	}

	resolvedUncertain := false
	var resolveErr error
	if hasUncertainMutation(current) &&
		((current.uncertainCommandKind == CommandAnswerIncoming && inspected.State == ProviderCallActive) ||
			inspected.State == ProviderCallEnded) {
		resolution := MutationResolution{Outcome: CommandUnknown, State: inspected.State}
		if !validOpaque(current.uncertainRecoveryID, 1, MaxMutationRecoveryIDBytes) ||
			resolution.Validate() != nil || c.journal == nil {
			resolveErr = ErrInvalidMutationJournalRecord
		} else {
			resolveErr = c.journal.Resolve(current.uncertainRecoveryID, resolution)
		}
		resolvedUncertain = resolveErr == nil
	}

	var activate MediaSession
	var closeSession MediaSession
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return PublicCallSnapshot{}, ErrClosed
	}
	if c.call == nil || c.call.public != public {
		c.mu.Unlock()
		return PublicCallSnapshot{}, ErrCallNotFound
	}
	previousRef := c.call.provider.Ref
	c.call.provider = inspected
	if hasUncertainMutation(*c.call) && !resolvedUncertain {
		switch inspected.State {
		case ProviderCallIncoming:
			if inspected.Ref != previousRef && c.call.media != nil {
				closeSession = c.call.media
				c.call.media = nil
				c.call.publicMediaLeaseID = ""
				c.call.mediaActivationEpoch = 0
				c.call.mediaActivationFailed = false
			}
		case ProviderCallEnded:
			closeSession = c.call.media
			c.call.media = nil
			c.call.publicMediaLeaseID = ""
			c.call.mediaActivationEpoch = 0
			c.call.mediaActivationFailed = false
		}
		c.call.phase = PhaseReconciling
		c.mu.Unlock()
		closeMedia(closeSession)
		call, snapshotErr := c.Snapshot(public)
		if resolveErr != nil {
			return call, errors.Join(ErrReconcileRequired, resolveErr, snapshotErr)
		}
		return call, snapshotErr
	}
	if resolvedUncertain {
		resolvedKind := c.call.uncertainCommandKind
		c.call.uncertainCommandID = ""
		c.call.uncertainCommandKind = ""
		c.call.uncertainRecoveryID = ""
		if resolvedKind == CommandAnswerIncoming && inspected.State == ProviderCallActive {
			c.call.controllerOwned = false
			c.call.phase = PhaseActiveUnverified
			activate = c.call.media
			c.mu.Unlock()
			if activate != nil {
				c.activateMedia(public, activate)
			}
			return c.Snapshot(public)
		}
	}
	switch inspected.State {
	case ProviderCallIncoming:
		if inspected.Ref != previousRef && c.call.media != nil {
			closeSession = c.call.media
			c.call.media = nil
			c.call.publicMediaLeaseID = ""
			c.call.mediaActivationEpoch = 0
			c.call.mediaActivationFailed = false
		}
		if hasUncertainMutation(*c.call) {
			c.call.phase = PhaseReconciling
		} else if c.call.media != nil {
			c.call.phase = PhaseMediaPreparing
		} else {
			c.call.phase = PhaseIncomingRinging
		}
	case ProviderCallActive:
		if c.call.controllerOwned ||
			(c.call.media != nil && c.call.phase == PhaseActiveUnverified) {
			c.call.phase = PhaseActiveUnverified
			activate = c.call.media
		} else {
			c.call.phase = PhaseActiveUnmanaged
			closeSession = c.call.media
			c.call.media = nil
			c.call.publicMediaLeaseID = ""
			c.call.mediaActivationEpoch = 0
		}
	case ProviderCallEnded:
		c.call.phase = PhaseEnded
		c.call.uncertainCommandID = ""
		c.call.uncertainCommandKind = ""
		c.call.uncertainRecoveryID = ""
		closeSession = c.call.media
		c.call.media = nil
		c.call.publicMediaLeaseID = ""
		c.call.mediaActivationEpoch = 0
	}
	c.mu.Unlock()
	closeMedia(closeSession)
	if activate != nil {
		c.activateMedia(public, activate)
	}
	return c.Snapshot(public)
}

// Snapshot recomputes media freshness from current post-activation counters.
func (c *Coordinator) Snapshot(public PublicCallRef) (PublicCallSnapshot, error) {
	if !validPublicCallRef(public) {
		return PublicCallSnapshot{}, ErrInvalidIdentity
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return PublicCallSnapshot{}, ErrClosed
	}
	if c.call == nil || c.call.public != public {
		c.mu.Unlock()
		return PublicCallSnapshot{}, ErrCallNotFound
	}
	state := cloneCoordinatorCall(c.call)
	c.mu.Unlock()
	return c.renderCall(state), nil
}

func (c *Coordinator) renderCall(state coordinatorCall) PublicCallSnapshot {
	result := PublicCallSnapshot{
		Call: state.public, Revision: state.provider.Ref.Revision, Phase: state.phase,
		Direction:         state.provider.Direction,
		ControllerOwned:   state.controllerOwned,
		ReconcileRequired: hasUncertainMutation(state),
	}
	if state.media == nil {
		return result
	}
	media := state.media.Snapshot()
	now := c.now()
	activationMatches := media.Activated && state.mediaActivationEpoch != 0 &&
		media.ActivationEpoch == state.mediaActivationEpoch
	activationFailed := state.mediaActivationFailed || (media.Activated && !activationMatches)
	gatewayFresh := activationMatches && !activationFailed &&
		media.GatewayToClientFresh(now, c.freshness)
	clientFresh := activationMatches && !activationFailed &&
		media.ClientToGatewayFresh(now, c.freshness)
	bidirectional := gatewayFresh && clientFresh
	result.Media = &PublicMediaSnapshot{
		LeaseID: state.publicMediaLeaseID, Codec: media.Codec, Prepared: media.Prepared && !media.Closed,
		Activated:            media.Activated && !media.Closed && activationMatches && !activationFailed,
		ActivationEpoch:      state.mediaActivationEpoch,
		GatewayToClientFresh: gatewayFresh, ClientToGatewayFresh: clientFresh,
		BidirectionalFresh:      bidirectional,
		GatewayToClientFrames:   media.GatewayToClientFrames,
		ClientToGatewayFrames:   media.ClientToGatewayFrames,
		GatewayToClientBaseline: media.GatewayToClientBaseline,
		ClientToGatewayBaseline: media.ClientToGatewayBaseline,
		ActivationFailed:        activationFailed,
	}
	if state.phase == PhaseMediaPreparing || state.phase == PhaseMediaReady {
		if result.Media.Prepared {
			result.Phase = PhaseMediaReady
		} else {
			result.Phase = PhaseMediaPreparing
		}
	}
	if state.phase == PhaseActiveUnverified && !activationFailed && bidirectional {
		result.Phase = PhaseActiveTransportVerified
	}
	return result
}

func (c *Coordinator) activateMedia(public PublicCallRef, media MediaSession) {
	c.mu.Lock()
	if c.call == nil || c.call.public != public || c.call.media != media {
		c.mu.Unlock()
		return
	}
	if c.call.mediaActivationEpoch == 0 {
		c.nextActivation++
		if c.nextActivation == 0 {
			c.nextActivation++
		}
		c.call.mediaActivationEpoch = c.nextActivation
	}
	epoch := c.call.mediaActivationEpoch
	c.mu.Unlock()
	activated, err := media.Activate(epoch)
	if err == nil && !validActivatedMedia(media, activated, epoch) {
		err = ErrMediaNotPrepared
	}
	c.mu.Lock()
	if c.call != nil && c.call.public == public && c.call.media == media {
		c.call.mediaActivationFailed = err != nil
	}
	c.mu.Unlock()
}

func (c *Coordinator) applyProviderStateLocked(
	current *coordinatorCall,
	snapshot CallSnapshot,
) (activate MediaSession, closeSession MediaSession) {
	previousRef := current.provider.Ref
	current.provider = snapshot
	// An asynchronous observation can update authoritative provider state and
	// release definitively dead media, but it cannot retire a durable pending
	// mutation. Only explicit Reconcile may call Journal.Resolve.
	if hasUncertainMutation(*current) {
		switch snapshot.State {
		case ProviderCallIncoming:
			if snapshot.Ref != previousRef && current.media != nil {
				closeSession = current.media
				current.media = nil
				current.publicMediaLeaseID = ""
				current.mediaActivationEpoch = 0
				current.mediaActivationFailed = false
			}
		case ProviderCallEnded:
			closeSession = current.media
			current.media = nil
			current.publicMediaLeaseID = ""
			current.mediaActivationEpoch = 0
			current.mediaActivationFailed = false
		}
		current.phase = PhaseReconciling
		return nil, closeSession
	}
	switch snapshot.State {
	case ProviderCallIncoming:
		if snapshot.Ref != previousRef && current.media != nil {
			closeSession = current.media
			current.media = nil
			current.publicMediaLeaseID = ""
			current.mediaActivationEpoch = 0
			current.mediaActivationFailed = false
		}
		if hasUncertainMutation(*current) {
			current.phase = PhaseReconciling
		} else if current.media != nil {
			current.phase = PhaseMediaPreparing
		} else {
			current.phase = PhaseIncomingRinging
		}
	case ProviderCallActive:
		if current.controllerOwned ||
			(current.media != nil && current.phase == PhaseActiveUnverified) {
			current.phase = PhaseActiveUnverified
			activate = current.media
		} else {
			current.phase = PhaseActiveUnmanaged
			closeSession = current.media
			current.media = nil
			current.publicMediaLeaseID = ""
			current.mediaActivationEpoch = 0
		}
	case ProviderCallEnded:
		current.phase = PhaseEnded
		closeSession = current.media
		current.media = nil
		current.publicMediaLeaseID = ""
		current.mediaActivationEpoch = 0
	}
	return activate, closeSession
}

func (c *Coordinator) requireCurrent(public PublicCallRef, revision uint64) (coordinatorCall, error) {
	if !validPublicCallRef(public) {
		return coordinatorCall{}, ErrInvalidIdentity
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return coordinatorCall{}, ErrClosed
	}
	if c.call == nil || c.call.public != public {
		return coordinatorCall{}, ErrCallNotFound
	}
	if revision != 0 && c.call.provider.Ref.Revision != revision {
		return coordinatorCall{}, ErrStaleRevision
	}
	return cloneCoordinatorCall(c.call), nil
}

func (c *Coordinator) newPublicCallID() (string, error) {
	buffer := make([]byte, publicCallIDBytes)
	if _, err := io.ReadFull(c.idReader, buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (c *Coordinator) newPublicMediaLeaseID() (string, error) {
	buffer := make([]byte, publicMediaIDBytes)
	if _, err := io.ReadFull(c.idReader, buffer); err != nil {
		return "", err
	}
	return publicMediaIDPrefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (c *Coordinator) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close invalidates current media before closing the adapter. It deliberately
// does not wait for opMu: adapter/media Close are the cancellation mechanism
// for a command, observation, inspection, or activation currently in flight.
// The short c.mu transition makes every later operation fail closed, while
// closeOnce makes concurrent callers wait for the same complete teardown.
func (c *Coordinator) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		media := mediaOf(c.call)
		c.call = nil
		c.mu.Unlock()
		mediaErr := error(nil)
		if media != nil {
			mediaErr = media.Close()
		}
		c.closeErr = errors.Join(mediaErr, c.adapter.Close())
		close(c.closeDone)
	})
	<-c.closeDone
	return c.closeErr
}

func validProviderTransition(from, to ProviderCallState) bool {
	switch from {
	case ProviderCallIncoming:
		return to == ProviderCallIncoming || to == ProviderCallActive || to == ProviderCallEnded
	case ProviderCallActive:
		return to == ProviderCallActive || to == ProviderCallEnded
	default:
		return false
	}
}

func validPreparedMedia(media MediaSession, call CallRef, requested []Codec) bool {
	if media == nil || !validOpaque(media.ID(), 1, 128) || media.CallRef() != call {
		return false
	}
	codec := media.Codec()
	requestedMatch := false
	for _, candidate := range requested {
		if candidate == codec {
			requestedMatch = true
			break
		}
	}
	if !requestedMatch {
		return false
	}
	snapshot := media.Snapshot()
	return validPreparedMediaSnapshot(media, call, snapshot)
}

func validPreparedMediaSnapshot(media MediaSession, call CallRef, snapshot MediaSnapshot) bool {
	return media != nil && snapshot.LeaseID == media.ID() && snapshot.Call == call &&
		media.CallRef() == call && snapshot.Codec == media.Codec() &&
		snapshot.Prepared && !snapshot.Closed && !snapshot.Activated
}

func sameMediaIdentity(wrapped, source MediaSession) bool {
	if wrapped == nil || source == nil || !validOpaque(wrapped.ID(), 1, 128) ||
		wrapped.ID() != source.ID() || wrapped.CallRef() != source.CallRef() ||
		wrapped.Codec() != source.Codec() {
		return false
	}
	snapshot := wrapped.Snapshot()
	return snapshot.LeaseID == source.ID() && snapshot.Call == source.CallRef() &&
		snapshot.Codec == source.Codec() && !snapshot.Closed && !snapshot.Activated
}

func validActivatedMedia(media MediaSession, snapshot MediaSnapshot, epoch uint64) bool {
	return media != nil && epoch != 0 && snapshot.LeaseID == media.ID() &&
		snapshot.Call == media.CallRef() && snapshot.Codec == media.Codec() &&
		snapshot.Prepared && snapshot.Activated && !snapshot.Closed &&
		snapshot.ActivationEpoch == epoch && !snapshot.ActivatedAt.IsZero()
}

func validPublicCallRef(public PublicCallRef) bool {
	return len(public.PublicCallID) == 43 && validToken(public.PublicCallID, 43, 43) && public.Generation != 0
}

func cloneCoordinatorCall(call *coordinatorCall) coordinatorCall {
	if call == nil {
		return coordinatorCall{}
	}
	return *call
}

func hasUncertainMutation(call coordinatorCall) bool {
	return call.uncertainCommandID != "" || call.uncertainRecoveryID != ""
}

func mediaOf(call *coordinatorCall) MediaSession {
	if call == nil {
		return nil
	}
	return call.media
}

func closeMedia(media MediaSession) {
	if media != nil {
		_ = media.Close()
	}
}

func pointerCall(call PublicCallSnapshot) *PublicCallSnapshot { return &call }
