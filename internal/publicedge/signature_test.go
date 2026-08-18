package publicedge

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testBinding(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func testClientFrame(t *testing.T, key *ecdsa.PrivateKey, binding string, sequence uint64) Frame {
	t.Helper()
	frame, err := NewFrame(FrameOperationProbe, FrameMeta{
		GatewayID: "gateway-1", DeviceID: "device-1", OperationID: "operation-1",
		ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: "boot-1", LeaseEpoch: 3,
		Sequence: sequence,
	}, OperationProbePayload{})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignClientFrame(key, frame, binding)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestClientProofBindsEveryAuthorizationAndIncarnationField(t *testing.T) {
	key := testP256Key(t)
	binding := testBinding(1)
	frame := testClientFrame(t, key, binding, 7)
	input, err := ClientSigningInput(frame, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyP256(&key.PublicKey, input, frame.Signature); err != nil {
		t.Fatalf("verify valid signature: %v", err)
	}

	otherDigest := sha256.Sum256([]byte("different exact body"))
	mutations := []struct {
		name   string
		mutate func(*SignatureInput)
	}{
		{"version", func(i *SignatureInput) { i.Version++ }},
		{"role", func(i *SignatureInput) { i.Role = ProofGateway }},
		{"frame type", func(i *SignatureInput) { i.Type = FrameStateSnapshot }},
		{"gateway id", func(i *SignatureInput) { i.GatewayID = "gateway-2" }},
		{"device id", func(i *SignatureInput) { i.DeviceID = "device-2" }},
		{"action", func(i *SignatureInput) { i.Action = actionStateSnapshot }},
		{"method", func(i *SignatureInput) { i.Method = methodWSS }},
		{"exact path", func(i *SignatureInput) { i.Path = SnapshotPath }},
		{"body SHA-256", func(i *SignatureInput) { i.BodySHA256 = fmtDigest(otherDigest) }},
		{"operation id", func(i *SignatureInput) { i.OperationID = "operation-2" }},
		{"expiry", func(i *SignatureInput) { i.ExpiresAt++ }},
		{"boot id", func(i *SignatureInput) { i.BootID = "boot-2" }},
		{"lease epoch", func(i *SignatureInput) { i.LeaseEpoch++ }},
		{"sequence", func(i *SignatureInput) { i.Sequence++ }},
		{"channel binding", func(i *SignatureInput) { i.ChannelBinding = testBinding(2) }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			test.mutate(&candidate)
			if err := VerifyP256(&key.PublicKey, candidate, frame.Signature); err == nil {
				t.Fatal("signature survived bound-field change")
			}
		})
	}
}

func TestPrincipalAndGatewayProofsAreRoleSeparated(t *testing.T) {
	key := testP256Key(t)
	binding := testBinding(3)
	principalFrame, err := NewFrame(FrameOperationProbe, FrameMeta{
		GatewayID: "gateway-1", Principal: "cf:issuer:subject", OperationID: "operation-1",
		ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: "boot-1", LeaseEpoch: 1, Sequence: 1,
	}, OperationProbePayload{})
	if err != nil {
		t.Fatal(err)
	}
	principalFrame, err = SignClientFrame(key, principalFrame, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyClientFrame(&key.PublicKey, principalFrame, binding); err != nil {
		t.Fatalf("principal proof: %v", err)
	}
	modified := principalFrame
	modified.Principal = "cf:issuer:other"
	if err := VerifyClientFrame(&key.PublicKey, modified, binding); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("principal substitution error = %v", err)
	}
	if err := VerifyGatewayFrame(&key.PublicKey, principalFrame, binding); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("client proof accepted as gateway: %v", err)
	}

	auth, err := NewFrame(FrameGatewayAuthenticate, FrameMeta{
		GatewayID: "gateway-1", ExpiresAt: fixedTestTime.Add(20 * time.Second).Unix(),
		BootID: "boot-1", LeaseEpoch: 1,
	}, GatewayAuthenticatePayload{Challenge: binding})
	if err != nil {
		t.Fatal(err)
	}
	auth, err = SignGatewayFrame(key, auth, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGatewayFrame(&key.PublicKey, auth, binding); err != nil {
		t.Fatalf("gateway auth proof: %v", err)
	}
	if err := VerifyClientFrame(&key.PublicKey, auth, binding); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("gateway proof accepted as client: %v", err)
	}
}

func TestP256ProofRejectsWrongCurveMalformedDERAndHighS(t *testing.T) {
	key := testP256Key(t)
	binding := testBinding(4)
	frame := testClientFrame(t, key, binding, 1)
	input, err := ClientSigningInput(frame, binding)
	if err != nil {
		t.Fatal(err)
	}

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignP256(p384, input); err == nil {
		t.Fatal("P-384 signing key was accepted")
	}
	if err := VerifyP256(&p384.PublicKey, input, frame.Signature); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("P-384 verifier error = %v", err)
	}
	mismatched := *key
	other := testP256Key(t)
	mismatched.PublicKey = other.PublicKey
	if _, err := SignP256(&mismatched, input); err == nil {
		t.Fatal("private scalar with mismatched public coordinates was accepted")
	}
	for _, encoded := range []string{"", "not+base64", base64.RawURLEncoding.EncodeToString([]byte{0x30, 0x00})} {
		if err := VerifyP256(&key.PublicKey, input, encoded); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("malformed signature %q error = %v", encoded, err)
		}
	}

	canonical, err := input.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	order := elliptic.P256().Params().N
	half := new(big.Int).Rsh(new(big.Int).Set(order), 1)
	if s.Cmp(half) <= 0 {
		s = new(big.Int).Sub(order, s)
	}
	highDER, err := asn1.Marshal(ecdsaDER{R: r, S: s})
	if err != nil {
		t.Fatal(err)
	}
	high := base64.RawURLEncoding.EncodeToString(highDER)
	if err := VerifyP256(&key.PublicKey, input, high); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("high-S proof error = %v", err)
	}
}

func TestDirectSigningInputStillEnforcesIdentifierBounds(t *testing.T) {
	key := testP256Key(t)
	binding := testBinding(7)
	frame := testClientFrame(t, key, binding, 1)
	input, err := ClientSigningInput(frame, binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SignatureInput){
		func(value *SignatureInput) { value.DeviceID = strings.Repeat("x", MaxDeviceIDBytes+1) },
		func(value *SignatureInput) { value.OperationID = "contains/slash" },
		func(value *SignatureInput) { value.Principal = "also-present" },
	} {
		candidate := input
		mutate(&candidate)
		if _, err := SignP256(key, candidate); err == nil {
			t.Fatal("invalid direct signing input was accepted")
		}
	}
}

func TestGatewayFrameProofBindsExactPayloadBytes(t *testing.T) {
	key := testP256Key(t)
	binding := testBinding(5)
	frame, err := NewFrame(FrameStateSnapshot, FrameMeta{
		GatewayID: "gateway-1", ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: "boot-1",
		LeaseEpoch: 1, Sequence: 1,
	}, SnapshotPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 1, Service: ServiceReady,
		Call: CallIdle, SMS: SMSReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignGatewayFrame(key, frame, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGatewayFrame(&key.PublicKey, frame, binding); err != nil {
		t.Fatal(err)
	}
	frame.Payload = append([]byte(" "), frame.Payload...)
	if err := VerifyGatewayFrame(&key.PublicKey, frame, binding); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("payload byte substitution error = %v", err)
	}
}

func fmtDigest(value [sha256.Size]byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, item := range value {
		result[index*2] = alphabet[item>>4]
		result[index*2+1] = alphabet[item&15]
	}
	return string(result)
}
