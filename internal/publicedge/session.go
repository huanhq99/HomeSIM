package publicedge

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"
)

type SessionState string

const (
	SessionChallenged    SessionState = "challenged"
	SessionAuthenticated SessionState = "authenticated"
	SessionClosed        SessionState = "closed"
	SessionFailed        SessionState = "failed"
)

type RegistryConfig struct {
	GatewayKeys   map[string]*ecdsa.PublicKey
	DeviceKeys    map[string]*ecdsa.PublicKey
	PrincipalKeys map[string]*ecdsa.PublicKey
	ChallengeTTL  time.Duration
	MessageTTL    time.Duration
}

type registryGateway struct {
	publicKey *ecdsa.PublicKey
	lastEpoch uint64
	active    *Session
}

// Registry owns the single-active gateway lease and all configured P-256
// verification keys. It is safe for concurrent use.
type Registry struct {
	mu            sync.Mutex
	gateways      map[string]*registryGateway
	deviceKeys    map[string]*ecdsa.PublicKey
	principalKeys map[string]*ecdsa.PublicKey
	challengeTTL  time.Duration
	messageTTL    time.Duration
}

// Session is a transport-independent gateway WebSocket state machine. The
// caller must close the actual WebSocket whenever a method returns a protocol,
// authorization, expiry, or sequence error.
type Session struct {
	registry *Registry
	state    SessionState

	connectionID string
	gatewayID    string
	bootID       string
	leaseEpoch   uint64
	challenge    string
	challengeEnd time.Time

	inboundSequence   uint64
	outboundSequence  uint64
	lastRevision      uint64
	lastEventSequence uint64
	pendingOperations map[string]struct{}
}

type Lease struct {
	GatewayID      string
	BootID         string
	LeaseEpoch     uint64
	ChannelBinding string
}

func NewRegistry(config RegistryConfig) (*Registry, error) {
	challengeTTL := config.ChallengeTTL
	if challengeTTL == 0 {
		challengeTTL = DefaultChallengeTTL
	}
	messageTTL := config.MessageTTL
	if messageTTL == 0 {
		messageTTL = DefaultMessageTTL
	}
	if challengeTTL < time.Second || challengeTTL > DefaultChallengeTTL ||
		messageTTL < time.Second || messageTTL > DefaultMessageTTL {
		return nil, fmt.Errorf("%w: registry TTL", ErrMalformed)
	}
	if len(config.GatewayKeys) == 0 {
		return nil, fmt.Errorf("%w: at least one gateway key is required", ErrMalformed)
	}
	registry := &Registry{
		gateways:      make(map[string]*registryGateway, len(config.GatewayKeys)),
		deviceKeys:    make(map[string]*ecdsa.PublicKey, len(config.DeviceKeys)),
		principalKeys: make(map[string]*ecdsa.PublicKey, len(config.PrincipalKeys)),
		challengeTTL:  challengeTTL,
		messageTTL:    messageTTL,
	}
	for gatewayID, key := range config.GatewayKeys {
		if !validGatewayID(gatewayID) || !validPublicKey(key) {
			return nil, fmt.Errorf("%w: gateway key", ErrMalformed)
		}
		registry.gateways[gatewayID] = &registryGateway{publicKey: clonePublicKey(key)}
	}
	for deviceID, key := range config.DeviceKeys {
		if !validOpaqueID(deviceID, MaxDeviceIDBytes) || !validPublicKey(key) {
			return nil, fmt.Errorf("%w: device key", ErrMalformed)
		}
		registry.deviceKeys[deviceID] = clonePublicKey(key)
	}
	for principal, key := range config.PrincipalKeys {
		if !validPrincipal(principal) || !validPublicKey(key) {
			return nil, fmt.Errorf("%w: principal key", ErrMalformed)
		}
		registry.principalKeys[principal] = clonePublicKey(key)
	}
	return registry, nil
}

// Open validates gateway.hello and issues a bounded challenge. An already
// authenticated connection is never replaced. Multiple unauthenticated hello
// attempts cannot reserve or advance the lease epoch.
func (registry *Registry) Open(connectionID string, hello Frame, now time.Time) (*Session, Frame, error) {
	if registry == nil || !validOpaqueID(connectionID, MaxDeviceIDBytes) {
		return nil, Frame{}, fmt.Errorf("%w: connection id", ErrMalformed)
	}
	if hello.Type != FrameGatewayHello {
		return nil, Frame{}, fmt.Errorf("%w: expected gateway hello", ErrProtocolViolation)
	}
	if err := validateFrame(hello, true); err != nil {
		return nil, Frame{}, err
	}
	if err := validateAt(hello, now, registry.messageTTL); err != nil {
		return nil, Frame{}, err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	gateway := registry.gateways[hello.GatewayID]
	if gateway == nil {
		return nil, Frame{}, ErrUnauthorized
	}
	if gateway.active != nil {
		return nil, Frame{}, ErrGatewayBusy
	}
	if gateway.lastEpoch == MaxWireCounter {
		return nil, Frame{}, fmt.Errorf("%w: lease epoch exhausted", ErrProtocolViolation)
	}
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return nil, Frame{}, fmt.Errorf("generate gateway challenge: %w", err)
	}
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes)
	challengeEnd := now.Add(registry.challengeTTL)
	session := &Session{
		registry: registry, state: SessionChallenged, connectionID: connectionID,
		gatewayID: hello.GatewayID, bootID: hello.BootID,
		leaseEpoch: gateway.lastEpoch + 1, challenge: challenge, challengeEnd: challengeEnd,
		pendingOperations: make(map[string]struct{}),
	}
	response, err := NewFrame(FrameEdgeChallenge, FrameMeta{
		GatewayID: session.gatewayID, ExpiresAt: challengeEnd.Unix(), BootID: session.bootID,
		LeaseEpoch: session.leaseEpoch,
	}, ChallengePayload{Challenge: challenge})
	if err != nil {
		return nil, Frame{}, err
	}
	return session, response, nil
}

// Authenticate verifies the challenge response and atomically acquires the
// sole active lease. A second connection can neither preempt nor share it.
func (session *Session) Authenticate(frame Frame, now time.Time) (Frame, error) {
	if session == nil || session.registry == nil {
		return Frame{}, ErrClosed
	}
	registry := session.registry
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if session.state != SessionChallenged {
		return Frame{}, session.failLocked(ErrProtocolViolation)
	}
	if !now.Before(session.challengeEnd) {
		return Frame{}, session.failLocked(ErrExpired)
	}
	if frame.Type != FrameGatewayAuthenticate || frame.GatewayID != session.gatewayID ||
		frame.BootID != session.bootID || frame.LeaseEpoch != session.leaseEpoch || frame.Sequence != 0 {
		return Frame{}, session.failLocked(ErrProtocolViolation)
	}
	if err := validateFrame(frame, true); err != nil {
		return Frame{}, session.failLocked(err)
	}
	if err := validateAt(frame, now, registry.messageTTL); err != nil || time.Unix(frame.ExpiresAt, 0).After(session.challengeEnd) {
		if err == nil {
			err = ErrProtocolViolation
		}
		return Frame{}, session.failLocked(err)
	}
	var payload GatewayAuthenticatePayload
	if err := decodeStrictJSON(frame.Payload, &payload); err != nil ||
		!hmac.Equal([]byte(payload.Challenge), []byte(session.challenge)) {
		return Frame{}, session.failLocked(ErrUnauthorized)
	}
	gateway := registry.gateways[session.gatewayID]
	if gateway == nil || gateway.active != nil || gateway.lastEpoch+1 != session.leaseEpoch {
		return Frame{}, session.failLocked(ErrGatewayBusy)
	}
	if err := VerifyGatewayFrame(gateway.publicKey, frame, session.challenge); err != nil {
		return Frame{}, session.failLocked(ErrUnauthorized)
	}
	gateway.lastEpoch = session.leaseEpoch
	gateway.active = session
	session.state = SessionAuthenticated
	response, err := NewFrame(FrameEdgeAuthenticated, FrameMeta{
		GatewayID: session.gatewayID, ExpiresAt: now.Add(registry.messageTTL).Unix(),
		BootID: session.bootID, LeaseEpoch: session.leaseEpoch,
	}, AuthenticatedPayload{})
	if err != nil {
		return Frame{}, session.failLocked(err)
	}
	return response, nil
}

// AcceptGateway accepts one authenticated gateway-to-edge state frame. Any
// malformed, stale, replayed, out-of-order, wrongly bound, or unsolicited
// result terminally fails the lease.
func (session *Session) AcceptGateway(frame Frame, now time.Time) error {
	if session == nil || session.registry == nil {
		return ErrClosed
	}
	registry := session.registry
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if session.state != SessionAuthenticated || registry.gateways[session.gatewayID].active != session {
		return ErrClosed
	}
	if frame.Type != FrameStateSnapshot && frame.Type != FrameStateEvent && frame.Type != FrameOperationResult {
		return session.failLocked(ErrProtocolViolation)
	}
	if err := validateFrame(frame, true); err != nil {
		return session.failLocked(err)
	}
	if err := validateAt(frame, now, registry.messageTTL); err != nil {
		return session.failLocked(err)
	}
	if frame.GatewayID != session.gatewayID || frame.BootID != session.bootID ||
		frame.LeaseEpoch != session.leaseEpoch || session.inboundSequence == MaxWireCounter ||
		frame.Sequence != session.inboundSequence+1 {
		return session.failLocked(ErrSequence)
	}
	if err := VerifyGatewayFrame(registry.gateways[session.gatewayID].publicKey, frame, session.challenge); err != nil {
		return session.failLocked(ErrUnauthorized)
	}
	if observedAt, present, err := frameObservedAt(frame); err != nil {
		return session.failLocked(ErrProtocolViolation)
	} else if present {
		if err := validateObservedAt(observedAt, now); err != nil {
			return session.failLocked(err)
		}
	}
	if err := session.acceptStateLocked(frame); err != nil {
		return session.failLocked(err)
	}
	session.inboundSequence = frame.Sequence
	return nil
}

// AcceptClient authenticates and sequences the only allowed client-to-gateway
// frame: a read-only operation probe. Invalid unauthenticated requests never
// disturb a healthy gateway lease; an authenticated sequence conflict
// terminally fails the lease. After success, the caller must write the exact
// accepted frame next and close the Session on any ambiguous write outcome.
func (session *Session) AcceptClient(frame Frame, now time.Time) error {
	if session == nil || session.registry == nil {
		return ErrClosed
	}
	registry := session.registry
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if session.state != SessionAuthenticated || registry.gateways[session.gatewayID].active != session {
		return ErrClosed
	}
	if frame.Type != FrameOperationProbe || validateFrame(frame, true) != nil ||
		validateAt(frame, now, registry.messageTTL) != nil {
		return ErrUnauthorized
	}
	var publicKey *ecdsa.PublicKey
	if frame.DeviceID != "" {
		publicKey = registry.deviceKeys[frame.DeviceID]
	} else {
		publicKey = registry.principalKeys[frame.Principal]
	}
	if publicKey == nil || VerifyClientFrame(publicKey, frame, session.challenge) != nil {
		return ErrUnauthorized
	}
	if frame.GatewayID != session.gatewayID || frame.BootID != session.bootID ||
		frame.LeaseEpoch != session.leaseEpoch || session.outboundSequence == MaxWireCounter ||
		frame.Sequence != session.outboundSequence+1 {
		return session.failLocked(ErrSequence)
	}
	if _, exists := session.pendingOperations[frame.OperationID]; exists {
		return session.failLocked(fmt.Errorf("%w: duplicate pending operation", ErrProtocolViolation))
	}
	if len(session.pendingOperations) >= MaxPendingProbes {
		return session.failLocked(fmt.Errorf("%w: too many pending probes", ErrProtocolViolation))
	}
	session.pendingOperations[frame.OperationID] = struct{}{}
	session.outboundSequence = frame.Sequence
	return nil
}

func (session *Session) acceptStateLocked(frame Frame) error {
	switch frame.Type {
	case FrameStateSnapshot:
		var payload SnapshotPayload
		if err := decodeStrictJSON(frame.Payload, &payload); err != nil ||
			payload.Revision < session.lastRevision || payload.EventHighWater < session.lastEventSequence {
			return ErrProtocolViolation
		}
		session.lastRevision = payload.Revision
		session.lastEventSequence = payload.EventHighWater
	case FrameStateEvent:
		var payload EventPayload
		if err := decodeStrictJSON(frame.Payload, &payload); err != nil ||
			session.lastEventSequence == MaxWireCounter || payload.EventSequence != session.lastEventSequence+1 ||
			payload.Revision < session.lastRevision {
			return ErrProtocolViolation
		}
		session.lastRevision = payload.Revision
		session.lastEventSequence = payload.EventSequence
	case FrameOperationResult:
		if _, exists := session.pendingOperations[frame.OperationID]; !exists {
			return ErrOperationNotProbed
		}
		delete(session.pendingOperations, frame.OperationID)
	default:
		return ErrProtocolViolation
	}
	return nil
}

func (session *Session) Close() {
	if session == nil || session.registry == nil {
		return
	}
	session.registry.mu.Lock()
	defer session.registry.mu.Unlock()
	if session.state == SessionClosed || session.state == SessionFailed {
		return
	}
	if gateway := session.registry.gateways[session.gatewayID]; gateway != nil && gateway.active == session {
		gateway.active = nil
	}
	session.state = SessionClosed
	session.clearLocked()
}

func (session *Session) failLocked(reason error) error {
	if session.state == SessionClosed || session.state == SessionFailed {
		return ErrClosed
	}
	if gateway := session.registry.gateways[session.gatewayID]; gateway != nil && gateway.active == session {
		gateway.active = nil
	}
	session.state = SessionFailed
	session.clearLocked()
	return reason
}

func (session *Session) clearLocked() {
	for operationID := range session.pendingOperations {
		delete(session.pendingOperations, operationID)
	}
	session.challenge = ""
	session.connectionID = ""
}

func (session *Session) State() SessionState {
	if session == nil || session.registry == nil {
		return SessionClosed
	}
	session.registry.mu.Lock()
	defer session.registry.mu.Unlock()
	return session.state
}

func (session *Session) Lease() (Lease, error) {
	if session == nil || session.registry == nil {
		return Lease{}, ErrClosed
	}
	session.registry.mu.Lock()
	defer session.registry.mu.Unlock()
	if session.state != SessionChallenged && session.state != SessionAuthenticated {
		return Lease{}, ErrClosed
	}
	return Lease{
		GatewayID: session.gatewayID, BootID: session.bootID,
		LeaseEpoch: session.leaseEpoch, ChannelBinding: session.challenge,
	}, nil
}

func clonePublicKey(key *ecdsa.PublicKey) *ecdsa.PublicKey {
	return &ecdsa.PublicKey{Curve: key.Curve, X: newBigInt(key.X), Y: newBigInt(key.Y)}
}

func newBigInt(value interface{ Bytes() []byte }) *big.Int {
	return new(big.Int).SetBytes(value.Bytes())
}

type redactedSession struct {
	State            SessionState `json:"state"`
	ConnectionID     string       `json:"connection_id"`
	GatewayID        string       `json:"gateway_id"`
	BootID           string       `json:"boot_id"`
	LeaseEpoch       uint64       `json:"lease_epoch"`
	ChannelBinding   string       `json:"channel_binding"`
	InboundSequence  uint64       `json:"inbound_sequence"`
	OutboundSequence uint64       `json:"outbound_sequence"`
}

func (session *Session) redactedLocked() redactedSession {
	return redactedSession{
		State: session.state, ConnectionID: redactIfSet(session.connectionID),
		GatewayID: redactIfSet(session.gatewayID), BootID: redactIfSet(session.bootID),
		LeaseEpoch: session.leaseEpoch, ChannelBinding: redactIfSet(session.challenge),
		InboundSequence: session.inboundSequence, OutboundSequence: session.outboundSequence,
	}
}

func (session *Session) MarshalJSON() ([]byte, error) {
	if session == nil || session.registry == nil {
		return json.Marshal(redactedSession{State: SessionClosed})
	}
	session.registry.mu.Lock()
	defer session.registry.mu.Unlock()
	return json.Marshal(session.redactedLocked())
}

func (session *Session) String() string {
	data, err := session.MarshalJSON()
	if err != nil {
		return "publicedge.Session{redacted}"
	}
	return string(data)
}

func (session *Session) GoString() string { return session.String() }
