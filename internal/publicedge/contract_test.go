package publicedge

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var fixedTestTime = time.Unix(2_000_000_000, 0)

func TestProtocolIsDefaultOffWithExactFrameAllowList(t *testing.T) {
	if DefaultEnabled {
		t.Fatal("public edge protocol became enabled by default")
	}
	want := map[FrameType]struct{}{
		FrameGatewayHello: {}, FrameEdgeChallenge: {}, FrameGatewayAuthenticate: {},
		FrameEdgeAuthenticated: {}, FrameStateSnapshot: {}, FrameStateEvent: {},
		FrameOperationProbe: {}, FrameOperationResult: {},
	}
	if len(frameProfiles) != len(want) {
		t.Fatalf("frame allowlist size = %d, want %d", len(frameProfiles), len(want))
	}
	for kind := range frameProfiles {
		if _, ok := want[kind]; !ok {
			t.Fatalf("unexpected frame type in allowlist: %q", kind)
		}
	}
}

func testP256Key(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate P-256 key: %v", err)
	}
	return key
}

func testHello(t *testing.T, gatewayID, bootID string, expires time.Time) Frame {
	t.Helper()
	frame, err := NewFrame(FrameGatewayHello, FrameMeta{
		GatewayID: gatewayID, BootID: bootID, ExpiresAt: expires.Unix(),
	}, GatewayHelloPayload{})
	if err != nil {
		t.Fatalf("new hello: %v", err)
	}
	return frame
}

func TestFrameRoundTripUsesExplicitWireEncoding(t *testing.T) {
	hello := testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute))
	wire, err := EncodeFrame(hello)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	decoded, err := DecodeFrame(wire)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if decoded.Type != FrameGatewayHello || decoded.GatewayID != hello.GatewayID ||
		!bytes.Equal(decoded.Payload, hello.Payload) {
		t.Fatalf("round trip mismatch: %#v", decoded)
	}

	redacted, err := json.Marshal(hello)
	if err != nil {
		t.Fatalf("marshal redacted frame: %v", err)
	}
	if bytes.Equal(redacted, wire) || bytes.Contains(redacted, []byte("gateway-1")) ||
		bytes.Contains(redacted, []byte("boot-1")) || bytes.Contains(redacted, hello.Payload) {
		t.Fatalf("ordinary JSON leaked wire fields: %s", redacted)
	}
}

func TestDecodeFrameRejectsStrictJSONViolations(t *testing.T) {
	hello := testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute))
	valid, err := EncodeFrame(hello)
	if err != nil {
		t.Fatal(err)
	}

	unknownTop := append([]byte(nil), valid[:len(valid)-1]...)
	unknownTop = append(unknownTop, []byte(`,"headers":{"x":"y"}}`)...)
	duplicateTop := []byte(strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1))
	caseVariantTop := []byte(strings.Replace(string(valid), `"version":1`, `"version":1,"Version":1`, 1))
	caseOnlyTop := []byte(strings.Replace(string(valid), `"version":1`, `"Version":1`, 1))
	trailing := append(append([]byte(nil), valid...), []byte(` {}`)...)
	unknownPayloadFrame := hello
	unknownPayloadFrame.Payload = json.RawMessage(`{"command":"ATA"}`)
	unknownPayloadFrame.BodySHA256 = payloadDigest(unknownPayloadFrame.Payload)
	unknownPayload, err := json.Marshal(wireFrame(unknownPayloadFrame))
	if err != nil {
		t.Fatal(err)
	}
	duplicatePayloadFrame := hello
	duplicatePayloadFrame.Payload = json.RawMessage(`{"x":1,"x":2}`)
	duplicatePayloadFrame.BodySHA256 = payloadDigest(duplicatePayloadFrame.Payload)
	duplicatePayload, err := json.Marshal(wireFrame(duplicatePayloadFrame))
	if err != nil {
		t.Fatal(err)
	}
	nullPayloadFrame := hello
	nullPayloadFrame.Payload = json.RawMessage(`null`)
	nullPayloadFrame.BodySHA256 = payloadDigest(nullPayloadFrame.Payload)
	nullPayload, err := json.Marshal(wireFrame(nullPayloadFrame))
	if err != nil {
		t.Fatal(err)
	}
	badHash := []byte(strings.Replace(string(valid), hello.BodySHA256, strings.Repeat("0", 64), 1))
	unsupported := []byte(strings.Replace(string(valid), string(FrameGatewayHello), "http.proxy", 1))
	tooLarge := bytes.Repeat([]byte{'x'}, MaxFrameBytes+1)

	tests := []struct {
		name string
		data []byte
	}{
		{"unknown top-level field", unknownTop},
		{"duplicate top-level field", duplicateTop},
		{"case-variant semantic duplicate", caseVariantTop},
		{"case-variant top-level field", caseOnlyTop},
		{"trailing value", trailing},
		{"unknown command payload", unknownPayload},
		{"duplicate nested payload field", duplicatePayload},
		{"null payload", nullPayload},
		{"body hash mismatch", badHash},
		{"unsupported proxy type", unsupported},
		{"oversized", tooLarge},
		{"empty", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeFrame(test.data); err == nil {
				t.Fatal("malformed frame was accepted")
			}
		})
	}
}

func TestPayloadRequiresExactKeysAndCanonicalBytes(t *testing.T) {
	binding := testBinding(4)
	challenge, err := NewFrame(FrameEdgeChallenge, FrameMeta{
		GatewayID: "gateway-1", ExpiresAt: fixedTestTime.Add(time.Minute).Unix(),
		BootID: "boot-1", LeaseEpoch: 1,
	}, ChallengePayload{Challenge: binding})
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]json.RawMessage{
		"case-variant semantic duplicate": json.RawMessage(`{"challenge":"` + binding + `","Challenge":"` + binding + `"}`),
		"case-variant field":              json.RawMessage(`{"Challenge":"` + binding + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			candidate := challenge
			candidate.Payload = payload
			candidate.BodySHA256 = payloadDigest(payload)
			if err := validateFrame(candidate, true); err == nil {
				t.Fatal("non-canonical payload key was accepted")
			}
		})
	}

	hello := testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute))
	hello.Payload = json.RawMessage(`{ }`)
	hello.BodySHA256 = payloadDigest(hello.Payload)
	if err := validateFrame(hello, true); err == nil {
		t.Fatal("non-canonical payload bytes were accepted")
	}
}

func TestUnknownJSONKeyIsNeverReflectedInErrors(t *testing.T) {
	secretKey := "private_attacker_controlled_key"
	hello := testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute))
	wire, err := EncodeFrame(hello)
	if err != nil {
		t.Fatal(err)
	}
	malformed := append([]byte(nil), wire[:len(wire)-1]...)
	malformed = append(malformed, []byte(`,"`+secretKey+`":true}`)...)
	_, err = DecodeFrame(malformed)
	if err == nil || strings.Contains(err.Error(), secretKey) {
		t.Fatalf("unknown key was accepted or reflected: %v", err)
	}
}

func TestFixedProfilesRejectArbitraryHTTPAndMutationDispatch(t *testing.T) {
	meta := FrameMeta{
		GatewayID: "gateway-1", DeviceID: "device-1", OperationID: "operation-1",
		ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: "boot-1", LeaseEpoch: 1, Sequence: 1,
	}
	probe, err := NewFrame(FrameOperationProbe, meta, OperationProbePayload{})
	if err != nil {
		t.Fatalf("new operation probe: %v", err)
	}

	mutations := []struct {
		name   string
		mutate func(*Frame)
	}{
		{"POST", func(f *Frame) { f.Method = "POST" }},
		{"arbitrary path", func(f *Frame) { f.Path = "/api/remote/v1/sms/send" }},
		{"mutation action", func(f *Frame) { f.Action = "sms.send" }},
		{"missing operation", func(f *Frame) { f.OperationID = "" }},
		{"both client identities", func(f *Frame) { f.Principal = "cf:issuer:subject" }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := probe
			test.mutate(&candidate)
			if err := validateFrame(candidate, false); err == nil {
				t.Fatal("mutation-like frame was accepted")
			}
		})
	}
	if _, err := NewFrame(FrameType("operation.execute"), meta, OperationProbePayload{}); !errors.Is(err, ErrUnsupportedFrame) {
		t.Fatalf("unsupported frame error = %v", err)
	}
}

func TestPayloadAllowListsAndTimeBounds(t *testing.T) {
	validSnapshot := SnapshotPayload{
		ObservedAt: fixedTestTime.Unix(), Revision: 1, EventHighWater: 0,
		Service: ServiceReady, Call: CallIdle, SMS: SMSReady,
	}
	if err := validSnapshot.Validate(); err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	invalidSnapshots := []SnapshotPayload{
		{},
		{ObservedAt: fixedTestTime.Unix(), Revision: 1, Service: "executing", Call: CallIdle, SMS: SMSReady},
		{ObservedAt: fixedTestTime.Unix(), Revision: 1, Service: ServiceReady, Call: "dial", SMS: SMSReady},
	}
	for index := range invalidSnapshots {
		if err := invalidSnapshots[index].Validate(); err == nil {
			t.Fatalf("invalid snapshot %d accepted", index)
		}
	}
	overflowSnapshot := validSnapshot
	overflowSnapshot.Revision = MaxWireCounter + 1
	if err := overflowSnapshot.Validate(); err == nil {
		t.Fatal("JSON-unsafe snapshot counter was accepted")
	}

	results := []struct {
		payload OperationResultPayload
		valid   bool
	}{
		{OperationResultPayload{ObservedAt: fixedTestTime.Unix(), State: OperationPending}, true},
		{OperationResultPayload{ObservedAt: fixedTestTime.Unix(), State: OperationSucceeded, Final: true}, true},
		{OperationResultPayload{ObservedAt: fixedTestTime.Unix(), State: OperationUnknown}, true},
		{OperationResultPayload{ObservedAt: fixedTestTime.Unix(), State: OperationPending, Final: true}, false},
		{OperationResultPayload{ObservedAt: fixedTestTime.Unix(), State: "retry", Final: false}, false},
	}
	for index, test := range results {
		if got := test.payload.Validate() == nil; got != test.valid {
			t.Fatalf("result %d validity = %v, want %v", index, got, test.valid)
		}
	}

	hello := testHello(t, "gateway-1", "boot-1", fixedTestTime.Add(time.Minute))
	if err := validateAt(hello, fixedTestTime, DefaultMessageTTL); err != nil {
		t.Fatalf("fresh frame: %v", err)
	}
	hello.ExpiresAt = fixedTestTime.Unix()
	if !errors.Is(validateAt(hello, fixedTestTime, DefaultMessageTTL), ErrExpired) {
		t.Fatal("expired frame was accepted")
	}
	hello.ExpiresAt = fixedTestTime.Add(DefaultMessageTTL + time.Second).Unix()
	if err := validateAt(hello, fixedTestTime, DefaultMessageTTL); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("overlong frame TTL error = %v", err)
	}
}

func TestWireCountersStayJSONExact(t *testing.T) {
	meta := FrameMeta{
		GatewayID: "gateway-1", DeviceID: "device-1", OperationID: "operation-1",
		ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: "boot-1",
		LeaseEpoch: MaxWireCounter, Sequence: MaxWireCounter,
	}
	if _, err := NewFrame(FrameOperationProbe, meta, OperationProbePayload{}); err != nil {
		t.Fatalf("maximum exact JSON counter rejected: %v", err)
	}
	meta.Sequence++
	if _, err := NewFrame(FrameOperationProbe, meta, OperationProbePayload{}); err == nil {
		t.Fatal("JSON-unsafe sequence was accepted")
	}
	meta.Sequence = 1
	meta.LeaseEpoch++
	if _, err := NewFrame(FrameOperationProbe, meta, OperationProbePayload{}); err == nil {
		t.Fatal("JSON-unsafe lease epoch was accepted")
	}
}

func TestFrameAndInputFormattingIsRedacted(t *testing.T) {
	key := testP256Key(t)
	bindingBytes := bytes.Repeat([]byte{7}, 32)
	binding := base64Raw(bindingBytes)
	frame, err := NewFrame(FrameOperationProbe, FrameMeta{
		GatewayID: "gateway-secret", DeviceID: "device-secret", OperationID: "operation-secret",
		ExpiresAt: fixedTestTime.Add(time.Minute).Unix(), BootID: "boot-secret", LeaseEpoch: 4, Sequence: 9,
	}, OperationProbePayload{})
	if err != nil {
		t.Fatal(err)
	}
	frame, err = SignClientFrame(key, frame, binding)
	if err != nil {
		t.Fatal(err)
	}
	input, err := ClientSigningInput(frame, binding)
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{
		"frame String": frame.String(), "frame GoString": fmt.Sprintf("%#v", frame),
		"frame JSON": string(mustJSON(t, frame)), "input String": input.String(),
		"input GoString": fmt.Sprintf("%#v", input), "input JSON": string(mustJSON(t, input)),
	} {
		for _, secret := range []string{"gateway-secret", "device-secret", "operation-secret", "boot-secret", binding, frame.Signature} {
			if strings.Contains(output, secret) {
				t.Fatalf("%s leaked %q: %s", name, secret, output)
			}
		}
		if !strings.Contains(output, redactedValue) {
			t.Fatalf("%s did not visibly redact: %s", name, output)
		}
	}
	meta := FrameMeta{
		GatewayID: "gateway-secret", DeviceID: "device-secret", Principal: "cf:issuer:subject",
		OperationID: "operation-secret", BootID: "boot-secret", LeaseEpoch: 4,
	}
	lease := Lease{
		GatewayID: "gateway-secret", BootID: "boot-secret", LeaseEpoch: 4, ChannelBinding: binding,
	}
	for name, output := range map[string]string{
		"meta String": meta.String(), "meta GoString": fmt.Sprintf("%#v", meta),
		"meta JSON": string(mustJSON(t, meta)), "lease String": lease.String(),
		"lease GoString": fmt.Sprintf("%#v", lease), "lease JSON": string(mustJSON(t, lease)),
	} {
		for _, secret := range []string{"gateway-secret", "device-secret", "cf:issuer:subject", "operation-secret", "boot-secret", binding} {
			if strings.Contains(output, secret) {
				t.Fatalf("%s leaked %q: %s", name, secret, output)
			}
		}
	}
}

func TestChallengePayloadRequiresExplicitFrameEncoding(t *testing.T) {
	binding := testBinding(9)
	payload := ChallengePayload{Challenge: binding}
	ordinary := string(mustJSON(t, payload))
	if strings.Contains(ordinary, binding) || !strings.Contains(ordinary, redactedValue) {
		t.Fatalf("challenge payload JSON was not redacted: %s", ordinary)
	}
	frame, err := NewFrame(FrameEdgeChallenge, FrameMeta{
		GatewayID: "gateway-1", BootID: "boot-1", LeaseEpoch: 1,
		ExpiresAt: fixedTestTime.Add(time.Minute).Unix(),
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire, []byte(binding)) {
		t.Fatalf("explicit wire encoding omitted challenge: %s", wire)
	}
}

func TestRegistryConfigurationFormattingRedactsIdentityKeys(t *testing.T) {
	key := testP256Key(t)
	config := RegistryConfig{
		GatewayKeys:   map[string]*ecdsa.PublicKey{"private-gateway": &key.PublicKey},
		DeviceKeys:    map[string]*ecdsa.PublicKey{"private-device": &key.PublicKey},
		PrincipalKeys: map[string]*ecdsa.PublicKey{"issuer:private-subject": &key.PublicKey},
		ChallengeTTL:  time.Second, MessageTTL: time.Second,
	}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{
		config.String(), fmt.Sprintf("%#v", config), string(mustJSON(t, config)),
		registry.String(), fmt.Sprintf("%#v", registry), string(mustJSON(t, registry)),
	} {
		for _, secret := range []string{"private-gateway", "private-device", "private-subject"} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("registry formatting leaked %q: %s", secret, rendered)
			}
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func base64Raw(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var result strings.Builder
	for index := 0; index < len(data); index += 3 {
		remaining := len(data) - index
		value := uint(data[index]) << 16
		if remaining > 1 {
			value |= uint(data[index+1]) << 8
		}
		if remaining > 2 {
			value |= uint(data[index+2])
		}
		result.WriteByte(alphabet[(value>>18)&63])
		result.WriteByte(alphabet[(value>>12)&63])
		if remaining > 1 {
			result.WriteByte(alphabet[(value>>6)&63])
		}
		if remaining > 2 {
			result.WriteByte(alphabet[value&63])
		}
	}
	return result.String()
}

func FuzzDecodeFrame(f *testing.F) {
	hello, err := NewFrame(FrameGatewayHello, FrameMeta{
		GatewayID: "gateway-1", BootID: "boot-1", ExpiresAt: fixedTestTime.Add(time.Minute).Unix(),
	}, GatewayHelloPayload{})
	if err != nil {
		f.Fatal(err)
	}
	wire, err := EncodeFrame(hello)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(wire)
	f.Add([]byte(`{"version":1,"type":"http.proxy"}`))
	f.Add(bytes.Repeat([]byte{'{'}, 32))
	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := DecodeFrame(data)
		if err != nil {
			return
		}
		reencoded, err := EncodeFrame(frame)
		if err != nil {
			t.Fatalf("decoded frame could not be encoded: %v", err)
		}
		if len(reencoded) > MaxFrameBytes {
			t.Fatalf("encoded frame exceeded bound: %d", len(reencoded))
		}
	})
}
