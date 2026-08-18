package sipwebrtc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

var (
	ErrClosed             = errors.New("sipwebrtc: media bridge closed")
	ErrInvalidConfig      = errors.New("sipwebrtc: invalid bridge configuration")
	ErrOfferConflict      = errors.New("sipwebrtc: a different offer already owns the bridge")
	ErrNetworkNotPrepared = errors.New("sipwebrtc: browser media is not prepared")
	ErrNetworkEnded       = errors.New("sipwebrtc: browser media transport ended")
	ErrGatewayMediaEnded  = errors.New("sipwebrtc: gateway media transport ended")
)

const (
	defaultFrameCapacity   = 4
	defaultFreshnessWindow = 2 * time.Second
)

// Config binds one adapter-private gateway media session to one future WebRTC
// peer. Source ownership transfers to Bridge only when New succeeds.
type Config struct {
	Source          sipgateway.MediaSession `json:"-"`
	FrameCapacity   int
	FreshnessWindow time.Duration
	Now             func() time.Time            `json:"-"`
	Answerer        remotevoice.MediaAnswerFunc `json:"-"`
}

func (Config) String() string   { return "sipwebrtc.Config{redacted}" }
func (Config) GoString() string { return "sipwebrtc.Config{redacted}" }

// Snapshot is safe for an authorized status response. It contains no provider
// handle, SDP, ICE address, credential, encoded audio, or caller identity.
type Snapshot struct {
	Prepared             bool                             `json:"prepared"`
	Activated            bool                             `json:"activated"`
	ActivationEpoch      uint64                           `json:"activation_epoch,omitempty"`
	BidirectionalFresh   bool                             `json:"bidirectional_fresh"`
	GatewayToClientFresh bool                             `json:"gateway_to_client_fresh"`
	ClientToGatewayFresh bool                             `json:"client_to_gateway_fresh"`
	WebRTCConnected      bool                             `json:"webrtc_connected"`
	WebRTCSelectedPair   remotevoice.SelectedPairSnapshot `json:"webrtc_selected_pair"`
	WebRTCRemoteTrack    bool                             `json:"webrtc_remote_track"`
	WebRTCReceiverReport bool                             `json:"webrtc_receiver_report"`
	Closed               bool                             `json:"closed"`
}

// Bridge implements sipgateway.MediaSession while retaining the adapter's
// private media identity. The coordinator can therefore apply its existing
// exact call/revision/command gates without exposing provider state remotely.
type Bridge struct {
	source    sipgateway.MediaSession
	port      *remotevoice.FramePort
	answerer  remotevoice.MediaAnswerFunc
	freshness time.Duration
	now       func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	// activated is closed exactly once after both the gateway media session and
	// the WebRTC peer have accepted the same non-zero epoch. Frame pumps wait on
	// this barrier so browser microphone audio can never reach a ringing caller,
	// and gateway early media is never exposed before the call owner activates.
	activated chan struct{}

	answerMu          sync.Mutex
	answerStarted     bool
	answerFingerprint [sha256.Size]byte
	answerSDP         []byte

	activateMu sync.Mutex
	barrierMu  sync.Mutex
	peerMu     sync.RWMutex
	peer       remotevoice.MediaPeer

	mu                        sync.Mutex
	closing                   bool
	terminalErr               error
	epoch                     uint64
	activatedAt               time.Time
	networkRealUplinkBaseline uint64
	networkRemoteRTPBaseline  uint64
	networkRRBaseline         uint64
	nextDownlinkSequence      uint64

	closeOnce sync.Once
	wg        sync.WaitGroup
	done      chan struct{}
}

func (*Bridge) String() string   { return "sipwebrtc.Bridge{redacted}" }
func (*Bridge) GoString() string { return "sipwebrtc.Bridge{redacted}" }

// New validates the source before taking ownership and starts bounded,
// context-aware frame pumps. It performs no call mutation and opens no network
// socket until Answer is called with a browser SDP offer.
func New(cfg Config) (*Bridge, error) {
	if cfg.Source == nil || cfg.Source.Codec() != sipgateway.CodecPCMU {
		return nil, ErrInvalidConfig
	}
	sourceSnapshot := cfg.Source.Snapshot()
	if sourceSnapshot.LeaseID != cfg.Source.ID() || sourceSnapshot.Call != cfg.Source.CallRef() ||
		sourceSnapshot.Codec != sipgateway.CodecPCMU || !sourceSnapshot.Prepared ||
		sourceSnapshot.Closed || sourceSnapshot.Activated {
		return nil, ErrInvalidConfig
	}
	if cfg.FrameCapacity == 0 {
		cfg.FrameCapacity = defaultFrameCapacity
	}
	if cfg.FrameCapacity < 1 || cfg.FrameCapacity > 256 {
		return nil, ErrInvalidConfig
	}
	if cfg.FreshnessWindow == 0 {
		cfg.FreshnessWindow = defaultFreshnessWindow
	}
	if cfg.FreshnessWindow < 2*remotevoice.FrameMillis*time.Millisecond ||
		cfg.FreshnessWindow > 30*time.Second {
		return nil, ErrInvalidConfig
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Answerer == nil {
		cfg.Answerer = func(ctx context.Context, offer []byte, network remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			return remotevoice.Answer(ctx, offer, network, port)
		}
	}
	port, err := remotevoice.NewFramePort(cfg.FrameCapacity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &Bridge{
		source: cfg.Source, port: port, answerer: cfg.Answerer,
		freshness: cfg.FreshnessWindow, now: cfg.Now,
		ctx: ctx, cancel: cancel, activated: make(chan struct{}), done: make(chan struct{}),
	}
	bridge.wg.Add(2)
	go bridge.pumpGatewayToBrowser()
	go bridge.pumpBrowserToGateway()
	return bridge, nil
}

func (b *Bridge) ID() string                  { return b.source.ID() }
func (b *Bridge) CallRef() sipgateway.CallRef { return b.source.CallRef() }
func (b *Bridge) Codec() sipgateway.Codec     { return b.source.Codec() }

// ReadGatewayFrame and WriteGatewayFrame are deliberately not a second media
// consumer. Bridge itself is the sole owner of the source pumps.
func (*Bridge) ReadGatewayFrame(context.Context) (sipgateway.MediaFrame, error) {
	return sipgateway.MediaFrame{}, sipgateway.ErrMediaLeaseMismatch
}
func (*Bridge) WriteGatewayFrame(context.Context, sipgateway.MediaFrame) error {
	return sipgateway.ErrMediaLeaseMismatch
}

// Answer creates exactly one restricted Pion peer. Exact retries return the
// cached SDP; a different offer or effective network policy is rejected.
func (b *Bridge) Answer(ctx context.Context, offer []byte, network remotevoice.Config) ([]byte, error) {
	b.answerMu.Lock()
	defer b.answerMu.Unlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	b.mu.Unlock()
	fingerprint, err := answerFingerprint(offer, network)
	if err != nil {
		return nil, err
	}
	if b.answerStarted {
		if fingerprint != b.answerFingerprint {
			return nil, ErrOfferConflict
		}
		return append([]byte(nil), b.answerSDP...), nil
	}
	b.answerStarted = true
	b.answerFingerprint = fingerprint

	// Register the potentially long-running answer operation before Close can
	// start waiting. Its context is canceled by either the caller or Bridge.Close,
	// and Close does not return while an answerer can still publish resources.
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()
	answerCtx, cancelAnswer := context.WithCancel(ctx)
	stopBridgeCancel := context.AfterFunc(b.ctx, cancelAnswer)
	defer func() {
		stopBridgeCancel()
		cancelAnswer()
	}()
	peer, answer, err := b.answerer(answerCtx, append([]byte(nil), offer...), cloneNetworkConfig(network), b.port)
	if err != nil || peer == nil || len(answer) == 0 {
		if peer != nil {
			_ = peer.Close()
		}
		b.beginClose(ErrNetworkNotPrepared)
		return nil, ErrNetworkNotPrepared
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		_ = peer.Close()
		return nil, ErrClosed
	}
	b.peerMu.Lock()
	b.peer = peer
	b.peerMu.Unlock()
	b.answerSDP = append([]byte(nil), answer...)
	b.wg.Add(1)
	b.mu.Unlock()
	go b.watchPeer(peer)
	return append([]byte(nil), answer...), nil
}

// Activate establishes one immutable post-answer epoch across the gateway,
// frame port, and WebRTC peer. It is idempotent only for the same epoch.
func (b *Bridge) Activate(epoch uint64) (sipgateway.MediaSnapshot, error) {
	b.activateMu.Lock()
	registered := false
	defer func() {
		// The WaitGroup covers every internal activation critical section. Close
		// must not return while Activate still owns activateMu or can publish
		// state, even though it cannot synchronize with code that runs after this
		// method returns in the caller's goroutine.
		b.activateMu.Unlock()
		if registered {
			b.wg.Done()
		}
	}()
	if epoch == 0 {
		return sipgateway.MediaSnapshot{}, ErrInvalidConfig
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return sipgateway.MediaSnapshot{}, ErrClosed
	}
	// Close starts its Wait only after setting closing under this same mutex.
	// Register activation first so Close cannot return while source/peer
	// activation is still publishing state or transport resources.
	b.wg.Add(1)
	registered = true
	if b.epoch != 0 {
		same := b.epoch == epoch
		b.mu.Unlock()
		if !same {
			return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaLeaseMismatch
		}
		return b.Snapshot(), nil
	}
	b.mu.Unlock()
	if !b.networkPrepared(b.now()) {
		return sipgateway.MediaSnapshot{}, ErrNetworkNotPrepared
	}
	b.peerMu.RLock()
	peer := b.peer
	b.peerMu.RUnlock()
	if peer == nil {
		return sipgateway.MediaSnapshot{}, ErrNetworkNotPrepared
	}

	b.barrierMu.Lock()
	sourceSnapshot, sourceErr := b.source.Activate(epoch)
	if sourceErr != nil || !validActivatedSource(sourceSnapshot, b.source, epoch) {
		b.barrierMu.Unlock()
		b.beginClose(sipgateway.ErrMediaNotPrepared)
		return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaNotPrepared
	}
	networkSnapshot, networkErr := peer.Activate(epoch)
	activatedAt := b.now()
	if networkErr != nil || !networkPrepared(networkSnapshot, activatedAt, b.freshness) {
		b.barrierMu.Unlock()
		b.beginClose(ErrNetworkNotPrepared)
		return sipgateway.MediaSnapshot{}, ErrNetworkNotPrepared
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		b.barrierMu.Unlock()
		return sipgateway.MediaSnapshot{}, ErrClosed
	}
	b.epoch = epoch
	b.activatedAt = activatedAt
	b.networkRealUplinkBaseline = networkSnapshot.RealUplinkPackets
	b.networkRemoteRTPBaseline = networkSnapshot.RemoteRTPPackets
	b.networkRRBaseline = networkSnapshot.ReceiverReports
	close(b.activated)
	b.mu.Unlock()
	b.barrierMu.Unlock()
	return b.Snapshot(), nil
}

// Snapshot folds current browser transport proof into the adapter media
// snapshot. Source counters remain visible, but freshness timestamps are
// cleared unless the corresponding WebRTC direction is also proven fresh.
func (b *Bridge) Snapshot() sipgateway.MediaSnapshot {
	source := b.source.Snapshot()
	status := b.Status()
	source.Prepared = source.Prepared && status.Prepared
	source.Closed = source.Closed || status.Closed
	source.Activated = source.Activated && status.Activated
	if !status.GatewayToClientFresh {
		source.LastGatewayToClientAt = time.Time{}
		source.LastGatewayToClientEpoch = 0
	}
	if !status.ClientToGatewayFresh {
		source.LastClientToGatewayAt = time.Time{}
		source.LastClientToGatewayEpoch = 0
	}
	return source
}

// Status returns a credential-free composite media proof.
func (b *Bridge) Status() Snapshot {
	source := b.source.Snapshot()
	b.peerMu.RLock()
	peer := b.peer
	network := remotevoice.NetworkActivitySnapshot{}
	if peer != nil {
		network = peer.NetworkActivitySnapshot()
	}
	b.peerMu.RUnlock()
	b.mu.Lock()
	closing := b.closing
	epoch := b.epoch
	activatedAt := b.activatedAt
	realUplinkBaseline := b.networkRealUplinkBaseline
	remoteRTPBaseline := b.networkRemoteRTPBaseline
	rrBaseline := b.networkRRBaseline
	b.mu.Unlock()
	now := b.now()
	prepared := !closing && source.Prepared && !source.Closed && networkPrepared(network, now, b.freshness)
	activated := prepared && epoch != 0 && source.Activated && source.ActivationEpoch == epoch
	gatewayFresh := activated && source.GatewayToClientFresh(now, b.freshness) &&
		network.RealUplinkPackets > realUplinkBaseline && network.ReceiverReports > rrBaseline &&
		network.LastRealUplinkEpoch == epoch && network.LastReceiverReportEpoch == epoch &&
		network.RealUplinkPacketsAtLastReceiverReport > realUplinkBaseline &&
		freshAfter(now, network.LastRealUplinkAt, activatedAt, b.freshness) &&
		freshAfter(now, network.LastReceiverReportAt, activatedAt, b.freshness)
	clientFresh := activated && source.ClientToGatewayFresh(now, b.freshness) &&
		network.RemoteRTPPackets > remoteRTPBaseline && network.LastRemoteRTPEpoch == epoch &&
		freshAfter(now, network.LastRemoteRTPAt, activatedAt, b.freshness)
	return Snapshot{
		Prepared: prepared, Activated: activated, ActivationEpoch: epoch,
		BidirectionalFresh:   gatewayFresh && clientFresh,
		GatewayToClientFresh: gatewayFresh, ClientToGatewayFresh: clientFresh,
		WebRTCConnected: network.Connected, WebRTCSelectedPair: network.SelectedPair,
		WebRTCRemoteTrack:    network.RemoteTrackReady,
		WebRTCReceiverReport: network.ReceiverReports > 0,
		Closed:               closing || source.Closed,
	}
}

func (b *Bridge) networkPrepared(now time.Time) bool {
	b.peerMu.RLock()
	peer := b.peer
	if peer == nil {
		b.peerMu.RUnlock()
		return false
	}
	network := peer.NetworkActivitySnapshot()
	b.peerMu.RUnlock()
	return networkPrepared(network, now, b.freshness)
}

func networkPrepared(network remotevoice.NetworkActivitySnapshot, now time.Time, freshness time.Duration) bool {
	return network.Connected && network.SelectedPair.PolicyValid() && network.LocalTrackStarted &&
		network.RemoteTrackReady && network.UplinkPackets > 0 && network.RemoteRTPPackets > 0 &&
		network.ReceiverReports > 0 && fresh(now, network.LastUplinkAt, freshness) &&
		fresh(now, network.LastRemoteRTPAt, freshness) && fresh(now, network.LastReceiverReportAt, freshness)
}

func validActivatedSource(snapshot sipgateway.MediaSnapshot, source sipgateway.MediaSession, epoch uint64) bool {
	return source != nil && epoch != 0 && snapshot.LeaseID == source.ID() &&
		snapshot.Call == source.CallRef() && snapshot.Codec == source.Codec() &&
		snapshot.Prepared && snapshot.Activated && !snapshot.Closed &&
		snapshot.ActivationEpoch == epoch && !snapshot.ActivatedAt.IsZero()
}

func (b *Bridge) pumpGatewayToBrowser() {
	defer b.wg.Done()
	for {
		b.mu.Lock()
		epoch := b.epoch
		b.mu.Unlock()
		frame, err := b.source.ReadGatewayFrame(b.ctx)
		if err != nil {
			if b.ctx.Err() == nil {
				b.beginClose(ErrGatewayMediaEnded)
			}
			return
		}
		if frame.Validate() != nil || len(frame.Payload) != remotevoice.FrameSamples {
			b.beginClose(sipgateway.ErrMediaNotPrepared)
			return
		}
		pcm := remotevoice.DecodePCMU(frame.Payload)
		b.barrierMu.Lock()
		b.mu.Lock()
		currentEpoch := b.epoch
		closing := b.closing
		b.mu.Unlock()
		// Continuously drain gateway early media so a bounded provider queue
		// cannot fill while the user is deciding whether to answer. Epoch zero
		// is never forwarded, and a frame whose read straddles activation is
		// discarded because its captured epoch no longer matches.
		if !closing && epoch != 0 && currentEpoch == epoch {
			err = b.port.PushUplinkAtEpoch(pcm, epoch)
		}
		b.barrierMu.Unlock()
		if closing {
			return
		}
		if epoch == 0 || currentEpoch != epoch || errors.Is(err, remotevoice.ErrStaleMediaEpoch) {
			continue
		}
		if err != nil {
			b.beginClose(ErrGatewayMediaEnded)
			return
		}
	}
}

func (b *Bridge) pumpBrowserToGateway() {
	defer b.wg.Done()
	if !b.waitForActivation() {
		return
	}
	ticker := time.NewTicker(remotevoice.FrameMillis * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
		}
		b.mu.Lock()
		epoch := b.epoch
		b.mu.Unlock()
		pcm, ok, err := b.port.TakeDownlinkAtEpoch(epoch)
		if errors.Is(err, remotevoice.ErrStaleMediaEpoch) {
			continue
		}
		if err != nil {
			if !errors.Is(err, remotevoice.ErrClosed) {
				b.beginClose(ErrNetworkEnded)
			}
			return
		}
		if !ok {
			continue
		}
		b.barrierMu.Lock()
		b.mu.Lock()
		currentEpoch := b.epoch
		closing := b.closing
		if !closing && currentEpoch == epoch {
			b.nextDownlinkSequence++
			if b.nextDownlinkSequence == 0 {
				closing = true
			}
		}
		sequence := b.nextDownlinkSequence
		b.mu.Unlock()
		if !closing && currentEpoch == epoch {
			err = b.source.WriteGatewayFrame(b.ctx, sipgateway.MediaFrame{
				Sequence: sequence, Payload: remotevoice.EncodePCMU(pcm),
			})
		}
		b.barrierMu.Unlock()
		if closing {
			b.beginClose(sipgateway.ErrMediaQueueFull)
			return
		}
		if currentEpoch != epoch {
			continue
		}
		if err != nil {
			b.beginClose(ErrGatewayMediaEnded)
			return
		}
	}
}

func (b *Bridge) waitForActivation() bool {
	select {
	case <-b.ctx.Done():
		return false
	case <-b.activated:
		return true
	}
}

func (b *Bridge) watchPeer(peer remotevoice.MediaPeer) {
	defer b.wg.Done()
	_ = peer.Wait(b.ctx)
	if b.ctx.Err() == nil {
		b.beginClose(ErrNetworkEnded)
	}
}

// Wait returns after the bridge and both transports are closed.
func (b *Bridge) Wait(ctx context.Context) error {
	if ctx == nil {
		return errors.New("sipwebrtc: context is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.terminalErr
	}
}

// Close is idempotent and waits for all frame pumps to exit.
func (b *Bridge) Close() error {
	b.beginClose(nil)
	<-b.done
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.terminalErr
}

func (b *Bridge) beginClose(terminal error) {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.terminalErr = terminal
		b.mu.Unlock()
		b.cancel()
		b.peerMu.RLock()
		peer := b.peer
		b.peerMu.RUnlock()
		if peer != nil {
			_ = peer.Close()
		}
		_ = b.source.Close()
		b.port.Close()
		go func() {
			b.wg.Wait()
			close(b.done)
		}()
	})
}

func answerFingerprint(offer []byte, network remotevoice.Config) ([sha256.Size]byte, error) {
	type relayFingerprint struct {
		URLs           []string
		Username       string
		Password       string
		CredentialType remotevoice.TURNCredentialType
	}
	var relay *relayFingerprint
	if network.TURNRelay != nil {
		relay = &relayFingerprint{
			URLs:     append([]string(nil), network.TURNRelay.URLs...),
			Username: network.TURNRelay.Username, Password: network.TURNRelay.Password,
			CredentialType: network.TURNRelay.CredentialType,
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
		Token: network.Token, Generation: network.Generation,
		AllowedInterfaces: network.AllowedInterfaces, AllowedLocalCIDRs: network.AllowedLocalCIDRs,
		AllowedRemoteCIDRs: network.AllowedRemoteCIDRs, UDPMin: network.UDPMin, UDPMax: network.UDPMax,
		AnswerTimeout: network.AnswerTimeout, TURNRelay: relay,
	})
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("sipwebrtc: fingerprint network config: %w", err)
	}
	hash := sha256.New()
	_, _ = hash.Write(offer)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(encoded)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func cloneNetworkConfig(cfg remotevoice.Config) remotevoice.Config {
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

func fresh(now, event time.Time, window time.Duration) bool {
	return !event.IsZero() && !event.After(now) && now.Sub(event) <= window
}

func freshAfter(now, event, activation time.Time, window time.Duration) bool {
	return !activation.IsZero() && !event.Before(activation) && fresh(now, event, window)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("sipwebrtc: context is required")
	}
	return ctx.Err()
}
