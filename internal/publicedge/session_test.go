package publicedge

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testRegistryKeys struct {
	gateway   *ecdsa.PrivateKey
	device    *ecdsa.PrivateKey
	principal *ecdsa.PrivateKey
}

func newTestRegistry(t *testing.T) (*Registry, testRegistryKeys) {
	t.Helper()
	keys := testRegistryKeys{
		gateway: testP256Key(t), device: testP256Key(t), principal: testP256Key(t),
	}
	registry, err := NewRegistry(RegistryConfig{
		GatewayKeys:   map[string]*ecdsa.PublicKey{"gateway-1": &keys.gateway.PublicKey},
		DeviceKeys:    map[string]*ecdsa.PublicKey{"device-1": &keys.device.PublicKey},
		PrincipalKeys: map[string]*ecdsa.PublicKey{"cf:issuer:subject": &keys.principal.PublicKey},
	})
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return registry, keys
}

func openTestSession(t *testing.T, registry *Registry, connection, boot string, now time.Time) (*Session, Lease) {
	t.Helper()
	session, challenge, err := registry.Open(connection, testHello(t, "gateway-1", boot, now.Add(time.Minute)), now)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	if challenge.Type != FrameEdgeChallenge {
		t.Fatalf("challenge type = %q", challenge.Type)
	}
	lease, err := session.Lease()
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return session, lease
}

func authenticateTestSession(t *testing.T, session *Session, lease Lease, key *ecdsa.PrivateKey, now time.Time) {
	t.Helper()
	frame, err := NewFrame(FrameGatewayAuthenticate, FrameMeta{
		GatewayID: lease.GatewayID, ExpiresAt: now.Add(20 * time.Second).Unix(),
		BootID: lease.BootID, LeaseEpoch: lease.LeaseEpoch,
	}, GatewayAuthenticatePayload{Challenge: lease.ChannelBinding})
	if err != nil {
		t.Fatalf("new auth: %v", err)
	}
	frame, err = SignGatewayFrame(key, frame, lease.ChannelBinding)
	if err != nil {
		t.Fatalf("sign auth: %v", err)
	}
	ack, err := session.Authenticate(frame, now)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if ack.Type != FrameEdgeAuthenticated || session.State() != SessionAuthenticated {
		t.Fatalf("authentication state = %s, ack = %s", session.State(), ack.Type)
	}
}

func signedSnapshot(t *testing.T, key *ecdsa.PrivateKey, lease Lease, sequence, revision, highWater uint64, now time.Time) Frame {
	t.Helper()
	frame, err := NewFrame(FrameStateSnapshot, FrameMeta{
		GatewayID: lease.GatewayID, ExpiresAt: now.Add(time.Minute).Unix(), BootID: lease.BootID,
		LeaseEpoch: lease.LeaseEpoch, Sequence: sequence,
	}, SnapshotPayload{
		ObservedAt: now.Unix(), Revision: revision, EventHighWater: highWater,
		Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignGatewayFrame(key, frame, lease.ChannelBinding)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func signedProbe(t *testing.T, key *ecdsa.PrivateKey, lease Lease, sequence uint64, operationID string, now time.Time) Frame {
	t.Helper()
	frame, err := NewFrame(FrameOperationProbe, FrameMeta{
		GatewayID: lease.GatewayID, DeviceID: "device-1", OperationID: operationID,
		ExpiresAt: now.Add(time.Minute).Unix(), BootID: lease.BootID,
		LeaseEpoch: lease.LeaseEpoch, Sequence: sequence,
	}, OperationProbePayload{})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignClientFrame(key, frame, lease.ChannelBinding)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func signedResult(t *testing.T, key *ecdsa.PrivateKey, lease Lease, sequence uint64, operationID string, now time.Time) Frame {
	t.Helper()
	frame, err := NewFrame(FrameOperationResult, FrameMeta{
		GatewayID: lease.GatewayID, OperationID: operationID,
		ExpiresAt: now.Add(time.Minute).Unix(), BootID: lease.BootID,
		LeaseEpoch: lease.LeaseEpoch, Sequence: sequence,
	}, OperationResultPayload{ObservedAt: now.Unix(), State: OperationSucceeded, Final: true, Code: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignGatewayFrame(key, frame, lease.ChannelBinding)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestGatewayHandshakeAndReadOnlyStateFlow(t *testing.T) {
	registry, keys := newTestRegistry(t)
	session, lease := openTestSession(t, registry, "connection-1", "boot-1", fixedTestTime)
	authenticateTestSession(t, session, lease, keys.gateway, fixedTestTime)

	if err := session.AcceptGateway(signedSnapshot(t, keys.gateway, lease, 1, 1, 0, fixedTestTime), fixedTestTime); err != nil {
		t.Fatalf("accept snapshot: %v", err)
	}
	event, err := NewFrame(FrameStateEvent, FrameMeta{
		GatewayID: lease.GatewayID, ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: lease.BootID,
		LeaseEpoch: lease.LeaseEpoch, Sequence: 2,
	}, EventPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 2, EventSequence: 1, Kind: EventStateChanged,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err = SignGatewayFrame(keys.gateway, event, lease.ChannelBinding)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AcceptGateway(event, fixedTestTime); err != nil {
		t.Fatalf("accept event: %v", err)
	}

	probe := signedProbe(t, keys.device, lease, 1, "operation-1", fixedTestTime)
	if err := session.AcceptClient(probe, fixedTestTime); err != nil {
		t.Fatalf("accept operation probe: %v", err)
	}
	result := signedResult(t, keys.gateway, lease, 3, "operation-1", fixedTestTime)
	if err := session.AcceptGateway(result, fixedTestTime); err != nil {
		t.Fatalf("accept operation result: %v", err)
	}
	if session.State() != SessionAuthenticated {
		t.Fatalf("session state = %s", session.State())
	}
}

func TestInboundReplayAndOutOfOrderFramesTerminallyFailLease(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence uint64
	}{
		{"replay", 1},
		{"gap", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, keys := newTestRegistry(t)
			session, lease := openTestSession(t, registry, "connection-1", "boot-1", fixedTestTime)
			authenticateTestSession(t, session, lease, keys.gateway, fixedTestTime)
			first := signedSnapshot(t, keys.gateway, lease, 1, 1, 0, fixedTestTime)
			if err := session.AcceptGateway(first, fixedTestTime); err != nil {
				t.Fatal(err)
			}
			bad := signedSnapshot(t, keys.gateway, lease, test.sequence, 2, 0, fixedTestTime)
			if err := session.AcceptGateway(bad, fixedTestTime); !errors.Is(err, ErrSequence) {
				t.Fatalf("sequence error = %v", err)
			}
			if session.State() != SessionFailed {
				t.Fatalf("state after violation = %s", session.State())
			}
			if _, _, err := registry.Open("connection-2", testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute)), fixedTestTime); err != nil {
				t.Fatalf("failed lease was not released: %v", err)
			}
		})
	}
}

func TestUnsolicitedOperationResultFailsClosed(t *testing.T) {
	registry, keys := newTestRegistry(t)
	session, lease := openTestSession(t, registry, "connection-1", "boot-1", fixedTestTime)
	authenticateTestSession(t, session, lease, keys.gateway, fixedTestTime)
	result := signedResult(t, keys.gateway, lease, 1, "never-probed", fixedTestTime)
	if err := session.AcceptGateway(result, fixedTestTime); !errors.Is(err, ErrOperationNotProbed) {
		t.Fatalf("unsolicited result error = %v", err)
	}
	if session.State() != SessionFailed {
		t.Fatalf("state = %s", session.State())
	}
}

func TestInvalidClientProofCannotDisturbHealthyLease(t *testing.T) {
	registry, keys := newTestRegistry(t)
	session, lease := openTestSession(t, registry, "connection-1", "boot-1", fixedTestTime)
	authenticateTestSession(t, session, lease, keys.gateway, fixedTestTime)
	wrongKey := testP256Key(t)
	bad := signedProbe(t, wrongKey, lease, 1, "operation-1", fixedTestTime)
	if err := session.AcceptClient(bad, fixedTestTime); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid client proof error = %v", err)
	}
	if session.State() != SessionAuthenticated {
		t.Fatalf("invalid public request disturbed gateway: %s", session.State())
	}
	good := signedProbe(t, keys.device, lease, 1, "operation-1", fixedTestTime)
	if err := session.AcceptClient(good, fixedTestTime); err != nil {
		t.Fatalf("valid client request after attack: %v", err)
	}
	if err := session.AcceptClient(good, fixedTestTime); !errors.Is(err, ErrSequence) {
		t.Fatalf("client replay error = %v", err)
	}
	if session.State() != SessionFailed {
		t.Fatalf("authenticated client replay did not fail closed: %s", session.State())
	}
}

func TestSingleActiveLeaseConcurrentAuthentication(t *testing.T) {
	registry, keys := newTestRegistry(t)
	sessionA, leaseA := openTestSession(t, registry, "connection-a", "boot-1", fixedTestTime)
	sessionB, leaseB := openTestSession(t, registry, "connection-b", "boot-1", fixedTestTime)
	if leaseA.LeaseEpoch != leaseB.LeaseEpoch {
		t.Fatalf("pending hellos reserved different epochs: %d, %d", leaseA.LeaseEpoch, leaseB.LeaseEpoch)
	}
	makeAuth := func(t *testing.T, lease Lease) Frame {
		frame, err := NewFrame(FrameGatewayAuthenticate, FrameMeta{
			GatewayID: lease.GatewayID, ExpiresAt: fixedTestTime.Add(20 * time.Second).Unix(),
			BootID: lease.BootID, LeaseEpoch: lease.LeaseEpoch,
		}, GatewayAuthenticatePayload{Challenge: lease.ChannelBinding})
		if err != nil {
			t.Fatal(err)
		}
		frame, err = SignGatewayFrame(keys.gateway, frame, lease.ChannelBinding)
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}
	authA := makeAuth(t, leaseA)
	authB := makeAuth(t, leaseB)

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, candidate := range []struct {
		session *Session
		frame   Frame
	}{{sessionA, authA}, {sessionB, authB}} {
		candidate := candidate
		go func() {
			<-start
			_, err := candidate.session.Authenticate(candidate.frame, fixedTestTime)
			results <- err
		}()
	}
	close(start)
	var successes int
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrGatewayBusy) {
			t.Fatalf("losing connection error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful active leases = %d, want 1", successes)
	}
	if _, _, err := registry.Open("connection-c", testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute)), fixedTestTime); !errors.Is(err, ErrGatewayBusy) {
		t.Fatalf("third connection error = %v", err)
	}
}

func TestNewLeaseRejectsOldIncarnationProof(t *testing.T) {
	registry, keys := newTestRegistry(t)
	oldSession, oldLease := openTestSession(t, registry, "connection-old", "boot-1", fixedTestTime)
	authenticateTestSession(t, oldSession, oldLease, keys.gateway, fixedTestTime)
	oldFrame := signedSnapshot(t, keys.gateway, oldLease, 1, 1, 0, fixedTestTime)
	oldSession.Close()

	newSession, newLease := openTestSession(t, registry, "connection-new", "boot-1", fixedTestTime)
	if newLease.LeaseEpoch != oldLease.LeaseEpoch+1 || newLease.ChannelBinding == oldLease.ChannelBinding {
		t.Fatalf("new incarnation not advanced: old=%d new=%d", oldLease.LeaseEpoch, newLease.LeaseEpoch)
	}
	authenticateTestSession(t, newSession, newLease, keys.gateway, fixedTestTime)
	if err := newSession.AcceptGateway(oldFrame, fixedTestTime); err == nil {
		t.Fatal("old incarnation proof was accepted")
	}
	if newSession.State() != SessionFailed {
		t.Fatalf("state after old proof = %s", newSession.State())
	}
}

func TestChallengeExpiryAndAuthenticationMismatchFailClosed(t *testing.T) {
	registry, keys := newTestRegistry(t)
	session, lease := openTestSession(t, registry, "connection-1", "boot-1", fixedTestTime)
	frame, err := NewFrame(FrameGatewayAuthenticate, FrameMeta{
		GatewayID: lease.GatewayID, ExpiresAt: fixedTestTime.Add(20 * time.Second).Unix(),
		BootID: lease.BootID, LeaseEpoch: lease.LeaseEpoch,
	}, GatewayAuthenticatePayload{Challenge: lease.ChannelBinding})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignGatewayFrame(keys.gateway, frame, lease.ChannelBinding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Authenticate(frame, fixedTestTime.Add(DefaultChallengeTTL)); !errors.Is(err, ErrExpired) {
		t.Fatalf("challenge expiry error = %v", err)
	}
	if session.State() != SessionFailed {
		t.Fatalf("state = %s", session.State())
	}
}

func TestSessionConcurrentObserversAndSequencedUpdates(t *testing.T) {
	registry, keys := newTestRegistry(t)
	session, lease := openTestSession(t, registry, "connection-1", "boot-1", fixedTestTime)
	authenticateTestSession(t, session, lease, keys.gateway, fixedTestTime)

	var stopped atomic.Bool
	var observers sync.WaitGroup
	for range 12 {
		observers.Add(1)
		go func() {
			defer observers.Done()
			for !stopped.Load() {
				_ = session.State()
				_, _ = session.Lease()
				_ = session.String()
				_, _ = json.Marshal(session)
			}
		}()
	}
	for sequence := uint64(1); sequence <= 200; sequence++ {
		frame := signedSnapshot(t, keys.gateway, lease, sequence, sequence, 0, fixedTestTime)
		if err := session.AcceptGateway(frame, fixedTestTime); err != nil {
			t.Fatalf("sequence %d: %v", sequence, err)
		}
	}
	stopped.Store(true)
	observers.Wait()
	if session.State() != SessionAuthenticated {
		t.Fatalf("state = %s", session.State())
	}
}

func TestRegistryCopiesCallerKeyMaterial(t *testing.T) {
	gateway := testP256Key(t)
	registry, err := NewRegistry(RegistryConfig{
		GatewayKeys: map[string]*ecdsa.PublicKey{"gateway-1": &gateway.PublicKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	storedX := new(big.Int).Set(registry.gateways["gateway-1"].publicKey.X)
	gateway.PublicKey.X.SetInt64(1)
	if registry.gateways["gateway-1"].publicKey.X.Cmp(storedX) != 0 {
		t.Fatal("registry retained mutable caller key coordinates")
	}
}

func TestSessionFormattingRedactsConnectionAndIncarnation(t *testing.T) {
	registry, _ := newTestRegistry(t)
	session, lease := openTestSession(t, registry, "connection-secret", "boot-secret", fixedTestTime)
	formatted := []string{session.String(), fmt.Sprintf("%#v", session), string(mustJSON(t, session))}
	for _, output := range formatted {
		for _, secret := range []string{"connection-secret", "gateway-1", "boot-secret", lease.ChannelBinding} {
			if strings.Contains(output, secret) {
				t.Fatalf("session formatting leaked %q: %s", secret, output)
			}
		}
	}
}
