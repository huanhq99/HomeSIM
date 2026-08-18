package publicrelay

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"fmt"
	"math/big"
)

type proofRole string

const (
	proofDevice  proofRole = "device"
	proofGateway proofRole = "gateway"
)

type ecdsaSignature struct {
	R *big.Int
	S *big.Int
}

// SealClientToGateway pads and encrypts one allowlisted c2g message, then adds
// a device P-256 proof over the typed envelope transcript.
func SealClientToGateway(
	keys KeySet,
	meta EnvelopeMeta,
	message Message,
	deviceKey crypto.Signer,
) (Envelope, error) {
	if meta.Direction != ClientToGateway {
		return Envelope{}, ErrDirection
	}
	envelope, err := sealUnsigned(keys, meta, message)
	if err != nil {
		return Envelope{}, err
	}
	return signDeviceEnvelope(deviceKey, envelope)
}

// SealGatewayToClient pads and encrypts one allowlisted g2c message, then adds
// a gateway P-256 proof over the typed envelope transcript.
func SealGatewayToClient(
	keys KeySet,
	meta EnvelopeMeta,
	message Message,
	gatewayKey crypto.Signer,
) (Envelope, error) {
	if meta.Direction != GatewayToClient {
		return Envelope{}, ErrDirection
	}
	envelope, err := sealUnsigned(keys, meta, message)
	if err != nil {
		return Envelope{}, err
	}
	return signGatewayEnvelope(gatewayKey, envelope)
}

func deviceProofDigest(envelope Envelope) ([sha256.Size]byte, error) {
	return typedProofDigest(proofDevice, ClientToGateway, envelope)
}

func gatewayProofDigest(envelope Envelope) ([sha256.Size]byte, error) {
	return typedProofDigest(proofGateway, GatewayToClient, envelope)
}

func signDeviceEnvelope(privateKey crypto.Signer, envelope Envelope) (Envelope, error) {
	if envelope.meta.Direction != ClientToGateway {
		return Envelope{}, ErrDirection
	}
	return signTypedEnvelope(privateKey, proofDevice, ClientToGateway, envelope)
}

func signGatewayEnvelope(privateKey crypto.Signer, envelope Envelope) (Envelope, error) {
	if envelope.meta.Direction != GatewayToClient {
		return Envelope{}, ErrDirection
	}
	return signTypedEnvelope(privateKey, proofGateway, GatewayToClient, envelope)
}

func signTypedEnvelope(
	privateKey crypto.Signer,
	role proofRole,
	direction Direction,
	envelope Envelope,
) (Envelope, error) {
	if privateKey == nil || envelope.signature != "" {
		return Envelope{}, fmt.Errorf("%w: signing key or envelope state", ErrMalformed)
	}
	publicKey, ok := privateKey.Public().(*ecdsa.PublicKey)
	if !ok || !validPublicKey(publicKey) {
		return Envelope{}, fmt.Errorf("%w: P-256 signing key", ErrMalformed)
	}
	digest, err := typedProofDigest(role, direction, envelope)
	if err != nil {
		return Envelope{}, err
	}
	der, err := privateKey.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return Envelope{}, fmt.Errorf("sign public relay proof: %w", err)
	}
	signature, ok := decodeDERSignature(der, false)
	if !ok || !ecdsa.Verify(publicKey, digest[:], signature.R, signature.S) {
		return Envelope{}, fmt.Errorf("%w: signer returned invalid P-256 proof", ErrMalformed)
	}
	order := elliptic.P256().Params().N
	halfOrder := new(big.Int).Rsh(new(big.Int).Set(order), 1)
	if signature.S.Cmp(halfOrder) > 0 {
		signature.S.Sub(order, signature.S)
	}
	der, err = asn1.Marshal(signature)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode public relay proof: %w", err)
	}
	envelope.signature = base64.RawURLEncoding.EncodeToString(der)
	if !validEncodedSignature(envelope.signature) {
		return Envelope{}, fmt.Errorf("%w: generated proof", ErrMalformed)
	}
	return envelope, nil
}

func verifyDeviceEnvelope(publicKey *ecdsa.PublicKey, envelope Envelope) error {
	return verifyTypedEnvelope(publicKey, proofDevice, ClientToGateway, envelope)
}

func verifyGatewayEnvelope(publicKey *ecdsa.PublicKey, envelope Envelope) error {
	return verifyTypedEnvelope(publicKey, proofGateway, GatewayToClient, envelope)
}

func verifyTypedEnvelope(
	publicKey *ecdsa.PublicKey,
	role proofRole,
	direction Direction,
	envelope Envelope,
) error {
	if !validPublicKey(publicKey) || envelope.meta.Direction != direction {
		return ErrUnauthorized
	}
	digest, err := typedProofDigest(role, direction, envelope)
	if err != nil {
		return ErrUnauthorized
	}
	signature, ok := decodeSignature(envelope.signature)
	if !ok || !ecdsa.Verify(publicKey, digest[:], signature.R, signature.S) {
		return ErrUnauthorized
	}
	return nil
}

func typedProofDigest(role proofRole, direction Direction, envelope Envelope) ([sha256.Size]byte, error) {
	if envelope.meta.Direction != direction {
		return [sha256.Size]byte{}, ErrDirection
	}
	if envelope.signature == "" {
		if err := validateEnvelope(envelope, false); err != nil {
			return [sha256.Size]byte{}, err
		}
	} else if err := validateEnvelope(envelope, true); err != nil {
		return [sha256.Size]byte{}, err
	}
	ciphertextHash := sha256.Sum256(envelope.ciphertext)
	var transcript bytes.Buffer
	writeCanonicalPart(&transcript, "DJONEHUB-PUBLIC-RELAY-P256-PROOF-V2")
	fields := []struct{ name, value string }{
		{"version", "2"},
		{"role", string(role)},
		{"route_id", envelope.meta.RouteID},
		{"key_epoch", fmt.Sprintf("%d", envelope.meta.KeyEpoch)},
		{"direction", string(envelope.meta.Direction)},
		{"sequence", fmt.Sprintf("%d", envelope.meta.Sequence)},
		{"message_id", envelope.meta.MessageID},
		{"issued_at", fmt.Sprintf("%d", envelope.meta.IssuedAt)},
		{"ciphertext_length", fmt.Sprintf("%d", len(envelope.ciphertext))},
		{"ciphertext_sha256", base64.RawURLEncoding.EncodeToString(ciphertextHash[:])},
	}
	for _, field := range fields {
		writeCanonicalPart(&transcript, field.name)
		writeCanonicalPart(&transcript, field.value)
	}
	return sha256.Sum256(transcript.Bytes()), nil
}

func validEncodedSignature(encoded string) bool {
	_, ok := decodeSignature(encoded)
	return ok
}

func decodeSignature(encoded string) (ecdsaSignature, bool) {
	if encoded == "" || len(encoded) > 96 {
		return ecdsaSignature{}, false
	}
	der, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(der) != encoded {
		return ecdsaSignature{}, false
	}
	return decodeDERSignature(der, true)
}

func decodeDERSignature(der []byte, requireLowS bool) (ecdsaSignature, bool) {
	var signature ecdsaSignature
	rest, err := asn1.Unmarshal(der, &signature)
	if err != nil || len(rest) != 0 || signature.R == nil || signature.S == nil ||
		signature.R.Sign() <= 0 || signature.S.Sign() <= 0 {
		return ecdsaSignature{}, false
	}
	reencoded, err := asn1.Marshal(signature)
	if err != nil || !bytes.Equal(reencoded, der) {
		return ecdsaSignature{}, false
	}
	order := elliptic.P256().Params().N
	halfOrder := new(big.Int).Rsh(new(big.Int).Set(order), 1)
	if signature.R.Cmp(order) >= 0 || signature.S.Cmp(order) >= 0 ||
		(requireLowS && signature.S.Cmp(halfOrder) > 0) {
		return ecdsaSignature{}, false
	}
	return signature, true
}

func validPublicKey(key *ecdsa.PublicKey) bool {
	return key != nil && key.Curve == elliptic.P256() && key.X != nil && key.Y != nil &&
		elliptic.P256().IsOnCurve(key.X, key.Y)
}

func cloneVerifierKey(key *ecdsa.PublicKey) (*ecdsa.PublicKey, [sha256.Size]byte, []byte, error) {
	if !validPublicKey(key) {
		return nil, [sha256.Size]byte{}, nil, ErrMalformed
	}
	clone := &ecdsa.PublicKey{
		Curve: elliptic.P256(), X: new(big.Int).Set(key.X), Y: new(big.Int).Set(key.Y),
	}
	spki, err := x509.MarshalPKIXPublicKey(clone)
	if err != nil {
		return nil, [sha256.Size]byte{}, nil, ErrMalformed
	}
	fingerprint := sha256.Sum256(spki)
	return clone, fingerprint, spki, nil
}
