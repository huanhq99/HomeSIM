package publicedge

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
)

const signingDomain = "DJONEHUB-PUBLIC-EDGE-P256-V1"

type ProofRole string

const (
	ProofGateway ProofRole = "gateway"
	ProofClient  ProofRole = "client"
)

// SignatureInput is a domain-separated, length-prefixed P-256 signing input.
// It includes every authorization and connection-incarnation field. Its
// String, GoString, and JSON forms are redacted.
type SignatureInput struct {
	Version        uint64
	Role           ProofRole
	Type           FrameType
	GatewayID      string
	DeviceID       string
	Principal      string
	Action         string
	Method         string
	Path           string
	BodySHA256     string
	OperationID    string
	ExpiresAt      int64
	BootID         string
	LeaseEpoch     uint64
	Sequence       uint64
	ChannelBinding string
}

// GatewaySigningInput returns the exact input for gateway authentication,
// snapshot, event, and operation-result frames. channelBinding is the random
// challenge of the current connection and binds every proof across edge
// restarts even if an in-memory lease epoch is reused.
func GatewaySigningInput(frame Frame, channelBinding string) (SignatureInput, error) {
	profile, ok := frameProfiles[frame.Type]
	if !ok || !profile.signedByGateway {
		return SignatureInput{}, fmt.Errorf("%w: frame is not gateway-signed", ErrProtocolViolation)
	}
	if err := validateFrame(frame, false); err != nil {
		return SignatureInput{}, err
	}
	if !validChallenge(channelBinding) {
		return SignatureInput{}, fmt.Errorf("%w: channel binding", ErrMalformed)
	}
	if frame.Type == FrameGatewayAuthenticate {
		var payload GatewayAuthenticatePayload
		if err := decodeStrictJSON(frame.Payload, &payload); err != nil || payload.Challenge != channelBinding {
			return SignatureInput{}, fmt.Errorf("%w: authentication challenge mismatch", ErrProtocolViolation)
		}
	}
	return inputFromFrame(ProofGateway, frame, channelBinding), nil
}

// ClientSigningInput returns the exact proof for a read-only operation probe.
// Native device IDs and namespaced principals are mutually exclusive.
func ClientSigningInput(frame Frame, channelBinding string) (SignatureInput, error) {
	profile, ok := frameProfiles[frame.Type]
	if !ok || !profile.signedByClient {
		return SignatureInput{}, fmt.Errorf("%w: frame is not client-signed", ErrProtocolViolation)
	}
	if err := validateFrame(frame, false); err != nil {
		return SignatureInput{}, err
	}
	if !validChallenge(channelBinding) {
		return SignatureInput{}, fmt.Errorf("%w: channel binding", ErrMalformed)
	}
	return inputFromFrame(ProofClient, frame, channelBinding), nil
}

func inputFromFrame(role ProofRole, frame Frame, binding string) SignatureInput {
	return SignatureInput{
		Version: frame.Version, Role: role, Type: frame.Type,
		GatewayID: frame.GatewayID, DeviceID: frame.DeviceID, Principal: frame.Principal,
		Action: frame.Action, Method: frame.Method, Path: frame.Path,
		BodySHA256: frame.BodySHA256, OperationID: frame.OperationID,
		ExpiresAt: frame.ExpiresAt, BootID: frame.BootID, LeaseEpoch: frame.LeaseEpoch,
		Sequence: frame.Sequence, ChannelBinding: binding,
	}
}

// CanonicalBytes emits the unambiguous input. It is binary by design: every
// field is identified and length-prefixed, so separators inside opaque values
// cannot create a second interpretation.
func (input SignatureInput) CanonicalBytes() ([]byte, error) {
	if err := input.validate(); err != nil {
		return nil, err
	}
	fields := []struct{ name, value string }{
		{"version", strconv.FormatUint(input.Version, 10)},
		{"role", string(input.Role)},
		{"frame_type", string(input.Type)},
		{"gateway_id", input.GatewayID},
		{"device_id", input.DeviceID},
		{"principal", input.Principal},
		{"action", input.Action},
		{"method", input.Method},
		{"exact_path", input.Path},
		{"body_sha256", input.BodySHA256},
		{"operation_id", input.OperationID},
		{"expires_at", strconv.FormatInt(input.ExpiresAt, 10)},
		{"boot_id", input.BootID},
		{"lease_epoch", strconv.FormatUint(input.LeaseEpoch, 10)},
		{"sequence", strconv.FormatUint(input.Sequence, 10)},
		{"channel_binding", input.ChannelBinding},
	}
	var result bytes.Buffer
	writeSigningPart(&result, signingDomain)
	for _, field := range fields {
		writeSigningPart(&result, field.name)
		writeSigningPart(&result, field.value)
	}
	return result.Bytes(), nil
}

func (input SignatureInput) validate() error {
	if input.Version != ProtocolVersion || !validGatewayID(input.GatewayID) ||
		!validOpaqueID(input.BootID, MaxBootIDBytes) || input.LeaseEpoch == 0 ||
		input.LeaseEpoch > MaxWireCounter || input.Sequence > MaxWireCounter || input.ExpiresAt <= 0 ||
		uint64(input.ExpiresAt) > MaxWireCounter || !validDigest(input.BodySHA256) ||
		!validChallenge(input.ChannelBinding) {
		return fmt.Errorf("%w: signing input", ErrMalformed)
	}
	profile, ok := frameProfiles[input.Type]
	if !ok || input.Action != profile.action || input.Method != profile.method || input.Path != profile.path {
		return fmt.Errorf("%w: signing profile", ErrMalformed)
	}
	switch input.Role {
	case ProofGateway:
		if !profile.signedByGateway || input.DeviceID != "" || input.Principal != "" {
			return fmt.Errorf("%w: gateway signing identity", ErrMalformed)
		}
	case ProofClient:
		if !profile.signedByClient || (input.DeviceID == "") == (input.Principal == "") {
			return fmt.Errorf("%w: client signing identity", ErrMalformed)
		}
		if input.DeviceID != "" && !validOpaqueID(input.DeviceID, MaxDeviceIDBytes) {
			return fmt.Errorf("%w: client device id", ErrMalformed)
		}
		if input.Principal != "" && !validPrincipal(input.Principal) {
			return fmt.Errorf("%w: client principal", ErrMalformed)
		}
	default:
		return fmt.Errorf("%w: signing role", ErrMalformed)
	}
	if profile.needsSequence != (input.Sequence > 0) || profile.needsOperation != (input.OperationID != "") {
		return fmt.Errorf("%w: signing sequence or operation", ErrMalformed)
	}
	if input.OperationID != "" && !validOpaqueID(input.OperationID, MaxOperationIDBytes) {
		return fmt.Errorf("%w: signing operation id", ErrMalformed)
	}
	return nil
}

func writeSigningPart(destination *bytes.Buffer, value string) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	destination.Write(length[:])
	destination.WriteString(value)
}

// SignP256 returns canonical base64url-encoded ASN.1 DER with a low-S value.
func SignP256(privateKey *ecdsa.PrivateKey, input SignatureInput) (string, error) {
	if !validPrivateKey(privateKey) {
		return "", fmt.Errorf("%w: P-256 private key", ErrMalformed)
	}
	canonical, err := input.CanonicalBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign P-256 proof: %w", err)
	}
	halfOrder := new(big.Int).Rsh(new(big.Int).Set(elliptic.P256().Params().N), 1)
	if s.Cmp(halfOrder) > 0 {
		s.Sub(elliptic.P256().Params().N, s)
	}
	der, err := asn1.Marshal(ecdsaDER{R: r, S: s})
	if err != nil {
		return "", fmt.Errorf("encode P-256 proof: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(der)
	if !validSignatureEncoding(encoded) {
		return "", fmt.Errorf("%w: encoded signature size", ErrMalformed)
	}
	return encoded, nil
}

// VerifyP256 rejects non-P-256 keys, non-canonical DER, high-S signatures,
// malformed base64, and any input mismatch.
func VerifyP256(publicKey *ecdsa.PublicKey, input SignatureInput, encoded string) error {
	if !validPublicKey(publicKey) || !validSignatureEncoding(encoded) {
		return ErrUnauthorized
	}
	canonical, err := input.CanonicalBytes()
	if err != nil {
		return ErrUnauthorized
	}
	der, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(der) != encoded {
		return ErrUnauthorized
	}
	var signature ecdsaDER
	rest, err := asn1.Unmarshal(der, &signature)
	if err != nil || len(rest) != 0 || signature.R == nil || signature.S == nil ||
		signature.R.Sign() <= 0 || signature.S.Sign() <= 0 {
		return ErrUnauthorized
	}
	reencoded, err := asn1.Marshal(signature)
	if err != nil || !bytes.Equal(reencoded, der) {
		return ErrUnauthorized
	}
	order := elliptic.P256().Params().N
	halfOrder := new(big.Int).Rsh(new(big.Int).Set(order), 1)
	if signature.R.Cmp(order) >= 0 || signature.S.Cmp(order) >= 0 || signature.S.Cmp(halfOrder) > 0 {
		return ErrUnauthorized
	}
	digest := sha256.Sum256(canonical)
	if !ecdsa.Verify(publicKey, digest[:], signature.R, signature.S) {
		return ErrUnauthorized
	}
	return nil
}

func SignGatewayFrame(privateKey *ecdsa.PrivateKey, frame Frame, channelBinding string) (Frame, error) {
	if frame.Signature != "" {
		return Frame{}, fmt.Errorf("%w: frame is already signed", ErrProtocolViolation)
	}
	input, err := GatewaySigningInput(frame, channelBinding)
	if err != nil {
		return Frame{}, err
	}
	frame.Signature, err = SignP256(privateKey, input)
	if err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func VerifyGatewayFrame(publicKey *ecdsa.PublicKey, frame Frame, channelBinding string) error {
	input, err := GatewaySigningInput(frame, channelBinding)
	if err != nil {
		return ErrUnauthorized
	}
	return VerifyP256(publicKey, input, frame.Signature)
}

func SignClientFrame(privateKey *ecdsa.PrivateKey, frame Frame, channelBinding string) (Frame, error) {
	if frame.Signature != "" {
		return Frame{}, fmt.Errorf("%w: frame is already signed", ErrProtocolViolation)
	}
	input, err := ClientSigningInput(frame, channelBinding)
	if err != nil {
		return Frame{}, err
	}
	frame.Signature, err = SignP256(privateKey, input)
	if err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func VerifyClientFrame(publicKey *ecdsa.PublicKey, frame Frame, channelBinding string) error {
	input, err := ClientSigningInput(frame, channelBinding)
	if err != nil {
		return ErrUnauthorized
	}
	return VerifyP256(publicKey, input, frame.Signature)
}

type ecdsaDER struct {
	R *big.Int
	S *big.Int
}

func validPrivateKey(key *ecdsa.PrivateKey) bool {
	if key == nil || key.D == nil || key.D.Sign() <= 0 || key.D.Cmp(elliptic.P256().Params().N) >= 0 ||
		!validPublicKey(&key.PublicKey) {
		return false
	}
	x, y := elliptic.P256().ScalarBaseMult(key.D.Bytes())
	return x.Cmp(key.X) == 0 && y.Cmp(key.Y) == 0
}

func validPublicKey(key *ecdsa.PublicKey) bool {
	return key != nil && key.Curve == elliptic.P256() && key.X != nil && key.Y != nil &&
		elliptic.P256().IsOnCurve(key.X, key.Y)
}

func validChallenge(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

type redactedSignatureInput struct {
	Version        uint64    `json:"version"`
	Role           ProofRole `json:"role"`
	Type           FrameType `json:"type"`
	GatewayID      string    `json:"gateway_id"`
	DeviceID       string    `json:"device_id,omitempty"`
	Principal      string    `json:"principal,omitempty"`
	Action         string    `json:"action"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	BodySHA256     string    `json:"body_sha256"`
	OperationID    string    `json:"operation_id,omitempty"`
	ExpiresAt      int64     `json:"expires_at"`
	BootID         string    `json:"boot_id"`
	LeaseEpoch     uint64    `json:"lease_epoch"`
	Sequence       uint64    `json:"sequence"`
	ChannelBinding string    `json:"channel_binding"`
}

func (input SignatureInput) redacted() redactedSignatureInput {
	return redactedSignatureInput{
		Version: input.Version, Role: input.Role, Type: input.Type,
		GatewayID: redactIfSet(input.GatewayID), DeviceID: redactIfSet(input.DeviceID),
		Principal: redactIfSet(input.Principal), Action: input.Action, Method: input.Method,
		Path: input.Path, BodySHA256: redactIfSet(input.BodySHA256),
		OperationID: redactIfSet(input.OperationID), ExpiresAt: input.ExpiresAt,
		BootID: redactIfSet(input.BootID), LeaseEpoch: input.LeaseEpoch, Sequence: input.Sequence,
		ChannelBinding: redactIfSet(input.ChannelBinding),
	}
}

func (input SignatureInput) MarshalJSON() ([]byte, error) { return json.Marshal(input.redacted()) }

func (input SignatureInput) String() string {
	data, err := json.Marshal(input.redacted())
	if err != nil {
		return "publicedge.SignatureInput{redacted}"
	}
	return string(data)
}

func (input SignatureInput) GoString() string { return input.String() }
