package remotevoice

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrMediaLeaseClosed             = errors.New("remotevoice: media lease is closing")
	ErrMediaLeaseIdentityMismatch   = errors.New("remotevoice: media lease identity mismatch")
	ErrMediaLeaseGenerationMismatch = errors.New("remotevoice: media lease generation mismatch")
	ErrMediaOfferConflict           = errors.New("remotevoice: a different offer already owns this media lease")
	ErrIPCLeaseAlreadyClaimed       = errors.New("remotevoice: local IPC lease was already claimed")
	ErrMediaAnswerFailed            = errors.New("remotevoice: WebRTC answer failed")
	ErrMediaPeerEnded               = errors.New("remotevoice: WebRTC peer ended")
	ErrLocalPCMBrokerEnded          = errors.New("remotevoice: local PCM broker ended")
)

// MediaLeasePhase is monotonic: New -> Offer -> Connected -> Closing. A
// transient WebRTC disconnect does not move the phase backwards; the current
// connection boolean in MediaLeaseSnapshot remains authoritative.
type MediaLeasePhase string

const (
	MediaLeasePhaseNew       MediaLeasePhase = "new"
	MediaLeasePhaseOffer     MediaLeasePhase = "offer"
	MediaLeasePhaseConnected MediaLeasePhase = "connected"
	MediaLeasePhaseClosing   MediaLeasePhase = "closing"
)

// MediaLeaseCloseReason is intentionally a small, redacted status vocabulary.
// Detailed peer, broker, SDP, path, and credential errors are never copied
// into a snapshot.
type MediaLeaseCloseReason string

const (
	MediaLeaseCloseNone               MediaLeaseCloseReason = ""
	MediaLeaseCloseRequested          MediaLeaseCloseReason = "requested"
	MediaLeaseCloseGenerationMismatch MediaLeaseCloseReason = "generation_mismatch"
	MediaLeaseCloseAnswerFailed       MediaLeaseCloseReason = "answer_failed"
	MediaLeaseClosePeerEnded          MediaLeaseCloseReason = "peer_ended"
	MediaLeaseCloseBrokerEnded        MediaLeaseCloseReason = "broker_ended"
)

// MediaPeer is the minimal lifecycle contract required from a WebRTC media
// endpoint. Session implements it. The exported seam also allows higher-level
// HTTP tests to avoid opening real ICE sockets.
type MediaPeer interface {
	Connected() bool
	NetworkActivitySnapshot() NetworkActivitySnapshot
	Activate(uint64) (NetworkActivitySnapshot, error)
	Wait(context.Context) error
	Close() error
}

// MediaAnswerFunc creates a generation-bound peer and its SDP answer. An
// implementation must return promptly when its context is canceled.
type MediaAnswerFunc func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error)

// LocalPCMBroker is the credential-free lifecycle view used by the lease
// coordinator. SyntheticIPCBroker implements it; the abstraction deliberately
// makes no claim that a live device audio route has been validated.
type LocalPCMBroker interface {
	ActivitySnapshot() IPCBrokerActivitySnapshot
	Activate(uint64) (IPCBrokerActivitySnapshot, error)
	Wait(context.Context) error
	Close() error
}

// LocalPCMBrokerStartConfig contains the one-use credential required to start
// a same-UID local bridge. Implementations must not persist or log it.
type LocalPCMBrokerStartConfig struct {
	SocketPath               string `json:"-"`
	LeaseGeneration          uint64
	SessionToken             [IPCSessionTokenSize]byte `json:"-"`
	Port                     *FramePort                `json:"-"`
	IOTimeout                time.Duration
	AuthenticatedIdleTimeout time.Duration
	ActivityNow              func() time.Time `json:"-"`
}

func (LocalPCMBrokerStartConfig) String() string {
	return "remotevoice.LocalPCMBrokerStartConfig{redacted}"
}
func (LocalPCMBrokerStartConfig) GoString() string {
	return "remotevoice.LocalPCMBrokerStartConfig{redacted}"
}

// LocalPCMBrokerFactory is injectable so route/API tests can exercise lease
// ownership without binding a Unix socket.
type LocalPCMBrokerFactory func(LocalPCMBrokerStartConfig) (LocalPCMBroker, error)

// MediaLeaseConfig creates one caller-owned media-generation owner. MediaSessionID is an
// opaque application-generated identifier, not the local IPC bearer token.
type MediaLeaseConfig struct {
	MediaSessionID string `json:"-"`
	// LeaseGeneration is caller-owned and need not equal a raw modem/CLCC
	// topology counter. For example, ringing-to-active may remain one media
	// lease when the call owner deliberately retains this value.
	LeaseGeneration             uint64
	SocketPath                  string `json:"-"`
	FrameCapacity               int
	IPCIOTimeout                time.Duration
	IPCAuthenticatedIdleTimeout time.Duration
	FreshnessWindow             time.Duration
	Now                         func() time.Time `json:"-"`

	Answerer    MediaAnswerFunc       `json:"-"`
	StartBroker LocalPCMBrokerFactory `json:"-"`
}

func (MediaLeaseConfig) String() string   { return "remotevoice.MediaLeaseConfig{redacted}" }
func (MediaLeaseConfig) GoString() string { return "remotevoice.MediaLeaseConfig{redacted}" }

// IPCLeaseClaim is returned exactly once to the local audio process launcher.
// String and GoString are redacted to reduce accidental credential logging.
type IPCLeaseClaim struct {
	SocketPath      string                    `json:"-"`
	SessionToken    [IPCSessionTokenSize]byte `json:"-"`
	LeaseGeneration uint64                    `json:"leaseGeneration"`
}

func (IPCLeaseClaim) String() string   { return "remotevoice.IPCLeaseClaim{redacted}" }
func (IPCLeaseClaim) GoString() string { return "remotevoice.IPCLeaseClaim{redacted}" }

// MediaLeaseSnapshot is safe for a remote status response. It intentionally
// contains neither the opaque media session identifier, SDP, ICE candidates,
// socket path, nor the one-use IPC token.
type MediaLeaseSnapshot struct {
	Phase                    MediaLeasePhase       `json:"phase"`
	LeaseGeneration          uint64                `json:"leaseGeneration"`
	WebRTCConnected          bool                  `json:"webrtcConnected"`
	WebRTCSelectedPair       SelectedPairSnapshot  `json:"webrtcSelectedPair"`
	WebRTCLocalTrackStarted  bool                  `json:"webrtcLocalTrackStarted"`
	WebRTCRemoteTrackReady   bool                  `json:"webrtcRemoteTrackReady"`
	WebRTCReceiverReportSeen bool                  `json:"webrtcReceiverReportSeen"`
	IPCClaimed               bool                  `json:"ipcClaimed"`
	IPCConsumed              bool                  `json:"ipcConsumed"`
	IPCReady                 bool                  `json:"ipcReady"`
	IPCLive                  bool                  `json:"ipcLive"`
	TransportPrepared        bool                  `json:"transportPrepared"`
	Activated                bool                  `json:"activated"`
	ActivationEpoch          uint64                `json:"activationEpoch"`
	DeviceUplinkFresh        bool                  `json:"deviceUplinkFresh"`
	RemoteDownlinkFresh      bool                  `json:"remoteDownlinkFresh"`
	ActiveFresh              bool                  `json:"activeFresh"`
	FrameQueueStats          PortStats             `json:"frameQueueStats"`
	RemoteRTPSamples         uint64                `json:"remoteRTPSamples"`
	RemoteRTPSignalSamples   uint64                `json:"remoteRTPSignalSamples"`
	RemoteRTPPeakPCM16       uint32                `json:"remoteRTPPeakPCM16"`
	LastActivityAt           time.Time             `json:"lastActivityAt"`
	LastDeviceUplinkAt       time.Time             `json:"lastDeviceUplinkAt,omitempty"`
	LastRemoteDownlinkAt     time.Time             `json:"lastRemoteDownlinkAt,omitempty"`
	CloseReason              MediaLeaseCloseReason `json:"closeReason,omitempty"`
}

// MediaLeaseCoordinator owns one FramePort, one local IPC bearer/broker, and
// at most one WebRTC peer for exactly one opaque call identity and generation.
type MediaLeaseCoordinator struct {
	mediaSessionID string
	generation     uint64
	freshness      time.Duration
	now            func() time.Time
	answerer       MediaAnswerFunc
	broker         LocalPCMBroker
	port           *FramePort

	ctx    context.Context
	cancel context.CancelFunc

	answerMu          sync.Mutex
	activateMu        sync.Mutex
	answerStarted     bool
	answerFingerprint [sha256.Size]byte
	answerSDP         []byte
	peerMu            sync.RWMutex
	peer              MediaPeer

	mu                          sync.Mutex
	phase                       MediaLeasePhase
	connectedEver               bool
	ipcClaimed                  bool
	ipcClaim                    IPCLeaseClaim
	lastActivity                time.Time
	closeReason                 MediaLeaseCloseReason
	terminalErr                 error
	closeErr                    error
	closing                     bool
	activated                   bool
	activationEpoch             uint64
	activationAt                time.Time
	activationUplinkFrames      uint64
	activationDownlinkFrames    uint64
	activationNetworkRealUplink uint64
	activationRemoteRTP         uint64
	activationReceiverReports   uint64

	closeOnce sync.Once
	closingCh chan struct{}
	watchers  sync.WaitGroup
}

func (*MediaLeaseCoordinator) String() string {
	return "remotevoice.MediaLeaseCoordinator{redacted}"
}
func (*MediaLeaseCoordinator) GoString() string {
	return "remotevoice.MediaLeaseCoordinator{redacted}"
}

// MediaActivationSnapshot identifies the immutable activation baseline for
// this coordinator. It contains no media session identity, IPC credential, or
// endpoint path.
type MediaActivationSnapshot struct {
	Activated       bool      `json:"activated"`
	ActivationEpoch uint64    `json:"activationEpoch"`
	ActivatedAt     time.Time `json:"activatedAt"`
}

// NewMediaLeaseCoordinator starts the local broker and returns a single-owner
// caller-defined media-generation coordinator. It performs no modem,
// call-control, or remote API action.
func NewMediaLeaseCoordinator(cfg MediaLeaseConfig) (*MediaLeaseCoordinator, error) {
	if err := validateMediaLeaseConfig(&cfg); err != nil {
		return nil, err
	}

	port, err := NewFramePort(cfg.FrameCapacity)
	if err != nil {
		return nil, err
	}
	ipcLease, err := NewSyntheticIPCLease(cfg.LeaseGeneration)
	if err != nil {
		port.Close()
		return nil, err
	}
	broker, err := cfg.StartBroker(LocalPCMBrokerStartConfig{
		SocketPath: cfg.SocketPath, LeaseGeneration: cfg.LeaseGeneration,
		SessionToken: ipcLease.SessionToken, Port: port,
		IOTimeout: cfg.IPCIOTimeout, AuthenticatedIdleTimeout: cfg.IPCAuthenticatedIdleTimeout,
		ActivityNow: cfg.Now,
	})
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("remotevoice: start local PCM broker: %w", err)
	}
	if broker == nil {
		port.Close()
		return nil, errors.New("remotevoice: local PCM broker factory returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &MediaLeaseCoordinator{
		mediaSessionID: cfg.MediaSessionID,
		generation:     cfg.LeaseGeneration,
		freshness:      cfg.FreshnessWindow,
		now:            cfg.Now,
		answerer:       cfg.Answerer,
		broker:         broker,
		port:           port,
		ctx:            ctx,
		cancel:         cancel,
		phase:          MediaLeasePhaseNew,
		ipcClaim: IPCLeaseClaim{
			SocketPath:      cfg.SocketPath,
			SessionToken:    ipcLease.SessionToken,
			LeaseGeneration: cfg.LeaseGeneration,
		},
		lastActivity: cfg.Now(),
		closingCh:    make(chan struct{}),
	}
	c.watchers.Add(1)
	go c.watchBroker()
	return c, nil
}

func validateMediaLeaseConfig(cfg *MediaLeaseConfig) error {
	if cfg == nil {
		return errors.New("remotevoice: media lease config is required")
	}
	if cfg.MediaSessionID != strings.TrimSpace(cfg.MediaSessionID) ||
		len(cfg.MediaSessionID) < 16 || len(cfg.MediaSessionID) > 256 {
		return errors.New("remotevoice: opaque media session identifier must contain 16..256 non-space bytes")
	}
	for _, char := range cfg.MediaSessionID {
		if char < 0x21 || char == 0x7f {
			return errors.New("remotevoice: opaque media session identifier contains a control or space character")
		}
	}
	if cfg.LeaseGeneration == 0 {
		return errors.New("remotevoice: media lease generation must be non-zero")
	}
	if strings.TrimSpace(cfg.SocketPath) == "" {
		return errors.New("remotevoice: local PCM socket path is required")
	}
	if cfg.FrameCapacity == 0 {
		cfg.FrameCapacity = 64
	}
	if cfg.FrameCapacity < 1 || cfg.FrameCapacity > 4096 {
		return errors.New("remotevoice: frame capacity must be between 1 and 4096")
	}
	if cfg.FreshnessWindow == 0 {
		cfg.FreshnessWindow = 2 * time.Second
	}
	if cfg.IPCAuthenticatedIdleTimeout == 0 {
		cfg.IPCAuthenticatedIdleTimeout = 30 * time.Second
	}
	if cfg.FreshnessWindow < 2*FrameMillis*time.Millisecond || cfg.FreshnessWindow > 30*time.Second {
		return errors.New("remotevoice: freshness window must be between 40 ms and 30 s")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Answerer == nil {
		cfg.Answerer = defaultMediaAnswer
	}
	if cfg.StartBroker == nil {
		cfg.StartBroker = defaultLocalPCMBrokerFactory
	}
	return nil
}

func defaultMediaAnswer(ctx context.Context, offer []byte, cfg Config, port *FramePort) (MediaPeer, []byte, error) {
	return Answer(ctx, offer, cfg, port)
}

func defaultLocalPCMBrokerFactory(cfg LocalPCMBrokerStartConfig) (LocalPCMBroker, error) {
	return StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath:               cfg.SocketPath,
		Lease:                    SyntheticIPCLease{CallGeneration: cfg.LeaseGeneration, SessionToken: cfg.SessionToken},
		Port:                     cfg.Port,
		IOTimeout:                cfg.IOTimeout,
		AuthenticatedIdleTimeout: cfg.AuthenticatedIdleTimeout,
		ActivityNow:              cfg.ActivityNow,
	})
}

// LeaseGeneration returns the non-secret, caller-owned media generation.
func (c *MediaLeaseCoordinator) LeaseGeneration() uint64 { return c.generation }

// Owns performs an exact identity and generation check without exposing the
// opaque media session identifier.
func (c *MediaLeaseCoordinator) Owns(mediaSessionID string, generation uint64) bool {
	return generation == c.generation && len(mediaSessionID) == len(c.mediaSessionID) &&
		subtle.ConstantTimeCompare([]byte(mediaSessionID), []byte(c.mediaSessionID)) == 1
}

// RequireOwner rejects both a different opaque call identity and generation.
func (c *MediaLeaseCoordinator) RequireOwner(mediaSessionID string, generation uint64) error {
	if generation != c.generation {
		return ErrMediaLeaseGenerationMismatch
	}
	if !c.Owns(mediaSessionID, generation) {
		return ErrMediaLeaseIdentityMismatch
	}
	return nil
}

// ClaimIPCLease transfers the local path/token/generation credential exactly
// once. The coordinator erases its retained copy immediately after transfer;
// the already-started broker owns the only other in-memory copy.
func (c *MediaLeaseCoordinator) ClaimIPCLease() (IPCLeaseClaim, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return IPCLeaseClaim{}, ErrMediaLeaseClosed
	}
	if c.ipcClaimed {
		return IPCLeaseClaim{}, ErrIPCLeaseAlreadyClaimed
	}
	c.ipcClaimed = true
	claim := c.ipcClaim
	c.ipcClaim.SocketPath = ""
	for index := range c.ipcClaim.SessionToken {
		c.ipcClaim.SessionToken[index] = 0
	}
	c.lastActivity = c.now()
	return claim, nil
}

// Activate records a one-time counter/time baseline after the caller has
// independently proved that ATD/ATA was accepted and the intended active call
// ticket was freshly adopted. Repeated calls are idempotent and never reset
// the baseline, preventing prepared/pre-call frames from satisfying ActiveFresh.
func (c *MediaLeaseCoordinator) Activate() (MediaActivationSnapshot, error) {
	c.activateMu.Lock()
	defer c.activateMu.Unlock()

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return MediaActivationSnapshot{}, ErrMediaLeaseClosed
	}
	if c.activated {
		snapshot := MediaActivationSnapshot{
			Activated: true, ActivationEpoch: c.activationEpoch, ActivatedAt: c.activationAt,
		}
		c.mu.Unlock()
		return snapshot, nil
	}
	c.mu.Unlock()

	const activationEpoch = uint64(1)
	c.peerMu.RLock()
	peer := c.peer
	c.peerMu.RUnlock()
	if peer == nil {
		return MediaActivationSnapshot{}, errors.New("remotevoice: media peer is not established")
	}
	// Epoch barriers are established before counters become authoritative.
	// In-flight reads retain epoch zero and cannot satisfy post-activation
	// freshness even if their timestamps happen to follow ActivatedAt. These
	// external calls deliberately run without c.mu so Close can cancel a peer
	// whose media writer is blocking at the barrier.
	activity, err := c.broker.Activate(activationEpoch)
	if err != nil {
		return MediaActivationSnapshot{}, fmt.Errorf("remotevoice: activate local PCM broker: %w", err)
	}
	networkActivity, err := peer.Activate(activationEpoch)
	if err != nil {
		return MediaActivationSnapshot{}, fmt.Errorf("remotevoice: activate network media: %w", err)
	}

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return MediaActivationSnapshot{}, ErrMediaLeaseClosed
	}
	c.activationAt = c.now()
	c.activated = true
	c.activationEpoch = activationEpoch
	c.activationUplinkFrames = activity.DeviceUplinkFrames
	c.activationDownlinkFrames = activity.RemoteDownlinkFrames
	c.activationNetworkRealUplink = networkActivity.RealUplinkPackets
	c.activationRemoteRTP = networkActivity.RemoteRTPPackets
	c.activationReceiverReports = networkActivity.ReceiverReports
	c.lastActivity = c.activationAt
	snapshot := MediaActivationSnapshot{
		Activated: true, ActivationEpoch: c.activationEpoch, ActivatedAt: c.activationAt,
	}
	c.mu.Unlock()
	return snapshot, nil
}

// Answer creates the sole WebRTC peer and returns its SDP answer. An exact
// retry is idempotent and returns the cached answer; a different offer or
// effective Config is rejected instead of replacing a live generation.
func (c *MediaLeaseCoordinator) Answer(ctx context.Context, offer []byte, cfg Config) ([]byte, error) {
	c.answerMu.Lock()

	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		c.answerMu.Unlock()
		return nil, ErrMediaLeaseClosed
	}
	c.mu.Unlock()

	if cfg.Token != "" && cfg.Token != c.mediaSessionID {
		c.answerMu.Unlock()
		return nil, ErrMediaLeaseIdentityMismatch
	}
	if cfg.Generation != 0 && cfg.Generation != c.generation {
		c.answerMu.Unlock()
		return nil, ErrMediaLeaseGenerationMismatch
	}
	cfg.Token = c.mediaSessionID
	cfg.Generation = c.generation
	fingerprint, err := mediaAnswerFingerprint(offer, cfg)
	if err != nil {
		c.answerMu.Unlock()
		return nil, err
	}
	if c.answerStarted {
		if fingerprint != c.answerFingerprint {
			c.answerMu.Unlock()
			return nil, ErrMediaOfferConflict
		}
		answer := append([]byte(nil), c.answerSDP...)
		c.answerMu.Unlock()
		return answer, nil
	}
	c.answerStarted = true
	c.answerFingerprint = fingerprint
	c.mu.Lock()
	c.phase = MediaLeasePhaseOffer
	c.lastActivity = c.now()
	c.mu.Unlock()

	answerCtx, cancelAnswer := context.WithCancel(ctx)
	stopLeaseCancellation := context.AfterFunc(c.ctx, cancelAnswer)
	peer, answer, answerErr := c.answerer(answerCtx, append([]byte(nil), offer...), cloneMediaConfig(cfg), c.port)
	stopLeaseCancellation()
	cancelAnswer()
	c.mu.Lock()
	closingAfterAnswer := c.closing
	c.mu.Unlock()
	if closingAfterAnswer {
		if peer != nil {
			_ = peer.Close()
		}
		c.answerMu.Unlock()
		return nil, ErrMediaLeaseClosed
	}
	if answerErr == nil && (peer == nil || len(answer) == 0) {
		answerErr = errors.New("remotevoice: answerer returned an empty peer or SDP")
	}
	if answerErr != nil {
		// MediaAnswerFunc is injectable and may return a partially constructed
		// peer together with an error (or with an empty SDP). It has not been
		// published to c.peer, so this failure path is its sole owner.
		if peer != nil {
			_ = peer.Close()
		}
		c.answerMu.Unlock()
		c.beginClose(MediaLeaseCloseAnswerFailed, ErrMediaAnswerFailed)
		return nil, ErrMediaAnswerFailed
	}
	c.peerMu.Lock()
	c.peer = peer
	c.peerMu.Unlock()
	c.answerSDP = append([]byte(nil), answer...)
	c.watchers.Add(1)
	go c.watchPeer(peer)
	c.answerMu.Unlock()
	return append([]byte(nil), answer...), nil
}

func mediaAnswerFingerprint(offer []byte, cfg Config) ([sha256.Size]byte, error) {
	type relayFingerprint struct {
		URLs           []string
		Username       string
		Password       string
		CredentialType TURNCredentialType
	}
	var relay *relayFingerprint
	if cfg.TURNRelay != nil {
		relay = &relayFingerprint{
			URLs:     append([]string(nil), cfg.TURNRelay.URLs...),
			Username: cfg.TURNRelay.Username, Password: cfg.TURNRelay.Password,
			CredentialType: cfg.TURNRelay.CredentialType,
		}
	}
	encoded, err := json.Marshal(struct {
		Token              string
		Generation         uint64
		AllowedInterfaces  []string
		AllowedLocalCIDRs  []string
		AllowedRemoteCIDRs []string
		UDPMin             uint16
		UDPMax             uint16
		AnswerTimeout      time.Duration
		TURNRelay          *relayFingerprint
	}{
		Token: cfg.Token, Generation: cfg.Generation,
		AllowedInterfaces: cfg.AllowedInterfaces, AllowedLocalCIDRs: cfg.AllowedLocalCIDRs,
		AllowedRemoteCIDRs: cfg.AllowedRemoteCIDRs, UDPMin: cfg.UDPMin, UDPMax: cfg.UDPMax,
		AnswerTimeout: cfg.AnswerTimeout, TURNRelay: relay,
	})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("remotevoice: fingerprint media config: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write(offer)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(encoded)
	if cfg.allowLoopbackForTests {
		_, _ = hash.Write([]byte{1})
	} else {
		_, _ = hash.Write([]byte{0})
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func cloneMediaConfig(cfg Config) Config {
	clone := cfg
	clone.AllowedInterfaces = append([]string(nil), cfg.AllowedInterfaces...)
	clone.AllowedLocalCIDRs = append([]string(nil), cfg.AllowedLocalCIDRs...)
	clone.AllowedRemoteCIDRs = append([]string(nil), cfg.AllowedRemoteCIDRs...)
	if cfg.TURNRelay != nil {
		relay := *cfg.TURNRelay
		relay.URLs = append([]string(nil), cfg.TURNRelay.URLs...)
		clone.TURNRelay = &relay
	}
	return clone
}

// Snapshot computes the current connection and real-frame freshness gates.
// TransportPrepared proves only the network/local transport plane: a connected
// WebRTC peer, a running local send track, a selected UDP ICE pair satisfying
// either the restricted-host address allowlists or the TURN relay/relay
// profile, the first accepted browser PCMU RTP packet, and a completed local
// IPC Ready handshake. A browser ReceiverReport is deliberately not part of
// this pre-call gate: Mobile Safari can delay its first report until useful
// audio starts, even though ICE and RTP are already flowing in both directions.
// The stronger post-activation ActiveFresh gate still requires a causally
// newer report. It is not
// a modem/UAC/QPCMV attestation and must be combined with the caller's exact
// hardware checks before call control.
// ActiveFresh further requires post-activation network and real IPC exchanges
// in both directions; synthesized IPC silence never satisfies it.
func (c *MediaLeaseCoordinator) Snapshot() MediaLeaseSnapshot {
	activity := c.broker.ActivitySnapshot()

	c.peerMu.RLock()
	peer := c.peer
	networkActivity := NetworkActivitySnapshot{}
	if peer != nil {
		networkActivity = peer.NetworkActivitySnapshot()
	}
	connected := networkActivity.Connected
	c.peerMu.RUnlock()

	now := c.now()
	authenticatedAt := boundedActivityTime(now, activity.AuthenticatedAt)
	readyAt := boundedActivityTime(now, activity.ReadyAt)
	deviceUplinkAt := boundedActivityTime(now, activity.LastDeviceUplinkAt)
	remoteDownlinkAt := boundedActivityTime(now, activity.LastRemoteDownlinkAt)
	networkUplinkAt := boundedActivityTime(now, networkActivity.LastUplinkAt)
	networkRealUplinkAt := boundedActivityTime(now, networkActivity.LastRealUplinkAt)
	networkRemoteRTPAt := boundedActivityTime(now, networkActivity.LastRemoteRTPAt)
	receiverReportAt := boundedActivityTime(now, networkActivity.LastReceiverReportAt)
	c.mu.Lock()
	if (connected || networkActivity.LocalTrackStarted || networkActivity.RemoteTrackReady) &&
		!c.connectedEver && !c.closing {
		c.connectedEver = true
		c.phase = MediaLeasePhaseConnected
		c.lastActivity = now
	}
	phase := c.phase
	if c.closing {
		phase = MediaLeasePhaseClosing
	}
	lastActivity := maxTime(c.lastActivity, authenticatedAt)
	lastActivity = maxTime(lastActivity, readyAt)
	lastActivity = maxTime(lastActivity, deviceUplinkAt)
	lastActivity = maxTime(lastActivity, remoteDownlinkAt)
	lastActivity = maxTime(lastActivity, networkUplinkAt)
	lastActivity = maxTime(lastActivity, networkRealUplinkAt)
	lastActivity = maxTime(lastActivity, networkRemoteRTPAt)
	lastActivity = maxTime(lastActivity, receiverReportAt)
	claimed := c.ipcClaimed
	closing := c.closing
	reason := c.closeReason
	activated := c.activated
	activationEpoch := c.activationEpoch
	activationAt := c.activationAt
	activationUplinkFrames := c.activationUplinkFrames
	activationDownlinkFrames := c.activationDownlinkFrames
	activationNetworkRealUplink := c.activationNetworkRealUplink
	activationRemoteRTP := c.activationRemoteRTP
	activationReceiverReports := c.activationReceiverReports
	c.mu.Unlock()

	selectedPair := networkActivity.SelectedPair
	if closing || !connected {
		selectedPair = SelectedPairSnapshot{}
	}
	uplinkFresh := activated && activity.DeviceUplinkFrames > activationUplinkFrames &&
		activity.LastDeviceUplinkEpoch == activationEpoch &&
		!deviceUplinkAt.Before(activationAt) && timestampFresh(now, deviceUplinkAt, c.freshness)
	downlinkFresh := activated && activity.RemoteDownlinkFrames > activationDownlinkFrames &&
		activity.LastRemoteDownlinkEpoch == activationEpoch &&
		!remoteDownlinkAt.Before(activationAt) && timestampFresh(now, remoteDownlinkAt, c.freshness)
	transportPrepared := !closing && connected && selectedPair.PolicyValid() &&
		claimed && activity.Consumed && activity.Ready && activity.Live &&
		networkActivity.LocalTrackStarted && networkActivity.RemoteTrackReady &&
		networkActivity.UplinkPackets > 0 && networkActivity.RemoteRTPPackets > 0 &&
		!networkUplinkAt.IsZero() && timestampFresh(now, networkUplinkAt, c.freshness) &&
		!networkRemoteRTPAt.IsZero() && timestampFresh(now, networkRemoteRTPAt, c.freshness)
	networkExchangeFresh := activated &&
		networkActivity.RealUplinkPackets > activationNetworkRealUplink &&
		networkActivity.RemoteRTPPackets > activationRemoteRTP &&
		networkActivity.ReceiverReports > activationReceiverReports &&
		networkActivity.LastRealUplinkEpoch == activationEpoch &&
		networkActivity.LastRemoteRTPEpoch == activationEpoch &&
		networkActivity.LastReceiverReportEpoch == activationEpoch &&
		networkActivity.RealUplinkPacketsAtLastReceiverReport > activationNetworkRealUplink &&
		!networkRealUplinkAt.Before(activationAt) &&
		!networkRemoteRTPAt.Before(activationAt) &&
		!receiverReportAt.Before(activationAt) &&
		timestampFresh(now, networkRealUplinkAt, c.freshness) &&
		timestampFresh(now, networkRemoteRTPAt, c.freshness) &&
		timestampFresh(now, receiverReportAt, c.freshness)
	return MediaLeaseSnapshot{
		Phase:                    phase,
		LeaseGeneration:          c.generation,
		WebRTCConnected:          connected,
		WebRTCSelectedPair:       selectedPair,
		WebRTCLocalTrackStarted:  networkActivity.LocalTrackStarted,
		WebRTCRemoteTrackReady:   networkActivity.RemoteTrackReady,
		WebRTCReceiverReportSeen: networkActivity.ReceiverReports > 0,
		IPCClaimed:               claimed,
		IPCConsumed:              activity.Consumed,
		IPCReady:                 activity.Ready,
		IPCLive:                  activity.Live,
		TransportPrepared:        transportPrepared,
		Activated:                activated,
		ActivationEpoch:          activationEpoch,
		DeviceUplinkFresh:        uplinkFresh,
		RemoteDownlinkFresh:      downlinkFresh,
		ActiveFresh:              transportPrepared && uplinkFresh && downlinkFresh && networkExchangeFresh,
		FrameQueueStats:          c.port.Stats(),
		RemoteRTPSamples:         networkActivity.RemoteRTPSamples,
		RemoteRTPSignalSamples:   networkActivity.RemoteRTPSignalSamples,
		RemoteRTPPeakPCM16:       networkActivity.RemoteRTPPeakPCM16,
		LastActivityAt:           lastActivity,
		LastDeviceUplinkAt:       deviceUplinkAt,
		LastRemoteDownlinkAt:     remoteDownlinkAt,
		CloseReason:              reason,
	}
}

func boundedActivityTime(now, event time.Time) time.Time {
	if event.After(now) {
		return time.Time{}
	}
	return event
}

func timestampFresh(now, event time.Time, window time.Duration) bool {
	if event.IsZero() || now.Before(event) {
		return false
	}
	return now.Sub(event) <= window
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func (c *MediaLeaseCoordinator) watchBroker() {
	defer c.watchers.Done()
	_ = c.broker.Wait(c.ctx)
	if c.ctx.Err() == nil {
		c.beginClose(MediaLeaseCloseBrokerEnded, ErrLocalPCMBrokerEnded)
	}
}

func (c *MediaLeaseCoordinator) watchPeer(peer MediaPeer) {
	defer c.watchers.Done()
	_ = peer.Wait(c.ctx)
	if c.ctx.Err() == nil {
		c.beginClose(MediaLeaseClosePeerEnded, ErrMediaPeerEnded)
	}
}

// CloseIfGenerationMismatch atomically retires a lease that no longer belongs
// to the observed call generation. Matching generations are a no-op.
func (c *MediaLeaseCoordinator) CloseIfGenerationMismatch(generation uint64) (bool, error) {
	if generation == c.generation {
		return false, nil
	}
	c.beginClose(MediaLeaseCloseGenerationMismatch, ErrMediaLeaseGenerationMismatch)
	c.watchers.Wait()
	return true, c.resourceCloseError()
}

// Wait returns after the lease has closed its owned resources. It reports only a stable,
// credential-free terminal category.
func (c *MediaLeaseCoordinator) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closingCh:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.terminalErr
	}
}

// Close is idempotent and does not return until the broker and peer watcher
// goroutines have exited.
func (c *MediaLeaseCoordinator) Close() error {
	c.beginClose(MediaLeaseCloseRequested, nil)
	c.watchers.Wait()
	return c.resourceCloseError()
}

func (c *MediaLeaseCoordinator) beginClose(reason MediaLeaseCloseReason, terminalErr error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.phase = MediaLeasePhaseClosing
		c.closeReason = reason
		c.terminalErr = terminalErr
		c.lastActivity = c.now()
		c.mu.Unlock()

		// Cancel before waiting on answerMu so a context-aware answerer cannot
		// deadlock Close while ICE gathering or another setup stage is blocked.
		c.cancel()

		// Serialize against Answer. This also guarantees no watcher is added
		// after Close starts waiting for the watcher set.
		c.answerMu.Lock()
		c.peerMu.RLock()
		peer := c.peer
		c.peerMu.RUnlock()
		var closeErrors []error
		if err := c.broker.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
		if peer != nil {
			if err := peer.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
		c.port.Close()
		c.answerMu.Unlock()

		c.mu.Lock()
		c.closeErr = errors.Join(closeErrors...)
		c.mu.Unlock()
		close(c.closingCh)
	})
}

func (c *MediaLeaseCoordinator) resourceCloseError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}
