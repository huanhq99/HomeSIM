package publicrelay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"hash"
)

const (
	rootKeyBytes = 32
	aeadKeyBytes = 32
	nonceBytes   = 12
)

// RootKey is the 256-bit root established by the local pairing protocol. This
// package consumes it but deliberately does not define QR enrollment, ECDH key
// storage, or user confirmation.
type RootKey struct{ value [rootKeyBytes]byte }

// NewRootKey copies one non-zero 256-bit root into a redacted value.
func NewRootKey(material []byte) (RootKey, error) {
	if len(material) != rootKeyBytes || allZero(material) {
		return RootKey{}, fmt.Errorf("%w: root key", ErrMalformed)
	}
	var root RootKey
	copy(root.value[:], material)
	return root, nil
}

// KeySet contains epoch- and route-bound c2g, g2c, and commitment/state
// binding keys. Its fields are intentionally not exported.
type KeySet struct {
	routeID  string
	keyEpoch uint64
	c2g      [aeadKeyBytes]byte
	g2c      [aeadKeyBytes]byte
	bind     [commitmentBytes]byte
}

func (keys KeySet) RouteID() string  { return keys.routeID }
func (keys KeySet) KeyEpoch() uint64 { return keys.keyEpoch }

// DeriveKeySet applies HKDF-SHA256 with an explicit versioned salt and
// independent labels for both traffic directions and the binding key.
func DeriveKeySet(root RootKey, routeID string, keyEpoch uint64) (KeySet, error) {
	if root == (RootKey{}) || !validIdentifier(routeID) || keyEpoch == 0 || keyEpoch > MaxWireCounter {
		return KeySet{}, fmt.Errorf("%w: key derivation context", ErrMalformed)
	}
	_, _, prk, err := deriveKDFIntermediates(root, routeID, keyEpoch)
	if err != nil {
		return KeySet{}, err
	}
	keys := KeySet{routeID: routeID, keyEpoch: keyEpoch}
	if err := expandKey(prk, "DJONEHUB-PUBLIC-RELAY-C2G-AEAD-V2", keys.c2g[:]); err != nil {
		return KeySet{}, err
	}
	if err := expandKey(prk, "DJONEHUB-PUBLIC-RELAY-G2C-AEAD-V2", keys.g2c[:]); err != nil {
		return KeySet{}, err
	}
	if err := expandKey(prk, "DJONEHUB-PUBLIC-RELAY-BIND-V2", keys.bind[:]); err != nil {
		return KeySet{}, err
	}
	return keys, nil
}

func deriveKDFIntermediates(
	root RootKey,
	routeID string,
	keyEpoch uint64,
) ([]byte, [sha256.Size]byte, []byte, error) {
	if root == (RootKey{}) || !validIdentifier(routeID) || keyEpoch == 0 || keyEpoch > MaxWireCounter {
		return nil, [sha256.Size]byte{}, nil, ErrMalformed
	}
	saltInput := kdfContext("DJONEHUB-PUBLIC-RELAY-KDF-SALT-V2", routeID, keyEpoch)
	salt := sha256.Sum256(saltInput)
	prk, err := hkdf.Extract(sha256.New, root.value[:], salt[:])
	if err != nil {
		return nil, [sha256.Size]byte{}, nil, fmt.Errorf("derive public relay extract key: %w", err)
	}
	return saltInput, salt, prk, nil
}

func expandKey(prk []byte, label string, destination []byte) error {
	derived, err := hkdf.Expand(sha256.New, prk, label, len(destination))
	if err != nil {
		return fmt.Errorf("derive public relay %s: %w", label, err)
	}
	copy(destination, derived)
	return nil
}

func kdfContext(domain, routeID string, epoch uint64) []byte {
	var result bytes.Buffer
	writeCanonicalPart(&result, domain)
	writeCanonicalPart(&result, "route_id")
	writeCanonicalPart(&result, routeID)
	writeCanonicalPart(&result, "key_epoch")
	writeCanonicalPart(&result, fmt.Sprintf("%d", epoch))
	return result.Bytes()
}

// ComputeSMSSendCommitment returns HMAC-SHA256(K_bind, canonical intent).
// Unlike a bare hash, the persisted result cannot be used as an offline oracle
// for a guessed destination or message without the paired root key.
func ComputeSMSSendCommitment(keys KeySet, intent SMSSendIntent) (Commitment, error) {
	if err := validateKeySet(keys); err != nil {
		return Commitment{}, err
	}
	canonical, err := canonicalSMSSendIntent(intent)
	if err != nil {
		return Commitment{}, err
	}
	mac := hmac.New(sha256.New, keys.bind[:])
	_, _ = mac.Write(canonical)
	digest := mac.Sum(nil)
	var commitment Commitment
	copy(commitment.value[:], digest)
	return commitment, nil
}

func validateKeySet(keys KeySet) error {
	if !validIdentifier(keys.routeID) || keys.keyEpoch == 0 || keys.keyEpoch > MaxWireCounter ||
		allZero(keys.c2g[:]) || allZero(keys.g2c[:]) || allZero(keys.bind[:]) ||
		subtle.ConstantTimeCompare(keys.c2g[:], keys.g2c[:]) == 1 ||
		subtle.ConstantTimeCompare(keys.c2g[:], keys.bind[:]) == 1 ||
		subtle.ConstantTimeCompare(keys.g2c[:], keys.bind[:]) == 1 {
		return fmt.Errorf("%w: derived key set", ErrMalformed)
	}
	return nil
}

func trafficKey(keys KeySet, direction Direction) ([]byte, error) {
	if err := validateKeySet(keys); err != nil {
		return nil, err
	}
	switch direction {
	case ClientToGateway:
		return keys.c2g[:], nil
	case GatewayToClient:
		return keys.g2c[:], nil
	default:
		return nil, ErrDirection
	}
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create public relay AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create public relay GCM: %w", err)
	}
	if aead.NonceSize() != nonceBytes || aead.Overhead() != gcmTagBytes {
		return nil, fmt.Errorf("%w: unexpected AES-GCM parameters", ErrMalformed)
	}
	return aead, nil
}

func sealUnsigned(keys KeySet, meta EnvelopeMeta, message Message) (Envelope, error) {
	if err := validateKeyContext(keys, meta); err != nil {
		return Envelope{}, err
	}
	if !messageAllowedInDirection(message.kind, meta.Direction) {
		return Envelope{}, ErrMessageKind
	}
	padded, err := padMessage(message)
	if err != nil {
		return Envelope{}, err
	}
	key, err := trafficKey(keys, meta.Direction)
	if err != nil {
		return Envelope{}, err
	}
	aead, err := newAEAD(key)
	if err != nil {
		return Envelope{}, err
	}
	aad, err := canonicalAAD(meta)
	if err != nil {
		return Envelope{}, err
	}
	nonce, err := deriveNonce(key, meta)
	if err != nil {
		return Envelope{}, err
	}
	ciphertext := aead.Seal(nil, nonce, padded, aad)
	envelope := Envelope{meta: meta, ciphertext: ciphertext}
	if err := validateEnvelope(envelope, false); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func openAuthenticated(keys KeySet, envelope Envelope) (Message, error) {
	if err := validateKeyContext(keys, envelope.meta); err != nil {
		return Message{}, ErrUnauthorized
	}
	if err := validateEnvelope(envelope, true); err != nil {
		return Message{}, ErrUnauthorized
	}
	key, err := trafficKey(keys, envelope.meta.Direction)
	if err != nil {
		return Message{}, ErrUnauthorized
	}
	aead, err := newAEAD(key)
	if err != nil {
		return Message{}, err
	}
	aad, err := canonicalAAD(envelope.meta)
	if err != nil {
		return Message{}, ErrUnauthorized
	}
	nonce, err := deriveNonce(key, envelope.meta)
	if err != nil {
		return Message{}, ErrUnauthorized
	}
	padded, err := aead.Open(nil, nonce, envelope.ciphertext, aad)
	if err != nil {
		return Message{}, ErrUnauthorized
	}
	message, err := unpadMessage(padded)
	if err != nil || !messageAllowedInDirection(message.kind, envelope.meta.Direction) {
		return Message{}, ErrUnauthorized
	}
	return message, nil
}

func validateKeyContext(keys KeySet, meta EnvelopeMeta) error {
	if err := validateKeySet(keys); err != nil {
		return err
	}
	if err := validateMeta(meta); err != nil {
		return err
	}
	if keys.routeID != meta.RouteID || keys.keyEpoch != meta.KeyEpoch {
		return fmt.Errorf("%w: key route or epoch", ErrStateMismatch)
	}
	return nil
}

func canonicalAAD(meta EnvelopeMeta) ([]byte, error) {
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	writeCanonicalPart(&result, "DJONEHUB-PUBLIC-RELAY-AES-GCM-AAD-V2")
	fields := []struct{ name, value string }{
		{"version", "2"},
		{"route_id", meta.RouteID},
		{"key_epoch", fmt.Sprintf("%d", meta.KeyEpoch)},
		{"direction", string(meta.Direction)},
		{"sequence", fmt.Sprintf("%d", meta.Sequence)},
		{"message_id", meta.MessageID},
		{"issued_at", fmt.Sprintf("%d", meta.IssuedAt)},
	}
	for _, field := range fields {
		writeCanonicalPart(&result, field.name)
		writeCanonicalPart(&result, field.value)
	}
	return result.Bytes(), nil
}

// deriveNonce produces a 96-bit nonce consisting of a context-derived four-
// byte prefix and uint64be(sequence). Each route/epoch/direction has an
// independently derived AES key and prefix, so uniqueness under one AES key is
// reduced to the durable no-reuse sequence invariant.
func deriveNonce(key []byte, meta EnvelopeMeta) ([]byte, error) {
	prefix, err := deriveNoncePrefix(key, meta)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceBytes)
	copy(nonce[:4], prefix[:])
	binary.BigEndian.PutUint64(nonce[4:], meta.Sequence)
	return nonce, nil
}

func deriveNoncePrefix(key []byte, meta EnvelopeMeta) ([4]byte, error) {
	if len(key) != aeadKeyBytes {
		return [4]byte{}, fmt.Errorf("%w: nonce input", ErrMalformed)
	}
	if err := validateMeta(meta); err != nil {
		return [4]byte{}, err
	}
	var context bytes.Buffer
	writeCanonicalPart(&context, "DJONEHUB-PUBLIC-RELAY-AES-GCM-NONCE-PREFIX-V2")
	for _, field := range []struct{ name, value string }{
		{"route_id", meta.RouteID},
		{"key_epoch", fmt.Sprintf("%d", meta.KeyEpoch)},
		{"direction", string(meta.Direction)},
	} {
		writeCanonicalPart(&context, field.name)
		writeCanonicalPart(&context, field.value)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(context.Bytes())
	digest := mac.Sum(nil)
	var prefix [4]byte
	copy(prefix[:], digest[:4])
	return prefix, nil
}

func writeMACPart(mac hash.Hash, value string) {
	writeMACBytes(mac, []byte(value))
}

func writeMACBytes(mac hash.Hash, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(value)
}

func allZero(value []byte) bool {
	var aggregate byte
	for _, item := range value {
		aggregate |= item
	}
	return aggregate == 0
}
