// Package publicedge defines the fail-closed wire contract for the optional
// public control edge. It deliberately contains no HTTP proxy, command
// dispatcher, WebSocket implementation, or device mutation.
package publicedge

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ProtocolVersion = 1
	DefaultEnabled  = false

	MaxFrameBytes       = 64 << 10
	MaxPayloadBytes     = 48 << 10
	MaxGatewayIDBytes   = 64
	MaxDeviceIDBytes    = 96
	MaxPrincipalBytes   = 256
	MaxOperationIDBytes = 128
	MaxBootIDBytes      = 96
	MaxPathBytes        = 192
	MaxSignatureBytes   = 96
	// MaxWireCounter stays exactly representable by JSON implementations that
	// use IEEE-754 numbers, including browser clients.
	MaxWireCounter = uint64(1<<53 - 1)

	DefaultMessageTTL   = 2 * time.Minute
	DefaultChallengeTTL = 30 * time.Second
	MaxObservedAge      = 5 * time.Minute
	MaxClockSkew        = 5 * time.Second
	MaxPendingProbes    = 128
	MaxJSONDepth        = 16
)

type FrameType string

const (
	FrameGatewayHello        FrameType = "gateway.hello"
	FrameEdgeChallenge       FrameType = "edge.challenge"
	FrameGatewayAuthenticate FrameType = "gateway.authenticate"
	FrameEdgeAuthenticated   FrameType = "edge.authenticated"
	FrameStateSnapshot       FrameType = "state.snapshot"
	FrameStateEvent          FrameType = "state.event"
	FrameOperationProbe      FrameType = "operation.probe"
	FrameOperationResult     FrameType = "operation.result"
	GatewayControlPath                 = "/publicedge/v1/gateway"
	SnapshotPath                       = "/publicedge/v1/state/snapshot"
	EventPath                          = "/publicedge/v1/state/event"
	OperationProbePath                 = "/publicedge/v1/operations/probe"
	OperationResultPath                = "/publicedge/v1/operations/result"
)

const (
	actionGatewayConnect      = "gateway.connect"
	actionGatewayAuthenticate = "gateway.authenticate"
	actionStateSnapshot       = "state.snapshot.publish"
	actionStateEvent          = "state.event.publish"
	actionOperationProbe      = "operation.probe"
	actionOperationResult     = "operation.result.publish"
	methodWSS                 = "WSS"
	methodGET                 = "GET"
)

var (
	ErrMalformed          = errors.New("public edge frame is malformed")
	ErrUnsupportedFrame   = errors.New("public edge frame type is unsupported")
	ErrExpired            = errors.New("public edge frame has expired")
	ErrUnauthorized       = errors.New("public edge proof is unauthorized")
	ErrProtocolViolation  = errors.New("public edge protocol violation")
	ErrGatewayBusy        = errors.New("public edge gateway already has an active lease")
	ErrClosed             = errors.New("public edge session is closed")
	ErrSequence           = errors.New("public edge sequence is replayed or out of order")
	ErrOperationNotProbed = errors.New("public edge operation was not probed")
)

// Frame is the only wire envelope. Payload is hashed byte-for-byte; callers
// must not decode and re-encode it between verification and use.
//
// MarshalJSON is intentionally redacted. Use EncodeFrame for wire output.
type Frame struct {
	Version     uint64          `json:"version"`
	Type        FrameType       `json:"type"`
	GatewayID   string          `json:"gateway_id"`
	DeviceID    string          `json:"device_id,omitempty"`
	Principal   string          `json:"principal,omitempty"`
	Action      string          `json:"action"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	BodySHA256  string          `json:"body_sha256"`
	OperationID string          `json:"operation_id,omitempty"`
	ExpiresAt   int64           `json:"expires_at"`
	BootID      string          `json:"boot_id"`
	LeaseEpoch  uint64          `json:"lease_epoch"`
	Sequence    uint64          `json:"sequence"`
	Payload     json.RawMessage `json:"payload"`
	Signature   string          `json:"signature,omitempty"`
}

// FrameMeta contains the common, non-payload fields needed by NewFrame.
type FrameMeta struct {
	GatewayID   string
	DeviceID    string
	Principal   string
	OperationID string
	ExpiresAt   int64
	BootID      string
	LeaseEpoch  uint64
	Sequence    uint64
}

type frameProfile struct {
	action          string
	method          string
	path            string
	signedByGateway bool
	signedByClient  bool
	needsEpoch      bool
	needsSequence   bool
	needsOperation  bool
}

var frameProfiles = map[FrameType]frameProfile{
	FrameGatewayHello: {
		action: actionGatewayConnect, method: methodWSS, path: GatewayControlPath,
	},
	FrameEdgeChallenge: {
		action: actionGatewayConnect, method: methodWSS, path: GatewayControlPath,
		needsEpoch: true,
	},
	FrameGatewayAuthenticate: {
		action: actionGatewayAuthenticate, method: methodWSS, path: GatewayControlPath,
		signedByGateway: true, needsEpoch: true,
	},
	FrameEdgeAuthenticated: {
		action: actionGatewayAuthenticate, method: methodWSS, path: GatewayControlPath,
		needsEpoch: true,
	},
	FrameStateSnapshot: {
		action: actionStateSnapshot, method: methodWSS, path: SnapshotPath,
		signedByGateway: true, needsEpoch: true, needsSequence: true,
	},
	FrameStateEvent: {
		action: actionStateEvent, method: methodWSS, path: EventPath,
		signedByGateway: true, needsEpoch: true, needsSequence: true,
	},
	FrameOperationProbe: {
		action: actionOperationProbe, method: methodGET, path: OperationProbePath,
		signedByClient: true, needsEpoch: true, needsSequence: true, needsOperation: true,
	},
	FrameOperationResult: {
		action: actionOperationResult, method: methodWSS, path: OperationResultPath,
		signedByGateway: true, needsEpoch: true, needsSequence: true, needsOperation: true,
	},
}

var (
	gatewayIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)
	opaqueIDPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._:-]{0,126}[A-Za-z0-9])?$`)
	principalPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9@._:/+=%-]{0,254}[A-Za-z0-9])?$`)
	codePattern      = regexp.MustCompile(`^[a-z](?:[a-z0-9_.-]{0,62}[a-z0-9])?$`)
)

var frameJSONKeys = exactKeySet(
	"version", "type", "gateway_id", "device_id", "principal", "action", "method", "path",
	"body_sha256", "operation_id", "expires_at", "boot_id", "lease_epoch", "sequence", "payload", "signature",
)

var payloadJSONKeys = map[FrameType]map[string]struct{}{
	FrameGatewayHello:        exactKeySet(),
	FrameEdgeChallenge:       exactKeySet("challenge"),
	FrameGatewayAuthenticate: exactKeySet("challenge"),
	FrameEdgeAuthenticated:   exactKeySet(),
	FrameStateSnapshot: exactKeySet(
		"observed_at", "revision", "event_high_water", "service", "call", "sms",
	),
	FrameStateEvent:      exactKeySet("observed_at", "revision", "event_sequence", "kind"),
	FrameOperationProbe:  exactKeySet(),
	FrameOperationResult: exactKeySet("observed_at", "state", "final", "code"),
}

// NewFrame creates a structurally valid, unsigned frame with a canonical JSON
// payload. Signed frame kinds must subsequently use SignGatewayFrame or
// SignClientFrame.
func NewFrame(kind FrameType, meta FrameMeta, payload any) (Frame, error) {
	profile, ok := frameProfiles[kind]
	if !ok {
		return Frame{}, ErrUnsupportedFrame
	}
	payloadBytes, err := marshalWirePayload(payload)
	if err != nil {
		return Frame{}, err
	}
	frame := Frame{
		Version: ProtocolVersion, Type: kind, GatewayID: meta.GatewayID,
		DeviceID: meta.DeviceID, Principal: meta.Principal,
		Action: profile.action, Method: profile.method, Path: profile.path,
		BodySHA256: payloadDigest(payloadBytes), OperationID: meta.OperationID,
		ExpiresAt: meta.ExpiresAt, BootID: meta.BootID, LeaseEpoch: meta.LeaseEpoch,
		Sequence: meta.Sequence, Payload: payloadBytes,
	}
	if err := validateFrame(frame, false); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func marshalWirePayload(payload any) ([]byte, error) {
	if payload == nil {
		return nil, fmt.Errorf("%w: payload is required", ErrMalformed)
	}
	var wirePayload any = payload
	switch typed := payload.(type) {
	case ChallengePayload:
		wirePayload = challengePayloadWire(typed)
	case *ChallengePayload:
		if typed == nil {
			return nil, fmt.Errorf("%w: payload is required", ErrMalformed)
		}
		wirePayload = challengePayloadWire(*typed)
	case GatewayAuthenticatePayload:
		wirePayload = gatewayAuthenticatePayloadWire(typed)
	case *GatewayAuthenticatePayload:
		if typed == nil {
			return nil, fmt.Errorf("%w: payload is required", ErrMalformed)
		}
		wirePayload = gatewayAuthenticatePayloadWire(*typed)
	}
	data, err := json.Marshal(wirePayload)
	if err != nil {
		return nil, fmt.Errorf("%w: encode payload", ErrMalformed)
	}
	if len(data) == 0 || len(data) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload size", ErrMalformed)
	}
	return data, nil
}

// EncodeFrame is the only API that emits an unredacted wire envelope.
func EncodeFrame(frame Frame) ([]byte, error) {
	if err := validateFrame(frame, true); err != nil {
		return nil, err
	}
	data, err := json.Marshal(wireFrame(frame))
	if err != nil {
		return nil, fmt.Errorf("%w: encode frame", ErrMalformed)
	}
	if len(data) > MaxFrameBytes {
		return nil, fmt.Errorf("%w: frame size", ErrMalformed)
	}
	return data, nil
}

// DecodeFrame accepts one bounded JSON object, rejects duplicate or unknown
// fields at every typed level, and verifies the exact payload hash.
func DecodeFrame(data []byte) (Frame, error) {
	if len(data) == 0 || len(data) > MaxFrameBytes {
		return Frame{}, fmt.Errorf("%w: frame size", ErrMalformed)
	}
	if err := validateNoDuplicateJSONKeys(data, MaxJSONDepth); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := validateExactObjectKeys(data, frameJSONKeys); err != nil {
		return Frame{}, fmt.Errorf("%w: frame keys: %v", ErrMalformed, err)
	}
	var wire wireFrame
	if err := decodeStrictJSON(data, &wire); err != nil {
		return Frame{}, fmt.Errorf("%w: frame JSON", ErrMalformed)
	}
	frame := Frame(wire)
	if err := validateFrame(frame, true); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

type wireFrame Frame

func validateFrame(frame Frame, requireSignature bool) error {
	profile, ok := frameProfiles[frame.Type]
	if !ok {
		return ErrUnsupportedFrame
	}
	if frame.Version != ProtocolVersion || !validGatewayID(frame.GatewayID) ||
		!validOpaqueID(frame.BootID, MaxBootIDBytes) || frame.ExpiresAt <= 0 ||
		uint64(frame.ExpiresAt) > MaxWireCounter {
		return fmt.Errorf("%w: required envelope field", ErrMalformed)
	}
	if frame.Action != profile.action || frame.Method != profile.method || frame.Path != profile.path ||
		len(frame.Path) > MaxPathBytes || !utf8.ValidString(frame.Path) {
		return fmt.Errorf("%w: action, method, or exact path", ErrMalformed)
	}
	if len(frame.Payload) == 0 || len(frame.Payload) > MaxPayloadBytes ||
		payloadDigest(frame.Payload) != frame.BodySHA256 || !validDigest(frame.BodySHA256) {
		return fmt.Errorf("%w: payload or body digest", ErrMalformed)
	}
	if profile.needsEpoch != (frame.LeaseEpoch > 0) || profile.needsSequence != (frame.Sequence > 0) ||
		profile.needsOperation != (frame.OperationID != "") || frame.LeaseEpoch > MaxWireCounter ||
		frame.Sequence > MaxWireCounter {
		return fmt.Errorf("%w: role-specific epoch, sequence, or operation", ErrMalformed)
	}
	if frame.OperationID != "" && !validOpaqueID(frame.OperationID, MaxOperationIDBytes) {
		return fmt.Errorf("%w: operation id", ErrMalformed)
	}
	if profile.signedByClient {
		if (frame.DeviceID == "") == (frame.Principal == "") {
			return fmt.Errorf("%w: exactly one client identity is required", ErrMalformed)
		}
		if frame.DeviceID != "" && !validOpaqueID(frame.DeviceID, MaxDeviceIDBytes) {
			return fmt.Errorf("%w: device id", ErrMalformed)
		}
		if frame.Principal != "" && !validPrincipal(frame.Principal) {
			return fmt.Errorf("%w: principal", ErrMalformed)
		}
	} else if frame.DeviceID != "" || frame.Principal != "" {
		return fmt.Errorf("%w: client identity on non-client frame", ErrMalformed)
	}
	needsSignature := profile.signedByGateway || profile.signedByClient
	if requireSignature && needsSignature && !validSignatureEncoding(frame.Signature) {
		return fmt.Errorf("%w: signature", ErrMalformed)
	}
	if !needsSignature && frame.Signature != "" {
		return fmt.Errorf("%w: unexpected signature", ErrMalformed)
	}
	if !requireSignature && needsSignature && frame.Signature != "" && !validSignatureEncoding(frame.Signature) {
		return fmt.Errorf("%w: signature", ErrMalformed)
	}
	if err := validatePayload(frame); err != nil {
		return err
	}
	return nil
}

func validatePayload(frame Frame) error {
	trimmed := bytes.TrimSpace(frame.Payload)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return fmt.Errorf("%w: payload must be a JSON object", ErrMalformed)
	}
	if err := validateNoDuplicateJSONKeys(frame.Payload, MaxJSONDepth); err != nil {
		return fmt.Errorf("%w: payload JSON: %v", ErrMalformed, err)
	}
	allowedKeys, ok := payloadJSONKeys[frame.Type]
	if !ok {
		return ErrUnsupportedFrame
	}
	if err := validateExactObjectKeys(frame.Payload, allowedKeys); err != nil {
		return fmt.Errorf("%w: payload keys: %v", ErrMalformed, err)
	}
	var payloadValidatable interface{ Validate() error }
	switch frame.Type {
	case FrameGatewayHello:
		payloadValidatable = &GatewayHelloPayload{}
	case FrameEdgeChallenge:
		payloadValidatable = &ChallengePayload{}
	case FrameGatewayAuthenticate:
		payloadValidatable = &GatewayAuthenticatePayload{}
	case FrameEdgeAuthenticated:
		payloadValidatable = &AuthenticatedPayload{}
	case FrameStateSnapshot:
		payloadValidatable = &SnapshotPayload{}
	case FrameStateEvent:
		payloadValidatable = &EventPayload{}
	case FrameOperationProbe:
		payloadValidatable = &OperationProbePayload{}
	case FrameOperationResult:
		payloadValidatable = &OperationResultPayload{}
	default:
		return ErrUnsupportedFrame
	}
	if err := decodeStrictJSON(frame.Payload, payloadValidatable); err != nil {
		return fmt.Errorf("%w: payload JSON", ErrMalformed)
	}
	if err := payloadValidatable.Validate(); err != nil {
		return fmt.Errorf("%w: payload: %v", ErrMalformed, err)
	}
	canonical, err := marshalWirePayload(payloadValidatable)
	if err != nil || !bytes.Equal(canonical, frame.Payload) {
		return fmt.Errorf("%w: payload is not canonical", ErrMalformed)
	}
	return nil
}

func validateAt(frame Frame, now time.Time, maxTTL time.Duration) error {
	if now.IsZero() || maxTTL <= 0 {
		return fmt.Errorf("%w: invalid time policy", ErrProtocolViolation)
	}
	expires := time.Unix(frame.ExpiresAt, 0)
	if !expires.After(now) {
		return ErrExpired
	}
	if expires.After(now.Add(maxTTL)) {
		return fmt.Errorf("%w: expiry exceeds maximum TTL", ErrProtocolViolation)
	}
	return nil
}

func payloadDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validGatewayID(value string) bool {
	return len(value) <= MaxGatewayIDBytes && gatewayIDPattern.MatchString(value)
}

func validOpaqueID(value string, limit int) bool {
	return len(value) > 0 && len(value) <= limit && opaqueIDPattern.MatchString(value)
}

func validPrincipal(value string) bool {
	return len(value) > 0 && len(value) <= MaxPrincipalBytes && utf8.ValidString(value) &&
		strings.Contains(value, ":") && principalPattern.MatchString(value)
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func exactKeySet(keys ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		result[key] = struct{}{}
	}
	return result
}

// validateExactObjectKeys closes encoding/json's case-insensitive struct-field
// matching. Duplicate-key validation runs separately first, so every decoded
// key here represents exactly one source key.
func validateExactObjectKeys(data []byte, allowed map[string]struct{}) error {
	var object map[string]json.RawMessage
	if err := decodeStrictJSON(data, &object); err != nil || object == nil {
		return errors.New("JSON value is not an object")
	}
	for key := range object {
		if _, ok := allowed[key]; !ok {
			return errors.New("unknown or non-canonical JSON key")
		}
	}
	return nil
}

func validateNoDuplicateJSONKeys(data []byte, maxDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkJSONValue(decoder, 0, maxDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return errors.New("JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func validSignatureEncoding(value string) bool {
	if len(value) == 0 || len(value) > MaxSignatureBytes {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) >= 8 && len(decoded) <= 72 &&
		base64.RawURLEncoding.EncodeToString(decoded) == value
}
