package remotevoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testMediaSessionID  = "opaque-media-session-0123456789"
	testLeaseGeneration = uint64(73)
)

func validSelectedPairSnapshot() SelectedPairSnapshot {
	return SelectedPairSnapshot{
		Established: true, Protocol: SelectedPairProtocolUDP,
		LocalCandidateType: SelectedCandidateTypeHost, RemoteCandidateType: SelectedCandidateTypeHost,
		LocalAddressAllowed: true, RemoteAddressAllowed: true,
	}
}

type lockedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockedClock) Advance(delta time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
	return c.now
}

type fakeLocalPCMBroker struct {
	mu         sync.Mutex
	activity   IPCBrokerActivitySnapshot
	waitErr    error
	closeErr   error
	closeCount int
	done       chan struct{}
	doneOnce   sync.Once
	epoch      uint64
}

func newFakeLocalPCMBroker() *fakeLocalPCMBroker {
	return &fakeLocalPCMBroker{done: make(chan struct{})}
}

func (b *fakeLocalPCMBroker) ActivitySnapshot() IPCBrokerActivitySnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activity
}

func (b *fakeLocalPCMBroker) Activate(epoch uint64) (IPCBrokerActivitySnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.epoch != 0 && b.epoch != epoch {
		return IPCBrokerActivitySnapshot{}, errors.New("epoch reset")
	}
	b.epoch = epoch
	return b.activity, nil
}

func (b *fakeLocalPCMBroker) setActivity(activity IPCBrokerActivitySnapshot) {
	b.mu.Lock()
	if b.epoch != 0 {
		if activity.DeviceUplinkFrames > 0 && activity.LastDeviceUplinkEpoch == 0 {
			activity.LastDeviceUplinkEpoch = b.epoch
		}
		if activity.RemoteDownlinkFrames > 0 && activity.LastRemoteDownlinkEpoch == 0 {
			activity.LastRemoteDownlinkEpoch = b.epoch
		}
	}
	b.activity = activity
	b.mu.Unlock()
}

func (b *fakeLocalPCMBroker) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.waitErr
	}
}

func (b *fakeLocalPCMBroker) Close() error {
	b.mu.Lock()
	b.closeCount++
	err := b.closeErr
	b.mu.Unlock()
	b.doneOnce.Do(func() { close(b.done) })
	return err
}

func (b *fakeLocalPCMBroker) fail(err error) {
	b.mu.Lock()
	b.waitErr = err
	b.mu.Unlock()
	b.doneOnce.Do(func() { close(b.done) })
}

func (b *fakeLocalPCMBroker) closes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeCount
}

type fakeMediaPeer struct {
	connected  atomic.Bool
	mu         sync.Mutex
	activity   NetworkActivitySnapshot
	waitErr    error
	closeErr   error
	closeCount int
	done       chan struct{}
	doneOnce   sync.Once
	epoch      uint64
}

type blockingActivationMediaPeer struct {
	*fakeMediaPeer
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newBlockingActivationMediaPeer() *blockingActivationMediaPeer {
	return &blockingActivationMediaPeer{
		fakeMediaPeer: newFakeMediaPeer(true),
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (p *blockingActivationMediaPeer) Activate(epoch uint64) (NetworkActivitySnapshot, error) {
	p.enteredOnce.Do(func() { close(p.entered) })
	<-p.release
	return p.fakeMediaPeer.Activate(epoch)
}

func (p *blockingActivationMediaPeer) Close() error {
	p.releaseOnce.Do(func() { close(p.release) })
	return p.fakeMediaPeer.Close()
}

func newFakeMediaPeer(connected bool) *fakeMediaPeer {
	peer := &fakeMediaPeer{done: make(chan struct{})}
	peer.connected.Store(connected)
	return peer
}

func (p *fakeMediaPeer) Connected() bool { return p.connected.Load() }

func (p *fakeMediaPeer) NetworkActivitySnapshot() NetworkActivitySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	activity := p.activity
	activity.Connected = p.connected.Load()
	return activity
}

func (p *fakeMediaPeer) Activate(epoch uint64) (NetworkActivitySnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epoch != 0 && p.epoch != epoch {
		return NetworkActivitySnapshot{}, errors.New("epoch reset")
	}
	p.epoch = epoch
	activity := p.activity
	activity.Connected = p.connected.Load()
	return activity, nil
}

func (p *fakeMediaPeer) setNetworkActivity(activity NetworkActivitySnapshot) {
	p.mu.Lock()
	if p.epoch != 0 {
		if activity.UplinkPackets > 0 && activity.LastUplinkEpoch == 0 {
			activity.LastUplinkEpoch = p.epoch
		}
		if activity.RealUplinkPackets > 0 && activity.LastRealUplinkEpoch == 0 {
			activity.LastRealUplinkEpoch = p.epoch
		}
		if activity.RemoteRTPPackets > 0 && activity.LastRemoteRTPEpoch == 0 {
			activity.LastRemoteRTPEpoch = p.epoch
		}
		if activity.ReceiverReports > 0 && activity.LastReceiverReportEpoch == 0 {
			activity.LastReceiverReportEpoch = p.epoch
		}
	}
	p.activity = activity
	p.mu.Unlock()
	p.connected.Store(activity.Connected)
}

func (p *fakeMediaPeer) setSelectedPair(pair SelectedPairSnapshot) {
	p.mu.Lock()
	p.activity.SelectedPair = pair
	p.mu.Unlock()
}

func (p *fakeMediaPeer) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.waitErr
	}
}

func (p *fakeMediaPeer) Close() error {
	p.connected.Store(false)
	p.mu.Lock()
	p.closeCount++
	err := p.closeErr
	p.mu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
	return err
}

func (p *fakeMediaPeer) fail(err error) {
	p.connected.Store(false)
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
}

func (p *fakeMediaPeer) closes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCount
}

type mediaLeaseHarness struct {
	lease       *MediaLeaseCoordinator
	broker      *fakeLocalPCMBroker
	clock       *lockedClock
	brokerCfg   LocalPCMBrokerStartConfig
	brokerCalls int
	mu          sync.Mutex
}

func newMediaLeaseHarness(t *testing.T, answerer MediaAnswerFunc) *mediaLeaseHarness {
	t.Helper()
	harness := &mediaLeaseHarness{
		broker: newFakeLocalPCMBroker(),
		clock:  &lockedClock{now: time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)},
	}
	lease, err := NewMediaLeaseCoordinator(MediaLeaseConfig{
		MediaSessionID:  testMediaSessionID,
		LeaseGeneration: testLeaseGeneration,
		SocketPath:      "/private/test/pcm.sock",
		FrameCapacity:   8,
		FreshnessWindow: time.Second,
		Now:             harness.clock.Now,
		Answerer:        answerer,
		StartBroker: func(cfg LocalPCMBrokerStartConfig) (LocalPCMBroker, error) {
			harness.mu.Lock()
			defer harness.mu.Unlock()
			harness.brokerCalls++
			harness.brokerCfg = cfg
			return harness.broker, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.lease = lease
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("cleanup media lease: %v", err)
		}
	})
	return harness
}

func TestMediaLeaseCreatesSingleGenerationOwnerAndRedactedSnapshot(t *testing.T) {
	t.Parallel()
	harness := newMediaLeaseHarness(t, nil)
	lease := harness.lease

	if lease.LeaseGeneration() != testLeaseGeneration ||
		!lease.Owns(testMediaSessionID, testLeaseGeneration) {
		t.Fatal("lease did not retain the exact opaque identity and caller-owned generation")
	}
	if lease.Owns(testMediaSessionID+"-other", testLeaseGeneration) ||
		lease.Owns(testMediaSessionID, testLeaseGeneration+1) {
		t.Fatal("lease accepted a different identity or generation")
	}
	if !errors.Is(lease.RequireOwner(testMediaSessionID, testLeaseGeneration+1), ErrMediaLeaseGenerationMismatch) ||
		!errors.Is(lease.RequireOwner(testMediaSessionID+"-other", testLeaseGeneration), ErrMediaLeaseIdentityMismatch) {
		t.Fatal("owner mismatch errors are not distinguishable")
	}

	harness.mu.Lock()
	brokerCfg := harness.brokerCfg
	brokerCalls := harness.brokerCalls
	harness.mu.Unlock()
	if brokerCalls != 1 || brokerCfg.LeaseGeneration != testLeaseGeneration ||
		brokerCfg.Port != lease.port || allZero(brokerCfg.SessionToken[:]) || brokerCfg.ActivityNow == nil {
		t.Fatalf("broker binding is incomplete: calls=%d generation=%d port=%p", brokerCalls, brokerCfg.LeaseGeneration, brokerCfg.Port)
	}
	if brokerCfg.AuthenticatedIdleTimeout != 30*time.Second {
		t.Fatalf("default prepared idle timeout = %v, want 30s", brokerCfg.AuthenticatedIdleTimeout)
	}

	snapshot := lease.Snapshot()
	if snapshot.Phase != MediaLeasePhaseNew || snapshot.LeaseGeneration != testLeaseGeneration ||
		snapshot.WebRTCConnected || snapshot.IPCClaimed || snapshot.IPCConsumed || snapshot.TransportPrepared || snapshot.ActiveFresh {
		t.Fatalf("new snapshot = %+v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testMediaSessionID) || strings.Contains(string(encoded), brokerCfg.SocketPath) ||
		strings.Contains(string(encoded), fmt.Sprintf("%x", brokerCfg.SessionToken)) {
		t.Fatalf("snapshot leaked lease credentials: %s", encoded)
	}
}

func TestTokenBearingLeaseValuesRedactFormattingAndJSON(t *testing.T) {
	t.Parallel()
	lease, err := NewSyntheticIPCLease(testLeaseGeneration)
	if err != nil {
		t.Fatal(err)
	}
	secret := fmt.Sprintf("%x", lease.SessionToken)
	values := []any{
		lease,
		Config{
			Token: secret, Generation: testLeaseGeneration,
			AllowedInterfaces: []string{"tailscale-secret-interface"},
		},
		&Session{token: secret, generation: testLeaseGeneration},
		&SyntheticIPCBroker{
			socketPath: "/private/redacted/pcm.sock", generation: testLeaseGeneration,
			token: lease.SessionToken,
		},
		&MediaLeaseCoordinator{
			mediaSessionID: testMediaSessionID, generation: testLeaseGeneration,
			ipcClaim: IPCLeaseClaim{
				SocketPath: "/private/redacted/pcm.sock", SessionToken: lease.SessionToken,
			},
		},
		SyntheticIPCBrokerConfig{
			SocketPath: "/private/redacted/pcm.sock", Lease: lease,
		},
		LocalPCMBrokerStartConfig{
			SocketPath: "/private/redacted/pcm.sock", LeaseGeneration: testLeaseGeneration,
			SessionToken: lease.SessionToken,
		},
		IPCLeaseClaim{
			SocketPath: "/private/redacted/pcm.sock", LeaseGeneration: testLeaseGeneration,
			SessionToken: lease.SessionToken,
		},
		MediaLeaseConfig{
			MediaSessionID: testMediaSessionID, SocketPath: "/private/redacted/pcm.sock",
			LeaseGeneration: testLeaseGeneration,
		},
	}
	for _, value := range values {
		for _, formatted := range []string{fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)} {
			if strings.Contains(formatted, secret) || strings.Contains(formatted, "/private/redacted") ||
				strings.Contains(formatted, testMediaSessionID) {
				t.Fatalf("formatted value leaked credential: %s", formatted)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "/private/redacted") ||
			strings.Contains(string(encoded), testMediaSessionID) ||
			strings.Contains(string(encoded), "SessionToken") {
			t.Fatalf("JSON leaked credential: %s", encoded)
		}
	}
}

func TestMediaLeaseIPCClaimIsExactlyOnceUnderConcurrency(t *testing.T) {
	t.Parallel()
	harness := newMediaLeaseHarness(t, nil)

	type result struct {
		claim IPCLeaseClaim
		err   error
	}
	results := make(chan result, 32)
	var workers sync.WaitGroup
	for index := 0; index < cap(results); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			claim, err := harness.lease.ClaimIPCLease()
			results <- result{claim: claim, err: err}
		}()
	}
	workers.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			if result.claim.SocketPath != harness.brokerCfg.SocketPath ||
				result.claim.LeaseGeneration != testLeaseGeneration || allZero(result.claim.SessionToken[:]) {
				t.Fatalf("invalid successful IPC claim: %s", result.claim)
			}
			if got := fmt.Sprintf("%#v", result.claim); strings.Contains(got, result.claim.SocketPath) ||
				strings.Contains(got, fmt.Sprintf("%x", result.claim.SessionToken)) {
				t.Fatalf("IPC claim formatter leaked credentials: %s", got)
			}
		} else if !errors.Is(result.err, ErrIPCLeaseAlreadyClaimed) {
			t.Fatalf("claim error = %v, want ErrIPCLeaseAlreadyClaimed", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims = %d, want exactly one", successes)
	}
	if !harness.lease.Snapshot().IPCClaimed {
		t.Fatal("snapshot did not retain the non-secret claimed state")
	}
}

func TestMediaLeaseAnswerIsIdempotentAndGenerationBound(t *testing.T) {
	t.Parallel()
	peer := newFakeMediaPeer(true)
	var answerCalls atomic.Int32
	var gotConfig Config
	var gotPort *FramePort
	answerer := func(_ context.Context, offer []byte, cfg Config, port *FramePort) (MediaPeer, []byte, error) {
		answerCalls.Add(1)
		gotConfig = cfg
		gotPort = port
		if string(offer) != "offer-a" {
			return nil, nil, errors.New("wrong offer")
		}
		return peer, []byte("answer-a"), nil
	}
	harness := newMediaLeaseHarness(t, answerer)
	cfg := Config{
		AllowedInterfaces: []string{"loopback"}, UDPMin: 1, UDPMax: 2,
		TURNRelay: &TURNRelayConfig{
			URLs: []string{
				"turn:turn.example.test:3478?transport=udp",
				"turns:turn.example.test:443?transport=tcp",
			},
			Username: "short-lived-user", Password: "short-lived-password",
			CredentialType: TURNCredentialTypePassword,
		},
	}

	answers := make(chan string, 16)
	errorsOut := make(chan error, 16)
	var workers sync.WaitGroup
	for index := 0; index < cap(answers); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			answer, err := harness.lease.Answer(context.Background(), []byte("offer-a"), cfg)
			if err != nil {
				errorsOut <- err
				return
			}
			answers <- string(answer)
		}()
	}
	workers.Wait()
	close(answers)
	close(errorsOut)
	for err := range errorsOut {
		t.Fatalf("idempotent answer: %v", err)
	}
	for answer := range answers {
		if answer != "answer-a" {
			t.Fatalf("answer = %q", answer)
		}
	}
	if answerCalls.Load() != 1 || gotConfig.Token != testMediaSessionID ||
		gotConfig.Generation != testLeaseGeneration || gotPort != harness.lease.port {
		t.Fatalf("answer binding calls=%d token-match=%v generation=%d port-match=%v",
			answerCalls.Load(), gotConfig.Token == testMediaSessionID, gotConfig.Generation, gotPort == harness.lease.port)
	}
	if gotConfig.TURNRelay == nil || gotConfig.TURNRelay == cfg.TURNRelay ||
		len(gotConfig.TURNRelay.URLs) != 2 || gotConfig.TURNRelay.URLs[0] != cfg.TURNRelay.URLs[0] ||
		gotConfig.TURNRelay.URLs[1] != cfg.TURNRelay.URLs[1] ||
		gotConfig.TURNRelay.Username != cfg.TURNRelay.Username ||
		gotConfig.TURNRelay.Password != cfg.TURNRelay.Password ||
		gotConfig.TURNRelay.CredentialType != cfg.TURNRelay.CredentialType {
		t.Fatal("media lease did not forward a complete independent TURN relay config clone")
	}
	gotConfig.TURNRelay.URLs[0] = "turn:mutated.example.test:3478?transport=udp"
	if gotConfig.TURNRelay.URLs[0] == cfg.TURNRelay.URLs[0] {
		t.Fatal("answerer-owned TURN URLs alias caller-owned config")
	}
	if _, err := harness.lease.Answer(context.Background(), []byte("offer-b"), cfg); !errors.Is(err, ErrMediaOfferConflict) {
		t.Fatalf("different offer error = %v, want ErrMediaOfferConflict", err)
	}
	changedRelay := cloneMediaConfig(cfg)
	changedRelay.TURNRelay.Password = "different-password"
	if _, err := harness.lease.Answer(context.Background(), []byte("offer-a"), changedRelay); !errors.Is(err, ErrMediaOfferConflict) {
		t.Fatalf("different TURN credentials error = %v, want ErrMediaOfferConflict", err)
	}
	wrongToken := cfg
	wrongToken.Token = testMediaSessionID + "-other"
	if _, err := harness.lease.Answer(context.Background(), []byte("offer-a"), wrongToken); !errors.Is(err, ErrMediaLeaseIdentityMismatch) {
		t.Fatalf("different identity error = %v", err)
	}
	wrongGeneration := cfg
	wrongGeneration.Generation = testLeaseGeneration + 1
	if _, err := harness.lease.Answer(context.Background(), []byte("offer-a"), wrongGeneration); !errors.Is(err, ErrMediaLeaseGenerationMismatch) {
		t.Fatalf("different generation error = %v", err)
	}
	if snapshot := harness.lease.Snapshot(); snapshot.Phase != MediaLeasePhaseConnected || !snapshot.WebRTCConnected {
		t.Fatalf("connected snapshot = %+v", snapshot)
	}
}

func TestMediaLeaseTransportPreparedAndBidirectionalFreshnessUseInjectedClock(t *testing.T) {
	t.Parallel()
	peer := newFakeMediaPeer(true)
	harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
		return peer, []byte("answer"), nil
	})
	if _, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{}); err != nil {
		t.Fatal(err)
	}
	now := harness.clock.Now()
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, AuthenticatedAt: now, Ready: true, ReadyAt: now, Live: true,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared {
		t.Fatalf("IPC Ready plus ICE without media tracks passed TransportPrepared: %+v", snapshot)
	}
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 1, LastUplinkAt: now, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
		ReceiverReports: 1, LastReceiverReportAt: now,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared {
		t.Fatalf("unclaimed IPC credential passed TransportPrepared: %+v", snapshot)
	}
	if _, err := harness.lease.ClaimIPCLease(); err != nil {
		t.Fatal(err)
	}
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true,
		UplinkPackets: 1, LastUplinkAt: now,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared {
		t.Fatalf("missing first browser RTP passed TransportPrepared: %+v", snapshot)
	}
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 1, LastUplinkAt: now, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
	})
	if snapshot := harness.lease.Snapshot(); !snapshot.TransportPrepared || snapshot.ActiveFresh {
		t.Fatalf("bidirectional RTP without an early browser ReceiverReport was not pre-call ready: %+v", snapshot)
	}
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 1, LastUplinkAt: now, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
		ReceiverReports: 1, LastReceiverReportAt: now,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared || snapshot.ActiveFresh {
		t.Fatalf("missing selected pair passed readiness gates: %+v", snapshot)
	}
	invalidPair := validSelectedPairSnapshot()
	invalidPair.RemoteCandidateType = SelectedCandidateTypeServerReflexive
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: invalidPair, LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 1, LastUplinkAt: now, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
		ReceiverReports: 1, LastReceiverReportAt: now,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared || snapshot.ActiveFresh {
		t.Fatalf("disallowed selected candidate type passed readiness gates: %+v", snapshot)
	}
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 1, LastUplinkAt: now, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
		ReceiverReports: 1, LastReceiverReportAt: now,
	})
	preparedBrokerActivity := IPCBrokerActivitySnapshot{
		Consumed: true, AuthenticatedAt: now, Ready: true, ReadyAt: now, Live: true,
		DeviceUplinkFrames: 1, LastDeviceUplinkAt: now,
		RemoteDownlinkFrames: 1, LastRemoteDownlinkAt: now,
	}
	harness.broker.setActivity(preparedBrokerActivity)
	snapshot := harness.lease.Snapshot()
	if !snapshot.TransportPrepared || snapshot.ActiveFresh || snapshot.DeviceUplinkFresh || snapshot.RemoteDownlinkFresh || snapshot.Activated {
		t.Fatalf("prepared-transport-prepared-without-real-frames snapshot = %+v", snapshot)
	}
	preparedBrokerActivity.Live = false
	harness.broker.setActivity(preparedBrokerActivity)
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared {
		t.Fatalf("historical Ready without a live IPC client passed TransportPrepared: %+v", snapshot)
	}
	preparedBrokerActivity.Live = true
	harness.broker.setActivity(preparedBrokerActivity)

	activation, err := harness.lease.Activate()
	if err != nil || !activation.Activated || activation.ActivationEpoch != 1 {
		t.Fatalf("activation = %+v/%v", activation, err)
	}
	if repeated, err := harness.lease.Activate(); err != nil || repeated != activation {
		t.Fatalf("idempotent activation = %+v/%v, want %+v", repeated, err, activation)
	}
	if snapshot := harness.lease.Snapshot(); snapshot.ActiveFresh || snapshot.DeviceUplinkFresh || snapshot.RemoteDownlinkFresh {
		t.Fatalf("pre-activation frames crossed baseline: %+v", snapshot)
	}

	now = harness.clock.Advance(50 * time.Millisecond)
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 2, LastUplinkAt: now, RemoteRTPPackets: 2, LastRemoteRTPAt: now,
		ReceiverReports: 2, LastReceiverReportAt: now,
		RealUplinkPackets: 1, LastRealUplinkAt: now,
		RealUplinkPacketsAtLastReceiverReport: 1,
		LastUplinkEpoch:                       99, LastRemoteRTPEpoch: 99, LastReceiverReportEpoch: 99,
		LastRealUplinkEpoch: 99,
	})
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, AuthenticatedAt: activation.ActivatedAt, Ready: true, ReadyAt: activation.ActivatedAt, Live: true,
		DeviceUplinkFrames: 2, LastDeviceUplinkAt: now, LastDeviceUplinkEpoch: 99,
		RemoteDownlinkFrames: 2, LastRemoteDownlinkAt: now, LastRemoteDownlinkEpoch: 99,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.ActiveFresh || snapshot.DeviceUplinkFresh || snapshot.RemoteDownlinkFresh {
		t.Fatalf("wrong-epoch exchanges crossed activation barrier: %+v", snapshot)
	}

	now = harness.clock.Advance(50 * time.Millisecond)
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 3, LastUplinkAt: now, RemoteRTPPackets: 3, LastRemoteRTPAt: now,
		ReceiverReports: 3, LastReceiverReportAt: now,
		LastUplinkEpoch: 1, LastRemoteRTPEpoch: 1, LastReceiverReportEpoch: 1,
	})
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, AuthenticatedAt: now.Add(-100 * time.Millisecond), Ready: true, ReadyAt: now.Add(-100 * time.Millisecond), Live: true,
		DeviceUplinkFrames: 3, LastDeviceUplinkAt: now,
		RemoteDownlinkFrames: 3, LastRemoteDownlinkAt: now,
		LastDeviceUplinkEpoch: 1, LastRemoteDownlinkEpoch: 1,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.ActiveFresh || !snapshot.DeviceUplinkFresh || !snapshot.RemoteDownlinkFresh {
		t.Fatalf("synthetic network uplink satisfied real ActiveFresh gate: %+v", snapshot)
	}

	now = harness.clock.Advance(50 * time.Millisecond)
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 4, LastUplinkAt: now, RealUplinkPackets: 1, LastRealUplinkAt: now,
		RemoteRTPPackets: 4, LastRemoteRTPAt: now,
		ReceiverReports: 3, LastReceiverReportAt: now.Add(-50 * time.Millisecond),
		RealUplinkPacketsAtLastReceiverReport: 0,
		LastUplinkEpoch:                       1, LastRealUplinkEpoch: 1, LastRemoteRTPEpoch: 1, LastReceiverReportEpoch: 1,
	})
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, AuthenticatedAt: now.Add(-100 * time.Millisecond), Ready: true, ReadyAt: now.Add(-100 * time.Millisecond), Live: true,
		DeviceUplinkFrames: 4, LastDeviceUplinkAt: now,
		RemoteDownlinkFrames: 4, LastRemoteDownlinkAt: now,
		LastDeviceUplinkEpoch: 1, LastRemoteDownlinkEpoch: 1,
	})
	if snapshot := harness.lease.Snapshot(); snapshot.ActiveFresh {
		t.Fatalf("real uplink without a later matching RR satisfied ActiveFresh: %+v", snapshot)
	}

	now = harness.clock.Advance(50 * time.Millisecond)
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 5, LastUplinkAt: now, RealUplinkPackets: 2, LastRealUplinkAt: now,
		RemoteRTPPackets: 5, LastRemoteRTPAt: now,
		ReceiverReports: 4, LastReceiverReportAt: now.Add(-10 * time.Millisecond),
		RealUplinkPacketsAtLastReceiverReport: 1,
		LastUplinkEpoch:                       1, LastRealUplinkEpoch: 1, LastRemoteRTPEpoch: 1, LastReceiverReportEpoch: 1,
	})
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, AuthenticatedAt: now.Add(-100 * time.Millisecond), Ready: true, ReadyAt: now.Add(-100 * time.Millisecond), Live: true,
		DeviceUplinkFrames: 5, LastDeviceUplinkAt: now,
		RemoteDownlinkFrames: 5, LastRemoteDownlinkAt: now,
		LastDeviceUplinkEpoch: 1, LastRemoteDownlinkEpoch: 1,
	})
	snapshot = harness.lease.Snapshot()
	if !snapshot.TransportPrepared || !snapshot.ActiveFresh || !snapshot.DeviceUplinkFresh || !snapshot.RemoteDownlinkFresh ||
		!snapshot.LastActivityAt.Equal(now) {
		t.Fatalf("fresh bidirectional snapshot = %+v", snapshot)
	}
	peer.setSelectedPair(SelectedPairSnapshot{})
	if snapshot := harness.lease.Snapshot(); snapshot.TransportPrepared || snapshot.ActiveFresh ||
		!snapshot.DeviceUplinkFresh || !snapshot.RemoteDownlinkFresh {
		t.Fatalf("lost selected pair did not fail closed independently of fresh media: %+v", snapshot)
	}
	peer.setSelectedPair(validSelectedPairSnapshot())

	harness.clock.Advance(time.Second + time.Nanosecond)
	snapshot = harness.lease.Snapshot()
	if snapshot.TransportPrepared || snapshot.ActiveFresh || snapshot.DeviceUplinkFresh || snapshot.RemoteDownlinkFresh {
		t.Fatalf("stale activity remained fresh: %+v", snapshot)
	}

	// A fresh RR, local exchange, and real uplink cannot substitute for a
	// currently fresh browser microphone RTP packet.
	freshAfterStale := harness.clock.Now()
	peer.setNetworkActivity(NetworkActivitySnapshot{
		Connected: true, SelectedPair: validSelectedPairSnapshot(), LocalTrackStarted: true, RemoteTrackReady: true,
		UplinkPackets: 6, LastUplinkAt: freshAfterStale,
		RealUplinkPackets: 3, LastRealUplinkAt: freshAfterStale,
		RemoteRTPPackets: 5, LastRemoteRTPAt: now,
		ReceiverReports: 5, LastReceiverReportAt: freshAfterStale,
		RealUplinkPacketsAtLastReceiverReport: 3,
		LastUplinkEpoch:                       1, LastRealUplinkEpoch: 1, LastRemoteRTPEpoch: 1, LastReceiverReportEpoch: 1,
	})
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, Ready: true, ReadyAt: freshAfterStale, Live: true,
		DeviceUplinkFrames: 6, LastDeviceUplinkAt: freshAfterStale, LastDeviceUplinkEpoch: 1,
		RemoteDownlinkFrames: 6, LastRemoteDownlinkAt: freshAfterStale, LastRemoteDownlinkEpoch: 1,
	})
	snapshot = harness.lease.Snapshot()
	if snapshot.TransportPrepared || snapshot.ActiveFresh {
		t.Fatalf("fresh RR masked stale remote RTP: %+v", snapshot)
	}

	future := harness.clock.Now().Add(time.Millisecond)
	harness.broker.setActivity(IPCBrokerActivitySnapshot{
		Consumed: true, Ready: true, ReadyAt: future, Live: true,
		DeviceUplinkFrames: 2, LastDeviceUplinkAt: future,
		RemoteDownlinkFrames: 2, LastRemoteDownlinkAt: future,
	})
	snapshot = harness.lease.Snapshot()
	if snapshot.ActiveFresh || snapshot.DeviceUplinkFresh || snapshot.RemoteDownlinkFresh {
		t.Fatalf("future timestamps passed freshness gate: %+v", snapshot)
	}
	if snapshot.LastActivityAt.After(harness.clock.Now()) || !snapshot.LastDeviceUplinkAt.IsZero() ||
		!snapshot.LastRemoteDownlinkAt.IsZero() {
		t.Fatalf("future timestamps leaked into safe snapshot: %+v", snapshot)
	}
}

func TestMediaLeaseBrokerTerminationClosesPeerAndLease(t *testing.T) {
	t.Parallel()
	peer := newFakeMediaPeer(true)
	harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
		return peer, []byte("answer"), nil
	})
	if _, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{}); err != nil {
		t.Fatal(err)
	}
	harness.broker.fail(errors.New("local process exited"))
	requireLeaseTerminal(t, harness.lease, ErrLocalPCMBrokerEnded)
	if snapshot := harness.lease.Snapshot(); snapshot.Phase != MediaLeasePhaseClosing ||
		snapshot.CloseReason != MediaLeaseCloseBrokerEnded || snapshot.TransportPrepared || snapshot.ActiveFresh {
		t.Fatalf("broker-terminal snapshot = %+v", snapshot)
	}
	if peer.closes() != 1 || harness.broker.closes() != 1 {
		t.Fatalf("close counts peer=%d broker=%d, want one each", peer.closes(), harness.broker.closes())
	}
	if err := harness.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if peer.closes() != 1 || harness.broker.closes() != 1 {
		t.Fatal("idempotent Close repeated a resource close")
	}
}

func TestMediaLeasePeerTerminationClosesBrokerAndLease(t *testing.T) {
	t.Parallel()
	peer := newFakeMediaPeer(true)
	harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
		return peer, []byte("answer"), nil
	})
	if _, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{}); err != nil {
		t.Fatal(err)
	}
	peer.fail(errors.New("ICE failed"))
	requireLeaseTerminal(t, harness.lease, ErrMediaPeerEnded)
	if snapshot := harness.lease.Snapshot(); snapshot.CloseReason != MediaLeaseClosePeerEnded || snapshot.WebRTCConnected {
		t.Fatalf("peer-terminal snapshot = %+v", snapshot)
	}
	if peer.closes() != 1 || harness.broker.closes() != 1 {
		t.Fatalf("close counts peer=%d broker=%d, want one each", peer.closes(), harness.broker.closes())
	}
}

func TestMediaLeaseAnswerFailureClosesBroker(t *testing.T) {
	t.Parallel()
	const privateAnswerMarker = "candidate-private-100-101-102-103"
	harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
		return nil, nil, errors.New(privateAnswerMarker)
	})
	if _, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{}); !errors.Is(err, ErrMediaAnswerFailed) {
		t.Fatalf("answer error = %v, want ErrMediaAnswerFailed", err)
	} else if strings.Contains(err.Error(), privateAnswerMarker) {
		t.Fatalf("answer error leaked injected peer detail: %v", err)
	}
	requireLeaseTerminal(t, harness.lease, ErrMediaAnswerFailed)
	if snapshot := harness.lease.Snapshot(); snapshot.CloseReason != MediaLeaseCloseAnswerFailed ||
		snapshot.Phase != MediaLeasePhaseClosing {
		t.Fatalf("answer-terminal snapshot = %+v", snapshot)
	}
	if harness.broker.closes() != 1 {
		t.Fatalf("broker closes = %d, want one", harness.broker.closes())
	}
}

func TestMediaLeaseAnswerFailureClosesUnpublishedPeerExactlyOnce(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		answer []byte
		err    error
	}{
		{name: "answerer error", answer: nil, err: errors.New("partial peer failure")},
		{name: "empty SDP", answer: nil, err: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			peer := newFakeMediaPeer(false)
			harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
				return peer, testCase.answer, testCase.err
			})
			if _, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{}); !errors.Is(err, ErrMediaAnswerFailed) {
				t.Fatalf("answer error = %v, want ErrMediaAnswerFailed", err)
			}
			requireLeaseTerminal(t, harness.lease, ErrMediaAnswerFailed)
			if peer.closes() != 1 {
				t.Fatalf("unpublished peer closes = %d, want one", peer.closes())
			}
			if err := harness.lease.Close(); err != nil {
				t.Fatal(err)
			}
			if peer.closes() != 1 {
				t.Fatalf("idempotent lease close repeated unpublished peer close: %d", peer.closes())
			}
		})
	}
}

func TestMediaLeaseCloseCancelsInFlightAnswerAndDoesNotLeakWatcher(t *testing.T) {
	t.Parallel()
	answerEntered := make(chan struct{})
	answerExited := make(chan struct{})
	harness := newMediaLeaseHarness(t, func(ctx context.Context, _ []byte, _ Config, _ *FramePort) (MediaPeer, []byte, error) {
		close(answerEntered)
		<-ctx.Done()
		close(answerExited)
		return nil, nil, ctx.Err()
	})
	answerDone := make(chan error, 1)
	go func() {
		_, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{})
		answerDone <- err
	}()
	<-answerEntered
	closeDone := make(chan error, 1)
	go func() { closeDone <- harness.lease.Close() }()

	select {
	case <-answerExited:
	case <-time.After(time.Second):
		t.Fatal("in-flight answer did not observe lease cancellation")
	}
	select {
	case err := <-answerDone:
		if !errors.Is(err, ErrMediaLeaseClosed) {
			t.Fatalf("answer error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight Answer did not return")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wait and finish")
	}
}

func TestMediaLeaseCloseDoesNotDeadlockBehindBlockedActivation(t *testing.T) {
	t.Parallel()
	peer := newBlockingActivationMediaPeer()
	harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
		return peer, []byte("answer"), nil
	})
	if _, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{}); err != nil {
		t.Fatal(err)
	}
	activationDone := make(chan error, 1)
	go func() {
		_, err := harness.lease.Activate()
		activationDone <- err
	}()
	select {
	case <-peer.entered:
	case <-time.After(time.Second):
		t.Fatal("activation did not reach blocking media peer")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- harness.lease.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close deadlocked behind blocked Activate")
	}
	select {
	case err := <-activationDone:
		if !errors.Is(err, ErrMediaLeaseClosed) {
			t.Fatalf("activation after concurrent close = %v, want ErrMediaLeaseClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Activate did not return after Close")
	}
}

func TestMediaLeaseClosesPeerReturnedAfterConcurrentClose(t *testing.T) {
	t.Parallel()
	answerEntered := make(chan struct{})
	releaseAnswer := make(chan struct{})
	peer := newFakeMediaPeer(false)
	harness := newMediaLeaseHarness(t, func(context.Context, []byte, Config, *FramePort) (MediaPeer, []byte, error) {
		close(answerEntered)
		<-releaseAnswer
		return peer, []byte("answer"), nil
	})
	answerDone := make(chan error, 1)
	go func() {
		_, err := harness.lease.Answer(context.Background(), []byte("offer"), Config{})
		answerDone <- err
	}()
	<-answerEntered
	closeDone := make(chan error, 1)
	go func() { closeDone <- harness.lease.Close() }()

	deadline := time.Now().Add(time.Second)
	for harness.lease.Snapshot().Phase != MediaLeasePhaseClosing && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if harness.lease.Snapshot().Phase != MediaLeasePhaseClosing {
		t.Fatal("concurrent Close did not enter closing phase")
	}
	close(releaseAnswer)
	if err := <-answerDone; !errors.Is(err, ErrMediaLeaseClosed) {
		t.Fatalf("Answer after concurrent close = %v, want ErrMediaLeaseClosed", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if peer.closes() != 1 {
		t.Fatalf("returned peer closes = %d, want one", peer.closes())
	}
}

func TestMediaLeaseCloseIfGenerationMismatch(t *testing.T) {
	t.Parallel()
	harness := newMediaLeaseHarness(t, nil)
	closed, err := harness.lease.CloseIfGenerationMismatch(testLeaseGeneration)
	if err != nil || closed {
		t.Fatalf("matching generation close = %v/%v, want false/nil", closed, err)
	}
	closed, err = harness.lease.CloseIfGenerationMismatch(testLeaseGeneration + 1)
	if err != nil || !closed {
		t.Fatalf("mismatched generation close = %v/%v, want true/nil", closed, err)
	}
	requireLeaseTerminal(t, harness.lease, ErrMediaLeaseGenerationMismatch)
	if snapshot := harness.lease.Snapshot(); snapshot.CloseReason != MediaLeaseCloseGenerationMismatch {
		t.Fatalf("mismatch close reason = %q", snapshot.CloseReason)
	}
	if _, err := harness.lease.ClaimIPCLease(); !errors.Is(err, ErrMediaLeaseClosed) {
		t.Fatalf("claim after close = %v, want ErrMediaLeaseClosed", err)
	}
	if _, err := harness.lease.Activate(); !errors.Is(err, ErrMediaLeaseClosed) {
		t.Fatalf("activate after close = %v, want ErrMediaLeaseClosed", err)
	}
}

func TestMediaLeaseConcurrentCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	harness := newMediaLeaseHarness(t, nil)
	var workers sync.WaitGroup
	errorsOut := make(chan error, 64)
	for index := 0; index < cap(errorsOut); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsOut <- harness.lease.Close()
		}()
	}
	workers.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if harness.broker.closes() != 1 {
		t.Fatalf("broker closes = %d, want one", harness.broker.closes())
	}
}

func TestMediaLeaseRejectsInvalidConfigAndBrokerFailure(t *testing.T) {
	t.Parallel()
	valid := MediaLeaseConfig{
		MediaSessionID:  testMediaSessionID,
		LeaseGeneration: testLeaseGeneration,
		SocketPath:      "/private/test/pcm.sock",
		StartBroker: func(LocalPCMBrokerStartConfig) (LocalPCMBroker, error) {
			return newFakeLocalPCMBroker(), nil
		},
	}
	tests := []struct {
		name   string
		mutate func(*MediaLeaseConfig)
	}{
		{"short identity", func(cfg *MediaLeaseConfig) { cfg.MediaSessionID = "short" }},
		{"identity whitespace", func(cfg *MediaLeaseConfig) { cfg.MediaSessionID = " opaque-media-session-0123456789" }},
		{"zero generation", func(cfg *MediaLeaseConfig) { cfg.LeaseGeneration = 0 }},
		{"missing socket", func(cfg *MediaLeaseConfig) { cfg.SocketPath = "" }},
		{"invalid capacity", func(cfg *MediaLeaseConfig) { cfg.FrameCapacity = -1 }},
		{"invalid freshness", func(cfg *MediaLeaseConfig) { cfg.FreshnessWindow = time.Millisecond }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if lease, err := NewMediaLeaseCoordinator(cfg); err == nil {
				_ = lease.Close()
				t.Fatal("invalid config unexpectedly accepted")
			}
		})
	}

	valid.StartBroker = func(LocalPCMBrokerStartConfig) (LocalPCMBroker, error) {
		return nil, errors.New("bind failed")
	}
	if lease, err := NewMediaLeaseCoordinator(valid); err == nil {
		_ = lease.Close()
		t.Fatal("broker failure unexpectedly accepted")
	}
}

func requireLeaseTerminal(t *testing.T, lease *MediaLeaseCoordinator, target error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lease.Wait(ctx); !errors.Is(err, target) {
		t.Fatalf("lease terminal error = %v, want %v", err, target)
	}
}
