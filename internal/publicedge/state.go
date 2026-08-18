package publicedge

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Handshake payloads intentionally contain no extensible bag of fields.
// Protocol expansion requires a new version or frame kind.
type GatewayHelloPayload struct{}

func (*GatewayHelloPayload) Validate() error { return nil }

type ChallengePayload struct {
	Challenge string `json:"challenge"`
}

type challengePayloadWire ChallengePayload

func (p *ChallengePayload) Validate() error {
	if !validChallenge(p.Challenge) {
		return errors.New("invalid challenge")
	}
	return nil
}

func (p ChallengePayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Challenge string `json:"challenge"`
	}{Challenge: redactIfSet(p.Challenge)})
}

func (p ChallengePayload) String() string { return `{"challenge":"[redacted]"}` }

func (p ChallengePayload) GoString() string { return p.String() }

type GatewayAuthenticatePayload struct {
	Challenge string `json:"challenge"`
}

type gatewayAuthenticatePayloadWire GatewayAuthenticatePayload

func (p *GatewayAuthenticatePayload) Validate() error {
	if !validChallenge(p.Challenge) {
		return errors.New("invalid challenge")
	}
	return nil
}

func (p GatewayAuthenticatePayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Challenge string `json:"challenge"`
	}{Challenge: redactIfSet(p.Challenge)})
}

func (p GatewayAuthenticatePayload) String() string { return `{"challenge":"[redacted]"}` }

func (p GatewayAuthenticatePayload) GoString() string { return p.String() }

type AuthenticatedPayload struct{}

func (*AuthenticatedPayload) Validate() error { return nil }

type ServiceState string

const (
	ServiceStarting         ServiceState = "starting"
	ServiceReady            ServiceState = "ready"
	ServiceDegraded         ServiceState = "degraded"
	ServiceRecoveryRequired ServiceState = "recovery_required"
)

type CallState string

const (
	CallUnavailable      CallState = "unavailable"
	CallIdle             CallState = "idle"
	CallRinging          CallState = "ringing"
	CallConnecting       CallState = "connecting"
	CallActive           CallState = "active"
	CallRecoveryRequired CallState = "recovery_required"
)

type SMSState string

const (
	SMSUnavailable SMSState = "unavailable"
	SMSReady       SMSState = "ready"
	SMSDegraded    SMSState = "degraded"
)

// SnapshotPayload deliberately exposes only coarse, read-only state and
// monotonically increasing watermarks. It has no command, URL, headers, raw
// body, phone number, message text, or media field.
type SnapshotPayload struct {
	ObservedAt     int64        `json:"observed_at"`
	Revision       uint64       `json:"revision"`
	EventHighWater uint64       `json:"event_high_water"`
	Service        ServiceState `json:"service"`
	Call           CallState    `json:"call"`
	SMS            SMSState     `json:"sms"`
}

func (p *SnapshotPayload) Validate() error {
	if p.ObservedAt <= 0 || uint64(p.ObservedAt) > MaxWireCounter || p.Revision == 0 ||
		p.Revision > MaxWireCounter || p.EventHighWater > MaxWireCounter {
		return errors.New("snapshot timestamp and revision are required")
	}
	if !validServiceState(p.Service) || !validCallState(p.Call) || !validSMSState(p.SMS) {
		return errors.New("snapshot state is invalid")
	}
	return nil
}

type EventKind string

const (
	EventHeartbeat        EventKind = "gateway.heartbeat"
	EventStateChanged     EventKind = "state.changed"
	EventOperationChanged EventKind = "operation.changed"
	EventRecoveryRequired EventKind = "gateway.recovery_required"
)

// EventPayload is a notification that state should be re-read. It contains no
// arbitrary event body and therefore cannot become a mutation tunnel.
type EventPayload struct {
	ObservedAt    int64     `json:"observed_at"`
	Revision      uint64    `json:"revision"`
	EventSequence uint64    `json:"event_sequence"`
	Kind          EventKind `json:"kind"`
}

func (p *EventPayload) Validate() error {
	if p.ObservedAt <= 0 || uint64(p.ObservedAt) > MaxWireCounter || p.Revision == 0 || p.Revision > MaxWireCounter ||
		p.EventSequence == 0 || p.EventSequence > MaxWireCounter || !validEventKind(p.Kind) {
		return errors.New("event fields are invalid")
	}
	return nil
}

// OperationProbePayload is intentionally empty. The signed envelope carries
// the sole opaque operation id.
type OperationProbePayload struct{}

func (*OperationProbePayload) Validate() error { return nil }

type OperationState string

const (
	OperationPending          OperationState = "pending"
	OperationSucceeded        OperationState = "succeeded"
	OperationFailed           OperationState = "failed"
	OperationUnknown          OperationState = "unknown"
	OperationRecoveryRequired OperationState = "recovery_required"
)

// OperationResultPayload reports metadata for an already-existing operation.
// It cannot request, describe, or retry a mutation.
type OperationResultPayload struct {
	ObservedAt int64          `json:"observed_at"`
	State      OperationState `json:"state"`
	Final      bool           `json:"final"`
	Code       string         `json:"code,omitempty"`
}

func (p *OperationResultPayload) Validate() error {
	if p.ObservedAt <= 0 || uint64(p.ObservedAt) > MaxWireCounter || !validOperationState(p.State) {
		return errors.New("operation result is invalid")
	}
	wantFinal := p.State == OperationSucceeded || p.State == OperationFailed
	if p.Final != wantFinal {
		return errors.New("operation final flag does not match state")
	}
	if p.Code != "" && (len(p.Code) > 64 || !codePattern.MatchString(p.Code)) {
		return errors.New("operation result code is invalid")
	}
	return nil
}

func validateObservedAt(observedAt int64, now time.Time) error {
	observed := time.Unix(observedAt, 0)
	if observed.Before(now.Add(-MaxObservedAge)) || observed.After(now.Add(MaxClockSkew)) {
		return fmt.Errorf("%w: observed time is outside the allowed window", ErrProtocolViolation)
	}
	return nil
}

func frameObservedAt(frame Frame) (int64, bool, error) {
	switch frame.Type {
	case FrameStateSnapshot:
		var payload SnapshotPayload
		if err := decodeStrictJSON(frame.Payload, &payload); err != nil {
			return 0, false, err
		}
		return payload.ObservedAt, true, nil
	case FrameStateEvent:
		var payload EventPayload
		if err := decodeStrictJSON(frame.Payload, &payload); err != nil {
			return 0, false, err
		}
		return payload.ObservedAt, true, nil
	case FrameOperationResult:
		var payload OperationResultPayload
		if err := decodeStrictJSON(frame.Payload, &payload); err != nil {
			return 0, false, err
		}
		return payload.ObservedAt, true, nil
	default:
		return 0, false, nil
	}
}

func validServiceState(value ServiceState) bool {
	switch value {
	case ServiceStarting, ServiceReady, ServiceDegraded, ServiceRecoveryRequired:
		return true
	default:
		return false
	}
}

func validCallState(value CallState) bool {
	switch value {
	case CallUnavailable, CallIdle, CallRinging, CallConnecting, CallActive, CallRecoveryRequired:
		return true
	default:
		return false
	}
}

func validSMSState(value SMSState) bool {
	switch value {
	case SMSUnavailable, SMSReady, SMSDegraded:
		return true
	default:
		return false
	}
}

func validEventKind(value EventKind) bool {
	switch value {
	case EventHeartbeat, EventStateChanged, EventOperationChanged, EventRecoveryRequired:
		return true
	default:
		return false
	}
}

func validOperationState(value OperationState) bool {
	switch value {
	case OperationPending, OperationSucceeded, OperationFailed, OperationUnknown, OperationRecoveryRequired:
		return true
	default:
		return false
	}
}
