package sipwebrtc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

type fakeGatewayMedia struct {
	ref    sipgateway.CallRef
	id     string
	read   chan sipgateway.MediaFrame
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	snap   sipgateway.MediaSnapshot
	writes []sipgateway.MediaFrame
	now    func() time.Time
}

func newFakeGatewayMedia(now func() time.Time) *fakeGatewayMedia {
	ref := sipgateway.CallRef{
		GatewayID: "asterisk-home", BootEpoch: "boot-epoch-bridge-1",
		ProviderHandle: "provider-dialog-bridge-1", Revision: 1,
	}
	id := "gateway-media-private-1"
	return &fakeGatewayMedia{
		ref: ref, id: id, read: make(chan sipgateway.MediaFrame, 16), done: make(chan struct{}), now: now,
		snap: sipgateway.MediaSnapshot{LeaseID: id, Call: ref, Codec: sipgateway.CodecPCMU, Prepared: true},
	}
}

func (m *fakeGatewayMedia) ID() string                  { return m.id }
func (m *fakeGatewayMedia) CallRef() sipgateway.CallRef { return m.ref }
func (m *fakeGatewayMedia) Codec() sipgateway.Codec     { return sipgateway.CodecPCMU }

func (m *fakeGatewayMedia) ReadGatewayFrame(ctx context.Context) (sipgateway.MediaFrame, error) {
	select {
	case <-ctx.Done():
		return sipgateway.MediaFrame{}, ctx.Err()
	case <-m.done:
		return sipgateway.MediaFrame{}, sipgateway.ErrMediaClosed
	case frame := <-m.read:
		m.mu.Lock()
		m.snap.GatewayToClientFrames++
		m.snap.LastGatewayToClientAt = m.now()
		m.snap.LastGatewayToClientEpoch = m.snap.ActivationEpoch
		m.mu.Unlock()
		return frame, nil
	}
}

func (m *fakeGatewayMedia) WriteGatewayFrame(ctx context.Context, frame sipgateway.MediaFrame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.done:
		return sipgateway.ErrMediaClosed
	default:
	}
	m.mu.Lock()
	m.writes = append(m.writes, frame)
	m.snap.ClientToGatewayFrames++
	m.snap.LastClientToGatewayAt = m.now()
	m.snap.LastClientToGatewayEpoch = m.snap.ActivationEpoch
	m.mu.Unlock()
	return nil
}

func (m *fakeGatewayMedia) Activate(epoch uint64) (sipgateway.MediaSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch == 0 || m.snap.Closed {
		return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaClosed
	}
	if m.snap.Activated {
		if m.snap.ActivationEpoch != epoch {
			return sipgateway.MediaSnapshot{}, sipgateway.ErrMediaLeaseMismatch
		}
		return m.snap, nil
	}
	m.snap.Activated = true
	m.snap.ActivationEpoch = epoch
	m.snap.ActivatedAt = m.now()
	m.snap.GatewayToClientBaseline = m.snap.GatewayToClientFrames
	m.snap.ClientToGatewayBaseline = m.snap.ClientToGatewayFrames
	return m.snap, nil
}

func (m *fakeGatewayMedia) Snapshot() sipgateway.MediaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

func (m *fakeGatewayMedia) Close() error {
	m.once.Do(func() {
		m.mu.Lock()
		m.snap.Closed = true
		m.mu.Unlock()
		close(m.done)
	})
	return nil
}

func (m *fakeGatewayMedia) offer(sequence uint64, payload byte) {
	m.read <- sipgateway.MediaFrame{Sequence: sequence, Payload: bytesOf(payload, remotevoice.FrameSamples)}
}

func (m *fakeGatewayMedia) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.writes)
}

type fakePeer struct {
	port        *remotevoice.FramePort
	mu          sync.Mutex
	net         remotevoice.NetworkActivitySnapshot
	activateErr error
	done        chan struct{}
	once        sync.Once
}

type readErrorMedia struct {
	*fakeGatewayMedia
	err     error
	release chan struct{}
}

func (m *readErrorMedia) ReadGatewayFrame(ctx context.Context) (sipgateway.MediaFrame, error) {
	select {
	case <-ctx.Done():
		return sipgateway.MediaFrame{}, ctx.Err()
	case <-m.release:
		return sipgateway.MediaFrame{}, m.err
	}
}

type blockingActivateMedia struct {
	*fakeGatewayMedia
	activateStarted chan struct{}
	activateExited  chan struct{}
	releaseActivate chan struct{}
	releaseOnce     sync.Once
}

func (m *blockingActivateMedia) Activate(epoch uint64) (sipgateway.MediaSnapshot, error) {
	close(m.activateStarted)
	defer close(m.activateExited)
	<-m.releaseActivate
	return m.fakeGatewayMedia.Activate(epoch)
}

func (m *blockingActivateMedia) Close() error {
	m.releaseOnce.Do(func() { close(m.releaseActivate) })
	return m.fakeGatewayMedia.Close()
}

func newFakePeer(port *remotevoice.FramePort, now time.Time) *fakePeer {
	return &fakePeer{
		port: port, done: make(chan struct{}),
		net: remotevoice.NetworkActivitySnapshot{
			Connected: true,
			SelectedPair: remotevoice.SelectedPairSnapshot{
				Established: true, Protocol: remotevoice.SelectedPairProtocolUDP,
				LocalCandidateType:  remotevoice.SelectedCandidateTypeHost,
				RemoteCandidateType: remotevoice.SelectedCandidateTypePeerReflexive,
				LocalAddressAllowed: true, RemoteAddressAllowed: true,
			},
			LocalTrackStarted: true, UplinkPackets: 1, LastUplinkAt: now,
			RemoteTrackReady: true, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
			ReceiverReports: 1, LastReceiverReportAt: now,
		},
	}
}

func (p *fakePeer) Connected() bool { return true }
func (p *fakePeer) NetworkActivitySnapshot() remotevoice.NetworkActivitySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.net
}
func (p *fakePeer) Activate(epoch uint64) (remotevoice.NetworkActivitySnapshot, error) {
	if p.activateErr != nil {
		return remotevoice.NetworkActivitySnapshot{}, p.activateErr
	}
	if err := p.port.Activate(epoch); err != nil {
		return remotevoice.NetworkActivitySnapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.net, nil
}
func (p *fakePeer) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return remotevoice.ErrPeerConnectionEnded
	}
}
func (p *fakePeer) Close() error { p.once.Do(func() { close(p.done) }); return nil }

func (p *fakePeer) receiveGatewayFrame(epoch uint64, now time.Time) bool {
	_, ok, err := p.port.TakeUplinkAtEpoch(epoch)
	if err != nil || !ok {
		return false
	}
	p.mu.Lock()
	p.net.UplinkPackets++
	p.net.LastUplinkAt = now
	p.net.LastUplinkEpoch = epoch
	p.net.RealUplinkPackets++
	p.net.LastRealUplinkAt = now
	p.net.LastRealUplinkEpoch = epoch
	p.net.ReceiverReports++
	p.net.LastReceiverReportAt = now
	p.net.LastReceiverReportEpoch = epoch
	p.net.RealUplinkPacketsAtLastReceiverReport = p.net.RealUplinkPackets
	p.mu.Unlock()
	return true
}

func (p *fakePeer) sendBrowserFrame(epoch uint64, now time.Time) error {
	if err := p.port.PushDownlinkAtEpoch(make([]int16, remotevoice.FrameSamples), epoch); err != nil {
		return err
	}
	p.mu.Lock()
	p.net.RemoteRTPPackets++
	p.net.LastRemoteRTPAt = now
	p.net.LastRemoteRTPEpoch = epoch
	p.mu.Unlock()
	return nil
}

func TestBridgeRequiresBothPostActivationDirections(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 15, 0, 0, 0, time.UTC)}
	source := newFakeGatewayMedia(clock.Now)
	var peer *fakePeer
	bridge, err := New(Config{
		Source: source, Now: clock.Now, FreshnessWindow: 2 * time.Second,
		Answerer: func(_ context.Context, _ []byte, _ remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			peer = newFakePeer(port, clock.Now())
			return peer, []byte("synthetic-answer"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	if bridge.Snapshot().Prepared {
		t.Fatal("gateway-only preparation must not authorize media")
	}
	answer, err := bridge.Answer(context.Background(), []byte("synthetic-offer"), remotevoice.Config{
		Token: "synthetic-session-token", Generation: 7,
	})
	if err != nil || string(answer) != "synthetic-answer" {
		t.Fatalf("Answer=%q err=%v", answer, err)
	}
	if !bridge.Snapshot().Prepared {
		t.Fatal("composite preparation should be visible after network proof")
	}
	if _, err := bridge.Activate(9); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if bridge.Status().BidirectionalFresh {
		t.Fatal("activation baselines must exclude preparation traffic")
	}

	clock.Advance(20 * time.Millisecond)
	// The read that was already blocked at epoch zero is deliberately discarded
	// across activation; the next frame is the first eligible epoch-nine frame.
	source.offer(1, 0xff)
	source.offer(2, 0xfe)
	eventually(t, func() bool { return peer.receiveGatewayFrame(9, clock.Now()) })
	if bridge.Status().BidirectionalFresh {
		t.Fatal("one-way gateway audio must not satisfy bidirectional freshness")
	}
	clock.Advance(20 * time.Millisecond)
	if err := peer.sendBrowserFrame(9, clock.Now()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return source.writeCount() == 1 })
	status := bridge.Status()
	if !status.GatewayToClientFresh || !status.ClientToGatewayFresh || !status.BidirectionalFresh {
		t.Fatalf("status=%+v", status)
	}
	snapshot := bridge.Snapshot()
	if !snapshot.BidirectionalFresh(clock.Now(), 2*time.Second) {
		t.Fatalf("coordinator snapshot did not preserve composite proof: %+v", snapshot)
	}
}

func TestBridgeOfferIsIdempotentAndConflictingOfferFails(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 15, 30, 0, 0, time.UTC)}
	source := newFakeGatewayMedia(clock.Now)
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(_ context.Context, _ []byte, _ remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			return newFakePeer(port, clock.Now()), []byte("answer-once"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	config := remotevoice.Config{Token: "opaque-network-owner", Generation: 11}
	first, err := bridge.Answer(context.Background(), []byte("offer-a"), config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bridge.Answer(context.Background(), []byte("offer-a"), config)
	if err != nil || string(first) != string(second) {
		t.Fatalf("exact replay answer=%q err=%v", second, err)
	}
	if _, err := bridge.Answer(context.Background(), []byte("offer-b"), config); !errors.Is(err, ErrOfferConflict) {
		t.Fatalf("conflicting offer error=%v", err)
	}
}

func TestBridgeFingerprintsClonesAndForwardsTURNRelayConfig(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 15, 35, 0, 0, time.UTC)}
	source := newFakeGatewayMedia(clock.Now)
	var captured remotevoice.Config
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(_ context.Context, _ []byte, network remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			captured = network
			return newFakePeer(port, clock.Now()), []byte("relay-answer"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	network := remotevoice.Config{
		Token: "opaque-relay-owner", Generation: 13,
		AllowedInterfaces: []string{"en7"}, UDPMin: 42000, UDPMax: 42999,
		AnswerTimeout: 4 * time.Second,
		TURNRelay: &remotevoice.TURNRelayConfig{
			URLs: []string{
				"turn:turn.example.test:3478?transport=udp",
				"turns:turn.example.test:443?transport=tcp",
			},
			Username: "short-lived-user", Password: "short-lived-password",
			CredentialType: remotevoice.TURNCredentialTypePassword,
		},
	}
	baseFingerprint, err := answerFingerprint([]byte("relay-offer"), network)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*remotevoice.Config){
		func(c *remotevoice.Config) { c.TURNRelay.URLs[0] = "turn:other.example.test:3478?transport=udp" },
		func(c *remotevoice.Config) { c.TURNRelay.URLs[1] = "turns:other.example.test:443?transport=tcp" },
		func(c *remotevoice.Config) {
			c.TURNRelay.URLs[0], c.TURNRelay.URLs[1] = c.TURNRelay.URLs[1], c.TURNRelay.URLs[0]
		},
		func(c *remotevoice.Config) { c.TURNRelay.Username = "other-user" },
		func(c *remotevoice.Config) { c.TURNRelay.Password = "other-password" },
		func(c *remotevoice.Config) { c.TURNRelay.CredentialType = "oauth" },
	} {
		candidate := cloneNetworkConfig(network)
		mutate(&candidate)
		fingerprint, fingerprintErr := answerFingerprint([]byte("relay-offer"), candidate)
		if fingerprintErr != nil {
			t.Fatal(fingerprintErr)
		}
		if fingerprint == baseFingerprint {
			t.Fatal("TURN relay configuration change did not alter answer fingerprint")
		}
	}
	answer, err := bridge.Answer(context.Background(), []byte("relay-offer"), network)
	if err != nil || string(answer) != "relay-answer" {
		t.Fatalf("Answer=%q err=%v", answer, err)
	}
	if captured.TURNRelay == nil || captured.TURNRelay == network.TURNRelay ||
		len(captured.TURNRelay.URLs) != 2 || captured.TURNRelay.URLs[0] != network.TURNRelay.URLs[0] ||
		captured.TURNRelay.URLs[1] != network.TURNRelay.URLs[1] ||
		captured.TURNRelay.Username != network.TURNRelay.Username ||
		captured.TURNRelay.Password != network.TURNRelay.Password ||
		captured.TURNRelay.CredentialType != network.TURNRelay.CredentialType {
		t.Fatal("bridge did not forward a complete independent TURN relay config clone")
	}
	captured.TURNRelay.URLs[0] = "turn:captured-mutated.example:3478?transport=udp"
	captured.TURNRelay.Username = "captured-mutated-user"
	if network.TURNRelay.URLs[0] == captured.TURNRelay.URLs[0] || network.TURNRelay.Username == captured.TURNRelay.Username {
		t.Fatal("answerer-owned TURN config aliases caller-owned relay config")
	}
}

func TestBridgeMovesNoAudioBeforeActivation(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 15, 45, 0, 0, time.UTC)}
	source := newFakeGatewayMedia(clock.Now)
	var peer *fakePeer
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(_ context.Context, _ []byte, _ remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			peer = newFakePeer(port, clock.Now())
			return peer, []byte("synthetic-answer"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	if _, err := bridge.Answer(context.Background(), []byte("synthetic-offer"), remotevoice.Config{
		Token: "synthetic-session-token", Generation: 17,
	}); err != nil {
		t.Fatal(err)
	}
	source.offer(1, 0xfd)
	if err := peer.sendBrowserFrame(0, clock.Now()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * remotevoice.FrameMillis * time.Millisecond)
	if got := source.writeCount(); got != 0 {
		t.Fatalf("browser audio reached gateway before activation: writes=%d", got)
	}
	eventually(t, func() bool { return len(source.read) == 0 })
	if peer.receiveGatewayFrame(0, clock.Now()) {
		t.Fatal("gateway early media reached the browser transport before activation")
	}
}

func TestBridgeCloseCancelsAndWaitsForInFlightAnswer(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC)}
	source := newFakeGatewayMedia(clock.Now)
	started := make(chan struct{})
	answererDone := make(chan struct{})
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(ctx context.Context, _ []byte, _ remotevoice.Config, _ *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			close(started)
			<-ctx.Done()
			close(answererDone)
			return nil, nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	answerDone := make(chan error, 1)
	go func() {
		_, answerErr := bridge.Answer(context.Background(), []byte("synthetic-offer"), remotevoice.Config{
			Token: "synthetic-session-token", Generation: 19,
		})
		answerDone <- answerErr
	}()
	<-started
	if err := bridge.Close(); !errors.Is(err, ErrNetworkNotPrepared) && err != nil {
		t.Fatalf("Close error=%v", err)
	}
	select {
	case <-answererDone:
	default:
		t.Fatal("Close returned before the in-flight answerer exited")
	}
	if answerErr := <-answerDone; !errors.Is(answerErr, ErrNetworkNotPrepared) {
		t.Fatalf("Answer error=%v", answerErr)
	}
}

func TestBridgePartialActivationClosesBothTransports(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 16, 15, 0, 0, time.UTC)}
	source := newFakeGatewayMedia(clock.Now)
	var peer *fakePeer
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(_ context.Context, _ []byte, _ remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			peer = newFakePeer(port, clock.Now())
			peer.activateErr = errors.New("synthetic activation failure")
			return peer, []byte("synthetic-answer"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Answer(context.Background(), []byte("synthetic-offer"), remotevoice.Config{
		Token: "synthetic-session-token", Generation: 23,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Activate(29); !errors.Is(err, ErrNetworkNotPrepared) {
		t.Fatalf("Activate error=%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bridge.Wait(ctx); !errors.Is(err, ErrNetworkNotPrepared) {
		t.Fatalf("Wait error=%v", err)
	}
	if !bridge.Status().Closed || !source.Snapshot().Closed {
		t.Fatal("partial activation did not close the composite lease")
	}
}

func TestBridgeWaitRejectsNilContext(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 16, 30, 0, 0, time.UTC)}
	bridge, err := New(Config{Source: newFakeGatewayMedia(clock.Now), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	if err := bridge.Wait(nil); err == nil {
		t.Fatal("nil context was accepted")
	}
}

func TestBridgeRedactsGatewayTransportError(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 16, 45, 0, 0, time.UTC)}
	source := &readErrorMedia{
		fakeGatewayMedia: newFakeGatewayMedia(clock.Now),
		err:              errors.New("synthetic-private-provider-url-marker"),
		release:          make(chan struct{}),
	}
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(_ context.Context, _ []byte, _ remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			return newFakePeer(port, clock.Now()), []byte("synthetic-answer"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Answer(context.Background(), []byte("synthetic-offer"), remotevoice.Config{
		Token: "synthetic-session-token", Generation: 31,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Activate(37); err != nil && !errors.Is(err, ErrGatewayMediaEnded) {
		t.Fatalf("Activate error=%v", err)
	}
	close(source.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = bridge.Wait(ctx)
	if !errors.Is(err, ErrGatewayMediaEnded) || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatalf("transport error was not stable and redacted: %v", err)
	}
}

func TestBridgeCloseWaitsForConcurrentActivation(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC)}
	source := &blockingActivateMedia{
		fakeGatewayMedia: newFakeGatewayMedia(clock.Now),
		activateStarted:  make(chan struct{}),
		activateExited:   make(chan struct{}),
		releaseActivate:  make(chan struct{}),
	}
	bridge, err := New(Config{
		Source: source, Now: clock.Now,
		Answerer: func(_ context.Context, _ []byte, _ remotevoice.Config, port *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
			return newFakePeer(port, clock.Now()), []byte("synthetic-answer"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Answer(context.Background(), []byte("synthetic-offer"), remotevoice.Config{
		Token: "synthetic-session-token", Generation: 41,
	}); err != nil {
		t.Fatal(err)
	}
	activateDone := make(chan struct{})
	go func() {
		_, _ = bridge.Activate(43)
		close(activateDone)
	}()
	<-source.activateStarted
	if err := bridge.Close(); err != nil {
		t.Fatalf("Close error=%v", err)
	}
	select {
	case <-source.activateExited:
	default:
		t.Fatal("Close returned before the source activation exited")
	}
	// The caller goroutine may not yet have executed code after Activate's
	// return. Wait for it only to avoid leaking a goroutine into later tests;
	// it is deliberately not the synchronization assertion above.
	<-activateDone
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func bytesOf(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}
