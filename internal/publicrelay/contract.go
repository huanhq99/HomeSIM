package publicrelay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	// ProtocolVersion is intentionally unrelated to publicedge.ProtocolVersion.
	ProtocolVersion uint64 = 2

	MaxEnvelopeBytes = 64 << 10
	MaxStateBytes    = 64 << 10
	MaxJSONDepth     = 8

	// MaxWireCounter remains exactly representable by JSON implementations that
	// store numbers as IEEE-754 doubles.
	MaxWireCounter = uint64(1<<53 - 1)

	identifierBytes = 16
	commitmentBytes = 32
	gcmTagBytes     = 16
)

var (
	ErrMalformed        = errors.New("public relay value is malformed")
	ErrUnauthorized     = errors.New("public relay proof is unauthorized")
	ErrDirection        = errors.New("public relay direction is invalid")
	ErrMessageKind      = errors.New("public relay message kind is invalid")
	ErrSequenceConflict = errors.New("public relay sequence has conflicting content")
	ErrSequenceTooOld   = errors.New("public relay sequence is outside the receive window")
	ErrSequenceTooFar   = errors.New("public relay sequence advances beyond the receive window")
	ErrStateMismatch    = errors.New("public relay receiver state does not match the key context")
	ErrStateCommit      = errors.New("public relay receiver state was not durably committed")
	ErrStateCAS         = errors.New("public relay receiver state compare-and-swap failed")
)

// Direction selects independently derived traffic keys. A direction is also
// bound into the AEAD associated data, nonce derivation, and P-256 proof.
type Direction string

const (
	ClientToGateway Direction = "c2g"
	GatewayToClient Direction = "g2c"
)

// EnvelopeMeta is the only caller-provided outer metadata. All identifiers are
// opaque canonical base64url values containing exactly 128 bits.
type EnvelopeMeta struct {
	RouteID   string
	KeyEpoch  uint64
	Direction Direction
	Sequence  uint64
	MessageID string
	IssuedAt  int64
}

// Envelope is immutable outside this package. EncodeEnvelope is the only wire
// encoder; MarshalJSON is deliberately redacted.
type Envelope struct {
	meta       EnvelopeMeta
	ciphertext []byte
	signature  string
}

// Meta returns a copy of the opaque outer metadata.
func (envelope Envelope) Meta() EnvelopeMeta { return envelope.meta }

// CiphertextSize returns the authenticated ciphertext size without exposing
// its contents to generic log formatting.
func (envelope Envelope) CiphertextSize() int { return len(envelope.ciphertext) }

// HasSignature reports whether the typed P-256 proof is present.
func (envelope Envelope) HasSignature() bool { return envelope.signature != "" }

type wireEnvelope struct {
	Version    uint64    `json:"version"`
	RouteID    string    `json:"route_id"`
	KeyEpoch   uint64    `json:"key_epoch"`
	Direction  Direction `json:"direction"`
	Sequence   uint64    `json:"sequence"`
	MessageID  string    `json:"message_id"`
	IssuedAt   int64     `json:"issued_at"`
	Ciphertext string    `json:"ciphertext"`
	Signature  string    `json:"signature"`
}

var envelopeKeys = exactKeySet(
	"version", "route_id", "key_epoch", "direction", "sequence",
	"message_id", "issued_at", "ciphertext", "signature",
)

// EncodeEnvelope emits the fixed version 2 outer schema. It refuses unsigned
// or otherwise malformed values.
func EncodeEnvelope(envelope Envelope) ([]byte, error) {
	if err := validateEnvelope(envelope, true); err != nil {
		return nil, err
	}
	wire := wireEnvelope{
		Version: ProtocolVersion, RouteID: envelope.meta.RouteID,
		KeyEpoch: envelope.meta.KeyEpoch, Direction: envelope.meta.Direction,
		Sequence: envelope.meta.Sequence, MessageID: envelope.meta.MessageID,
		IssuedAt:   envelope.meta.IssuedAt,
		Ciphertext: base64.RawURLEncoding.EncodeToString(envelope.ciphertext),
		Signature:  envelope.signature,
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode public relay envelope: %w", err)
	}
	if len(encoded) > MaxEnvelopeBytes {
		return nil, fmt.Errorf("%w: envelope exceeds %d bytes", ErrMalformed, MaxEnvelopeBytes)
	}
	return encoded, nil
}

// DecodeEnvelope parses one fixed-schema object. Unknown, missing, duplicate,
// nested, trailing, non-canonical base64url, and oversized inputs fail closed.
func DecodeEnvelope(encoded []byte) (Envelope, error) {
	if len(encoded) == 0 || len(encoded) > MaxEnvelopeBytes {
		return Envelope{}, ErrMalformed
	}
	var wire wireEnvelope
	if err := decodeExactJSON(encoded, &wire, envelopeKeys, MaxJSONDepth); err != nil {
		return Envelope{}, ErrMalformed
	}
	if wire.Version != ProtocolVersion {
		return Envelope{}, ErrMalformed
	}
	ciphertext, err := decodeCanonicalBase64(wire.Ciphertext, maxCiphertextBytes())
	if err != nil {
		return Envelope{}, ErrMalformed
	}
	envelope := Envelope{
		meta: EnvelopeMeta{
			RouteID: wire.RouteID, KeyEpoch: wire.KeyEpoch, Direction: wire.Direction,
			Sequence: wire.Sequence, MessageID: wire.MessageID, IssuedAt: wire.IssuedAt,
		},
		ciphertext: ciphertext,
		signature:  wire.Signature,
	}
	if err := validateEnvelope(envelope, true); err != nil {
		return Envelope{}, ErrMalformed
	}
	return envelope, nil
}

// DecodeEnvelopeReader bounds the read before parsing, including streams whose
// advertised length is absent or false.
func DecodeEnvelopeReader(reader io.Reader) (Envelope, error) {
	if reader == nil {
		return Envelope{}, ErrMalformed
	}
	limited := io.LimitReader(reader, MaxEnvelopeBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return Envelope{}, ErrMalformed
	}
	return DecodeEnvelope(encoded)
}

func validateEnvelope(envelope Envelope, requireSignature bool) error {
	if err := validateMeta(envelope.meta); err != nil {
		return err
	}
	if !validCiphertextSize(len(envelope.ciphertext)) {
		return fmt.Errorf("%w: ciphertext size", ErrMalformed)
	}
	if requireSignature {
		if !validEncodedSignature(envelope.signature) {
			return fmt.Errorf("%w: signature", ErrMalformed)
		}
	} else if envelope.signature != "" {
		return fmt.Errorf("%w: unexpected signature", ErrMalformed)
	}
	return nil
}

func validateMeta(meta EnvelopeMeta) error {
	if !validIdentifier(meta.RouteID) || !validIdentifier(meta.MessageID) ||
		meta.KeyEpoch == 0 || meta.KeyEpoch > MaxWireCounter ||
		meta.Sequence == 0 || meta.Sequence > MaxWireCounter ||
		meta.IssuedAt <= 0 || uint64(meta.IssuedAt) > MaxWireCounter {
		return fmt.Errorf("%w: envelope metadata", ErrMalformed)
	}
	if meta.Direction != ClientToGateway && meta.Direction != GatewayToClient {
		return ErrDirection
	}
	return nil
}

func validIdentifier(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == identifierBytes &&
		!allZero(decoded) && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func decodeCanonicalBase64(value string, maximum int) ([]byte, error) {
	if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(maximum) {
		return nil, ErrMalformed
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > maximum || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, ErrMalformed
	}
	return decoded, nil
}

func exactKeySet(keys ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		result[key] = struct{}{}
	}
	return result
}

func decodeExactJSON(encoded []byte, destination any, keys map[string]struct{}, maxDepth int) error {
	if !utf8.Valid(encoded) || !validJSONSurrogateEscapes(encoded) || !json.Valid(encoded) {
		return errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := walkJSONValue(decoder, 1, maxDepth); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return errors.New("trailing JSON value")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return err
	}
	if len(raw) != len(keys) {
		return errors.New("wrong object key count")
	}
	for key := range raw {
		if _, ok := keys[key]; !ok {
			return errors.New("unexpected object key")
		}
	}
	strict := json.NewDecoder(bytes.NewReader(encoded))
	strict.DisallowUnknownFields()
	strict.UseNumber()
	if err := strict.Decode(destination); err != nil {
		return err
	}
	if err := strict.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validJSONSurrogateEscapes(encoded []byte) bool {
	inString := false
	for index := 0; index < len(encoded); index++ {
		switch encoded[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(encoded) {
				continue
			}
			if encoded[index+1] != 'u' {
				index++
				continue
			}
			value, ok := decodeHexQuad(encoded, index+2)
			if !ok {
				return false
			}
			switch {
			case value >= 0xd800 && value <= 0xdbff:
				if index+11 >= len(encoded) || encoded[index+6] != '\\' || encoded[index+7] != 'u' {
					return false
				}
				low, ok := decodeHexQuad(encoded, index+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				index += 11
			case value >= 0xdc00 && value <= 0xdfff:
				return false
			default:
				index += 5
			}
		}
	}
	return true
}

func decodeHexQuad(encoded []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(encoded) {
		return 0, false
	}
	var value uint16
	for _, digit := range encoded[start : start+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func walkJSONValue(decoder *json.Decoder, depth, maximum int) error {
	if depth > maximum {
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
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder, depth+1, maximum); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, depth+1, maximum); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
