package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxProviderHandleBytes bounds adapter-private dialog identifiers.
	MaxProviderHandleBytes = 256
	// MaxRecoveryTokenBytes bounds one adapter-private, persistable recovery token.
	MaxRecoveryTokenBytes = 2048
	// MaxMediaFrameBytes bounds one normalized encoded media frame.
	MaxMediaFrameBytes = 64 << 10
)

var (
	ErrClosed                = errors.New("sipgateway: closed")
	ErrInvalidIdentity       = errors.New("sipgateway: invalid identity")
	ErrInvalidEvent          = errors.New("sipgateway: invalid event")
	ErrInvalidCommand        = errors.New("sipgateway: invalid command")
	ErrCallNotFound          = errors.New("sipgateway: call not found")
	ErrStaleRevision         = errors.New("sipgateway: stale call revision")
	ErrBootEpochMismatch     = errors.New("sipgateway: boot epoch mismatch")
	ErrBootEpochStoreFull    = errors.New("sipgateway: boot epoch store full")
	ErrConcurrentCall        = errors.New("sipgateway: another call is already live")
	ErrMutationDisabled      = errors.New("sipgateway: mutation disabled by policy")
	ErrCapabilityUnavailable = errors.New("sipgateway: adapter capability unavailable")
	ErrWrongCallPhase        = errors.New("sipgateway: wrong call phase")
	ErrCommandConflict       = errors.New("sipgateway: command identifier conflict")
	ErrCommandStoreFull      = errors.New("sipgateway: command store full")
	ErrCommandOutcomeUnknown = errors.New("sipgateway: command outcome unknown")
	ErrRecoveryRequired      = errors.New("sipgateway: provider recovery required")
	ErrReconcileRequired     = errors.New("sipgateway: inspection and reconciliation required")
	ErrMediaNotPrepared      = errors.New("sipgateway: media not prepared")
	ErrMediaLeaseMismatch    = errors.New("sipgateway: media lease mismatch")
	ErrCodecMismatch         = errors.New("sipgateway: no mutually supported codec")
	ErrMediaClosed           = errors.New("sipgateway: media session closed")
	ErrMediaQueueFull        = errors.New("sipgateway: media queue full")
	ErrObservationQueueFull  = errors.New("sipgateway: observation queue full")
)

// CallRef is an adapter-private, exact reference to one provider dialog state.
// BootEpoch must be unique for every gateway process incarnation and must never
// be reused. ProviderHandle must never be exposed as a remote-client call
// identifier.
type CallRef struct {
	GatewayID      string `json:"-"`
	BootEpoch      string `json:"-"`
	ProviderHandle string `json:"-"`
	Revision       uint64 `json:"revision"`
}

func (CallRef) String() string   { return "sipgateway.CallRef{redacted}" }
func (CallRef) GoString() string { return "sipgateway.CallRef{redacted}" }

// Validate rejects partial and unbounded provider references.
func (r CallRef) Validate() error {
	if !validToken(r.GatewayID, 1, 64) ||
		!validOpaque(r.BootEpoch, 8, 128) ||
		!validOpaque(r.ProviderHandle, 1, MaxProviderHandleBytes) ||
		r.Revision == 0 {
		return ErrInvalidIdentity
	}
	return nil
}

// SameDialog compares immutable dialog identity while deliberately ignoring
// Revision.
func (r CallRef) SameDialog(other CallRef) bool {
	return r.GatewayID == other.GatewayID && r.BootEpoch == other.BootEpoch &&
		r.ProviderHandle == other.ProviderHandle
}

// PublicCallRef is the only call identity intended for remote clients.
type PublicCallRef struct {
	PublicCallID string `json:"public_call_id"`
	Generation   uint64 `json:"generation"`
}

// CallDirection identifies which side created a provider dialog. Empty values
// are treated as incoming for compatibility with the original incoming-only
// contract; new adapters should always set it explicitly.
type CallDirection string

const (
	CallDirectionIncoming CallDirection = "incoming"
	CallDirectionOutgoing CallDirection = "outgoing"
)

func (d CallDirection) normalized() CallDirection {
	if d == "" {
		return CallDirectionIncoming
	}
	return d
}

// ProviderCallState is the deliberately small state vocabulary accepted from
// adapters in the first incoming-call slice.
type ProviderCallState string

const (
	ProviderCallIncoming ProviderCallState = "incoming"
	ProviderCallActive   ProviderCallState = "active"
	ProviderCallEnded    ProviderCallState = "ended"
)

// CallSnapshot is an adapter-private authoritative observation.
type CallSnapshot struct {
	Ref       CallRef           `json:"-"`
	State     ProviderCallState `json:"state"`
	Direction CallDirection     `json:"direction,omitempty"`
}

// RecoveryToken is an opaque, adapter-private recovery capability. Its encoded
// bytes may be persisted locally, but must never be exposed as a remote-client
// identifier. Its formatting methods deliberately redact the token.
//
// MarshalBinary and UnmarshalBinary are the only supported persistence
// boundary. JSON encoding deliberately has no exported token field.
type RecoveryToken struct {
	encoded string
}

func (RecoveryToken) String() string   { return "sipgateway.RecoveryToken{redacted}" }
func (RecoveryToken) GoString() string { return "sipgateway.RecoveryToken{redacted}" }

// MarshalBinary returns a bounded copy of the encoded adapter token.
func (t RecoveryToken) MarshalBinary() ([]byte, error) {
	if !validOpaque(t.encoded, 1, MaxRecoveryTokenBytes) {
		return nil, ErrInvalidIdentity
	}
	return append([]byte(nil), t.encoded...), nil
}

// UnmarshalBinary imports a bounded encoded adapter token without interpreting
// provider identity. Only the provider that issued it can inspect it.
func (t *RecoveryToken) UnmarshalBinary(data []byte) error {
	if t == nil || !validOpaque(string(data), 1, MaxRecoveryTokenBytes) {
		return ErrInvalidIdentity
	}
	t.encoded = string(append([]byte(nil), data...))
	return nil
}

// RecoverySnapshot is the deliberately identity-free result of a read-only
// recovery inspection. A recovered call is not adopted into an Adapter and no
// new CallRef is issued.
type RecoverySnapshot struct {
	State ProviderCallState `json:"state"`
}

func (RecoverySnapshot) String() string   { return "sipgateway.RecoverySnapshot{redacted}" }
func (RecoverySnapshot) GoString() string { return "sipgateway.RecoverySnapshot{redacted}" }

// Validate accepts only the closed provider state vocabulary.
func (s RecoverySnapshot) Validate() error {
	switch s.State {
	case ProviderCallIncoming, ProviderCallActive, ProviderCallEnded:
		return nil
	default:
		return ErrInvalidEvent
	}
}

func (CallSnapshot) String() string   { return "sipgateway.CallSnapshot{redacted}" }
func (CallSnapshot) GoString() string { return "sipgateway.CallSnapshot{redacted}" }

// Validate checks both identity and the closed provider-state vocabulary.
func (s CallSnapshot) Validate() error {
	if err := s.Ref.Validate(); err != nil {
		return err
	}
	if direction := s.Direction.normalized(); direction != CallDirectionIncoming && direction != CallDirectionOutgoing {
		return ErrInvalidEvent
	}
	switch s.State {
	case ProviderCallIncoming, ProviderCallActive, ProviderCallEnded:
		return nil
	default:
		return ErrInvalidEvent
	}
}

// EventKind distinguishes call observations from an adapter boot epoch change.
type EventKind string

const (
	EventCallChanged    EventKind = "call_changed"
	EventGatewayRestart EventKind = "gateway_restart"
)

// Event is one adapter observation. Restart events carry GatewayID and a
// never-reused BootEpoch; call events carry Call instead.
type Event struct {
	Kind       EventKind     `json:"kind"`
	GatewayID  string        `json:"-"`
	BootEpoch  string        `json:"-"`
	Call       *CallSnapshot `json:"-"`
	ObservedAt time.Time     `json:"observed_at"`
}

func (Event) String() string   { return "sipgateway.Event{redacted}" }
func (Event) GoString() string { return "sipgateway.Event{redacted}" }

// Validate rejects mixed or incomplete event shapes.
func (e Event) Validate() error {
	if e.ObservedAt.IsZero() {
		return ErrInvalidEvent
	}
	switch e.Kind {
	case EventCallChanged:
		if e.GatewayID != "" || e.BootEpoch != "" || e.Call == nil {
			return ErrInvalidEvent
		}
		return e.Call.Validate()
	case EventGatewayRestart:
		if e.Call != nil || !validToken(e.GatewayID, 1, 64) || !validOpaque(e.BootEpoch, 8, 128) {
			return ErrInvalidEvent
		}
		return nil
	default:
		return ErrInvalidEvent
	}
}

// Codec identifies a negotiated RTP payload format without assuming a vendor.
type Codec struct {
	Name      string `json:"name"`
	ClockRate int    `json:"clock_rate"`
	Channels  int    `json:"channels"`
}

var (
	// CodecPCMU is G.711 mu-law, 8 kHz, mono.
	CodecPCMU = Codec{Name: "PCMU", ClockRate: 8000, Channels: 1}
	// CodecPCMA is G.711 A-law, 8 kHz, mono.
	CodecPCMA = Codec{Name: "PCMA", ClockRate: 8000, Channels: 1}
)

// Validate rejects ambiguous or excessive codec descriptions.
func (c Codec) Validate() error {
	if !validToken(c.Name, 1, 32) || c.ClockRate < 8000 || c.ClockRate > 192000 ||
		c.Channels < 1 || c.Channels > 2 {
		return ErrCodecMismatch
	}
	return nil
}

// Capabilities are runtime facts reported by an adapter. Coordinator policy is
// an independent gate and never becomes enabled merely because these are true.
type Capabilities struct {
	Incoming       bool    `json:"incoming"`
	Dial           bool    `json:"dial"`
	PrepareMedia   bool    `json:"prepare_media"`
	AnswerIncoming bool    `json:"answer_incoming"`
	EndActive      bool    `json:"end_active"`
	Codecs         []Codec `json:"codecs"`
}

// Validate requires explicit media and codec support for the incoming slice.
func (c Capabilities) Validate() error {
	if !c.Incoming || !c.PrepareMedia || len(c.Codecs) == 0 || len(c.Codecs) > 16 {
		return ErrCapabilityUnavailable
	}
	seen := make(map[Codec]struct{}, len(c.Codecs))
	for _, codec := range c.Codecs {
		if err := codec.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[codec]; duplicate {
			return ErrCodecMismatch
		}
		seen[codec] = struct{}{}
	}
	return nil
}

// MediaFrame is one bounded, encoded frame in the negotiated codec.
type MediaFrame struct {
	Sequence uint64 `json:"sequence"`
	Payload  []byte `json:"-"`
}

func (MediaFrame) String() string   { return "sipgateway.MediaFrame{redacted}" }
func (MediaFrame) GoString() string { return "sipgateway.MediaFrame{redacted}" }

// Validate rejects empty, unsequenced, or oversized frames.
func (f MediaFrame) Validate() error {
	if f.Sequence == 0 || len(f.Payload) == 0 || len(f.Payload) > MaxMediaFrameBytes {
		return ErrMediaNotPrepared
	}
	return nil
}

// MediaSnapshot contains only counters and timing needed for readiness. The
// activation baseline prevents early or queued frames from proving live media.
type MediaSnapshot struct {
	LeaseID                  string    `json:"lease_id"`
	Call                     CallRef   `json:"-"`
	Codec                    Codec     `json:"codec"`
	Prepared                 bool      `json:"prepared"`
	Closed                   bool      `json:"closed"`
	Activated                bool      `json:"activated"`
	ActivationEpoch          uint64    `json:"activation_epoch,omitempty"`
	ActivatedAt              time.Time `json:"activated_at,omitempty"`
	GatewayToClientFrames    uint64    `json:"gateway_to_client_frames"`
	ClientToGatewayFrames    uint64    `json:"client_to_gateway_frames"`
	GatewayToClientBaseline  uint64    `json:"gateway_to_client_baseline"`
	ClientToGatewayBaseline  uint64    `json:"client_to_gateway_baseline"`
	LastGatewayToClientAt    time.Time `json:"last_gateway_to_client_at,omitempty"`
	LastClientToGatewayAt    time.Time `json:"last_client_to_gateway_at,omitempty"`
	LastGatewayToClientEpoch uint64    `json:"last_gateway_to_client_epoch,omitempty"`
	LastClientToGatewayEpoch uint64    `json:"last_client_to_gateway_epoch,omitempty"`
}

func (MediaSnapshot) String() string   { return "sipgateway.MediaSnapshot{redacted}" }
func (MediaSnapshot) GoString() string { return "sipgateway.MediaSnapshot{redacted}" }

// GatewayToClientFresh proves new post-activation media in one direction.
func (s MediaSnapshot) GatewayToClientFresh(now time.Time, window time.Duration) bool {
	return s.Activated && !s.Closed && s.GatewayToClientFrames > s.GatewayToClientBaseline &&
		s.LastGatewayToClientEpoch == s.ActivationEpoch &&
		freshAfter(now, s.LastGatewayToClientAt, s.ActivatedAt, window)
}

// ClientToGatewayFresh proves new post-activation media in the other direction.
func (s MediaSnapshot) ClientToGatewayFresh(now time.Time, window time.Duration) bool {
	return s.Activated && !s.Closed && s.ClientToGatewayFrames > s.ClientToGatewayBaseline &&
		s.LastClientToGatewayEpoch == s.ActivationEpoch &&
		freshAfter(now, s.LastClientToGatewayAt, s.ActivatedAt, window)
}

// BidirectionalFresh requires both post-activation directions to be fresh.
func (s MediaSnapshot) BidirectionalFresh(now time.Time, window time.Duration) bool {
	return s.GatewayToClientFresh(now, window) && s.ClientToGatewayFresh(now, window)
}

// MediaSession is the vendor-neutral normalized frame boundary. ID is
// adapter-private and must not be exposed as a remote-client lease identifier.
// Read carries gateway-to-client frames; Write carries client-to-gateway
// frames. Reads and writes must honor context cancellation and use bounded
// queues. Activate must be idempotent when called again with the same epoch.
// Activate must perform no unbounded network wait and must be safe to run
// concurrently with Close. Close must be idempotent and promptly unblock every
// pending read, write, and activation operation.
type MediaSession interface {
	ID() string
	CallRef() CallRef
	Codec() Codec
	ReadGatewayFrame(context.Context) (MediaFrame, error)
	WriteGatewayFrame(context.Context, MediaFrame) error
	Activate(uint64) (MediaSnapshot, error)
	Snapshot() MediaSnapshot
	Close() error
}

// PrepareMediaRequest reserves media for one exact incoming revision.
type PrepareMediaRequest struct {
	Call   CallRef
	Codecs []Codec
}

func (PrepareMediaRequest) String() string   { return "sipgateway.PrepareMediaRequest{redacted}" }
func (PrepareMediaRequest) GoString() string { return "sipgateway.PrepareMediaRequest{redacted}" }

// CommandOutcome deliberately separates an explicit rejection from ambiguity.
type CommandOutcome string

const (
	CommandApplied  CommandOutcome = "applied"
	CommandRejected CommandOutcome = "rejected"
	CommandUnknown  CommandOutcome = "unknown"
)

// AnswerIncomingRequest is a one-shot, exact-revision answer intent.
type AnswerIncomingRequest struct {
	Call         CallRef
	CommandID    string
	MediaLeaseID string
}

func (AnswerIncomingRequest) String() string {
	return "sipgateway.AnswerIncomingRequest{redacted}"
}
func (AnswerIncomingRequest) GoString() string {
	return "sipgateway.AnswerIncomingRequest{redacted}"
}

// EndActiveRequest is a one-shot, exact-revision active-dialog end intent.
type EndActiveRequest struct {
	Call      CallRef
	CommandID string
}

func (EndActiveRequest) String() string   { return "sipgateway.EndActiveRequest{redacted}" }
func (EndActiveRequest) GoString() string { return "sipgateway.EndActiveRequest{redacted}" }

// CommandResult never treats an ambiguous transport result as success.
type CommandResult struct {
	CommandID string         `json:"command_id"`
	Outcome   CommandOutcome `json:"outcome"`
	Current   *CallSnapshot  `json:"-"`
}

func (CommandResult) String() string   { return "sipgateway.CommandResult{redacted}" }
func (CommandResult) GoString() string { return "sipgateway.CommandResult{redacted}" }

// Adapter is the minimum provider boundary. It deliberately excludes dialing,
// rejection, tones, messaging, management, and provider configuration. Every
// context-taking method must honor cancellation. Close must promptly unblock
// every pending method and must be safe to call concurrently with them;
// implementations must not hide unbounded queues.
type Adapter interface {
	Capabilities() Capabilities
	Observe(context.Context) (Event, error)
	Inspect(context.Context, CallRef) (CallSnapshot, error)
	PrepareMedia(context.Context, PrepareMediaRequest) (MediaSession, error)
	AnswerIncoming(context.Context, AnswerIncomingRequest) (CommandResult, error)
	EndActive(context.Context, EndActiveRequest) (CommandResult, error)
	Close() error
}

// CallDialer is an optional provider boundary for an explicitly configured
// outbound trunk. Dial returns only after the provider has admitted the new
// dialog; the normal Observe stream then owns its public lifecycle. It is not
// part of Adapter so existing incoming-only providers remain unchanged.
type CallDialer interface {
	Dial(context.Context, string) (CallSnapshot, error)
}

// RecoveryInspector is an optional, read-only recovery boundary. Callers must
// fail closed when it is unavailable. InspectRecovery must not adopt the
// recovered dialog or perform any provider mutation.
type RecoveryInspector interface {
	InspectRecovery(context.Context, RecoveryToken) (RecoverySnapshot, error)
	Close() error
}

// RecoveryAdapter extends a live Adapter with recovery-token issuance and
// read-only inspection. RecoveryToken may only be issued for a locally
// observed immutable dialog identity.
type RecoveryAdapter interface {
	Adapter
	RecoveryInspector
	RecoveryToken(CallRef) (RecoveryToken, error)
}

func validToken(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validDialNumber(value string) bool {
	if len(value) == 0 || len(value) > 32 || value != strings.TrimSpace(value) {
		return false
	}
	for index, char := range value {
		if index == 0 && char == '+' {
			continue
		}
		if !(char >= '0' && char <= '9') && char != '*' && char != '#' {
			return false
		}
	}
	return value != "+"
}

func validOpaque(value string, min, max int) bool {
	if value != strings.TrimSpace(value) || len(value) < min || len(value) > max {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

func validCommandID(value string) bool { return validOpaque(value, 8, 128) }

func freshAfter(now, event, activation time.Time, window time.Duration) bool {
	if window <= 0 || event.IsZero() || activation.IsZero() || event.Before(activation) || event.After(now) {
		return false
	}
	return now.Sub(event) <= window
}

func cloneCodecs(codecs []Codec) []Codec { return append([]Codec(nil), codecs...) }

func cloneCallSnapshot(snapshot CallSnapshot) *CallSnapshot {
	copy := snapshot
	return &copy
}

func validateRequestedCodecs(codecs []Codec) error {
	if len(codecs) == 0 || len(codecs) > 16 {
		return ErrCodecMismatch
	}
	seen := make(map[Codec]struct{}, len(codecs))
	for _, codec := range codecs {
		if err := codec.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[codec]; duplicate {
			return fmt.Errorf("%w: duplicate codec", ErrCodecMismatch)
		}
		seen[codec] = struct{}{}
	}
	return nil
}
