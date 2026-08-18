package publicedge

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAccessTeam     = "test-team.cloudflareaccess.com"
	testAccessAudience = "phone_audience-1"
	testAccessEmail    = "owner@example.com"
	testAccessSubject  = "7335d417-61da-459d-899c-0a01c76a2f94"
)

var (
	testAccessKeysOnce sync.Once
	testAccessKeys     [3]*rsa.PrivateKey
	testAccessKeysErr  error
)

type accessTestEndpoint struct {
	mu       sync.Mutex
	body     []byte
	status   int
	location string
	calls    int
	urls     []string
	started  chan struct{}
	release  <-chan struct{}
}

func (endpoint *accessTestEndpoint) RoundTrip(request *http.Request) (*http.Response, error) {
	endpoint.mu.Lock()
	endpoint.calls++
	endpoint.urls = append(endpoint.urls, request.URL.String())
	body := append([]byte(nil), endpoint.body...)
	status := endpoint.status
	location := endpoint.location
	started := endpoint.started
	release := endpoint.release
	endpoint.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	if status == 0 {
		status = http.StatusOK
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if location != "" {
		headers.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    request,
	}, nil
}

func (endpoint *accessTestEndpoint) setBody(body []byte) {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	endpoint.body = append([]byte(nil), body...)
}

func (endpoint *accessTestEndpoint) callCount() int {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	return endpoint.calls
}

func (endpoint *accessTestEndpoint) requestedURLs() []string {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()
	return append([]string(nil), endpoint.urls...)
}

func testAccessKey(t *testing.T, index int) *rsa.PrivateKey {
	t.Helper()
	testAccessKeysOnce.Do(func() {
		for keyIndex := range testAccessKeys {
			testAccessKeys[keyIndex], testAccessKeysErr = rsa.GenerateKey(rand.Reader, 2048)
			if testAccessKeysErr != nil {
				return
			}
		}
	})
	if testAccessKeysErr != nil {
		t.Fatalf("generate RSA test keys: %v", testAccessKeysErr)
	}
	return testAccessKeys[index]
}

func testAccessClock(unix int64) (*atomic.Int64, func() time.Time) {
	clock := &atomic.Int64{}
	clock.Store(unix)
	return clock, func() time.Time { return time.Unix(clock.Load(), 0) }
}

func testAccessClaims(now int64) map[string]any {
	return map[string]any{
		"aud":            []string{testAccessAudience},
		"email":          testAccessEmail,
		"exp":            now + 300,
		"iat":            now - 5,
		"nbf":            now - 5,
		"iss":            "https://" + testAccessTeam,
		"type":           "app",
		"identity_nonce": "identity-nonce-1",
		"sub":            testAccessSubject,
		"country":        "CN",
	}
}

func testAccessJWT(t *testing.T, key *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return testAccessRawJWT(t, key, header, payload)
}

func testAccessRawJWT(t *testing.T, key *rsa.PrivateKey, header, payload []byte) string {
	t.Helper()
	headerPart := base64.RawURLEncoding.EncodeToString(header)
	payloadPart := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(headerPart + "." + payloadPart))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign test JWT: %v", err)
	}
	return headerPart + "." + payloadPart + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func testAccessJWK(key *rsa.PrivateKey, keyID string) map[string]any {
	exponent := []byte{byte(key.E >> 16), byte(key.E >> 8), byte(key.E)}
	return map[string]any{
		"kid": keyID, "kty": "RSA", "alg": "RS256", "use": "sig",
		"e": base64.RawURLEncoding.EncodeToString(exponent),
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
	}
}

func testAccessJWKS(t *testing.T, entries ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"keys": entries,
		"public_cert": map[string]any{
			"kid": entries[0]["kid"], "cert": "-----BEGIN CERTIFICATE-----\nredacted\n-----END CERTIFICATE-----",
		},
		"public_certs": []map[string]any{{
			"kid": entries[0]["kid"], "cert": "-----BEGIN CERTIFICATE-----\nredacted\n-----END CERTIFICATE-----",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func newAccessTestVerifier(t *testing.T, endpoint *accessTestEndpoint, now int64) (*AccessVerifier, *atomic.Int64) {
	t.Helper()
	clock, nowFunc := testAccessClock(now)
	verifier, err := NewAccessVerifier(AccessConfig{
		TeamDomain: testAccessTeam, Audience: testAccessAudience,
		AllowedEmails: []string{testAccessEmail},
		HTTPClient:    &http.Client{Transport: endpoint, Timeout: time.Minute},
		Clock:         nowFunc, CacheTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("new access verifier: %v", err)
	}
	return verifier, clock
}

func testAccessRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "https://phone.example.com/status", nil)
	if token != "" {
		request.Header.Set(AccessAssertionHeader, token)
	}
	return request
}

func TestAccessVerifierAcceptsOnlySignedAllowedIdentity(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key, "key-1"))}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	token := testAccessJWT(t, key, "key-1", testAccessClaims(now))
	request := testAccessRequest(token)
	request.Header.Set("Cf-Access-Authenticated-User-Email", "attacker@example.com")
	request.Header.Set("Forwarded", "for=127.0.0.1;host=trusted")
	request.Header.Set("X-Forwarded-User", "attacker@example.com")
	request.Header.Set("X-Tailscale-User-Login", "attacker@example.com")

	identity, err := verifier.VerifyRequest(request)
	if err != nil {
		t.Fatalf("verify valid Access assertion: %v", err)
	}
	if identity.Email != testAccessEmail || identity.Subject != testAccessSubject {
		t.Fatalf("verified identity mismatch: %#v", identity)
	}
	urls := endpoint.requestedURLs()
	wantURL := "https://" + testAccessTeam + "/cdn-cgi/access/certs"
	if len(urls) != 1 || urls[0] != wantURL {
		t.Fatalf("JWKS requests = %v, want exactly %q", urls, wantURL)
	}

	if _, err := verifier.VerifyRequest(testAccessRequest(token)); err != nil {
		t.Fatalf("verify using cached key: %v", err)
	}
	if endpoint.callCount() != 1 {
		t.Fatalf("cached verification made %d JWKS calls, want 1", endpoint.callCount())
	}
}

func TestAccessVerifierAcceptsCloudflareHeaderWithoutOptionalType(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key, "key-1"))}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "key-1"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(testAccessClaims(now))
	if err != nil {
		t.Fatal(err)
	}

	identity, err := verifier.VerifyRequest(testAccessRequest(testAccessRawJWT(t, key, header, claims)))
	if err != nil {
		t.Fatalf("verify Access assertion without optional typ: %v", err)
	}
	if identity.Email != testAccessEmail || identity.Subject != testAccessSubject {
		t.Fatalf("verified identity mismatch: %#v", identity)
	}
}

func TestAccessVerifierAcceptsSignedIgnoredExtensionClaims(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key, "key-1"))}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	claims := testAccessClaims(now)
	claims["custom"] = map[string]any{"groups": []string{"owner"}}
	claims["future_cloudflare_claim"] = true

	identity, err := verifier.VerifyRequest(testAccessRequest(testAccessJWT(t, key, "key-1", claims)))
	if err != nil {
		t.Fatalf("verify signed Access extension claims: %v", err)
	}
	if identity.Email != testAccessEmail || identity.Subject != testAccessSubject {
		t.Fatalf("verified identity mismatch: %#v", identity)
	}
}

func TestAccessFailureCodeIsBoundedAndRedacted(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "staged", err: rejectAccess("claims_schema"), want: "claims_schema"},
		{name: "unavailable", err: fmt.Errorf("wrapped: %w", ErrAccessUnavailable), want: "keys_unavailable"},
		{name: "legacy unauthorized", err: ErrAccessUnauthorized, want: "unauthorized"},
		{name: "other", err: errors.New("private marker"), want: "verification_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AccessFailureCode(test.err); got != test.want {
				t.Fatalf("AccessFailureCode()=%q want %q", got, test.want)
			}
			if strings.Contains(AccessFailureCode(test.err), "private") {
				t.Fatal("diagnostic code reflected error contents")
			}
		})
	}
}

func TestAccessVerifierRequiresExactlyOneAssertionHeader(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key, "key-1"))}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	token := testAccessJWT(t, key, "key-1", testAccessClaims(now))

	tests := map[string]func(*http.Request){
		"missing assertion with spoofed identity": func(request *http.Request) {
			request.Header.Set("Cf-Access-Authenticated-User-Email", testAccessEmail)
			request.Header.Set("X-Forwarded-Email", testAccessEmail)
		},
		"authorization cookie only": func(request *http.Request) {
			request.AddCookie(&http.Cookie{Name: "CF_Authorization", Value: token})
		},
		"two field values": func(request *http.Request) {
			request.Header[http.CanonicalHeaderKey(AccessAssertionHeader)] = []string{token, token}
		},
		"case variant semantic duplicate": func(request *http.Request) {
			request.Header[AccessAssertionHeader] = []string{token}
			request.Header[strings.ToLower(AccessAssertionHeader)] = []string{token}
		},
		"comma coalesced values": func(request *http.Request) {
			request.Header.Set(AccessAssertionHeader, token+","+token)
		},
		"leading whitespace": func(request *http.Request) {
			request.Header.Set(AccessAssertionHeader, " "+token)
		},
		"empty": func(request *http.Request) {
			request.Header[AccessAssertionHeader] = []string{""}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := testAccessRequest("")
			mutate(request)
			if _, err := verifier.VerifyRequest(request); !errors.Is(err, ErrAccessUnauthorized) {
				t.Fatalf("error = %v, want unauthorized", err)
			}
		})
	}
	if endpoint.callCount() != 0 {
		t.Fatalf("invalid headers triggered %d JWKS calls", endpoint.callCount())
	}
}

func TestAccessVerifierRejectsJWTHeaderClaimAndSignatureViolations(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	wrongKey := testAccessKey(t, 1)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key, "key-1"))}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	validHeader, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "key-1", "typ": "JWT"})
	validClaims, _ := json.Marshal(testAccessClaims(now))

	claimToken := func(mutate func(map[string]any)) string {
		claims := testAccessClaims(now)
		mutate(claims)
		return testAccessJWT(t, key, "key-1", claims)
	}
	headerToken := func(header map[string]any) string {
		raw, err := json.Marshal(header)
		if err != nil {
			t.Fatal(err)
		}
		return testAccessRawJWT(t, key, raw, validClaims)
	}
	duplicateHeader := bytes.Replace(validHeader, []byte(`"alg":"RS256"`), []byte(`"alg":"RS256","alg":"RS256"`), 1)
	caseHeader := bytes.Replace(validHeader, []byte(`"alg":"RS256"`), []byte(`"Alg":"RS256"`), 1)
	unknownHeader := append(append([]byte(nil), validHeader[:len(validHeader)-1]...), []byte(`,"jku":"https://attacker.invalid/keys"}`)...)
	duplicateClaims := bytes.Replace(validClaims, []byte(`"email":"`+testAccessEmail+`"`), []byte(`"email":"`+testAccessEmail+`","email":"attacker@example.com"`), 1)
	caseClaims := bytes.Replace(validClaims, []byte(`"email":"`+testAccessEmail+`"`), []byte(`"Email":"`+testAccessEmail+`"`), 1)
	fractionalClaims := bytes.Replace(validClaims, []byte(`"exp":2000000300`), []byte(`"exp":2000000300.5`), 1)

	tests := map[string]string{
		"algorithm confusion":     headerToken(map[string]any{"alg": "HS256", "kid": "key-1", "typ": "JWT"}),
		"missing key id":          headerToken(map[string]any{"alg": "RS256", "kid": "", "typ": "JWT"}),
		"wrong token type":        headerToken(map[string]any{"alg": "RS256", "kid": "key-1", "typ": "JWS"}),
		"duplicate header key":    testAccessRawJWT(t, key, duplicateHeader, validClaims),
		"case variant header key": testAccessRawJWT(t, key, caseHeader, validClaims),
		"unknown header key":      testAccessRawJWT(t, key, unknownHeader, validClaims),
		"duplicate claim key":     testAccessRawJWT(t, key, validHeader, duplicateClaims),
		"case variant claim key":  testAccessRawJWT(t, key, validHeader, caseClaims),
		"fractional timestamp":    testAccessRawJWT(t, key, validHeader, fractionalClaims),
		"wrong audience":          claimToken(func(claims map[string]any) { claims["aud"] = []string{"other"} }),
		"extra audience":          claimToken(func(claims map[string]any) { claims["aud"] = []string{testAccessAudience, "other"} }),
		"string audience":         claimToken(func(claims map[string]any) { claims["aud"] = testAccessAudience }),
		"wrong issuer":            claimToken(func(claims map[string]any) { claims["iss"] = "https://other.cloudflareaccess.com" }),
		"issuer trailing slash":   claimToken(func(claims map[string]any) { claims["iss"] = "https://" + testAccessTeam + "/" }),
		"expired":                 claimToken(func(claims map[string]any) { claims["exp"] = now }),
		"not before future":       claimToken(func(claims map[string]any) { claims["nbf"] = now + 1 }),
		"issued future":           claimToken(func(claims map[string]any) { claims["iat"] = now + 1 }),
		"missing not before":      claimToken(func(claims map[string]any) { delete(claims, "nbf") }),
		"expiry before issuance":  claimToken(func(claims map[string]any) { claims["exp"] = now - 1 }),
		"email not allowed":       claimToken(func(claims map[string]any) { claims["email"] = "attacker@example.com" }),
		"empty subject":           claimToken(func(claims map[string]any) { claims["sub"] = "" }),
		"empty identity nonce":    claimToken(func(claims map[string]any) { claims["identity_nonce"] = "" }),
		"service token shape": claimToken(func(claims map[string]any) {
			delete(claims, "email")
			claims["sub"] = ""
		}),
		"organization token": claimToken(func(claims map[string]any) { claims["type"] = "org" }),
		"invalid country":    claimToken(func(claims map[string]any) { claims["country"] = "cn" }),
		"wrong signature":    testAccessJWT(t, wrongKey, "key-1", testAccessClaims(now)),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := verifier.VerifyRequest(testAccessRequest(token))
			if !errors.Is(err, ErrAccessUnauthorized) {
				t.Fatalf("error = %v, want unauthorized", err)
			}
			for _, secret := range []string{"attacker@example.com", "private_claim", "attacker.invalid"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error reflected attacker-controlled value: %v", err)
				}
			}
		})
	}
}

func TestAccessVerifierStrictJWKSetParsing(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	token := testAccessJWT(t, key, "key-1", testAccessClaims(now))
	jwk, err := json.Marshal(testAccessJWK(key, "key-1"))
	if err != nil {
		t.Fatal(err)
	}
	duplicateTop := []byte(`{"keys":[` + string(jwk) + `],"keys":[` + string(jwk) + `]}`)
	caseTop := []byte(`{"Keys":[` + string(jwk) + `]}`)
	unknownTop := []byte(`{"keys":[` + string(jwk) + `],"private_jwks":"do-not-reflect"}`)
	duplicateNested := []byte(`{"keys":[` + strings.Replace(string(jwk), `"kid":"key-1"`, `"kid":"key-1","kid":"key-2"`, 1) + `]}`)
	caseNested := []byte(`{"keys":[` + strings.Replace(string(jwk), `"kid":"key-1"`, `"Kid":"key-1"`, 1) + `]}`)
	unknownNested := []byte(`{"keys":[` + strings.TrimSuffix(string(jwk), "}") + `,"private_jwk":"do-not-reflect"}]}`)
	badAlgorithm := []byte(`{"keys":[` + strings.Replace(string(jwk), `"alg":"RS256"`, `"alg":"RS512"`, 1) + `]}`)
	badUse := []byte(`{"keys":[` + strings.Replace(string(jwk), `"use":"sig"`, `"use":"enc"`, 1) + `]}`)
	badType := []byte(`{"keys":[` + strings.Replace(string(jwk), `"kty":"RSA"`, `"kty":"EC"`, 1) + `]}`)
	badExponent := []byte(`{"keys":[` + strings.Replace(string(jwk), `"e":"AQAB"`, `"e":"Aw"`, 1) + `]}`)
	duplicateKID := []byte(`{"keys":[` + string(jwk) + `,` + string(jwk) + `]}`)
	badCert := []byte(`{"keys":[` + string(jwk) + `],"public_cert":{"kid":"key-1","cert":"pem","Cert":"pem"}}`)

	tests := map[string][]byte{
		"malformed":               []byte(`{"keys":`),
		"empty keys":              []byte(`{"keys":[]}`),
		"duplicate top key":       duplicateTop,
		"case variant top key":    caseTop,
		"unknown top key":         unknownTop,
		"duplicate nested key":    duplicateNested,
		"case variant nested key": caseNested,
		"unknown nested key":      unknownNested,
		"wrong algorithm":         badAlgorithm,
		"wrong use":               badUse,
		"wrong key type":          badType,
		"wrong exponent":          badExponent,
		"duplicate key id":        duplicateKID,
		"noncanonical cert key":   badCert,
		"null current cert":       []byte(`{"keys":[` + string(jwk) + `],"public_cert":null}`),
		"null cert array":         []byte(`{"keys":[` + string(jwk) + `],"public_certs":null}`),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			endpoint := &accessTestEndpoint{body: body}
			verifier, _ := newAccessTestVerifier(t, endpoint, now)
			_, err := verifier.VerifyRequest(testAccessRequest(token))
			if !errors.Is(err, ErrAccessUnavailable) {
				t.Fatalf("error = %v, want key unavailable", err)
			}
			if strings.Contains(err.Error(), "private_jwks") || strings.Contains(err.Error(), "key-1") {
				t.Fatalf("JWK material or key id was reflected: %v", err)
			}
		})
	}
}

func TestAccessVerifierCachesRefreshesDriftAndNegativeKID(t *testing.T) {
	now := int64(2_000_000_000)
	key1 := testAccessKey(t, 0)
	key2 := testAccessKey(t, 1)
	key3 := testAccessKey(t, 2)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key1, "key-1"))}
	verifier, clock := newAccessTestVerifier(t, endpoint, now)

	verify := func(key *rsa.PrivateKey, keyID string) error {
		_, err := verifier.VerifyRequest(testAccessRequest(testAccessJWT(t, key, keyID, testAccessClaims(clock.Load()))))
		return err
	}
	if err := verify(key1, "key-1"); err != nil {
		t.Fatal(err)
	}
	if err := verify(key1, "key-1"); err != nil {
		t.Fatal(err)
	}
	if endpoint.callCount() != 1 {
		t.Fatalf("initial cache calls = %d, want 1", endpoint.callCount())
	}

	endpoint.setBody(testAccessJWKS(t, testAccessJWK(key2, "key-2")))
	if err := verify(key2, "key-2"); err != nil {
		t.Fatalf("verify after key drift: %v", err)
	}
	if endpoint.callCount() != 2 {
		t.Fatalf("unknown rotated key refresh calls = %d, want 2", endpoint.callCount())
	}
	if err := verify(key2, "key-2"); err != nil || endpoint.callCount() != 2 {
		t.Fatalf("rotated key was not cached: err=%v calls=%d", err, endpoint.callCount())
	}

	if err := verify(key3, "unknown-key"); !errors.Is(err, ErrAccessUnauthorized) {
		t.Fatalf("unknown kid error = %v", err)
	}
	if endpoint.callCount() != 3 {
		t.Fatalf("unknown kid made %d calls, want one refresh (3 total)", endpoint.callCount())
	}
	if err := verify(key3, "unknown-key"); !errors.Is(err, ErrAccessUnauthorized) || endpoint.callCount() != 3 {
		t.Fatalf("negative kid was not cached: err=%v calls=%d", err, endpoint.callCount())
	}

	endpoint.setBody(testAccessJWKS(t, testAccessJWK(key3, "unknown-key")))
	clock.Add(int64((accessUnknownKIDCooldown(time.Minute) + time.Second) / time.Second))
	if err := verify(key3, "unknown-key"); err != nil {
		t.Fatalf("newly published key remained negatively cached: %v", err)
	}
	if endpoint.callCount() != 4 {
		t.Fatalf("post-cooldown unknown-key refresh calls = %d, want 4", endpoint.callCount())
	}

	endpoint.setBody(testAccessJWKS(t, testAccessJWK(key2, "key-2")))
	clock.Add(int64((time.Minute + time.Second) / time.Second))
	if err := verify(key2, "key-2"); err != nil {
		t.Fatalf("verify after cache expiry: %v", err)
	}
	if endpoint.callCount() != 5 {
		t.Fatalf("expired cache calls = %d, want 5", endpoint.callCount())
	}
}

func TestAccessVerifierCollapsesConcurrentRefreshes(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	endpoint := &accessTestEndpoint{
		body:    testAccessJWKS(t, testAccessJWK(key, "key-1")),
		started: started, release: release,
	}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	token := testAccessJWT(t, key, "key-1", testAccessClaims(now))

	const workers = 48
	errorsFound := make(chan error, workers)
	for range workers {
		go func() {
			_, err := verifier.VerifyRequest(testAccessRequest(token))
			errorsFound <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("JWKS refresh did not start")
	}
	close(release)
	for range workers {
		if err := <-errorsFound; err != nil {
			t.Fatalf("concurrent verify: %v", err)
		}
	}
	if endpoint.callCount() != 1 {
		t.Fatalf("concurrent verification made %d refreshes, want 1", endpoint.callCount())
	}
}

func TestAccessVerifierCollapsesConcurrentUnknownKIDRefresh(t *testing.T) {
	now := int64(2_000_000_000)
	key1 := testAccessKey(t, 0)
	key2 := testAccessKey(t, 1)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key1, "key-1"))}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	if _, err := verifier.VerifyRequest(testAccessRequest(testAccessJWT(t, key1, "key-1", testAccessClaims(now)))); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	endpoint.mu.Lock()
	endpoint.started = started
	endpoint.release = release
	endpoint.mu.Unlock()
	unknownToken := testAccessJWT(t, key2, "unknown-key", testAccessClaims(now))

	const workers = 48
	errorsFound := make(chan error, workers)
	for range workers {
		go func() {
			_, err := verifier.VerifyRequest(testAccessRequest(unknownToken))
			errorsFound <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("unknown-kid refresh did not start")
	}
	close(release)
	for range workers {
		if err := <-errorsFound; !errors.Is(err, ErrAccessUnauthorized) {
			t.Fatalf("unknown-kid concurrent error = %v", err)
		}
	}
	if endpoint.callCount() != 2 {
		t.Fatalf("concurrent unknown kid made %d total calls, want 2", endpoint.callCount())
	}
}

func TestAccessVerifierBoundsConcurrentFailureRefreshWaves(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	token := testAccessJWT(t, key, "key-1", testAccessClaims(now))

	for _, test := range []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "fast upstream 5xx", status: http.StatusServiceUnavailable, body: []byte(`{"keys":[]}`)},
		{name: "malformed JWK set", status: http.StatusOK, body: []byte(`{"keys":`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint := &accessTestEndpoint{status: test.status, body: test.body}
			verifier, clock := newAccessTestVerifier(t, endpoint, now)

			runConcurrentWave := func(blockFetch bool) {
				t.Helper()
				var started chan struct{}
				var release chan struct{}
				if blockFetch {
					started = make(chan struct{}, 1)
					release = make(chan struct{})
				}
				endpoint.mu.Lock()
				endpoint.started = started
				endpoint.release = release
				endpoint.mu.Unlock()

				const workers = 48
				start := make(chan struct{})
				errorsFound := make(chan error, workers)
				for range workers {
					go func() {
						<-start
						_, err := verifier.VerifyRequest(testAccessRequest(token))
						errorsFound <- err
					}()
				}
				close(start)
				if blockFetch {
					select {
					case <-started:
					case <-time.After(2 * time.Second):
						close(release)
						t.Fatal("failed JWKS refresh did not start")
					}
					close(release)
				}
				for range workers {
					if err := <-errorsFound; !errors.Is(err, ErrAccessUnavailable) {
						t.Fatalf("failed refresh wave error = %v, want unavailable", err)
					}
				}
			}

			runConcurrentWave(true)
			if calls := endpoint.callCount(); calls != 1 {
				t.Fatalf("first failure wave made %d refreshes, want 1", calls)
			}
			runConcurrentWave(false)
			if calls := endpoint.callCount(); calls != 1 {
				t.Fatalf("cooldown failure wave made %d refreshes, want 1 total", calls)
			}

			clock.Add(int64(accessRefreshFailureCooldown(time.Minute)/time.Second) + 1)
			runConcurrentWave(true)
			if calls := endpoint.callCount(); calls != 2 {
				t.Fatalf("post-cooldown failure wave made %d refreshes, want 2 total", calls)
			}
		})
	}
}

func TestAccessVerifierRejectsRedirectAndOversizedJWKSet(t *testing.T) {
	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	token := testAccessJWT(t, key, "key-1", testAccessClaims(now))

	t.Run("redirect", func(t *testing.T) {
		endpoint := &accessTestEndpoint{
			status: http.StatusFound, location: "https://attacker.invalid/keys",
			body: []byte("redirect secret"),
		}
		verifier, _ := newAccessTestVerifier(t, endpoint, now)
		_, err := verifier.VerifyRequest(testAccessRequest(token))
		if !errors.Is(err, ErrAccessUnavailable) {
			t.Fatalf("redirect error = %v", err)
		}
		if endpoint.callCount() != 1 {
			t.Fatalf("redirect was followed: calls=%d urls=%v", endpoint.callCount(), endpoint.requestedURLs())
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		secretTail := []byte("private-jwk-tail")
		body := append(bytes.Repeat([]byte{'x'}, maxAccessJWKSBytes+1), secretTail...)
		endpoint := &accessTestEndpoint{body: body}
		verifier, _ := newAccessTestVerifier(t, endpoint, now)
		_, err := verifier.VerifyRequest(testAccessRequest(token))
		if !errors.Is(err, ErrAccessUnavailable) || strings.Contains(err.Error(), string(secretTail)) {
			t.Fatalf("oversize error was wrong or leaked body: %v", err)
		}
	})
}

func TestAccessVerifierConfigurationAndDefensiveCopies(t *testing.T) {
	base := AccessConfig{
		TeamDomain: testAccessTeam, Audience: testAccessAudience,
		AllowedEmails: []string{testAccessEmail}, CacheTTL: time.Minute,
	}
	tests := map[string]AccessConfig{
		"scheme in team domain": func() AccessConfig { c := base; c.TeamDomain = "https://" + testAccessTeam; return c }(),
		"uppercase team domain": func() AccessConfig { c := base; c.TeamDomain = "MacCellular.cloudflareaccess.com"; return c }(),
		"non-Cloudflare domain": func() AccessConfig { c := base; c.TeamDomain = "non-cloudflare.example"; return c }(),
		"path in team domain":   func() AccessConfig { c := base; c.TeamDomain += "/path"; return c }(),
		"missing audience":      func() AccessConfig { c := base; c.Audience = ""; return c }(),
		"ambiguous audience":    func() AccessConfig { c := base; c.Audience = "aud other"; return c }(),
		"missing allowlist":     func() AccessConfig { c := base; c.AllowedEmails = nil; return c }(),
		"duplicate email":       func() AccessConfig { c := base; c.AllowedEmails = []string{testAccessEmail, testAccessEmail}; return c }(),
		"display name email":    func() AccessConfig { c := base; c.AllowedEmails = []string{"Owner <owner@example.com>"}; return c }(),
		"too short TTL":         func() AccessConfig { c := base; c.CacheTTL = time.Millisecond; return c }(),
		"too long TTL":          func() AccessConfig { c := base; c.CacheTTL = 25 * time.Hour; return c }(),
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAccessVerifier(config); !errors.Is(err, ErrAccessConfiguration) {
				t.Fatalf("error = %v, want configuration error", err)
			}
		})
	}

	now := int64(2_000_000_000)
	key := testAccessKey(t, 0)
	endpoint := &accessTestEndpoint{body: testAccessJWKS(t, testAccessJWK(key, "key-1"))}
	allowed := []string{testAccessEmail}
	sourceClient := &http.Client{Transport: endpoint, Timeout: time.Hour}
	_, nowFunc := testAccessClock(now)
	verifier, err := NewAccessVerifier(AccessConfig{
		TeamDomain: testAccessTeam, Audience: testAccessAudience, AllowedEmails: allowed,
		HTTPClient: sourceClient, Clock: nowFunc,
	})
	if err != nil {
		t.Fatal(err)
	}
	allowed[0] = "attacker@example.com"
	sourceClient.Timeout = time.Nanosecond
	if verifier.client.Timeout != accessHTTPTimeout {
		t.Fatalf("bounded cloned HTTP timeout = %s, want %s", verifier.client.Timeout, accessHTTPTimeout)
	}
	if _, err := verifier.VerifyRequest(testAccessRequest(testAccessJWT(t, key, "key-1", testAccessClaims(now)))); err != nil {
		t.Fatalf("configuration mutation changed verifier trust: %v", err)
	}
}

func TestAccessVerifierFormattingAndErrorsAreRedacted(t *testing.T) {
	now := int64(2_000_000_000)
	secretTeam := testAccessTeam
	secretAudience := testAccessAudience
	secretEmail := testAccessEmail
	config := AccessConfig{
		TeamDomain: secretTeam, Audience: secretAudience,
		AllowedEmails: []string{secretEmail}, CacheTTL: time.Minute,
	}
	identity := AccessIdentity{Email: secretEmail, Subject: testAccessSubject}
	for name, value := range map[string]any{
		"config":   config,
		"identity": identity,
	} {
		t.Run(name, func(t *testing.T) {
			jsonValue, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			forms := []string{string(jsonValue), fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value)}
			for _, form := range forms {
				for _, secret := range []string{secretTeam, secretAudience, secretEmail, testAccessSubject} {
					if strings.Contains(form, secret) {
						t.Fatalf("%s formatting leaked protected value: %s", name, form)
					}
				}
			}
		})
	}

	key := testAccessKey(t, 0)
	endpoint := &accessTestEndpoint{body: []byte(`{"keys":[],"jwk_secret":"private-public-material"}`)}
	verifier, _ := newAccessTestVerifier(t, endpoint, now)
	token := testAccessJWT(t, key, "private-key-id", func() map[string]any {
		claims := testAccessClaims(now)
		claims["email"] = "jwt-secret@example.com"
		return claims
	}())
	_, err := verifier.VerifyRequest(testAccessRequest(token))
	if err == nil {
		t.Fatal("malformed JWKS was accepted")
	}
	for _, secret := range []string{"private-key-id", "jwt-secret@example.com", "private-public-material", token} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("verification error leaked protected value: %v", err)
		}
	}
	for _, form := range []string{fmt.Sprintf("%v", verifier), fmt.Sprintf("%#v", verifier)} {
		for _, secret := range []string{secretTeam, secretAudience, secretEmail, "private-public-material"} {
			if strings.Contains(form, secret) {
				t.Fatalf("verifier formatting leaked protected value: %s", form)
			}
		}
	}
}
