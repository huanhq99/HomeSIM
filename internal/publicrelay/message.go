package publicrelay

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"
)

const (
	MaxSMSDestinationBytes = 32
	MaxSMSMessageBytes     = 4096
	maxInnerJSONBytes      = 32760
	paddingHeaderBytes     = 8
)

var paddingBuckets = [...]int{256, 512, 1024, 2048, 4096, 8192, 16384, 32768}

var (
	paddingMagic      = [4]byte{'D', 'J', 'R', '2'}
	destinationRegexp = regexp.MustCompile(`^\+?[0-9]{1,31}$`)
)

// MessageKind is an allowlisted encrypted action/result. It never appears in
// the outer Envelope.
type MessageKind string

const (
	MessageSMSSend                  MessageKind = "sms.send"
	MessageSMSInspectAndFence       MessageKind = "sms.inspect_and_fence"
	MessageSMSInspectAndFenceResult MessageKind = "sms.inspect_and_fence.result"
)

// InspectAndFenceOutcome deliberately has no "not_found" value. Only an
// exact, durably journaled FencedAbsent result represents the absent-and-now-
// permanently-fenced state. This package defines that contract but does not
// perform the required store lock, append, fsync, or modem serialization.
type InspectAndFenceOutcome string

const (
	OperationPending      InspectAndFenceOutcome = "pending"
	OperationApplied      InspectAndFenceOutcome = "applied"
	OperationFailed       InspectAndFenceOutcome = "failed"
	OperationUnknown      InspectAndFenceOutcome = "unknown"
	OperationFencedAbsent InspectAndFenceOutcome = "fenced_absent"
)

// SMSSendIntent is the exact plaintext committed before a send may leave the
// device. Fields are byte-preserving: neither destination nor message is
// normalized before commitment or encryption.
type SMSSendIntent struct {
	OperationID string
	Destination string
	Message     string
}

// SMSInspectAndFenceRequest asks the gateway to inspect an operation or,
// while holding the same durable operation lock used by send, persist a
// permanent fenced_absent tombstone before replying.
type SMSInspectAndFenceRequest struct {
	operationID string
	commitment  Commitment
	valid       bool
}

// SMSInspectAndFenceResult is a pure wire contract. Constructing this value is
// not evidence that the gateway performed durable fencing.
type SMSInspectAndFenceResult struct {
	operationID string
	commitment  Commitment
	outcome     InspectAndFenceOutcome
	valid       bool
}

// NewSMSInspectAndFenceRequest constructs a non-zero typed recovery request.
func NewSMSInspectAndFenceRequest(
	operationID string,
	commitment Commitment,
) (SMSInspectAndFenceRequest, error) {
	request := SMSInspectAndFenceRequest{
		operationID: operationID, commitment: commitment, valid: true,
	}
	if err := validateInspectRequest(request); err != nil {
		return SMSInspectAndFenceRequest{}, err
	}
	return request, nil
}

// OperationID explicitly releases the opaque identifier for the durable
// operation journal. Generic formatting remains redacted.
func (request SMSInspectAndFenceRequest) OperationID() (string, bool) {
	if validateInspectRequest(request) != nil {
		return "", false
	}
	return request.operationID, true
}

// OperationCommitment returns the keyed send commitment.
func (request SMSInspectAndFenceRequest) OperationCommitment() (Commitment, bool) {
	if validateInspectRequest(request) != nil {
		return Commitment{}, false
	}
	return request.commitment, true
}

// NewSMSInspectAndFenceResult constructs a non-zero result contract. It does
// not prove that fencing was durably performed.
func NewSMSInspectAndFenceResult(
	operationID string,
	commitment Commitment,
	outcome InspectAndFenceOutcome,
) (SMSInspectAndFenceResult, error) {
	result := SMSInspectAndFenceResult{
		operationID: operationID, commitment: commitment, outcome: outcome, valid: true,
	}
	if err := validateInspectResult(result); err != nil {
		return SMSInspectAndFenceResult{}, err
	}
	return result, nil
}

// Commitment is HMAC-SHA256 over the typed canonical SMS send intent. Its
// default formatting and JSON forms are redacted; Encode is the explicit wire
// conversion used inside encrypted message bodies.
type Commitment struct{ value [commitmentBytes]byte }

// ParseCommitment accepts one canonical unpadded base64url-encoded 256-bit
// commitment.
func ParseCommitment(encoded string) (Commitment, error) {
	decoded, err := decodeCanonicalBase64(encoded, commitmentBytes)
	if err != nil || len(decoded) != commitmentBytes {
		return Commitment{}, ErrMalformed
	}
	var commitment Commitment
	copy(commitment.value[:], decoded)
	return commitment, nil
}

// Encode explicitly releases the commitment for inclusion inside encrypted
// protocol data. It should not be used for logging.
func (commitment Commitment) Encode() string {
	return base64.RawURLEncoding.EncodeToString(commitment.value[:])
}

// Equal compares commitments without data-dependent early exit.
func (commitment Commitment) Equal(other Commitment) bool {
	return subtle.ConstantTimeCompare(commitment.value[:], other.value[:]) == 1
}

// Message is an allowlisted typed inner value. Generic byte/string payloads
// cannot be constructed or signed through this API.
type Message struct {
	kind    MessageKind
	send    *SMSSendIntent
	inspect *SMSInspectAndFenceRequest
	result  *SMSInspectAndFenceResult
}

// NewSMSSendMessage constructs the only currently defined SMS mutation
// plaintext. This does not authorize or execute a send.
func NewSMSSendMessage(intent SMSSendIntent) (Message, error) {
	if err := validateSMSSendIntent(intent); err != nil {
		return Message{}, err
	}
	copy := intent
	return Message{kind: MessageSMSSend, send: &copy}, nil
}

// NewSMSInspectAndFenceMessage constructs the typed recovery request.
func NewSMSInspectAndFenceMessage(request SMSInspectAndFenceRequest) (Message, error) {
	if err := validateInspectRequest(request); err != nil {
		return Message{}, err
	}
	copy := request
	return Message{kind: MessageSMSInspectAndFence, inspect: &copy}, nil
}

// NewSMSInspectAndFenceResultMessage constructs the typed gateway result.
func NewSMSInspectAndFenceResultMessage(result SMSInspectAndFenceResult) (Message, error) {
	if err := validateInspectResult(result); err != nil {
		return Message{}, err
	}
	copy := result
	return Message{kind: MessageSMSInspectAndFenceResult, result: &copy}, nil
}

func (message Message) Kind() MessageKind { return message.kind }

func (message Message) SMSSend() (SMSSendIntent, bool) {
	if message.send == nil || message.kind != MessageSMSSend {
		return SMSSendIntent{}, false
	}
	return *message.send, true
}

func (message Message) SMSInspectAndFence() (SMSInspectAndFenceRequest, bool) {
	if message.inspect == nil || message.kind != MessageSMSInspectAndFence {
		return SMSInspectAndFenceRequest{}, false
	}
	return *message.inspect, true
}

func (message Message) smsInspectAndFenceResult() (SMSInspectAndFenceResult, bool) {
	if message.result == nil || message.kind != MessageSMSInspectAndFenceResult {
		return SMSInspectAndFenceResult{}, false
	}
	return *message.result, true
}

func validateSMSSendIntent(intent SMSSendIntent) error {
	if !validIdentifier(intent.OperationID) || len(intent.Destination) == 0 ||
		len(intent.Destination) > MaxSMSDestinationBytes ||
		!destinationRegexp.MatchString(intent.Destination) ||
		len(intent.Message) == 0 || len(intent.Message) > MaxSMSMessageBytes ||
		!utf8.ValidString(intent.Message) || bytes.IndexByte([]byte(intent.Message), 0) >= 0 {
		return fmt.Errorf("%w: SMS send intent", ErrMalformed)
	}
	return nil
}

func validateInspectRequest(request SMSInspectAndFenceRequest) error {
	if !request.valid || !validIdentifier(request.operationID) || request.commitment == (Commitment{}) {
		return fmt.Errorf("%w: inspect-and-fence request", ErrMalformed)
	}
	return nil
}

func validateInspectResult(result SMSInspectAndFenceResult) error {
	if !result.valid || !validIdentifier(result.operationID) || result.commitment == (Commitment{}) {
		return fmt.Errorf("%w: inspect-and-fence result", ErrMalformed)
	}
	switch result.outcome {
	case OperationPending, OperationApplied, OperationFailed, OperationUnknown, OperationFencedAbsent:
		return nil
	default:
		return fmt.Errorf("%w: inspect-and-fence outcome", ErrMalformed)
	}
}

type innerEnvelope struct {
	Version uint64          `json:"version"`
	Kind    MessageKind     `json:"kind"`
	Body    json.RawMessage `json:"body"`
}

type wireSMSSendIntent struct {
	OperationID string `json:"operation_id"`
	Destination string `json:"destination"`
	Message     string `json:"message"`
}

type wireInspectRequest struct {
	OperationID string `json:"operation_id"`
	Commitment  string `json:"commitment"`
}

type wireInspectResult struct {
	OperationID string                 `json:"operation_id"`
	Commitment  string                 `json:"commitment"`
	Outcome     InspectAndFenceOutcome `json:"outcome"`
}

var (
	innerEnvelopeKeys  = exactKeySet("version", "kind", "body")
	smsSendKeys        = exactKeySet("operation_id", "destination", "message")
	inspectRequestKeys = exactKeySet("operation_id", "commitment")
	inspectResultKeys  = exactKeySet("operation_id", "commitment", "outcome")
)

func encodeInnerMessage(message Message) ([]byte, error) {
	var body []byte
	var err error
	switch message.kind {
	case MessageSMSSend:
		if message.send == nil || message.inspect != nil || message.result != nil {
			return nil, fmt.Errorf("%w: SMS send message", ErrMalformed)
		}
		if err = validateSMSSendIntent(*message.send); err != nil {
			return nil, err
		}
		body, err = json.Marshal(wireSMSSendIntent{
			OperationID: message.send.OperationID,
			Destination: message.send.Destination,
			Message:     message.send.Message,
		})
	case MessageSMSInspectAndFence:
		if message.inspect == nil || message.send != nil || message.result != nil {
			return nil, fmt.Errorf("%w: inspect-and-fence message", ErrMalformed)
		}
		if err = validateInspectRequest(*message.inspect); err != nil {
			return nil, err
		}
		body, err = json.Marshal(wireInspectRequest{
			OperationID: message.inspect.operationID,
			Commitment:  message.inspect.commitment.Encode(),
		})
	case MessageSMSInspectAndFenceResult:
		if message.result == nil || message.send != nil || message.inspect != nil {
			return nil, fmt.Errorf("%w: inspect-and-fence result message", ErrMalformed)
		}
		if err = validateInspectResult(*message.result); err != nil {
			return nil, err
		}
		body, err = json.Marshal(wireInspectResult{
			OperationID: message.result.operationID,
			Commitment:  message.result.commitment.Encode(),
			Outcome:     message.result.outcome,
		})
	default:
		return nil, ErrMessageKind
	}
	if err != nil {
		return nil, fmt.Errorf("encode public relay message: %w", err)
	}
	encoded, err := json.Marshal(innerEnvelope{Version: ProtocolVersion, Kind: message.kind, Body: body})
	if err != nil {
		return nil, fmt.Errorf("encode public relay message envelope: %w", err)
	}
	if len(encoded) > maxInnerJSONBytes {
		return nil, fmt.Errorf("%w: inner message size", ErrMalformed)
	}
	return encoded, nil
}

func decodeInnerMessage(encoded []byte) (Message, error) {
	if len(encoded) == 0 || len(encoded) > maxInnerJSONBytes {
		return Message{}, ErrMalformed
	}
	var inner innerEnvelope
	if err := decodeExactJSON(encoded, &inner, innerEnvelopeKeys, MaxJSONDepth); err != nil {
		return Message{}, ErrMalformed
	}
	if inner.Version != ProtocolVersion {
		return Message{}, fmt.Errorf("%w: inner protocol version", ErrMalformed)
	}
	switch inner.Kind {
	case MessageSMSSend:
		var wire wireSMSSendIntent
		if err := decodeExactJSON(inner.Body, &wire, smsSendKeys, MaxJSONDepth); err != nil {
			return Message{}, ErrMalformed
		}
		return NewSMSSendMessage(SMSSendIntent{
			OperationID: wire.OperationID, Destination: wire.Destination, Message: wire.Message,
		})
	case MessageSMSInspectAndFence:
		var wire wireInspectRequest
		if err := decodeExactJSON(inner.Body, &wire, inspectRequestKeys, MaxJSONDepth); err != nil {
			return Message{}, ErrMalformed
		}
		commitment, err := ParseCommitment(wire.Commitment)
		if err != nil {
			return Message{}, err
		}
		request, err := NewSMSInspectAndFenceRequest(wire.OperationID, commitment)
		if err != nil {
			return Message{}, err
		}
		return NewSMSInspectAndFenceMessage(request)
	case MessageSMSInspectAndFenceResult:
		var wire wireInspectResult
		if err := decodeExactJSON(inner.Body, &wire, inspectResultKeys, MaxJSONDepth); err != nil {
			return Message{}, ErrMalformed
		}
		commitment, err := ParseCommitment(wire.Commitment)
		if err != nil {
			return Message{}, err
		}
		result, err := NewSMSInspectAndFenceResult(wire.OperationID, commitment, wire.Outcome)
		if err != nil {
			return Message{}, err
		}
		return NewSMSInspectAndFenceResultMessage(result)
	default:
		return Message{}, ErrMessageKind
	}
}

func messageAllowedInDirection(kind MessageKind, direction Direction) bool {
	switch direction {
	case ClientToGateway:
		return kind == MessageSMSSend || kind == MessageSMSInspectAndFence
	case GatewayToClient:
		return kind == MessageSMSInspectAndFenceResult
	default:
		return false
	}
}

func padMessage(message Message) ([]byte, error) {
	encoded, err := encodeInnerMessage(message)
	if err != nil {
		return nil, err
	}
	required := paddingHeaderBytes + len(encoded)
	bucket := minimumPaddingBucket(required)
	if bucket == 0 {
		return nil, fmt.Errorf("%w: no padding bucket", ErrMalformed)
	}
	padded := make([]byte, bucket)
	copy(padded[:4], paddingMagic[:])
	binary.BigEndian.PutUint32(padded[4:8], uint32(len(encoded)))
	copy(padded[paddingHeaderBytes:], encoded)
	return padded, nil
}

func unpadMessage(padded []byte) (Message, error) {
	if !isPaddingBucket(len(padded)) || len(padded) < paddingHeaderBytes ||
		!bytes.Equal(padded[:4], paddingMagic[:]) {
		return Message{}, fmt.Errorf("%w: padded message header", ErrMalformed)
	}
	length := int(binary.BigEndian.Uint32(padded[4:8]))
	if length <= 0 || length > maxInnerJSONBytes || paddingHeaderBytes+length > len(padded) {
		return Message{}, fmt.Errorf("%w: padded message length", ErrMalformed)
	}
	if minimumPaddingBucket(paddingHeaderBytes+length) != len(padded) {
		return Message{}, fmt.Errorf("%w: non-canonical padding bucket", ErrMalformed)
	}
	for _, value := range padded[paddingHeaderBytes+length:] {
		if value != 0 {
			return Message{}, fmt.Errorf("%w: non-zero message padding", ErrMalformed)
		}
	}
	return decodeInnerMessage(padded[paddingHeaderBytes : paddingHeaderBytes+length])
}

func minimumPaddingBucket(required int) int {
	for _, candidate := range paddingBuckets {
		if required <= candidate {
			return candidate
		}
	}
	return 0
}

func isPaddingBucket(size int) bool {
	for _, bucket := range paddingBuckets {
		if size == bucket {
			return true
		}
	}
	return false
}

func validCiphertextSize(size int) bool {
	return size > gcmTagBytes && isPaddingBucket(size-gcmTagBytes)
}

func maxCiphertextBytes() int { return paddingBuckets[len(paddingBuckets)-1] + gcmTagBytes }

func canonicalSMSSendIntent(intent SMSSendIntent) ([]byte, error) {
	if err := validateSMSSendIntent(intent); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	writeCanonicalPart(&result, "DJONEHUB-PUBLIC-RELAY-SMS-COMMITMENT-V2")
	writeCanonicalPart(&result, "version")
	writeCanonicalPart(&result, "2")
	writeCanonicalPart(&result, "kind")
	writeCanonicalPart(&result, string(MessageSMSSend))
	writeCanonicalPart(&result, "operation_id")
	writeCanonicalPart(&result, intent.OperationID)
	writeCanonicalPart(&result, "destination")
	writeCanonicalPart(&result, intent.Destination)
	writeCanonicalPart(&result, "message")
	writeCanonicalPart(&result, intent.Message)
	return result.Bytes(), nil
}

func writeCanonicalPart(destination *bytes.Buffer, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	destination.Write(length[:])
	destination.WriteString(value)
}
