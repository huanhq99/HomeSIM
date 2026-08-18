package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
)

const (
	remoteMediaOfferBodyLimit   = 96 << 10
	remoteMediaControlBodyLimit = 64 << 10
	remoteMediaDefaultOfferTTL  = 45 * time.Second
	remoteMediaDefaultActionTTL = 30 * time.Second
	remoteMediaDefaultRescueTTL = 30 * time.Second
	// The macOS UAC helper submits bounded batches of up to eight 20 ms
	// frames to amortize Unix-socket wakeups. Public TURNS/TCP can deliver RTP
	// in short bursts on cellular networks; four complete batches provide a
	// 640 ms jitter cushion without adding multi-second conversational delay.
	remoteMediaDefaultFrameQueue = 32
	remoteMediaPublicUDPMin      = 55000
	remoteMediaPublicUDPMax      = 55063
	remoteMediaMaxJSSafeInteger  = uint64(1<<53 - 1)
)

var (
	remoteMediaInterfacePattern         = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	remoteMediaDigestPattern            = regexp.MustCompile(`^[0-9a-f]{64}$`)
	remoteMediaSessionPattern           = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	remoteMediaPurposePattern           = regexp.MustCompile(`^(?:incoming|outgoing)$`)
	remoteMediaTailscaleIPv4            = mustRemoteMediaCIDR("100.64.0.0/10")
	remoteMediaTailscaleIPv6            = mustRemoteMediaCIDR("fd7a:115c:a1e0::/48")
	errRemoteMediaTransportsNotPrepared = errors.New("outgoing transports are not prepared")
)

func mustRemoteMediaCIDR(value string) *net.IPNet {
	_, network, err := net.ParseCIDR(value)
	if err != nil {
		panic(err)
	}
	return network
}

// remoteMediaRuntimeConfig is deliberately explicit and disabled by default.
// It selects exactly one network profile: the legacy Tailnet allowlists or a
// short-lived public TURN relay credential issuer. The two profiles must never
// be combined.
type remoteMediaRuntimeConfig struct {
	Interface   string
	LocalCIDRs  []string
	RemoteCIDRs []string
	UDPMin      uint16
	UDPMax      uint16
	IssueTURN   func() (*remotevoice.TURNRelayConfig, error) `json:"-"`
	RootDir     string
	ControlPath string
	OfferTTL    time.Duration
}

func (c remoteMediaRuntimeConfig) enabled() bool {
	return c.networkMode() != remoteMediaNetworkDisabled && c.RootDir != "" && c.ControlPath != ""
}

type remoteMediaNetworkMode uint8

const (
	remoteMediaNetworkDisabled remoteMediaNetworkMode = iota
	remoteMediaNetworkTailnet
	remoteMediaNetworkPublicTURN
)

func (c remoteMediaRuntimeConfig) networkMode() remoteMediaNetworkMode {
	tailnetAny := c.Interface != "" || len(c.LocalCIDRs) != 0 || len(c.RemoteCIDRs) != 0 ||
		c.UDPMin != 0 || c.UDPMax != 0
	if c.IssueTURN != nil {
		if tailnetAny {
			return remoteMediaNetworkDisabled
		}
		return remoteMediaNetworkPublicTURN
	}
	if c.Interface != "" && len(c.LocalCIDRs) > 0 && len(c.RemoteCIDRs) > 0 &&
		c.UDPMin != 0 && c.UDPMax >= c.UDPMin {
		return remoteMediaNetworkTailnet
	}
	return remoteMediaNetworkDisabled
}

type remoteMediaSource struct {
	Purpose                string
	Call                   remoteCallExpectation
	ExpectedCallGeneration uint64
	DialNumber             string
}

func (s remoteMediaSource) callGeneration() uint64 {
	if s.Purpose == "incoming" {
		return s.Call.CallGeneration
	}
	return s.ExpectedCallGeneration
}

type remoteMediaLease struct {
	coordinator *remotevoice.MediaLeaseCoordinator
	sessionID   string
	generation  uint64
	identity    string
	clientNonce string
	requestHash [sha256.Size]byte
	answerSDP   []byte
	expiresAt   time.Time
	offerTimer  *time.Timer
	actionTimer *time.Timer
	socketDir   string
	source      remoteMediaSource

	claimDevice             *usbAT
	claimIdentity           usbATPhysicalIdentity
	hostPrepared            bool
	uacUIDDigest            string
	activeTicket            callMediaTicket
	activationPending       bool
	actionPending           bool
	actionToken             uint64
	activationAt            time.Time
	activationEpoch         uint64
	hostCaptureFrames       uint64
	hostPlaybackFrames      uint64
	hostCaptureAt           time.Time
	hostPlaybackAt          time.Time
	moduleVoiceIdentityHash string
}

// remoteMediaRescueGrant is a short-lived, in-memory-only tombstone. It keeps
// just enough exact ownership to end a cellular call after the WebRTC or IPC
// transport has failed. It is never sufficient to answer, dial or send DTMF.
type remoteMediaRescueGrant struct {
	identity                string
	sessionID               string
	generation              uint64
	activationEpoch         uint64
	proofAt                 time.Time
	source                  remoteMediaSource
	activeTicket            callMediaTicket
	claimDevice             *usbAT
	claimIdentity           usbATPhysicalIdentity
	moduleVoiceIdentityHash string
	phase                   string
	token                   uint64
	attemptToken            uint64
	expiresAt               time.Time
	expirationTimer         *time.Timer
}

type remoteMediaStatus struct {
	Enabled             bool   `json:"enabled"`
	Phase               string `json:"phase"`
	TransportPrepared   bool   `json:"transport_prepared"`
	HostUACPrepared     bool   `json:"host_uac_prepared"`
	CallActionReady     bool   `json:"call_action_ready"`
	Activated           bool   `json:"activated"`
	BidirectionalFresh  bool   `json:"bidirectional_fresh"`
	LeaseGeneration     uint64 `json:"lease_generation,omitempty"`
	WebRTCConnected     bool   `json:"webrtc_connected"`
	BrowserTrackReady   bool   `json:"browser_track_ready"`
	BrowserReceiveProof bool   `json:"browser_receive_proof"`
	IPCReady            bool   `json:"ipc_ready"`
	HostCaptureFresh    bool   `json:"host_capture_fresh"`
	HostPlaybackFresh   bool   `json:"host_playback_fresh"`
	RescueHangupReady   bool   `json:"rescue_hangup_ready"`
	ICEPairEstablished  bool   `json:"ice_pair_established"`
	ICEPairPolicyValid  bool   `json:"ice_pair_policy_valid"`
	ICEProtocol         string `json:"ice_protocol,omitempty"`
	ICELocalType        string `json:"ice_local_candidate_type,omitempty"`
	ICERemoteType       string `json:"ice_remote_candidate_type,omitempty"`
	ICELocalTailnet     bool   `json:"ice_local_address_allowed"`
	ICERemoteTailnet    bool   `json:"ice_remote_address_allowed"`
}

// remoteMediaManager owns at most one in-memory media lease. SDP, candidates,
// session IDs, bearer tokens, CoreAudio UIDs, and phone numbers are never
// persisted or exposed through status snapshots.
type remoteMediaManager struct {
	offerMu sync.Mutex
	mu      sync.Mutex
	cfg     remoteMediaRuntimeConfig
	now     func() time.Time
	active  *remoteMediaLease
	rescue  *remoteMediaRescueGrant
	closed  bool
	// Test seams keep ownership/API tests free of live ICE and socket binds.
	// Production instances leave all three nil and use the secure defaults.
	answerer        remotevoice.MediaAnswerFunc
	startBroker     remotevoice.LocalPCMBrokerFactory
	validateNetwork func(remoteMediaRuntimeConfig) error
	validateUAC     func(uint32) error
	actionTTL       time.Duration
	rescueTTL       time.Duration
	nextActionToken uint64
	nextRescueToken uint64
}

func (m *remoteMediaManager) activeDiagnostics() (remotevoice.MediaLeaseSnapshot, bool) {
	if m == nil {
		return remotevoice.MediaLeaseSnapshot{}, false
	}
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil {
		m.mu.Unlock()
		return remotevoice.MediaLeaseSnapshot{}, false
	}
	coordinator := entry.coordinator
	m.mu.Unlock()
	if coordinator == nil {
		return remotevoice.MediaLeaseSnapshot{}, false
	}
	return coordinator.Snapshot(), true
}

func (*remoteMediaLease) String() string   { return "remoteMediaLease{redacted}" }
func (*remoteMediaLease) GoString() string { return "remoteMediaLease{redacted}" }
func (*remoteMediaManager) String() string { return "remoteMediaManager{redacted}" }
func (*remoteMediaManager) GoString() string {
	return "remoteMediaManager{redacted}"
}

func newRemoteMediaManager(cfg remoteMediaRuntimeConfig) (*remoteMediaManager, error) {
	if !cfg.enabled() {
		return nil, errors.New("remote media configuration is incomplete")
	}
	mode := cfg.networkMode()
	if cfg.OfferTTL == 0 {
		cfg.OfferTTL = remoteMediaDefaultOfferTTL
	}
	if cfg.OfferTTL < 10*time.Second || cfg.OfferTTL > 2*time.Minute {
		return nil, errors.New("remote media offer TTL must be between 10 seconds and 2 minutes")
	}
	if mode == remoteMediaNetworkTailnet {
		if !remoteMediaInterfacePattern.MatchString(cfg.Interface) {
			return nil, errors.New("remote media interface name is invalid")
		}
		if cfg.UDPMax-cfg.UDPMin > 1024 {
			return nil, errors.New("remote media UDP range must contain at most 1025 ports")
		}
		for _, values := range [][]string{cfg.LocalCIDRs, cfg.RemoteCIDRs} {
			for _, value := range values {
				_, network, err := net.ParseCIDR(value)
				if err != nil || !remoteMediaNetworkWithin(network, remoteMediaTailscaleIPv4) &&
					!remoteMediaNetworkWithin(network, remoteMediaTailscaleIPv6) {
					return nil, errors.New("remote media CIDRs must stay inside Tailscale ranges")
				}
			}
		}
		for _, value := range cfg.RemoteCIDRs {
			_, network, _ := net.ParseCIDR(value)
			ones, bits := network.Mask.Size()
			if ones != bits {
				return nil, errors.New("remote media peers must be exact /32 or /128 addresses")
			}
		}
		if len(cfg.RemoteCIDRs) != 1 {
			return nil, errors.New("remote media currently supports exactly one configured Tailnet peer")
		}
	}
	if !filepath.IsAbs(cfg.RootDir) || !filepath.IsAbs(cfg.ControlPath) ||
		filepath.Clean(cfg.RootDir) != cfg.RootDir || filepath.Clean(cfg.ControlPath) != cfg.ControlPath {
		return nil, errors.New("remote media paths must be clean absolute paths")
	}
	return &remoteMediaManager{
		cfg: cfg, now: time.Now, actionTTL: remoteMediaDefaultActionTTL,
		rescueTTL: remoteMediaDefaultRescueTTL,
	}, nil
}

func remoteMediaNetworkWithin(candidate, allowed *net.IPNet) bool {
	if candidate == nil || allowed == nil || !allowed.Contains(candidate.IP) {
		return false
	}
	candidateOnes, candidateBits := candidate.Mask.Size()
	allowedOnes, allowedBits := allowed.Mask.Size()
	return candidateBits == allowedBits && candidateOnes >= allowedOnes
}

type remoteMediaOfferRequest struct {
	Purpose                string                `json:"purpose"`
	Call                   remoteCallExpectation `json:"call,omitempty"`
	ExpectedCallGeneration uint64                `json:"expected_call_generation,omitempty"`
	Number                 string                `json:"number,omitempty"`
	ClientNonce            string                `json:"client_nonce"`
	SDPOffer               string                `json:"sdp_offer"`
}

type remoteMediaOfferResponse struct {
	SDPAnswer       string    `json:"sdp_answer"`
	MediaSessionID  string    `json:"media_session_id"`
	LeaseGeneration uint64    `json:"lease_generation"`
	CallGeneration  uint64    `json:"call_generation"`
	ExpiresAt       time.Time `json:"expires_at"`
	Phase           string    `json:"phase"`
}

func (m *remoteMediaManager) offer(
	ctx context.Context,
	identity string,
	body []byte,
	req remoteMediaOfferRequest,
	sourceCurrent func(remoteMediaSource) bool,
) (remoteMediaOfferResponse, error) {
	if m == nil || !m.cfg.enabled() {
		return remoteMediaOfferResponse{}, errors.New("remote media is disabled")
	}
	m.offerMu.Lock()
	defer m.offerMu.Unlock()
	identity = strings.ToLower(strings.TrimSpace(identity))
	source, sourceErr := remoteMediaSourceFromOffer(req)
	if identity == "" || sourceErr != nil || !remoteMediaPurposePattern.MatchString(req.Purpose) ||
		!remoteLedgerKeyPattern.MatchString(req.ClientNonce) || len(req.SDPOffer) == 0 ||
		len(req.SDPOffer) > remotevoice.MaxOfferBytes {
		return remoteMediaOfferResponse{}, errors.New("invalid generation-bound media offer")
	}
	if sourceCurrent == nil || !sourceCurrent(source) {
		return remoteMediaOfferResponse{}, errors.New("call state no longer matches the media offer")
	}
	mode := m.cfg.networkMode()
	if mode == remoteMediaNetworkTailnet {
		// Tailscale Serve forwards authenticated identity and app-capability
		// headers, but its HTTP proxy does not expose the original peer IP. The
		// single-peer contract prevents one authorized browser from inducing ICE
		// to probe another configured node.
		validateNetwork := m.validateNetwork
		if validateNetwork == nil {
			validateNetwork = validateRemoteMediaNetworkBinding
		}
		if err := validateNetwork(m.cfg); err != nil {
			return remoteMediaOfferResponse{}, err
		}
	}

	requestHash := sha256.Sum256(body)
	now := m.now()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return remoteMediaOfferResponse{}, errors.New("remote media manager is closed")
	}
	if current := m.active; current != nil &&
		(current.actionPending || current.activationPending || current.activationEpoch != 0) {
		if current.identity != identity || current.clientNonce != req.ClientNonce ||
			subtle.ConstantTimeCompare(current.requestHash[:], requestHash[:]) != 1 {
			m.mu.Unlock()
			return remoteMediaOfferResponse{}, errors.New("an in-flight call action owns the media gateway")
		}
		response := current.offerResponse()
		m.mu.Unlock()
		return response, nil
	}
	if current := m.active; current != nil && now.Before(current.expiresAt) &&
		current.identity == identity && current.clientNonce == req.ClientNonce {
		if subtle.ConstantTimeCompare(current.requestHash[:], requestHash[:]) != 1 {
			m.mu.Unlock()
			return remoteMediaOfferResponse{}, errors.New("client nonce already owns a different media offer")
		}
		response := current.offerResponse()
		m.mu.Unlock()
		return response, nil
	}
	old := m.active
	m.active = nil
	m.clearRescueGrantLocked()
	m.mu.Unlock()
	closeRemoteMediaLease(old)

	sessionID, generation, err := newRemoteMediaIdentity()
	if err != nil {
		return remoteMediaOfferResponse{}, err
	}
	socketDir, socketPath, err := createRemoteMediaSocketPath(m.cfg.RootDir)
	if err != nil {
		return remoteMediaOfferResponse{}, err
	}
	coordinator, err := remotevoice.NewMediaLeaseCoordinator(remotevoice.MediaLeaseConfig{
		MediaSessionID:              sessionID,
		LeaseGeneration:             generation,
		SocketPath:                  socketPath,
		FrameCapacity:               remoteMediaDefaultFrameQueue,
		IPCIOTimeout:                2 * time.Second,
		IPCAuthenticatedIdleTimeout: 30 * time.Second,
		// Cellular setup and CLCC confirmation commonly take 5-7 seconds.
		// Keep an actively exchanging browser transport valid across that gap.
		// Cellular setup plus CLCC confirmation regularly takes 12-15 seconds on
		// this module. Keep the already-bound media claim valid through that
		// measured interval so the Mac audio helper can activate it.
		FreshnessWindow: 30 * time.Second,
		Answerer:        m.answerer,
		StartBroker:     m.startBroker,
	})
	if err != nil {
		_ = os.Remove(socketDir)
		return remoteMediaOfferResponse{}, err
	}
	entry := &remoteMediaLease{
		coordinator: coordinator,
		sessionID:   sessionID,
		generation:  generation,
		identity:    identity,
		clientNonce: req.ClientNonce,
		requestHash: requestHash,
		expiresAt:   now.Add(m.cfg.OfferTTL),
		socketDir:   socketDir,
		source:      source,
	}
	mediaConfig := remotevoice.Config{AnswerTimeout: 10 * time.Second}
	switch mode {
	case remoteMediaNetworkTailnet:
		mediaConfig.AllowedInterfaces = []string{m.cfg.Interface}
		mediaConfig.AllowedLocalCIDRs = append([]string(nil), m.cfg.LocalCIDRs...)
		mediaConfig.AllowedRemoteCIDRs = []string{m.cfg.RemoteCIDRs[0]}
		mediaConfig.UDPMin = m.cfg.UDPMin
		mediaConfig.UDPMax = m.cfg.UDPMax
	case remoteMediaNetworkPublicTURN:
		relay, issueErr := m.cfg.IssueTURN()
		if issueErr != nil || relay == nil {
			closeRemoteMediaLease(entry)
			return remoteMediaOfferResponse{}, errors.New("remote media TURN credential issuance failed")
		}
		relayCopy := *relay
		relayCopy.URLs = append([]string(nil), relay.URLs...)
		mediaConfig.TURNRelay = &relayCopy
		mediaConfig.UDPMin = remoteMediaPublicUDPMin
		mediaConfig.UDPMax = remoteMediaPublicUDPMax
	default:
		closeRemoteMediaLease(entry)
		return remoteMediaOfferResponse{}, errors.New("remote media network profile is disabled")
	}
	answer, err := coordinator.Answer(ctx, []byte(req.SDPOffer), mediaConfig)
	if err != nil {
		closeRemoteMediaLease(entry)
		return remoteMediaOfferResponse{}, err
	}
	entry.answerSDP = append([]byte(nil), answer...)
	entry.offerTimer = time.AfterFunc(time.Until(entry.expiresAt), func() {
		m.expireOfferIfGeneration(entry.generation)
	})

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		closeRemoteMediaLease(entry)
		return remoteMediaOfferResponse{}, errors.New("remote media manager is closed")
	}
	// A concurrent newer offer wins. Never publish this older lease afterward.
	if m.active != nil {
		m.mu.Unlock()
		closeRemoteMediaLease(entry)
		return remoteMediaOfferResponse{}, errors.New("a newer media offer already owns the gateway")
	}
	m.active = entry
	m.mu.Unlock()
	go func(generation uint64, coordinator *remotevoice.MediaLeaseCoordinator) {
		_ = coordinator.Wait(context.Background())
		m.closeAfterCoordinatorEnd(generation, coordinator.Snapshot().CloseReason)
	}(entry.generation, entry.coordinator)
	return entry.offerResponse(), nil
}

func remoteMediaSourceFromOffer(req remoteMediaOfferRequest) (remoteMediaSource, error) {
	switch req.Purpose {
	case "incoming":
		if !validRemoteCallExpectation(req.Call) || req.Call.CallDirection != "incoming" ||
			req.ExpectedCallGeneration != 0 || req.Number != "" {
			return remoteMediaSource{}, errors.New("invalid incoming media source")
		}
		return remoteMediaSource{Purpose: "incoming", Call: req.Call}, nil
	case "outgoing":
		number := normalizeDialNumber(req.Number)
		if req.Call != (remoteCallExpectation{}) || req.ExpectedCallGeneration == 0 ||
			req.ExpectedCallGeneration > remoteMediaMaxJSSafeInteger || number == "" {
			return remoteMediaSource{}, errors.New("invalid outgoing media source")
		}
		return remoteMediaSource{
			Purpose: "outgoing", ExpectedCallGeneration: req.ExpectedCallGeneration,
			DialNumber: number,
		}, nil
	default:
		return remoteMediaSource{}, errors.New("unknown media purpose")
	}
}

// remoteMediaAvailableForBrowser keeps the two browser transports honest when
// one process serves both listeners. A public-TURN manager belongs only to the
// Cloudflare listener; the legacy Tailnet manager belongs only to Tailscale.
func (a *app) remoteMediaAvailableForBrowser(browser remoteBrowserContext) bool {
	if a == nil || a.remoteMedia == nil || browser.SMSOnly {
		return false
	}
	switch a.remoteMedia.cfg.networkMode() {
	case remoteMediaNetworkTailnet:
		return browser.Transport == "tailscale-serve"
	case remoteMediaNetworkPublicTURN:
		return browser.Transport == "cloudflare-access"
	default:
		return false
	}
}

func (l *remoteMediaLease) offerResponse() remoteMediaOfferResponse {
	if l == nil {
		return remoteMediaOfferResponse{}
	}
	return remoteMediaOfferResponse{
		SDPAnswer: string(l.answerSDP), MediaSessionID: l.sessionID,
		LeaseGeneration: l.generation, CallGeneration: l.source.callGeneration(),
		ExpiresAt: l.expiresAt, Phase: "negotiating",
	}
}

func (m *remoteMediaManager) snapshot(
	identity string,
	sourceCurrent func(remoteMediaSource) bool,
	activeCurrent func(callMediaTicket) bool,
) remoteMediaStatus {
	if m == nil {
		return remoteMediaStatus{Enabled: false, Phase: "disabled"}
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	m.mu.Lock()
	entry := m.active
	now := m.now()
	rescueReady, rescueGeneration, rescueTicket := m.rescueStatusLocked(now, identity)
	if m.closed || entry == nil {
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return remoteMediaStatus{Enabled: false, Phase: "disabled"}
		}
		if rescueReady && activeCurrent != nil && activeCurrent(rescueTicket) {
			return remoteMediaStatus{
				Enabled: true, Phase: "rescue_only", LeaseGeneration: rescueGeneration,
				RescueHangupReady: true,
			}
		}
		return remoteMediaStatus{Enabled: true, Phase: "idle"}
	}
	hostPrepared := entry.hostPrepared
	expiresAt := entry.expiresAt
	generation := entry.generation
	source := entry.source
	activeTicket := entry.activeTicket
	actionPending := entry.actionPending
	activationPending := entry.activationPending
	activationEpoch := entry.activationEpoch
	hostCaptureFrames := entry.hostCaptureFrames
	hostPlaybackFrames := entry.hostPlaybackFrames
	hostCaptureAt := entry.hostCaptureAt
	hostPlaybackAt := entry.hostPlaybackAt
	coordinator := entry.coordinator
	m.mu.Unlock()
	if activationEpoch == 0 && !actionPending && !activationPending && !now.Before(expiresAt) {
		m.closeIfGeneration(generation)
		return remoteMediaStatus{Enabled: true, Phase: "expired"}
	}
	callOwnerCurrent := false
	if actionPending {
		// ATA may already have changed CLCC from ringing to active while Swift is
		// still requesting activation. The exact action timer owns this bounded
		// transition; neither cached topology callback is authoritative here.
		callOwnerCurrent = true
	} else if activationEpoch == 0 && !activationPending {
		callOwnerCurrent = sourceCurrent != nil && sourceCurrent(source)
	} else {
		callOwnerCurrent = activeCurrent != nil && activeCurrent(activeTicket)
	}
	if !callOwnerCurrent {
		m.closeIfGeneration(generation)
		return remoteMediaStatus{Enabled: true, Phase: "stale_call"}
	}
	media := coordinator.Snapshot()
	hostCaptureFresh := activationEpoch != 0 && hostCaptureFrames > 0 &&
		!hostCaptureAt.IsZero() && !now.Before(hostCaptureAt) && now.Sub(hostCaptureAt) <= 2*time.Second
	hostPlaybackFresh := activationEpoch != 0 && hostPlaybackFrames > 0 &&
		!hostPlaybackAt.IsZero() && !now.Before(hostPlaybackAt) && now.Sub(hostPlaybackAt) <= 2*time.Second
	callReady := activationEpoch == 0 && !actionPending && !activationPending &&
		media.TransportPrepared && hostPrepared && callOwnerCurrent
	if media.ActiveFresh && hostCaptureFresh && hostPlaybackFresh {
		m.maybeLatchRescueProof(entry, media, now)
		m.mu.Lock()
		rescueReady, rescueGeneration, rescueTicket = m.rescueStatusLocked(now, identity)
		m.mu.Unlock()
	}
	rescueReady = rescueReady && activeCurrent != nil && activeCurrent(rescueTicket)
	return remoteMediaStatus{
		Enabled: true, Phase: string(media.Phase), LeaseGeneration: generation,
		TransportPrepared: media.TransportPrepared, HostUACPrepared: hostPrepared,
		CallActionReady: callReady, Activated: media.Activated,
		BidirectionalFresh:  media.ActiveFresh && hostCaptureFresh && hostPlaybackFresh,
		WebRTCConnected:     media.WebRTCConnected,
		BrowserTrackReady:   media.WebRTCRemoteTrackReady,
		BrowserReceiveProof: media.WebRTCReceiverReportSeen, IPCReady: media.IPCReady,
		HostCaptureFresh: hostCaptureFresh, HostPlaybackFresh: hostPlaybackFresh,
		RescueHangupReady:  rescueReady && rescueGeneration == generation,
		ICEPairEstablished: media.WebRTCSelectedPair.Established,
		ICEPairPolicyValid: media.WebRTCSelectedPair.PolicyValid(),
		ICEProtocol:        string(media.WebRTCSelectedPair.Protocol),
		ICELocalType:       string(media.WebRTCSelectedPair.LocalCandidateType),
		ICERemoteType:      string(media.WebRTCSelectedPair.RemoteCandidateType),
		ICELocalTailnet:    media.WebRTCSelectedPair.LocalAddressAllowed,
		ICERemoteTailnet:   media.WebRTCSelectedPair.RemoteAddressAllowed,
	}
}

// authorizeIncomingAction is a live, in-memory gate. It intentionally returns
// no reusable bearer: the caller must run it again immediately before ATA in
// the lifecycle-pinned AT session.
func (m *remoteMediaManager) authorizeIncomingAction(
	identity string,
	mediaSessionID string,
	generation uint64,
	expected remoteCallExpectation,
) error {
	if m == nil || !validRemoteCallExpectation(expected) || expected.CallDirection != "incoming" {
		return errors.New("invalid incoming media owner")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || entry.identity != identity ||
		entry.source.Purpose != "incoming" || entry.source.Call != expected ||
		!entry.coordinator.Owns(mediaSessionID, generation) ||
		!entry.hostPrepared || !remoteMediaDigestPattern.MatchString(entry.uacUIDDigest) ||
		entry.claimDevice == nil || entry.actionPending || entry.activationPending || entry.activationEpoch != 0 ||
		!m.now().Before(entry.expiresAt) {
		m.mu.Unlock()
		return errors.New("remote media lease does not own this ringing call")
	}
	coordinator := entry.coordinator
	m.mu.Unlock()
	if !coordinator.Snapshot().TransportPrepared {
		return errors.New("remote media transports are not currently prepared")
	}
	return nil
}

func remoteMediaSourceMatchesOutgoing(source remoteMediaSource, expectedGeneration uint64, number string) bool {
	return source.Purpose == "outgoing" && expectedGeneration != 0 &&
		source.ExpectedCallGeneration == expectedGeneration && source.DialNumber == normalizeDialNumber(number) &&
		source.DialNumber != ""
}

func (m *remoteMediaManager) authorizeOutgoingAction(
	identity string,
	mediaSessionID string,
	generation uint64,
	expectedCallGeneration uint64,
	number string,
) error {
	if m == nil || expectedCallGeneration == 0 || normalizeDialNumber(number) == "" {
		return errors.New("invalid outgoing media owner")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil {
		m.mu.Unlock()
		return errors.New("outgoing lease missing")
	}
	if entry.identity != identity {
		m.mu.Unlock()
		return errors.New("outgoing identity changed")
	}
	if !remoteMediaSourceMatchesOutgoing(entry.source, expectedCallGeneration, number) {
		m.mu.Unlock()
		return errors.New("outgoing source changed")
	}
	if !entry.coordinator.Owns(mediaSessionID, generation) {
		m.mu.Unlock()
		return errors.New("outgoing coordinator changed")
	}
	if !entry.hostPrepared || !remoteMediaDigestPattern.MatchString(entry.uacUIDDigest) || entry.claimDevice == nil {
		m.mu.Unlock()
		return errors.New("outgoing host is not prepared")
	}
	if entry.actionPending || entry.activationPending || entry.activationEpoch != 0 {
		m.mu.Unlock()
		return errors.New("outgoing action is already pending")
	}
	if !m.now().Before(entry.expiresAt) {
		m.mu.Unlock()
		return errors.New("outgoing lease expired")
	}
	coordinator := entry.coordinator
	m.mu.Unlock()
	if !coordinator.Snapshot().TransportPrepared {
		return errRemoteMediaTransportsNotPrepared
	}
	return nil
}

func (m *remoteMediaManager) waitOutgoingPrepared(
	ctx context.Context,
	identity string,
	mediaSessionID string,
	generation uint64,
	expectedCallGeneration uint64,
	number string,
) error {
	if ctx == nil {
		return errors.New("invalid outgoing preparation wait")
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	attempts := 0
	for {
		err := m.authorizeOutgoingAction(
			identity, mediaSessionID, generation, expectedCallGeneration, number,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errRemoteMediaTransportsNotPrepared) {
			return err
		}
		attempts++
		if attempts%40 == 0 {
			m.mu.Lock()
			entry := m.active
			var coordinator *remotevoice.MediaLeaseCoordinator
			if entry != nil && entry.coordinator.Owns(mediaSessionID, generation) {
				coordinator = entry.coordinator
			}
			m.mu.Unlock()
			if coordinator != nil {
				snapshot := coordinator.Snapshot()
				log.Printf(
					"remote dial media wait: connected=%t pair=%t pair_profile=%q pair_protocol=%q pair_local=%q pair_remote=%q local_track=%t remote_track=%t rr=%t ipc_claimed=%t ipc_consumed=%t ipc_ready=%t ipc_live=%t",
					snapshot.WebRTCConnected,
					snapshot.WebRTCSelectedPair.PolicyValid(),
					snapshot.WebRTCSelectedPair.Profile,
					snapshot.WebRTCSelectedPair.Protocol,
					snapshot.WebRTCSelectedPair.LocalCandidateType,
					snapshot.WebRTCSelectedPair.RemoteCandidateType,
					snapshot.WebRTCLocalTrackStarted,
					snapshot.WebRTCRemoteTrackReady,
					snapshot.WebRTCReceiverReportSeen,
					snapshot.IPCClaimed,
					snapshot.IPCConsumed,
					snapshot.IPCReady,
					snapshot.IPCLive,
				)
			}
		}
		select {
		case <-ctx.Done():
			return errRemoteMediaTransportsNotPrepared
		case <-ticker.C:
		}
	}
}

// waitHostPrepared closes the only race between returning the WebRTC answer
// and the one-second macOS helper poll. The browser must not be told that an
// outgoing media session is ready until the exact UAC/PCM host has claimed and
// acknowledged the same lease.
func (m *remoteMediaManager) waitHostPrepared(
	ctx context.Context,
	identity string,
	mediaSessionID string,
	generation uint64,
) error {
	if m == nil || ctx == nil || identity == "" || mediaSessionID == "" || generation == 0 {
		return errors.New("invalid remote media host wait")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		entry := m.active
		ready := !m.closed && entry != nil && entry.identity == identity &&
			entry.coordinator.Owns(mediaSessionID, generation) && entry.hostPrepared &&
			remoteMediaDigestPattern.MatchString(entry.uacUIDDigest) && entry.claimDevice != nil &&
			!entry.actionPending && !entry.activationPending && entry.activationEpoch == 0 &&
			m.now().Before(entry.expiresAt)
		current := !m.closed && entry != nil && entry.identity == identity &&
			entry.coordinator.Owns(mediaSessionID, generation) && m.now().Before(entry.expiresAt)
		m.mu.Unlock()
		if ready {
			return nil
		}
		if !current {
			return errors.New("remote media lease changed before host preparation")
		}
		select {
		case <-ctx.Done():
			return errors.New("remote media host preparation timed out")
		case <-ticker.C:
		}
	}
}

type remoteMediaActionReservation struct {
	generation uint64
	token      uint64
}

func (r remoteMediaActionReservation) valid() bool {
	return r.generation != 0 && r.token != 0
}

// beginIncomingAction is the final call-mutation gate. It is invoked from the
// lifecycle-pinned AT callback after fresh CLCC/QPCMV validation and therefore
// compares both the exact *usbAT and the immutable physical identity claimed by
// Swift. Descriptor validation deliberately happens without m.mu held.
func (m *remoteMediaManager) beginIncomingAction(
	identity string,
	mediaSessionID string,
	generation uint64,
	expected remoteCallExpectation,
	device *usbAT,
	pinnedIdentity usbATPhysicalIdentity,
) (remoteMediaActionReservation, error) {
	if m == nil || device == nil || !validRemoteCallExpectation(expected) ||
		expected.CallDirection != "incoming" || pinnedIdentity.VendorID != quectelUSBVendorID ||
		pinnedIdentity.ProductID != quectelUSBProductID || pinnedIdentity.Location == 0 {
		return remoteMediaActionReservation{}, errors.New("invalid exact incoming media owner")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))

	// Capture only a non-owning coordinator pointer, then repeat every ownership
	// check under m.mu after the potentially slow UAC descriptor walk.
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || !entry.coordinator.Owns(mediaSessionID, generation) {
		m.mu.Unlock()
		return remoteMediaActionReservation{}, errors.New("remote media lease changed before call action")
	}
	coordinator := entry.coordinator
	m.mu.Unlock()
	validateUAC := m.validateUAC
	if validateUAC == nil {
		validateUAC = validateDirectUACUSB
	}
	if err := validateUAC(pinnedIdentity.Location); err != nil {
		return remoteMediaActionReservation{}, errors.New("exact target UAC descriptor is unavailable")
	}
	if !coordinator.Snapshot().TransportPrepared {
		return remoteMediaActionReservation{}, errors.New("remote media transports are not currently prepared")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	entry = m.active
	if m.closed || entry == nil || entry.identity != identity ||
		entry.source.Purpose != "incoming" || entry.source.Call != expected ||
		!entry.coordinator.Owns(mediaSessionID, generation) || entry.coordinator != coordinator ||
		!entry.hostPrepared || !remoteMediaDigestPattern.MatchString(entry.uacUIDDigest) ||
		entry.claimDevice != device || entry.claimIdentity != pinnedIdentity ||
		entry.actionPending || entry.activationPending || entry.activationEpoch != 0 ||
		!m.now().Before(entry.expiresAt) {
		return remoteMediaActionReservation{}, errors.New("remote media lease does not own the pinned call action")
	}
	if entry.offerTimer != nil {
		entry.offerTimer.Stop()
		entry.offerTimer = nil
	}
	m.nextActionToken++
	if m.nextActionToken == 0 {
		m.nextActionToken++
	}
	entry.actionPending = true
	entry.actionToken = m.nextActionToken
	return remoteMediaActionReservation{generation: entry.generation, token: entry.actionToken}, nil
}

func (m *remoteMediaManager) beginOutgoingAction(
	identity string,
	mediaSessionID string,
	generation uint64,
	expectedCallGeneration uint64,
	number string,
	device *usbAT,
	pinnedIdentity usbATPhysicalIdentity,
) (remoteMediaActionReservation, error) {
	if m == nil || device == nil || expectedCallGeneration == 0 || normalizeDialNumber(number) == "" ||
		pinnedIdentity.VendorID != quectelUSBVendorID || pinnedIdentity.ProductID != quectelUSBProductID ||
		pinnedIdentity.Location == 0 {
		return remoteMediaActionReservation{}, errors.New("invalid exact outgoing media owner")
	}
	identity = strings.ToLower(strings.TrimSpace(identity))
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || !entry.coordinator.Owns(mediaSessionID, generation) {
		m.mu.Unlock()
		return remoteMediaActionReservation{}, errors.New("remote media lease changed before call action")
	}
	coordinator := entry.coordinator
	m.mu.Unlock()
	validateUAC := m.validateUAC
	if validateUAC == nil {
		validateUAC = validateDirectUACUSB
	}
	if err := validateUAC(pinnedIdentity.Location); err != nil {
		return remoteMediaActionReservation{}, errors.New("exact target UAC descriptor is unavailable")
	}
	if !coordinator.Snapshot().TransportPrepared {
		return remoteMediaActionReservation{}, errors.New("remote media transports are not currently prepared")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	entry = m.active
	if m.closed || entry == nil || entry.identity != identity ||
		!remoteMediaSourceMatchesOutgoing(entry.source, expectedCallGeneration, number) ||
		!entry.coordinator.Owns(mediaSessionID, generation) || entry.coordinator != coordinator ||
		!entry.hostPrepared || !remoteMediaDigestPattern.MatchString(entry.uacUIDDigest) ||
		entry.claimDevice != device || entry.claimIdentity != pinnedIdentity ||
		entry.actionPending || entry.activationPending || entry.activationEpoch != 0 ||
		!m.now().Before(entry.expiresAt) {
		return remoteMediaActionReservation{}, errors.New("remote media lease does not own the pinned outgoing action")
	}
	if entry.offerTimer != nil {
		entry.offerTimer.Stop()
		entry.offerTimer = nil
	}
	m.nextActionToken++
	if m.nextActionToken == 0 {
		m.nextActionToken++
	}
	entry.actionPending = true
	entry.actionToken = m.nextActionToken
	return remoteMediaActionReservation{generation: entry.generation, token: entry.actionToken}, nil
}

// finishIncomingAction closes an explicitly rejected/non-executed reservation.
// Explicit OK and unknown outcomes keep the exact reservation alive only until
// Swift activates it or the bounded action timer expires.
func (m *remoteMediaManager) finishIncomingAction(reservation remoteMediaActionReservation, keep bool) {
	if m == nil || !reservation.valid() {
		return
	}
	m.mu.Lock()
	entry := m.active
	if entry == nil || entry.generation != reservation.generation ||
		!entry.actionPending || entry.actionToken != reservation.token {
		m.mu.Unlock()
		return
	}
	if !keep {
		m.active = nil
		m.mu.Unlock()
		closeRemoteMediaLease(entry)
		return
	}
	ttl := m.actionTTL
	if ttl <= 0 {
		ttl = remoteMediaDefaultActionTTL
	}
	if entry.actionTimer != nil {
		entry.actionTimer.Stop()
	}
	entry.actionTimer = time.AfterFunc(ttl, func() {
		m.expireActionIfReservation(reservation)
	})
	m.mu.Unlock()
}

func (m *remoteMediaManager) expireActionIfReservation(reservation remoteMediaActionReservation) {
	if m == nil || !reservation.valid() {
		return
	}
	m.mu.Lock()
	entry := m.active
	if entry == nil || entry.generation != reservation.generation ||
		!entry.actionPending || entry.actionToken != reservation.token {
		m.mu.Unlock()
		return
	}
	m.active = nil
	m.mu.Unlock()
	closeRemoteMediaLease(entry)
}

func (m *remoteMediaManager) activate(
	mediaSessionID string,
	generation uint64,
	ticket callMediaTicket,
	moduleIdentityHash string,
) (uint64, error) {
	if m == nil || ticket.Generation == 0 || ticket.CallID == "" || ticket.Index < 0 ||
		moduleIdentityHash == "" {
		return 0, errors.New("invalid active media owner")
	}
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || !entry.coordinator.Owns(mediaSessionID, generation) ||
		!entry.hostPrepared || entry.claimDevice == nil || !remoteMediaSourceAllowsTicket(entry.source, ticket) {
		m.mu.Unlock()
		return 0, errors.New("remote media lease does not own the active call")
	}
	if entry.activationEpoch != 0 {
		epoch := entry.activationEpoch
		matches := entry.activeTicket == ticket
		m.mu.Unlock()
		if !matches {
			return 0, errors.New("remote media lease is active for another call generation")
		}
		return epoch, nil
	}
	if entry.activationPending {
		m.mu.Unlock()
		return 0, errors.New("remote media activation is already in progress")
	}
	if !entry.actionPending || entry.actionToken == 0 {
		m.mu.Unlock()
		return 0, errors.New("remote media lease has no completed call action")
	}
	if entry.offerTimer != nil {
		entry.offerTimer.Stop()
		entry.offerTimer = nil
	}
	if entry.actionTimer != nil {
		entry.actionTimer.Stop()
		entry.actionTimer = nil
	}
	// Publish the activation transition while holding the same mutex used by
	// the offer-expiry callback. A timer that has fired but is waiting for this
	// lock must not tear down a lease whose active-call validation has already
	// begun.
	entry.activeTicket = ticket
	entry.actionPending = false
	entry.actionToken = 0
	entry.activationPending = true
	coordinator := entry.coordinator
	m.mu.Unlock()
	if !coordinator.Snapshot().TransportPrepared {
		m.closeIfGeneration(generation)
		return 0, errors.New("remote media transport is not prepared at activation")
	}

	activation, err := coordinator.Activate()
	if err != nil || !activation.Activated || activation.ActivationEpoch == 0 {
		m.closeIfGeneration(generation)
		return 0, errors.New("remote media activation was refused")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active != entry || !entry.coordinator.Owns(mediaSessionID, generation) {
		return 0, errors.New("remote media lease changed during activation")
	}
	entry.activationPending = false
	entry.activationAt = activation.ActivatedAt
	entry.activationEpoch = activation.ActivationEpoch
	entry.moduleVoiceIdentityHash = moduleIdentityHash
	return entry.activationEpoch, nil
}

func remoteMediaSourceAllowsTicket(source remoteMediaSource, ticket callMediaTicket) bool {
	switch source.Purpose {
	case "incoming":
		return source.Call.CallID == ticket.CallID && source.Call.CallIndex == ticket.Index &&
			source.Call.CallDirection == ticket.Direction
	case "outgoing":
		return ticket.Direction == "outgoing" && ticket.Generation > source.ExpectedCallGeneration
	default:
		return false
	}
}

func (m *remoteMediaManager) markHostActivity(
	mediaSessionID string,
	generation uint64,
	activationEpoch uint64,
	captureFrames uint64,
	playbackFrames uint64,
) error {
	if m == nil || activationEpoch == 0 || (captureFrames == 0 && playbackFrames == 0) {
		return errors.New("invalid host media activity")
	}
	m.mu.Lock()
	entry := m.active
	if m.closed || entry == nil || !entry.coordinator.Owns(mediaSessionID, generation) ||
		entry.activationEpoch != activationEpoch || entry.activationAt.IsZero() ||
		captureFrames < entry.hostCaptureFrames || playbackFrames < entry.hostPlaybackFrames {
		m.mu.Unlock()
		return errors.New("host media activity does not own the active lease")
	}
	now := m.now()
	if now.Before(entry.activationAt) {
		m.mu.Unlock()
		return errors.New("host media activity predates activation")
	}
	// 8 kHz mono counters are cumulative sample frames. Allow two times the
	// nominal rate plus a four-second scheduling cushion, but reject absurd
	// jumps which could otherwise manufacture freshness through a corrupt host.
	elapsedSeconds := now.Sub(entry.activationAt).Seconds()
	maximumFrames := uint64((elapsedSeconds + 4) * 16000)
	if captureFrames > maximumFrames || playbackFrames > maximumFrames {
		m.mu.Unlock()
		return errors.New("host media activity exceeds its physical rate bound")
	}
	if captureFrames > entry.hostCaptureFrames {
		entry.hostCaptureFrames = captureFrames
		entry.hostCaptureAt = now
	}
	if playbackFrames > entry.hostPlaybackFrames {
		entry.hostPlaybackFrames = playbackFrames
		entry.hostPlaybackAt = now
	}
	// Latch the rescue proof on the activity path itself. A status endpoint is
	// observability only and must never be required to create safety authority.
	m.maybeLatchRescueProofLocked(entry, entry.coordinator.Snapshot(), now)
	m.mu.Unlock()
	return nil
}

func (m *remoteMediaManager) expireOfferIfGeneration(generation uint64) {
	if m == nil || generation == 0 {
		return
	}
	m.mu.Lock()
	entry := m.active
	if entry == nil || entry.generation != generation || entry.actionPending ||
		entry.activationPending || entry.activationEpoch != 0 {
		m.mu.Unlock()
		return
	}
	if m.now().Before(entry.expiresAt) {
		m.mu.Unlock()
		return
	}
	m.active = nil
	m.mu.Unlock()
	closeRemoteMediaLease(entry)
}

func (m *remoteMediaManager) closeIfGeneration(generation uint64) {
	if m == nil || generation == 0 {
		return
	}
	m.mu.Lock()
	entry := m.active
	if entry == nil || entry.generation != generation {
		m.mu.Unlock()
		return
	}
	m.active = nil
	if grant := m.rescue; grant != nil && grant.generation == generation {
		m.clearRescueGrantLocked()
	}
	m.mu.Unlock()
	closeRemoteMediaLease(entry)
}

func (m *remoteMediaManager) closeAfterCoordinatorEnd(
	generation uint64,
	reason remotevoice.MediaLeaseCloseReason,
) {
	if m == nil || generation == 0 {
		return
	}
	m.mu.Lock()
	entry := m.active
	if entry == nil || entry.generation != generation {
		m.mu.Unlock()
		return
	}
	m.active = nil
	m.retireRescueGrantAfterTransportEndLocked(entry, reason)
	m.mu.Unlock()
	closeRemoteMediaLease(entry)
}

func (m *remoteMediaManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	m.closed = true
	entry := m.active
	m.active = nil
	m.clearRescueGrantLocked()
	m.mu.Unlock()
	closeRemoteMediaLease(entry)
	return nil
}

func closeRemoteMediaLease(entry *remoteMediaLease) {
	if entry == nil {
		return
	}
	if entry.offerTimer != nil {
		entry.offerTimer.Stop()
	}
	if entry.actionTimer != nil {
		entry.actionTimer.Stop()
	}
	if entry.coordinator != nil {
		_ = entry.coordinator.Close()
	}
	if entry.socketDir != "" {
		// The broker owns and unlinks the only socket. Remove only the exact,
		// now-empty per-lease directory; never recurse through runtime data.
		_ = os.Remove(entry.socketDir)
	}
}

func newRemoteMediaIdentity() (string, uint64, error) {
	random := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, random); err != nil {
		return "", 0, fmt.Errorf("create remote media identity: %w", err)
	}
	generationBytes := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, generationBytes); err != nil {
		return "", 0, fmt.Errorf("create remote media generation: %w", err)
	}
	generation := binary.BigEndian.Uint64(generationBytes)
	generation &= remoteMediaMaxJSSafeInteger
	if generation == 0 {
		generation = 1
	}
	return base64.RawURLEncoding.EncodeToString(random), generation, nil
}

func createRemoteMediaSocketPath(root string) (string, string, error) {
	if err := ensurePrivateRuntimeDirectory(root); err != nil {
		return "", "", err
	}
	for attempts := 0; attempts < 16; attempts++ {
		random := make([]byte, 4)
		if _, err := io.ReadFull(rand.Reader, random); err != nil {
			return "", "", err
		}
		dir := filepath.Join(root, "l-"+hex.EncodeToString(random))
		if err := os.Mkdir(dir, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", "", fmt.Errorf("create remote media lease directory: %w", err)
		}
		path := filepath.Join(dir, "p.sock")
		if len([]byte(path)) > 103 {
			_ = os.Remove(dir)
			return "", "", errors.New("remote media PCM socket path exceeds macOS limit")
		}
		return dir, path, nil
	}
	return "", "", errors.New("could not allocate a unique remote media lease directory")
}

func ensurePrivateRuntimeDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("runtime directory must be a clean absolute path")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private runtime directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure private runtime directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("private runtime directory failed ownership/type/mode validation")
	}
	if !fileOwnedByCurrentUser(info) {
		return errors.New("private runtime directory is not owned by the current user")
	}
	return nil
}

func validateRemoteMediaNetworkBinding(cfg remoteMediaRuntimeConfig) error {
	if !cfg.enabled() {
		return errors.New("remote media network binding is disabled")
	}
	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil || iface.Flags&net.FlagUp == 0 {
		return errors.New("configured Tailscale media interface is unavailable")
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return errors.New("configured Tailscale media interface addresses are unavailable")
	}
	for _, rawCIDR := range cfg.LocalCIDRs {
		_, allowed, parseErr := net.ParseCIDR(rawCIDR)
		if parseErr != nil {
			return errors.New("configured local media CIDR is invalid")
		}
		for _, address := range addresses {
			ipText := strings.SplitN(address.String(), "/", 2)[0]
			if ip := net.ParseIP(ipText); ip != nil && allowed.Contains(ip) {
				return nil
			}
		}
	}
	return errors.New("configured Tailscale interface has no address in the local media CIDR allowlist")
}

func parseRemoteMediaCIDRs(raw string, exactPeer bool) ([]string, error) {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ip, network, err := net.ParseCIDR(part)
		if err != nil || !ip.Equal(network.IP) {
			return nil, errors.New("media CIDR must be canonical")
		}
		canonical := network.String()
		ones, bits := network.Mask.Size()
		if exactPeer && ones != bits {
			return nil, errors.New("remote media peer must be an exact /32 or /128 address")
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	if len(result) == 0 {
		return nil, errors.New("at least one media CIDR is required")
	}
	return result, nil
}

func (a *app) configureRemoteMedia(interfaceName, localRaw, remoteRaw string, udpMin, udpMax uint16) error {
	interfaceName = strings.TrimSpace(interfaceName)
	if interfaceName == "" {
		if strings.TrimSpace(localRaw) != "" || strings.TrimSpace(remoteRaw) != "" || udpMin != 0 || udpMax != 0 {
			return errors.New("remote media interface is required when any media option is set")
		}
		a.remoteMedia = nil
		return nil
	}
	if !a.remoteAccess.enabled() || !a.remoteAccess.Control {
		return errors.New("remote media requires the authenticated remote gateway with control enabled")
	}
	localCIDRs, err := parseRemoteMediaCIDRs(localRaw, false)
	if err != nil {
		return fmt.Errorf("local media allowlist: %w", err)
	}
	remoteCIDRs, err := parseRemoteMediaCIDRs(remoteRaw, true)
	if err != nil {
		return fmt.Errorf("remote media allowlist: %w", err)
	}
	configBase, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("resolve remote media runtime directory: %w", err)
	}
	remoteRoot := filepath.Join(configBase, "MacCellular", "remote")
	manager, err := newRemoteMediaManager(remoteMediaRuntimeConfig{
		Interface: interfaceName, LocalCIDRs: localCIDRs, RemoteCIDRs: remoteCIDRs,
		UDPMin: udpMin, UDPMax: udpMax,
		RootDir:     filepath.Join(remoteRoot, "pcm"),
		ControlPath: filepath.Join(remoteRoot, "media-control.sock"),
		OfferTTL:    remoteMediaDefaultOfferTTL,
	})
	if err != nil {
		return err
	}
	a.remoteMedia = manager
	return nil
}

func (a *app) remoteMediaCallStillRinging(expected remoteCallExpectation) bool {
	if !validRemoteCallExpectation(expected) || expected.CallDirection != "incoming" {
		return false
	}
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	maxAge := 6 * time.Second
	if a.callPollInterval > 0 && 2*a.callPollInterval > maxAge {
		maxAge = 2 * a.callPollInterval
	}
	return a.activeCall != nil && a.callTopologyKnown && a.callLastPollError == "" &&
		!a.callLastPoll.IsZero() && time.Since(a.callLastPoll) <= maxAge &&
		a.callGeneration == expected.CallGeneration && a.activeCall.ID == expected.CallID &&
		a.activeCall.Index == expected.CallIndex && a.activeCall.Direction == "incoming" &&
		(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
}

func (a *app) remoteMediaIdleGenerationCurrent(expected uint64) bool {
	if expected == 0 {
		return false
	}
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	maxAge := 6 * time.Second
	if a.callPollInterval > 0 && 2*a.callPollInterval > maxAge {
		maxAge = 2 * a.callPollInterval
	}
	return a.callTopologyKnown && a.callGeneration == expected && a.activeCall == nil &&
		a.callLastPollError == "" && !a.callLastPoll.IsZero() &&
		time.Since(a.callLastPoll) <= maxAge
}

func (a *app) remoteMediaSourceCurrent(source remoteMediaSource) bool {
	switch source.Purpose {
	case "incoming":
		return a.remoteMediaCallStillRinging(source.Call)
	case "outgoing":
		return source.DialNumber != "" && a.remoteMediaIdleGenerationCurrent(source.ExpectedCallGeneration)
	default:
		return false
	}
}

func (a *app) remoteMediaOffer(w http.ResponseWriter, r *http.Request) {
	if !a.remoteMediaAvailableForBrowser(remoteBrowserContextFromRequest(r)) {
		writeError(w, http.StatusServiceUnavailable, "remote media is disabled")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, remoteMediaOfferBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "media offer body is too large or unreadable")
		return
	}
	var request remoteMediaOfferRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(w, http.StatusBadRequest, "media offer must be exactly one strict JSON object")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	response, err := a.remoteMedia.offer(
		r.Context(), authorization.Identity, body, request, a.remoteMediaSourceCurrent,
	)
	if err != nil {
		writeError(w, http.StatusConflict, "remote media offer was refused")
		return
	}
	if request.Purpose == "outgoing" {
		waitContext, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		err = a.remoteMedia.waitHostPrepared(
			waitContext,
			authorization.Identity,
			response.MediaSessionID,
			response.LeaseGeneration,
		)
		cancel()
		if err != nil {
			a.remoteMedia.closeIfGeneration(response.LeaseGeneration)
			writeError(w, http.StatusConflict, "remote media host preparation was refused")
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) remoteMediaStatusSnapshot(identity string) remoteMediaStatus {
	if a.remoteMedia == nil {
		return remoteMediaStatus{Enabled: false, Phase: "disabled"}
	}
	return a.remoteMedia.snapshot(identity, a.remoteMediaSourceCurrent, a.callMediaTicketIsCurrent)
}
