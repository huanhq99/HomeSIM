package publicrelay

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type goldenTrafficVector struct {
	AADHex                string `json:"aad_hex"`
	CiphertextBase64URL   string `json:"ciphertext_b64url"`
	InnerJSONBase64URL    string `json:"inner_json_b64url"`
	IssuedAt              int64  `json:"issued_at"`
	MessageID             string `json:"message_id"`
	NonceHex              string `json:"nonce_hex"`
	NoncePrefixHex        string `json:"nonce_prefix_hex"`
	PaddedPlaintextBase64 string `json:"padded_plaintext_b64url"`
	ProofDigestHex        string `json:"proof_digest_hex"`
	Sequence              uint64 `json:"sequence"`
	SignatureBase64URL    string `json:"signature_b64url"`
	WireEnvelopeBase64URL string `json:"wire_envelope_b64url"`
}

type goldenPublicKey struct {
	SEC1Hex       string `json:"sec1_hex"`
	SPKIDERHex    string `json:"spki_der_hex"`
	SPKISHA256Hex string `json:"spki_sha256_hex"`
}

type goldenVectors struct {
	C2G         goldenTrafficVector `json:"c2g"`
	DerivedKeys struct {
		BindHex    string `json:"bind_hex"`
		C2GAEADHex string `json:"c2g_aead_hex"`
		G2CAEADHex string `json:"g2c_aead_hex"`
	} `json:"derived_keys"`
	DevicePublicKey  goldenPublicKey     `json:"device_public_key"`
	G2CFencedAbsent  goldenTrafficVector `json:"g2c_fenced_absent"`
	GatewayPublicKey goldenPublicKey     `json:"gateway_public_key"`
	KDF              struct {
		PRKHex       string `json:"prk_hex"`
		SaltHex      string `json:"salt_hex"`
		SaltInputHex string `json:"salt_input_hex"`
	} `json:"kdf"`
	KeyEpoch        uint64 `json:"key_epoch"`
	Note            string `json:"note"`
	ProtocolVersion uint64 `json:"protocol_version"`
	RootKeyHex      string `json:"root_key_hex"`
	RouteID         string `json:"route_id"`
	SMSSend         struct {
		CommitmentBase64URL string `json:"commitment_b64url"`
		Destination         string `json:"destination"`
		Message             string `json:"message"`
		OperationID         string `json:"operation_id"`
	} `json:"sms_send"`
	Unicode struct {
		DecodedUTF8Hex           string `json:"decoded_utf8_hex"`
		InvalidHighJSONBase64URL string `json:"invalid_high_json_b64url"`
		InvalidLowJSONBase64URL  string `json:"invalid_low_json_b64url"`
		RawJSONBase64URL         string `json:"raw_json_b64url"`
		SurrogateJSONBase64URL   string `json:"surrogate_json_b64url"`
	} `json:"unicode"`
}

var goldenVectorKeys = exactKeySet(
	"c2g", "derived_keys", "device_public_key", "g2c_fenced_absent",
	"gateway_public_key", "kdf", "key_epoch", "note", "protocol_version",
	"root_key_hex", "route_id", "sms_send", "unicode",
)

var (
	goldenTrafficKeys = exactKeySet(
		"aad_hex", "ciphertext_b64url", "inner_json_b64url", "issued_at",
		"message_id", "nonce_hex", "nonce_prefix_hex", "padded_plaintext_b64url",
		"proof_digest_hex", "sequence", "signature_b64url", "wire_envelope_b64url",
	)
	goldenDerivedKeyKeys = exactKeySet("bind_hex", "c2g_aead_hex", "g2c_aead_hex")
	goldenPublicKeyKeys  = exactKeySet("sec1_hex", "spki_der_hex", "spki_sha256_hex")
	goldenKDFKeys        = exactKeySet("prk_hex", "salt_hex", "salt_input_hex")
	goldenSMSSendKeys    = exactKeySet(
		"commitment_b64url", "destination", "message", "operation_id",
	)
	goldenUnicodeKeys = exactKeySet(
		"decoded_utf8_hex", "invalid_high_json_b64url", "invalid_low_json_b64url",
		"raw_json_b64url", "surrogate_json_b64url",
	)
)

func decodeGoldenVectors(encoded []byte) (goldenVectors, error) {
	var raw map[string]json.RawMessage
	if err := decodeExactJSON(encoded, &raw, goldenVectorKeys, MaxJSONDepth); err != nil {
		return goldenVectors{}, err
	}
	for name, keys := range map[string]map[string]struct{}{
		"c2g":                goldenTrafficKeys,
		"derived_keys":       goldenDerivedKeyKeys,
		"device_public_key":  goldenPublicKeyKeys,
		"g2c_fenced_absent":  goldenTrafficKeys,
		"gateway_public_key": goldenPublicKeyKeys,
		"kdf":                goldenKDFKeys,
		"sms_send":           goldenSMSSendKeys,
		"unicode":            goldenUnicodeKeys,
	} {
		var nested map[string]json.RawMessage
		if err := decodeExactJSON(raw[name], &nested, keys, MaxJSONDepth); err != nil {
			return goldenVectors{}, err
		}
	}
	var vectors goldenVectors
	if err := decodeExactJSON(encoded, &vectors, goldenVectorKeys, MaxJSONDepth); err != nil {
		return goldenVectors{}, err
	}
	if !goldenRequiredStringsPresent(vectors) {
		return goldenVectors{}, ErrMalformed
	}
	return vectors, nil
}

func goldenRequiredStringsPresent(vectors goldenVectors) bool {
	required := []string{
		vectors.Note, vectors.RootKeyHex, vectors.RouteID,
		vectors.DerivedKeys.BindHex, vectors.DerivedKeys.C2GAEADHex,
		vectors.DerivedKeys.G2CAEADHex,
		vectors.DevicePublicKey.SEC1Hex, vectors.DevicePublicKey.SPKIDERHex,
		vectors.DevicePublicKey.SPKISHA256Hex,
		vectors.GatewayPublicKey.SEC1Hex, vectors.GatewayPublicKey.SPKIDERHex,
		vectors.GatewayPublicKey.SPKISHA256Hex,
		vectors.KDF.PRKHex, vectors.KDF.SaltHex, vectors.KDF.SaltInputHex,
		vectors.SMSSend.CommitmentBase64URL, vectors.SMSSend.Destination,
		vectors.SMSSend.Message, vectors.SMSSend.OperationID,
		vectors.Unicode.DecodedUTF8Hex, vectors.Unicode.InvalidHighJSONBase64URL,
		vectors.Unicode.InvalidLowJSONBase64URL, vectors.Unicode.RawJSONBase64URL,
		vectors.Unicode.SurrogateJSONBase64URL,
	}
	for _, traffic := range []goldenTrafficVector{vectors.C2G, vectors.G2CFencedAbsent} {
		required = append(required,
			traffic.AADHex, traffic.CiphertextBase64URL, traffic.InnerJSONBase64URL,
			traffic.MessageID, traffic.NonceHex, traffic.NoncePrefixHex,
			traffic.PaddedPlaintextBase64, traffic.ProofDigestHex,
			traffic.SignatureBase64URL, traffic.WireEnvelopeBase64URL,
		)
	}
	for _, value := range required {
		if value == "" {
			return false
		}
	}
	return true
}

func TestGoldenVectorsForJavaAlignment(t *testing.T) {
	encoded, err := os.ReadFile("testdata/golden_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := decodeGoldenVectors(encoded)
	if err != nil {
		t.Fatalf("strictly decode golden vectors: %v", err)
	}
	if vectors.Note != "SYNTHETIC TEST VECTOR ONLY - NOT A CREDENTIAL" ||
		vectors.ProtocolVersion != ProtocolVersion {
		t.Fatal("golden vector provenance or version mismatch")
	}
	rootBytes := mustDecodeHex(t, vectors.RootKeyHex)
	root, err := NewRootKey(rootBytes)
	if err != nil {
		t.Fatal(err)
	}
	saltInput, salt, prk, err := deriveKDFIntermediates(root, vectors.RouteID, vectors.KeyEpoch)
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, "KDF salt input", saltInput, vectors.KDF.SaltInputHex)
	assertHex(t, "KDF salt", salt[:], vectors.KDF.SaltHex)
	assertHex(t, "KDF PRK", prk, vectors.KDF.PRKHex)
	keys, err := DeriveKeySet(root, vectors.RouteID, vectors.KeyEpoch)
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, "c2g key", keys.c2g[:], vectors.DerivedKeys.C2GAEADHex)
	assertHex(t, "g2c key", keys.g2c[:], vectors.DerivedKeys.G2CAEADHex)
	assertHex(t, "bind key", keys.bind[:], vectors.DerivedKeys.BindHex)

	deviceKey := testDeviceKey(t)
	gatewayKey := testGatewayKey(t)
	assertGoldenPublicKey(t, "device", &deviceKey.PublicKey, vectors.DevicePublicKey)
	assertGoldenPublicKey(t, "gateway", &gatewayKey.PublicKey, vectors.GatewayPublicKey)

	intent := SMSSendIntent{
		OperationID: vectors.SMSSend.OperationID,
		Destination: vectors.SMSSend.Destination,
		Message:     vectors.SMSSend.Message,
	}
	commitment, err := ComputeSMSSendCommitment(keys, intent)
	if err != nil {
		t.Fatal(err)
	}
	if commitment.Encode() != vectors.SMSSend.CommitmentBase64URL {
		t.Fatalf("commitment=%s", commitment.Encode())
	}
	send, err := NewSMSSendMessage(intent)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenTraffic(t, keys, ClientToGateway, send, vectors.C2G, deviceKey)

	resultValue, err := NewSMSInspectAndFenceResult(
		intent.OperationID, commitment, OperationFencedAbsent,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewSMSInspectAndFenceResultMessage(resultValue)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenTraffic(
		t, keys, GatewayToClient, result, vectors.G2CFencedAbsent, gatewayKey,
	)
	assertGoldenUnicode(t, vectors)
}

func TestGoldenVectorParserRejectsDuplicateAndTrailingData(t *testing.T) {
	encoded, err := os.ReadFile("testdata/golden_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	duplicate := []byte(strings.Replace(
		string(encoded), `"protocol_version": 2,`, `"protocol_version": 2,"protocol_version": 2,`, 1,
	))
	if _, err := decodeGoldenVectors(duplicate); err == nil {
		t.Fatal("golden parser accepted duplicate key")
	}
	if _, err := decodeGoldenVectors(append(append([]byte(nil), encoded...), []byte(` {}`)...)); err == nil {
		t.Fatal("golden parser accepted trailing JSON value")
	}
}

func TestGoldenVectorParserRequiresEveryNestedField(t *testing.T) {
	encoded, err := os.ReadFile("testdata/golden_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		object string
		field  string
	}{
		{"c2g", "nonce_prefix_hex"},
		{"derived_keys", "bind_hex"},
		{"device_public_key", "spki_der_hex"},
		{"g2c_fenced_absent", "proof_digest_hex"},
		{"gateway_public_key", "sec1_hex"},
		{"kdf", "prk_hex"},
		{"sms_send", "commitment_b64url"},
		{"unicode", "invalid_high_json_b64url"},
		{"unicode", "invalid_low_json_b64url"},
	}
	for _, test := range tests {
		t.Run(test.object+"/"+test.field, func(t *testing.T) {
			missing := mutateGoldenObjectField(t, encoded, test.object, test.field, nil)
			if _, err := decodeGoldenVectors(missing); err == nil {
				t.Fatal("golden parser accepted a missing nested field")
			}
		})
	}
}

func TestGoldenVectorParserRejectsEmptyCriticalStrings(t *testing.T) {
	encoded, err := os.ReadFile("testdata/golden_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	empty := json.RawMessage(`""`)
	for _, test := range []struct {
		object string
		field  string
	}{
		{"c2g", "signature_b64url"},
		{"kdf", "salt_input_hex"},
		{"unicode", "invalid_high_json_b64url"},
	} {
		t.Run(test.object+"/"+test.field, func(t *testing.T) {
			changed := mutateGoldenObjectField(t, encoded, test.object, test.field, &empty)
			if _, err := decodeGoldenVectors(changed); err == nil {
				t.Fatal("golden parser accepted an empty critical string")
			}
		})
	}
}

func mutateGoldenObjectField(
	t *testing.T,
	encoded []byte,
	object string,
	field string,
	replacement *json.RawMessage,
) []byte {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &root); err != nil {
		t.Fatal(err)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(root[object], &nested); err != nil {
		t.Fatal(err)
	}
	if replacement == nil {
		delete(nested, field)
	} else {
		nested[field] = append(json.RawMessage(nil), (*replacement)...)
	}
	nestedEncoded, err := json.Marshal(nested)
	if err != nil {
		t.Fatal(err)
	}
	root[object] = nestedEncoded
	result, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertGoldenTraffic(
	t *testing.T,
	keys KeySet,
	direction Direction,
	message Message,
	vector goldenTrafficVector,
	signingKey *ecdsa.PrivateKey,
) {
	t.Helper()
	meta := EnvelopeMeta{
		RouteID: keys.routeID, KeyEpoch: keys.keyEpoch, Direction: direction,
		Sequence: vector.Sequence, MessageID: vector.MessageID, IssuedAt: vector.IssuedAt,
	}
	inner, err := encodeInnerMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	assertBase64(t, "inner JSON", inner, vector.InnerJSONBase64URL)
	padded, err := padMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	assertBase64(t, "padded plaintext", padded, vector.PaddedPlaintextBase64)
	aad, err := canonicalAAD(meta)
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, "AAD", aad, vector.AADHex)
	key, err := trafficKey(keys, direction)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := deriveNoncePrefix(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, "nonce prefix", prefix[:], vector.NoncePrefixHex)
	nonce, err := deriveNonce(key, meta)
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, "nonce", nonce, vector.NonceHex)
	envelope, err := sealUnsigned(keys, meta, message)
	if err != nil {
		t.Fatal(err)
	}
	assertBase64(t, "ciphertext", envelope.ciphertext, vector.CiphertextBase64URL)
	envelope.signature = vector.SignatureBase64URL
	var digest [32]byte
	if direction == ClientToGateway {
		digest, err = deviceProofDigest(envelope)
	} else {
		digest, err = gatewayProofDigest(envelope)
	}
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, "proof digest", digest[:], vector.ProofDigestHex)

	if direction == ClientToGateway {
		if err := verifyDeviceEnvelope(&signingKey.PublicKey, envelope); err != nil {
			t.Fatalf("verify fixed device signature: %v", err)
		}
	} else if err := verifyGatewayEnvelope(&signingKey.PublicKey, envelope); err != nil {
		t.Fatalf("verify fixed gateway signature: %v", err)
	}
	wire, err := EncodeEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertBase64(t, "wire envelope", wire, vector.WireEnvelopeBase64URL)
	decoded, err := DecodeEnvelope(wire)
	if err != nil || decoded.meta != envelope.meta || !bytes.Equal(decoded.ciphertext, envelope.ciphertext) {
		t.Fatalf("golden wire round trip err=%v", err)
	}
}

func assertGoldenPublicKey(
	t *testing.T,
	name string,
	publicKey *ecdsa.PublicKey,
	vector goldenPublicKey,
) {
	t.Helper()
	assertHex(t, name+" SEC1", elliptic.Marshal(elliptic.P256(), publicKey.X, publicKey.Y), vector.SEC1Hex)
	_, fingerprint, spki, err := cloneVerifierKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	assertHex(t, name+" SPKI", spki, vector.SPKIDERHex)
	assertHex(t, name+" SPKI fingerprint", fingerprint[:], vector.SPKISHA256Hex)
}

func assertGoldenUnicode(t *testing.T, vectors goldenVectors) {
	t.Helper()
	type valueWire struct {
		Value string `json:"value"`
	}
	keys := exactKeySet("value")
	var decodedValues []string
	for _, encoded := range []string{
		vectors.Unicode.RawJSONBase64URL, vectors.Unicode.SurrogateJSONBase64URL,
	} {
		wireBytes := mustDecodeBase64(t, encoded)
		var value valueWire
		if err := decodeExactJSON(wireBytes, &value, keys, MaxJSONDepth); err != nil {
			t.Fatalf("decode golden Unicode: %v", err)
		}
		decodedValues = append(decodedValues, value.Value)
	}
	if len(decodedValues) != 2 || decodedValues[0] != decodedValues[1] ||
		hex.EncodeToString([]byte(decodedValues[0])) != vectors.Unicode.DecodedUTF8Hex {
		t.Fatal("golden raw/surrogate Unicode values differ")
	}
	for _, encoded := range []string{
		vectors.Unicode.InvalidHighJSONBase64URL, vectors.Unicode.InvalidLowJSONBase64URL,
	} {
		var value valueWire
		if err := decodeExactJSON(mustDecodeBase64(t, encoded), &value, keys, MaxJSONDepth); err == nil {
			t.Fatal("golden isolated surrogate accepted")
		}
	}
}

func mustDecodeHex(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func mustDecodeBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		t.Fatalf("invalid golden base64url")
	}
	return decoded
}

func assertHex(t *testing.T, name string, actual []byte, expected string) {
	t.Helper()
	if hex.EncodeToString(actual) != expected {
		t.Fatalf("%s=%s want=%s", name, hex.EncodeToString(actual), expected)
	}
}

func assertBase64(t *testing.T, name string, actual []byte, expected string) {
	t.Helper()
	if base64.RawURLEncoding.EncodeToString(actual) != expected {
		t.Fatalf("%s mismatch", name)
	}
}
