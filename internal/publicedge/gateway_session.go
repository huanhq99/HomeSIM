package publicedge

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// GatewaySessionState is the local Mac's transport-independent connection
// state. Network reads and writes remain the caller's responsibility.
type GatewaySessionState string

const (
	GatewaySessionNew            GatewaySessionState = "new"
	GatewaySessionHelloSent      GatewaySessionState = "hello_sent"
	GatewaySessionAuthenticating GatewaySessionState = "authenticating"
	GatewaySessionAuthenticated  GatewaySessionState = "authenticated"
	GatewaySessionClosed         GatewaySessionState = "closed"
	GatewaySessionFailed         GatewaySessionState = "failed"
)

// GatewaySessionConfig contains the immutable identity and verification keys
// for one outbound gateway connection. Key material is copied at construction.
type GatewaySessionConfig struct {
	ConnectionID  string
	GatewayID     string
	BootID        string
	PrivateKey    *ecdsa.PrivateKey
	DeviceKeys    map[string]*ecdsa.PublicKey
	PrincipalKeys map[string]*ecdsa.PublicKey
	ChallengeTTL  time.Duration
	MessageTTL    time.Duration
}

// GatewaySession is the home-Mac side of Session. It can only authenticate,
// publish bounded read-only state, receive a signed operation probe, and
// report that already-existing operation's state. It has no transport or
// mutation callback and is safe for concurrent use.
type GatewaySession struct {
	mu sync.Mutex

	state         GatewaySessionState
	connectionID  string
	gatewayID     string
	bootID        string
	privateKey    *ecdsa.PrivateKey
	deviceKeys    map[string]*ecdsa.PublicKey
	principalKeys map[string]*ecdsa.PublicKey
	challengeTTL  time.Duration
	messageTTL    time.Duration

	challenge    string
	challengeEnd time.Time
	leaseEpoch   uint64

	inboundSequence   uint64
	outboundSequence  uint64
	lastRevision      uint64
	lastEventSequence uint64
	pendingOperations map[string]struct{}
}

// NewGatewaySession constructs one connection incarnation in the default-off
// state. The caller must create a fresh session for every transport connection.
func NewGatewaySession(config GatewaySessionConfig) (*GatewaySession, error) {
	challengeTTL := config.ChallengeTTL
	if challengeTTL == 0 {
		challengeTTL = DefaultChallengeTTL
	}
	messageTTL := config.MessageTTL
	if messageTTL == 0 {
		messageTTL = DefaultMessageTTL
	}
	if !validOpaqueID(config.ConnectionID, MaxDeviceIDBytes) ||
		!validGatewayID(config.GatewayID) || !validOpaqueID(config.BootID, MaxBootIDBytes) ||
		!validPrivateKey(config.PrivateKey) {
		return nil, fmt.Errorf("%w: gateway session identity or key", ErrMalformed)
	}
	if challengeTTL < time.Second || challengeTTL > DefaultChallengeTTL ||
		messageTTL < time.Second || messageTTL > DefaultMessageTTL || challengeTTL > messageTTL {
		return nil, fmt.Errorf("%w: gateway session TTL", ErrMalformed)
	}

	deviceKeys := make(map[string]*ecdsa.PublicKey, len(config.DeviceKeys))
	for deviceID, key := range config.DeviceKeys {
		if !validOpaqueID(deviceID, MaxDeviceIDBytes) || !validPublicKey(key) {
			return nil, fmt.Errorf("%w: gateway device key", ErrMalformed)
		}
		deviceKeys[deviceID] = clonePublicKey(key)
	}
	principalKeys := make(map[string]*ecdsa.PublicKey, len(config.PrincipalKeys))
	for principal, key := range config.PrincipalKeys {
		if !validPrincipal(principal) || !validPublicKey(key) {
			return nil, fmt.Errorf("%w: gateway principal key", ErrMalformed)
		}
		principalKeys[principal] = clonePublicKey(key)
	}

	return &GatewaySession{
		state: GatewaySessionNew, connectionID: config.ConnectionID,
		gatewayID: config.GatewayID, bootID: config.BootID,
		privateKey: cloneGatewayPrivateKey(config.PrivateKey),
		deviceKeys: deviceKeys, principalKeys: principalKeys,
		challengeTTL: challengeTTL, messageTTL: messageTTL,
		pendingOperations: make(map[string]struct{}),
	}, nil
}

// Hello creates the sole unsigned gateway.hello for this connection.
func (session *GatewaySession) Hello(now time.Time) (Frame, error) {
	if session == nil {
		return Frame{}, ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireStateLocked(GatewaySessionNew); err != nil {
		return Frame{}, err
	}
	frame, err := NewFrame(FrameGatewayHello, FrameMeta{
		GatewayID: session.gatewayID, BootID: session.bootID,
		ExpiresAt: now.Add(session.messageTTL).Unix(),
	}, GatewayHelloPayload{})
	if err != nil || validateAt(frame, now, session.messageTTL) != nil {
		if err == nil {
			err = fmt.Errorf("%w: hello time", ErrProtocolViolation)
		}
		return Frame{}, session.failLocked(err)
	}
	session.state = GatewaySessionHelloSent
	return frame, nil
}

// AcceptChallenge binds the edge challenge to this exact transport
// incarnation and returns a P-256 gateway.authenticate frame. Its expiry is
// copied from the challenge, so the signed response cannot outlive it.
func (session *GatewaySession) AcceptChallenge(connectionID string, frame Frame, now time.Time) (Frame, error) {
	if session == nil {
		return Frame{}, ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireConnectionStateLocked(connectionID, GatewaySessionHelloSent); err != nil {
		return Frame{}, err
	}
	if frame.Type != FrameEdgeChallenge {
		return Frame{}, session.failLocked(fmt.Errorf("%w: expected edge challenge", ErrProtocolViolation))
	}
	if err := validateFrame(frame, true); err != nil {
		return Frame{}, session.failLocked(err)
	}
	if err := validateAt(frame, now, session.challengeTTL); err != nil {
		return Frame{}, session.failLocked(err)
	}
	if frame.GatewayID != session.gatewayID || frame.BootID != session.bootID || frame.Sequence != 0 {
		return Frame{}, session.failLocked(fmt.Errorf("%w: challenge incarnation", ErrProtocolViolation))
	}
	var payload ChallengePayload
	if err := decodeStrictJSON(frame.Payload, &payload); err != nil {
		return Frame{}, session.failLocked(fmt.Errorf("%w: challenge payload", ErrProtocolViolation))
	}

	session.challenge = payload.Challenge
	session.challengeEnd = time.Unix(frame.ExpiresAt, 0)
	session.leaseEpoch = frame.LeaseEpoch
	authenticate, err := NewFrame(FrameGatewayAuthenticate, FrameMeta{
		GatewayID: session.gatewayID, ExpiresAt: frame.ExpiresAt,
		BootID: session.bootID, LeaseEpoch: session.leaseEpoch,
	}, GatewayAuthenticatePayload{Challenge: session.challenge})
	if err == nil {
		authenticate, err = SignGatewayFrame(session.privateKey, authenticate, session.challenge)
	}
	if err != nil {
		return Frame{}, session.failLocked(err)
	}
	session.state = GatewaySessionAuthenticating
	return authenticate, nil
}

// AcceptAuthenticated completes the handshake once. Replayed, duplicate, or
// wrongly bound acknowledgements terminally fail this connection.
func (session *GatewaySession) AcceptAuthenticated(connectionID string, frame Frame, now time.Time) error {
	if session == nil {
		return ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireConnectionStateLocked(connectionID, GatewaySessionAuthenticating); err != nil {
		return err
	}
	if !now.Before(session.challengeEnd) {
		return session.failLocked(ErrExpired)
	}
	if frame.Type != FrameEdgeAuthenticated {
		return session.failLocked(fmt.Errorf("%w: expected edge authenticated", ErrProtocolViolation))
	}
	if err := validateFrame(frame, true); err != nil {
		return session.failLocked(err)
	}
	if err := validateAt(frame, now, session.messageTTL); err != nil {
		return session.failLocked(err)
	}
	if frame.GatewayID != session.gatewayID || frame.BootID != session.bootID ||
		frame.LeaseEpoch != session.leaseEpoch || frame.Sequence != 0 {
		return session.failLocked(fmt.Errorf("%w: authenticated incarnation", ErrProtocolViolation))
	}
	session.state = GatewaySessionAuthenticated
	return nil
}

// Snapshot creates the next signed state.snapshot on the single gateway
// outbound sequence.
func (session *GatewaySession) Snapshot(connectionID string, payload SnapshotPayload, now time.Time) (Frame, error) {
	if session == nil {
		return Frame{}, ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireConnectionStateLocked(connectionID, GatewaySessionAuthenticated); err != nil {
		return Frame{}, err
	}
	if err := validateObservedAt(payload.ObservedAt, now); err != nil ||
		payload.Revision < session.lastRevision || payload.EventHighWater < session.lastEventSequence {
		if err == nil {
			err = fmt.Errorf("%w: snapshot watermark", ErrProtocolViolation)
		}
		return Frame{}, session.failLocked(err)
	}
	frame, err := session.nextGatewayFrameLocked(FrameStateSnapshot, "", payload, now)
	if err != nil {
		return Frame{}, err
	}
	session.lastRevision = payload.Revision
	session.lastEventSequence = payload.EventHighWater
	return frame, nil
}

// Event creates the next signed state.event and enforces the event watermark
// locally before it can reach the edge.
func (session *GatewaySession) Event(connectionID string, payload EventPayload, now time.Time) (Frame, error) {
	if session == nil {
		return Frame{}, ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireConnectionStateLocked(connectionID, GatewaySessionAuthenticated); err != nil {
		return Frame{}, err
	}
	if err := validateObservedAt(payload.ObservedAt, now); err != nil ||
		session.lastEventSequence == MaxWireCounter || payload.EventSequence != session.lastEventSequence+1 ||
		payload.Revision < session.lastRevision {
		if err == nil {
			err = fmt.Errorf("%w: event watermark", ErrProtocolViolation)
		}
		return Frame{}, session.failLocked(err)
	}
	frame, err := session.nextGatewayFrameLocked(FrameStateEvent, "", payload, now)
	if err != nil {
		return Frame{}, err
	}
	session.lastRevision = payload.Revision
	session.lastEventSequence = payload.EventSequence
	return frame, nil
}

// AcceptOperationProbe re-verifies the sole client frame using the gateway's
// own enrollment key table. Its sequence is global across all client and
// principal identities for the entire lease.
func (session *GatewaySession) AcceptOperationProbe(connectionID string, frame Frame, now time.Time) error {
	if session == nil {
		return ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireConnectionStateLocked(connectionID, GatewaySessionAuthenticated); err != nil {
		return err
	}
	if frame.Type != FrameOperationProbe {
		return session.failLocked(fmt.Errorf("%w: client frame is not an operation probe", ErrProtocolViolation))
	}
	if err := validateFrame(frame, true); err != nil {
		return session.failLocked(err)
	}
	if err := validateAt(frame, now, session.messageTTL); err != nil {
		return session.failLocked(err)
	}
	var publicKey *ecdsa.PublicKey
	if frame.DeviceID != "" {
		publicKey = session.deviceKeys[frame.DeviceID]
	} else {
		publicKey = session.principalKeys[frame.Principal]
	}
	if publicKey == nil || VerifyClientFrame(publicKey, frame, session.challenge) != nil {
		return session.failLocked(ErrUnauthorized)
	}
	if frame.GatewayID != session.gatewayID || frame.BootID != session.bootID ||
		frame.LeaseEpoch != session.leaseEpoch {
		return session.failLocked(fmt.Errorf("%w: probe incarnation", ErrProtocolViolation))
	}
	if session.inboundSequence == MaxWireCounter || frame.Sequence != session.inboundSequence+1 {
		return session.failLocked(ErrSequence)
	}
	if _, exists := session.pendingOperations[frame.OperationID]; exists {
		return session.failLocked(fmt.Errorf("%w: duplicate pending operation", ErrProtocolViolation))
	}
	if len(session.pendingOperations) >= MaxPendingProbes {
		return session.failLocked(fmt.Errorf("%w: too many pending probes", ErrProtocolViolation))
	}
	session.pendingOperations[frame.OperationID] = struct{}{}
	session.inboundSequence = frame.Sequence
	return nil
}

// OperationResult reports one pending probe on the same signed gateway
// outbound sequence. It cannot create, execute, or retry an operation.
func (session *GatewaySession) OperationResult(connectionID, operationID string, payload OperationResultPayload, now time.Time) (Frame, error) {
	if session == nil {
		return Frame{}, ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.requireConnectionStateLocked(connectionID, GatewaySessionAuthenticated); err != nil {
		return Frame{}, err
	}
	if _, exists := session.pendingOperations[operationID]; !exists {
		return Frame{}, session.failLocked(ErrOperationNotProbed)
	}
	if err := validateObservedAt(payload.ObservedAt, now); err != nil {
		return Frame{}, session.failLocked(err)
	}
	frame, err := session.nextGatewayFrameLocked(FrameOperationResult, operationID, payload, now)
	if err != nil {
		return Frame{}, err
	}
	delete(session.pendingOperations, operationID)
	return frame, nil
}

func (session *GatewaySession) nextGatewayFrameLocked(kind FrameType, operationID string, payload any, now time.Time) (Frame, error) {
	if session.outboundSequence == MaxWireCounter {
		return Frame{}, session.failLocked(ErrSequence)
	}
	frame, err := NewFrame(kind, FrameMeta{
		GatewayID: session.gatewayID, OperationID: operationID,
		ExpiresAt: now.Add(session.messageTTL).Unix(), BootID: session.bootID,
		LeaseEpoch: session.leaseEpoch, Sequence: session.outboundSequence + 1,
	}, payload)
	if err == nil {
		err = validateAt(frame, now, session.messageTTL)
	}
	if err == nil {
		frame, err = SignGatewayFrame(session.privateKey, frame, session.challenge)
	}
	if err != nil {
		return Frame{}, session.failLocked(err)
	}
	session.outboundSequence = frame.Sequence
	return frame, nil
}

func (session *GatewaySession) requireStateLocked(want GatewaySessionState) error {
	if session.state == GatewaySessionClosed || session.state == GatewaySessionFailed {
		return ErrClosed
	}
	if session.state != want {
		return session.failLocked(ErrProtocolViolation)
	}
	return nil
}

func (session *GatewaySession) requireConnectionStateLocked(connectionID string, want GatewaySessionState) error {
	if err := session.requireStateLocked(want); err != nil {
		return err
	}
	if !validOpaqueID(connectionID, MaxDeviceIDBytes) || connectionID != session.connectionID {
		return session.failLocked(fmt.Errorf("%w: stale connection", ErrProtocolViolation))
	}
	return nil
}

func (session *GatewaySession) failLocked(reason error) error {
	if session.state == GatewaySessionClosed || session.state == GatewaySessionFailed {
		return ErrClosed
	}
	session.state = GatewaySessionFailed
	session.clearLocked()
	return reason
}

func (session *GatewaySession) clearLocked() {
	for operationID := range session.pendingOperations {
		delete(session.pendingOperations, operationID)
	}
	session.challenge = ""
	session.challengeEnd = time.Time{}
	if session.privateKey != nil && session.privateKey.D != nil {
		session.privateKey.D.SetInt64(0)
	}
	session.privateKey = nil
	session.deviceKeys = nil
	session.principalKeys = nil
}

// Close invalidates this connection incarnation and clears channel binding,
// pending operation IDs, and the session's private-key copy.
func (session *GatewaySession) Close() {
	if session == nil {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state == GatewaySessionClosed || session.state == GatewaySessionFailed {
		return
	}
	session.state = GatewaySessionClosed
	session.clearLocked()
}

func (session *GatewaySession) State() GatewaySessionState {
	if session == nil {
		return GatewaySessionClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.state
}

// Lease returns the current incarnation only after a challenge was accepted.
// Its ordinary formatting is redacted by Lease's existing methods.
func (session *GatewaySession) Lease() (Lease, error) {
	if session == nil {
		return Lease{}, ErrClosed
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.state != GatewaySessionAuthenticating && session.state != GatewaySessionAuthenticated {
		return Lease{}, ErrClosed
	}
	return Lease{
		GatewayID: session.gatewayID, BootID: session.bootID,
		LeaseEpoch: session.leaseEpoch, ChannelBinding: session.challenge,
	}, nil
}

func cloneGatewayPrivateKey(key *ecdsa.PrivateKey) *ecdsa.PrivateKey {
	publicKey := clonePublicKey(&key.PublicKey)
	return &ecdsa.PrivateKey{PublicKey: *publicKey, D: newBigInt(key.D)}
}

type redactedGatewaySessionConfig struct {
	ConnectionID      string        `json:"connection_id"`
	GatewayID         string        `json:"gateway_id"`
	BootID            string        `json:"boot_id"`
	PrivateKey        string        `json:"private_key"`
	DeviceKeyCount    int           `json:"device_key_count"`
	PrincipalKeyCount int           `json:"principal_key_count"`
	ChallengeTTL      time.Duration `json:"challenge_ttl"`
	MessageTTL        time.Duration `json:"message_ttl"`
}

func (config GatewaySessionConfig) redacted() redactedGatewaySessionConfig {
	return redactedGatewaySessionConfig{
		ConnectionID: redactIfSet(config.ConnectionID), GatewayID: redactIfSet(config.GatewayID),
		BootID: redactIfSet(config.BootID), PrivateKey: redactIfSet("configured"),
		DeviceKeyCount: len(config.DeviceKeys), PrincipalKeyCount: len(config.PrincipalKeys),
		ChallengeTTL: config.ChallengeTTL, MessageTTL: config.MessageTTL,
	}
}

func (config GatewaySessionConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(config.redacted())
}

func (config GatewaySessionConfig) String() string {
	data, err := json.Marshal(config.redacted())
	if err != nil {
		return "publicedge.GatewaySessionConfig{redacted}"
	}
	return string(data)
}

func (config GatewaySessionConfig) GoString() string { return config.String() }

type redactedGatewaySession struct {
	State             GatewaySessionState `json:"state"`
	ConnectionID      string              `json:"connection_id"`
	GatewayID         string              `json:"gateway_id"`
	BootID            string              `json:"boot_id"`
	LeaseEpoch        uint64              `json:"lease_epoch"`
	ChannelBinding    string              `json:"channel_binding"`
	InboundSequence   uint64              `json:"inbound_sequence"`
	OutboundSequence  uint64              `json:"outbound_sequence"`
	PendingOperations int                 `json:"pending_operations"`
}

func (session *GatewaySession) redactedLocked() redactedGatewaySession {
	return redactedGatewaySession{
		State: session.state, ConnectionID: redactIfSet(session.connectionID),
		GatewayID: redactIfSet(session.gatewayID), BootID: redactIfSet(session.bootID),
		LeaseEpoch: session.leaseEpoch, ChannelBinding: redactIfSet(session.challenge),
		InboundSequence: session.inboundSequence, OutboundSequence: session.outboundSequence,
		PendingOperations: len(session.pendingOperations),
	}
}

func (session *GatewaySession) MarshalJSON() ([]byte, error) {
	if session == nil {
		return json.Marshal(redactedGatewaySession{State: GatewaySessionClosed})
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return json.Marshal(session.redactedLocked())
}

func (session *GatewaySession) String() string {
	data, err := session.MarshalJSON()
	if err != nil {
		return "publicedge.GatewaySession{redacted}"
	}
	return string(data)
}

func (session *GatewaySession) GoString() string { return session.String() }
