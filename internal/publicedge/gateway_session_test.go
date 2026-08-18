package publicedge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const gatewayTestConnection = "connection-gateway-1"

type gatewayTestHarness struct {
	gateway  *GatewaySession
	edge     *Session
	registry *Registry
	keys     testRegistryKeys
	lease    Lease
}

func newGatewayTestSession(t *testing.T, keys testRegistryKeys, connection, boot string) *GatewaySession {
	t.Helper()
	session, err := NewGatewaySession(GatewaySessionConfig{
		ConnectionID: connection,
		GatewayID:    "gateway-1",
		BootID:       boot,
		PrivateKey:   keys.gateway,
		DeviceKeys: map[string]*ecdsa.PublicKey{
			"device-1": &keys.device.PublicKey,
		},
		PrincipalKeys: map[string]*ecdsa.PublicKey{
			"cf:issuer:subject": &keys.principal.PublicKey,
		},
	})
	if err != nil {
		t.Fatalf("new gateway session: %v", err)
	}
	return session
}

func newGatewayTestKeys(t *testing.T) testRegistryKeys {
	t.Helper()
	return testRegistryKeys{
		gateway: testP256Key(t), device: testP256Key(t), principal: testP256Key(t),
	}
}

func newAuthenticatedGatewayHarness(t *testing.T) gatewayTestHarness {
	t.Helper()
	registry, keys := newTestRegistry(t)
	gateway := newGatewayTestSession(t, keys, gatewayTestConnection, "boot-gateway-1")
	hello, err := gateway.Hello(fixedTestTime)
	if err != nil {
		t.Fatalf("gateway hello: %v", err)
	}
	edge, challenge, err := registry.Open(gatewayTestConnection, hello, fixedTestTime)
	if err != nil {
		t.Fatalf("edge open: %v", err)
	}
	authenticate, err := gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime)
	if err != nil {
		t.Fatalf("gateway challenge: %v", err)
	}
	ack, err := edge.Authenticate(authenticate, fixedTestTime)
	if err != nil {
		t.Fatalf("edge authenticate: %v", err)
	}
	if err := gateway.AcceptAuthenticated(gatewayTestConnection, ack, fixedTestTime); err != nil {
		t.Fatalf("gateway authenticated: %v", err)
	}
	lease, err := gateway.Lease()
	if err != nil {
		t.Fatalf("gateway lease: %v", err)
	}
	return gatewayTestHarness{gateway: gateway, edge: edge, registry: registry, keys: keys, lease: lease}
}

func gatewayTestChallenge(t *testing.T, gatewayID, bootID string, epoch uint64, expires time.Time, binding string) Frame {
	t.Helper()
	frame, err := NewFrame(FrameEdgeChallenge, FrameMeta{
		GatewayID: gatewayID, ExpiresAt: expires.Unix(), BootID: bootID, LeaseEpoch: epoch,
	}, ChallengePayload{Challenge: binding})
	if err != nil {
		t.Fatalf("new challenge: %v", err)
	}
	return frame
}

func gatewayTestAck(t *testing.T, gatewayID, bootID string, epoch uint64, expires time.Time) Frame {
	t.Helper()
	frame, err := NewFrame(FrameEdgeAuthenticated, FrameMeta{
		GatewayID: gatewayID, ExpiresAt: expires.Unix(), BootID: bootID, LeaseEpoch: epoch,
	}, AuthenticatedPayload{})
	if err != nil {
		t.Fatalf("new authenticated ack: %v", err)
	}
	return frame
}

func gatewayTestSignedProbe(
	t *testing.T,
	key *ecdsa.PrivateKey,
	lease Lease,
	deviceID, principal, operationID string,
	sequence uint64,
	now time.Time,
) Frame {
	t.Helper()
	frame, err := NewFrame(FrameOperationProbe, FrameMeta{
		GatewayID: lease.GatewayID, DeviceID: deviceID, Principal: principal,
		OperationID: operationID, ExpiresAt: now.Add(time.Minute).Unix(),
		BootID: lease.BootID, LeaseEpoch: lease.LeaseEpoch, Sequence: sequence,
	}, OperationProbePayload{})
	if err != nil {
		t.Fatalf("new probe: %v", err)
	}
	frame, err = SignClientFrame(key, frame, lease.ChannelBinding)
	if err != nil {
		t.Fatalf("sign probe: %v", err)
	}
	return frame
}

func TestGatewaySessionEndToEndReadOnlyFlow(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)

	snapshot, err := harness.gateway.Snapshot(gatewayTestConnection, SnapshotPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 1, EventHighWater: 0,
		Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
	}, fixedTestTime)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Sequence != 1 || VerifyGatewayFrame(&harness.keys.gateway.PublicKey, snapshot, harness.lease.ChannelBinding) != nil {
		t.Fatalf("invalid first gateway frame: %#v", snapshot)
	}
	if err := harness.edge.AcceptGateway(snapshot, fixedTestTime); err != nil {
		t.Fatalf("edge snapshot: %v", err)
	}

	event, err := harness.gateway.Event(gatewayTestConnection, EventPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 2,
		EventSequence: 1, Kind: EventStateChanged,
	}, fixedTestTime)
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if event.Sequence != 2 {
		t.Fatalf("event sequence = %d", event.Sequence)
	}
	if err := harness.edge.AcceptGateway(event, fixedTestTime); err != nil {
		t.Fatalf("edge event: %v", err)
	}

	probe := gatewayTestSignedProbe(
		t, harness.keys.device, harness.lease, "device-1", "", "operation-1", 1, fixedTestTime,
	)
	if err := harness.edge.AcceptClient(probe, fixedTestTime); err != nil {
		t.Fatalf("edge first verification: %v", err)
	}
	if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime); err != nil {
		t.Fatalf("gateway second verification: %v", err)
	}
	result, err := harness.gateway.OperationResult(gatewayTestConnection, "operation-1", OperationResultPayload{
		ObservedAt: fixedTestTime.Unix(), State: OperationSucceeded, Final: true, Code: "ok",
	}, fixedTestTime)
	if err != nil {
		t.Fatalf("operation result: %v", err)
	}
	if result.Sequence != 3 {
		t.Fatalf("result sequence = %d", result.Sequence)
	}
	if err := harness.edge.AcceptGateway(result, fixedTestTime); err != nil {
		t.Fatalf("edge result: %v", err)
	}
	if harness.gateway.State() != GatewaySessionAuthenticated || len(harness.gateway.pendingOperations) != 0 {
		t.Fatalf("gateway state=%s pending=%d", harness.gateway.State(), len(harness.gateway.pendingOperations))
	}
}

func TestGatewayHelloChallengeAndAuthenticationBindExactIncarnation(t *testing.T) {
	keys := newGatewayTestKeys(t)
	gateway := newGatewayTestSession(t, keys, gatewayTestConnection, "boot-exact")
	hello, err := gateway.Hello(fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Type != FrameGatewayHello || hello.GatewayID != "gateway-1" || hello.BootID != "boot-exact" ||
		hello.ExpiresAt != fixedTestTime.Add(DefaultMessageTTL).Unix() || hello.Signature != "" {
		t.Fatalf("hello did not bind identity and expiry: %#v", hello)
	}
	binding := testBinding(31)
	challenge := gatewayTestChallenge(
		t, "gateway-1", "boot-exact", 17, fixedTestTime.Add(DefaultChallengeTTL), binding,
	)
	authenticate, err := gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if authenticate.GatewayID != challenge.GatewayID || authenticate.BootID != challenge.BootID ||
		authenticate.LeaseEpoch != challenge.LeaseEpoch || authenticate.ExpiresAt != challenge.ExpiresAt ||
		authenticate.Sequence != 0 {
		t.Fatalf("authenticate did not exactly bind challenge incarnation: %#v", authenticate)
	}
	var payload GatewayAuthenticatePayload
	if err := decodeStrictJSON(authenticate.Payload, &payload); err != nil || payload.Challenge != binding {
		t.Fatalf("authentication payload mismatch: %#v, %v", payload, err)
	}
	if err := VerifyGatewayFrame(&keys.gateway.PublicKey, authenticate, binding); err != nil {
		t.Fatalf("authentication signature: %v", err)
	}
	lease, err := gateway.Lease()
	if err != nil || lease.LeaseEpoch != 17 || lease.ChannelBinding != binding {
		t.Fatalf("lease = %#v, %v", lease, err)
	}
	ack := gatewayTestAck(t, "gateway-1", "boot-exact", 17, fixedTestTime.Add(time.Minute))
	if err := gateway.AcceptAuthenticated(gatewayTestConnection, ack, fixedTestTime); err != nil {
		t.Fatal(err)
	}
	if gateway.State() != GatewaySessionAuthenticated {
		t.Fatalf("state = %s", gateway.State())
	}
}

func TestGatewayChallengeViolationsFailClosed(t *testing.T) {
	baseBinding := testBinding(32)
	for _, test := range []struct {
		name       string
		connection string
		mutate     func(*Frame)
		now        time.Time
	}{
		{name: "stale connection", connection: "connection-old"},
		{name: "gateway", connection: gatewayTestConnection, mutate: func(frame *Frame) { frame.GatewayID = "gateway-2" }},
		{name: "boot", connection: gatewayTestConnection, mutate: func(frame *Frame) { frame.BootID = "boot-old" }},
		{name: "epoch", connection: gatewayTestConnection, mutate: func(frame *Frame) { frame.LeaseEpoch = 0 }},
		{name: "sequence", connection: gatewayTestConnection, mutate: func(frame *Frame) { frame.Sequence = 1 }},
		{name: "expired", connection: gatewayTestConnection, now: fixedTestTime.Add(DefaultChallengeTTL)},
		{name: "excessive expiry", connection: gatewayTestConnection, mutate: func(frame *Frame) {
			frame.ExpiresAt = fixedTestTime.Add(DefaultChallengeTTL + time.Second).Unix()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			keys := newGatewayTestKeys(t)
			gateway := newGatewayTestSession(t, keys, gatewayTestConnection, "boot-current")
			if _, err := gateway.Hello(fixedTestTime); err != nil {
				t.Fatal(err)
			}
			challenge := gatewayTestChallenge(
				t, "gateway-1", "boot-current", 1,
				fixedTestTime.Add(DefaultChallengeTTL), baseBinding,
			)
			if test.mutate != nil {
				test.mutate(&challenge)
			}
			now := test.now
			if now.IsZero() {
				now = fixedTestTime
			}
			if _, err := gateway.AcceptChallenge(test.connection, challenge, now); err == nil {
				t.Fatal("invalid challenge was accepted")
			}
			if gateway.State() != GatewaySessionFailed || gateway.challenge != "" || gateway.privateKey != nil {
				t.Fatalf("challenge failure did not clear state: %s", gateway.State())
			}
		})
	}
}

func TestGatewayDoubleChallengeAndDoubleAuthenticatedFailClosed(t *testing.T) {
	t.Run("challenge", func(t *testing.T) {
		keys := newGatewayTestKeys(t)
		gateway := newGatewayTestSession(t, keys, gatewayTestConnection, "boot-double")
		_, _ = gateway.Hello(fixedTestTime)
		challenge := gatewayTestChallenge(
			t, "gateway-1", "boot-double", 1,
			fixedTestTime.Add(DefaultChallengeTTL), testBinding(33),
		)
		if _, err := gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime); err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime); !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("double challenge error = %v", err)
		}
		if gateway.State() != GatewaySessionFailed || gateway.challenge != "" {
			t.Fatalf("double challenge state = %s", gateway.State())
		}
	})

	t.Run("authenticated", func(t *testing.T) {
		harness := newAuthenticatedGatewayHarness(t)
		ack := gatewayTestAck(
			t, harness.lease.GatewayID, harness.lease.BootID,
			harness.lease.LeaseEpoch, fixedTestTime.Add(time.Minute),
		)
		if err := harness.gateway.AcceptAuthenticated(gatewayTestConnection, ack, fixedTestTime); !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("double authenticated error = %v", err)
		}
		if harness.gateway.State() != GatewaySessionFailed || harness.gateway.challenge != "" {
			t.Fatalf("double authenticated state = %s", harness.gateway.State())
		}
	})
}

func TestGatewayAuthenticatedAckRejectsOldEpochBootAndConnection(t *testing.T) {
	for _, test := range []struct {
		name       string
		connection string
		gatewayID  string
		bootID     string
		epoch      uint64
		expires    time.Time
	}{
		{"old connection", "connection-old", "gateway-1", "boot-ack", 9, fixedTestTime.Add(time.Minute)},
		{"gateway", gatewayTestConnection, "gateway-2", "boot-ack", 9, fixedTestTime.Add(time.Minute)},
		{"old boot", gatewayTestConnection, "gateway-1", "boot-old", 9, fixedTestTime.Add(time.Minute)},
		{"old epoch", gatewayTestConnection, "gateway-1", "boot-ack", 8, fixedTestTime.Add(time.Minute)},
		{"expired", gatewayTestConnection, "gateway-1", "boot-ack", 9, fixedTestTime},
		{"challenge expired", gatewayTestConnection, "gateway-1", "boot-ack", 9, fixedTestTime.Add(time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			keys := newGatewayTestKeys(t)
			gateway := newGatewayTestSession(t, keys, gatewayTestConnection, "boot-ack")
			_, _ = gateway.Hello(fixedTestTime)
			challenge := gatewayTestChallenge(
				t, "gateway-1", "boot-ack", 9,
				fixedTestTime.Add(DefaultChallengeTTL), testBinding(34),
			)
			if _, err := gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime); err != nil {
				t.Fatal(err)
			}
			ack := gatewayTestAck(t, test.gatewayID, test.bootID, test.epoch, test.expires)
			now := fixedTestTime
			if test.name == "challenge expired" {
				now = fixedTestTime.Add(DefaultChallengeTTL)
			}
			if err := gateway.AcceptAuthenticated(test.connection, ack, now); err == nil {
				t.Fatal("old or mismatched authenticated ack was accepted")
			}
			if gateway.State() != GatewaySessionFailed || gateway.challenge != "" {
				t.Fatalf("ack failure state = %s", gateway.State())
			}
		})
	}
}

func TestGatewayReverifiesDeviceAndPrincipalProbesOnWholeLeaseSequence(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)
	deviceProbe := gatewayTestSignedProbe(
		t, harness.keys.device, harness.lease, "device-1", "", "operation-device", 1, fixedTestTime,
	)
	principalProbe := gatewayTestSignedProbe(
		t, harness.keys.principal, harness.lease, "", "cf:issuer:subject", "operation-principal", 2, fixedTestTime,
	)
	if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, deviceProbe, fixedTestTime); err != nil {
		t.Fatalf("device probe: %v", err)
	}
	if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, principalProbe, fixedTestTime); err != nil {
		t.Fatalf("principal probe: %v", err)
	}
	if harness.gateway.inboundSequence != 2 || len(harness.gateway.pendingOperations) != 2 {
		t.Fatalf("inbound=%d pending=%d", harness.gateway.inboundSequence, len(harness.gateway.pendingOperations))
	}
	replayed := gatewayTestSignedProbe(
		t, harness.keys.device, harness.lease, "device-1", "", "operation-replay", 2, fixedTestTime,
	)
	if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, replayed, fixedTestTime); !errors.Is(err, ErrSequence) {
		t.Fatalf("cross-identity replay error = %v", err)
	}
	if harness.gateway.State() != GatewaySessionFailed || len(harness.gateway.pendingOperations) != 0 {
		t.Fatalf("replay did not fail and clear pending: state=%s pending=%d", harness.gateway.State(), len(harness.gateway.pendingOperations))
	}
}

func TestGatewayProbeProofAndIncarnationViolationsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		build  func(*testing.T, gatewayTestHarness) Frame
		want   error
		connID string
	}{
		{
			name: "wrong key", want: ErrUnauthorized,
			build: func(t *testing.T, harness gatewayTestHarness) Frame {
				return gatewayTestSignedProbe(t, testP256Key(t), harness.lease, "device-1", "", "operation-1", 1, fixedTestTime)
			},
		},
		{
			name: "unknown device", want: ErrUnauthorized,
			build: func(t *testing.T, harness gatewayTestHarness) Frame {
				return gatewayTestSignedProbe(t, testP256Key(t), harness.lease, "device-unknown", "", "operation-1", 1, fixedTestTime)
			},
		},
		{
			name: "wrong boot", want: ErrProtocolViolation,
			build: func(t *testing.T, harness gatewayTestHarness) Frame {
				lease := harness.lease
				lease.BootID = "boot-old"
				return gatewayTestSignedProbe(t, harness.keys.device, lease, "device-1", "", "operation-1", 1, fixedTestTime)
			},
		},
		{
			name: "wrong epoch", want: ErrProtocolViolation,
			build: func(t *testing.T, harness gatewayTestHarness) Frame {
				lease := harness.lease
				lease.LeaseEpoch++
				return gatewayTestSignedProbe(t, harness.keys.device, lease, "device-1", "", "operation-1", 1, fixedTestTime)
			},
		},
		{
			name: "sequence gap", want: ErrSequence,
			build: func(t *testing.T, harness gatewayTestHarness) Frame {
				return gatewayTestSignedProbe(t, harness.keys.device, harness.lease, "device-1", "", "operation-1", 2, fixedTestTime)
			},
		},
		{
			name: "stale connection", want: ErrProtocolViolation, connID: "connection-old",
			build: func(t *testing.T, harness gatewayTestHarness) Frame {
				return gatewayTestSignedProbe(t, harness.keys.device, harness.lease, "device-1", "", "operation-1", 1, fixedTestTime)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newAuthenticatedGatewayHarness(t)
			connectionID := test.connID
			if connectionID == "" {
				connectionID = gatewayTestConnection
			}
			err := harness.gateway.AcceptOperationProbe(connectionID, test.build(t, harness), fixedTestTime)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if harness.gateway.State() != GatewaySessionFailed || harness.gateway.challenge != "" {
				t.Fatalf("probe violation state = %s", harness.gateway.State())
			}
		})
	}
}

func TestGatewayPendingProbeLimitAndDuplicateAreFailClosed(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		harness := newAuthenticatedGatewayHarness(t)
		first := gatewayTestSignedProbe(
			t, harness.keys.device, harness.lease, "device-1", "", "operation-same", 1, fixedTestTime,
		)
		if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, first, fixedTestTime); err != nil {
			t.Fatal(err)
		}
		duplicate := gatewayTestSignedProbe(
			t, harness.keys.device, harness.lease, "device-1", "", "operation-same", 2, fixedTestTime,
		)
		if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, duplicate, fixedTestTime); !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("duplicate error = %v", err)
		}
		if harness.gateway.State() != GatewaySessionFailed || len(harness.gateway.pendingOperations) != 0 {
			t.Fatalf("duplicate state=%s pending=%d", harness.gateway.State(), len(harness.gateway.pendingOperations))
		}
	})

	t.Run("limit", func(t *testing.T) {
		harness := newAuthenticatedGatewayHarness(t)
		for index := 1; index <= MaxPendingProbes; index++ {
			operationID := fmt.Sprintf("operation-%d", index)
			probe := gatewayTestSignedProbe(
				t, harness.keys.device, harness.lease, "device-1", "", operationID, uint64(index), fixedTestTime,
			)
			if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime); err != nil {
				t.Fatalf("probe %d: %v", index, err)
			}
		}
		overflow := gatewayTestSignedProbe(
			t, harness.keys.device, harness.lease, "device-1", "", "operation-overflow",
			MaxPendingProbes+1, fixedTestTime,
		)
		if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, overflow, fixedTestTime); !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("pending limit error = %v", err)
		}
		if harness.gateway.State() != GatewaySessionFailed || len(harness.gateway.pendingOperations) != 0 {
			t.Fatalf("limit state=%s pending=%d", harness.gateway.State(), len(harness.gateway.pendingOperations))
		}
	})
}

func TestGatewayOutboundFramesAreSerializedAcrossConcurrentPublishers(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)
	const publishers = 32
	start := make(chan struct{})
	frames := make(chan Frame, publishers)
	errorsSeen := make(chan error, publishers)
	var workers sync.WaitGroup
	for range publishers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			frame, err := harness.gateway.Snapshot(gatewayTestConnection, SnapshotPayload{
				ObservedAt: fixedTestTime.Unix(), Revision: 1, EventHighWater: 0,
				Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
			}, fixedTestTime)
			if err != nil {
				errorsSeen <- err
				return
			}
			frames <- frame
		}()
	}
	close(start)
	workers.Wait()
	close(frames)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent snapshot: %v", err)
	}
	var ordered []Frame
	for frame := range frames {
		ordered = append(ordered, frame)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	if len(ordered) != publishers {
		t.Fatalf("frames = %d", len(ordered))
	}
	for index, frame := range ordered {
		want := uint64(index + 1)
		if frame.Sequence != want {
			t.Fatalf("sequence[%d] = %d", index, frame.Sequence)
		}
		if err := VerifyGatewayFrame(&harness.keys.gateway.PublicKey, frame, harness.lease.ChannelBinding); err != nil {
			t.Fatalf("signature[%d]: %v", index, err)
		}
		if err := harness.edge.AcceptGateway(frame, fixedTestTime); err != nil {
			t.Fatalf("edge sequence %d: %v", want, err)
		}
	}
}

func TestGatewayConcurrentDoubleAcceptFailsClosed(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)
	probe := gatewayTestSignedProbe(
		t, harness.keys.device, harness.lease, "device-1", "", "operation-double", 1, fixedTestTime,
	)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- harness.gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime)
		}()
	}
	close(start)
	var successes int
	var failures int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrSequence) {
			failures++
		} else {
			t.Fatalf("double accept error = %v", err)
		}
	}
	if successes != 1 || failures != 1 || harness.gateway.State() != GatewaySessionFailed {
		t.Fatalf("success=%d failures=%d state=%s", successes, failures, harness.gateway.State())
	}
}

func TestGatewayOperationResultRequiresPendingAndSharesOutboundSequence(t *testing.T) {
	t.Run("unsolicited", func(t *testing.T) {
		harness := newAuthenticatedGatewayHarness(t)
		_, err := harness.gateway.OperationResult(gatewayTestConnection, "operation-missing", OperationResultPayload{
			ObservedAt: fixedTestTime.Unix(), State: OperationUnknown,
		}, fixedTestTime)
		if !errors.Is(err, ErrOperationNotProbed) || harness.gateway.State() != GatewaySessionFailed {
			t.Fatalf("unsolicited result: %v, %s", err, harness.gateway.State())
		}
	})

	t.Run("sequenced", func(t *testing.T) {
		harness := newAuthenticatedGatewayHarness(t)
		probe := gatewayTestSignedProbe(
			t, harness.keys.device, harness.lease, "device-1", "", "operation-known", 1, fixedTestTime,
		)
		if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime); err != nil {
			t.Fatal(err)
		}
		snapshot, err := harness.gateway.Snapshot(gatewayTestConnection, SnapshotPayload{
			ObservedAt: fixedTestTime.Unix(), Revision: 1,
			Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
		}, fixedTestTime)
		if err != nil {
			t.Fatal(err)
		}
		result, err := harness.gateway.OperationResult(gatewayTestConnection, "operation-known", OperationResultPayload{
			ObservedAt: fixedTestTime.Unix(), State: OperationUnknown,
		}, fixedTestTime)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Sequence != 1 || result.Sequence != 2 || len(harness.gateway.pendingOperations) != 0 {
			t.Fatalf("snapshot=%d result=%d pending=%d", snapshot.Sequence, result.Sequence, len(harness.gateway.pendingOperations))
		}
		if _, err := harness.gateway.OperationResult(gatewayTestConnection, "operation-known", OperationResultPayload{
			ObservedAt: fixedTestTime.Unix(), State: OperationUnknown,
		}, fixedTestTime); !errors.Is(err, ErrOperationNotProbed) {
			t.Fatalf("duplicate result error = %v", err)
		}
	})
}

func TestGatewayStateWatermarksFailClosedBeforePublishing(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)
	if _, err := harness.gateway.Snapshot(gatewayTestConnection, SnapshotPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 2, EventHighWater: 2,
		Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
	}, fixedTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.gateway.Event(gatewayTestConnection, EventPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 3, EventSequence: 4, Kind: EventStateChanged,
	}, fixedTestTime); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("event gap error = %v", err)
	}
	if harness.gateway.State() != GatewaySessionFailed || harness.gateway.outboundSequence != 1 {
		t.Fatalf("state=%s outbound=%d", harness.gateway.State(), harness.gateway.outboundSequence)
	}
}

func TestGatewayCloseClearsSecretsPendingAndRejectsOldConnectionWork(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)
	probe := gatewayTestSignedProbe(
		t, harness.keys.device, harness.lease, "device-1", "", "operation-secret", 1, fixedTestTime,
	)
	if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime); err != nil {
		t.Fatal(err)
	}
	harness.gateway.Close()
	if harness.gateway.State() != GatewaySessionClosed || harness.gateway.challenge != "" ||
		len(harness.gateway.pendingOperations) != 0 || harness.gateway.privateKey != nil ||
		harness.gateway.deviceKeys != nil || harness.gateway.principalKeys != nil {
		t.Fatalf("close did not clear sensitive session state: %#v", harness.gateway)
	}
	if _, err := harness.gateway.Lease(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed lease error = %v", err)
	}
	if err := harness.gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed probe error = %v", err)
	}
	if _, err := harness.gateway.Snapshot(gatewayTestConnection, SnapshotPayload{}, fixedTestTime); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed snapshot error = %v", err)
	}
}

func TestGatewaySessionAndConfigFormattingAreRedacted(t *testing.T) {
	keys := newGatewayTestKeys(t)
	config := GatewaySessionConfig{
		ConnectionID: gatewayTestConnection, GatewayID: "gateway-1", BootID: "boot-format-secret",
		PrivateKey:    keys.gateway,
		DeviceKeys:    map[string]*ecdsa.PublicKey{"device-format-secret": &keys.device.PublicKey},
		PrincipalKeys: map[string]*ecdsa.PublicKey{"cf:format:secret": &keys.principal.PublicKey},
	}
	gateway, err := NewGatewaySession(config)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = gateway.Hello(fixedTestTime)
	binding := testBinding(35)
	challenge := gatewayTestChallenge(
		t, "gateway-1", "boot-format-secret", 1,
		fixedTestTime.Add(DefaultChallengeTTL), binding,
	)
	_, err = gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	privateScalar := keys.gateway.D.String()
	outputs := []string{
		config.String(), fmt.Sprintf("%v", config), fmt.Sprintf("%#v", config), string(mustJSON(t, config)),
		gateway.String(), fmt.Sprintf("%v", gateway), fmt.Sprintf("%#v", gateway), string(mustJSON(t, gateway)),
	}
	for _, output := range outputs {
		for _, secret := range []string{
			gatewayTestConnection, "gateway-1", "boot-format-secret", "device-format-secret",
			"cf:format:secret", binding, privateScalar,
		} {
			if strings.Contains(output, secret) {
				t.Fatalf("formatting leaked %q: %s", secret, output)
			}
		}
	}
}

func TestGatewaySessionCopiesCallerKeyMaterial(t *testing.T) {
	keys := newGatewayTestKeys(t)
	originalGateway := cloneGatewayPrivateKey(keys.gateway)
	originalDevice := cloneGatewayPrivateKey(keys.device)
	gateway := newGatewayTestSession(t, keys, gatewayTestConnection, "boot-copy")
	keys.gateway.D.SetInt64(1)
	keys.gateway.PublicKey.X.SetInt64(1)
	keys.device.PublicKey.X.SetInt64(1)

	if _, err := gateway.Hello(fixedTestTime); err != nil {
		t.Fatal(err)
	}
	binding := testBinding(36)
	challenge := gatewayTestChallenge(
		t, "gateway-1", "boot-copy", 1,
		fixedTestTime.Add(DefaultChallengeTTL), binding,
	)
	authenticate, err := gateway.AcceptChallenge(gatewayTestConnection, challenge, fixedTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGatewayFrame(&originalGateway.PublicKey, authenticate, binding); err != nil {
		t.Fatalf("copied gateway key did not sign: %v", err)
	}
	ack := gatewayTestAck(t, "gateway-1", "boot-copy", 1, fixedTestTime.Add(time.Minute))
	if err := gateway.AcceptAuthenticated(gatewayTestConnection, ack, fixedTestTime); err != nil {
		t.Fatal(err)
	}
	lease, _ := gateway.Lease()
	probe := gatewayTestSignedProbe(
		t, originalDevice, lease, "device-1", "", "operation-copy", 1, fixedTestTime,
	)
	if err := gateway.AcceptOperationProbe(gatewayTestConnection, probe, fixedTestTime); err != nil {
		t.Fatalf("copied device key did not verify: %v", err)
	}
}

func TestGatewaySessionConstructorRejectsInvalidIdentityKeysAndTTL(t *testing.T) {
	keys := newGatewayTestKeys(t)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	valid := GatewaySessionConfig{
		ConnectionID: gatewayTestConnection, GatewayID: "gateway-1", BootID: "boot-1",
		PrivateKey: keys.gateway,
	}
	tests := []struct {
		name   string
		mutate func(*GatewaySessionConfig)
	}{
		{"connection", func(config *GatewaySessionConfig) { config.ConnectionID = "bad connection" }},
		{"gateway", func(config *GatewaySessionConfig) { config.GatewayID = "Gateway" }},
		{"boot", func(config *GatewaySessionConfig) { config.BootID = "bad/boot" }},
		{"nil private key", func(config *GatewaySessionConfig) { config.PrivateKey = nil }},
		{"wrong curve", func(config *GatewaySessionConfig) { config.PrivateKey = p384 }},
		{"challenge ttl", func(config *GatewaySessionConfig) { config.ChallengeTTL = DefaultChallengeTTL + time.Second }},
		{"ttl ordering", func(config *GatewaySessionConfig) {
			config.ChallengeTTL = 2 * time.Second
			config.MessageTTL = time.Second
		}},
		{"device key", func(config *GatewaySessionConfig) {
			config.DeviceKeys = map[string]*ecdsa.PublicKey{"bad/device": &keys.device.PublicKey}
		}},
		{"principal key", func(config *GatewaySessionConfig) {
			config.PrincipalKeys = map[string]*ecdsa.PublicKey{"not-namespaced": &keys.principal.PublicKey}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := NewGatewaySession(candidate); !errors.Is(err, ErrMalformed) {
				t.Fatalf("constructor error = %v", err)
			}
		})
	}
}

func TestGatewaySessionConcurrentObservers(t *testing.T) {
	harness := newAuthenticatedGatewayHarness(t)
	const observers = 16
	const iterations = 100
	var workers sync.WaitGroup
	for range observers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range iterations {
				_ = harness.gateway.State()
				_, _ = harness.gateway.Lease()
				_ = harness.gateway.String()
				_, _ = json.Marshal(harness.gateway)
			}
		}()
	}
	for index := 1; index <= iterations; index++ {
		if _, err := harness.gateway.Snapshot(gatewayTestConnection, SnapshotPayload{
			ObservedAt: fixedTestTime.Unix(), Revision: uint64(index),
			Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
		}, fixedTestTime); err != nil {
			t.Fatalf("snapshot %d: %v", index, err)
		}
	}
	workers.Wait()
	if harness.gateway.State() != GatewaySessionAuthenticated || harness.gateway.outboundSequence != iterations {
		t.Fatalf("state=%s outbound=%d", harness.gateway.State(), harness.gateway.outboundSequence)
	}
}
