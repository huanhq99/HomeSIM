package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/publicedge"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

const (
	testPublicWebHost     = "phone.example.com"
	testPublicWebTeam     = "test-team.cloudflareaccess.com"
	testPublicWebAudience = "phone_audience-1"
	testPublicWebEmail    = "owner@example.com"
	testPublicWebSubject  = "synthetic-access-subject-1"
)

func testPublicWebTURNResolveIPv4(_ context.Context, host string) (string, error) {
	if host != "turn.example.com" {
		return "", errors.New("unexpected TURN host")
	}
	return "8.8.8.8", nil
}

var (
	publicWebKeyOnce sync.Once
	publicWebKey     *rsa.PrivateKey
	publicWebKeyErr  error
)

type publicWebTestTransport struct {
	status int
	body   []byte
}

type publicWebExternalVoiceStatusStub struct {
	status externalVoiceStatus
}

func (stub *publicWebExternalVoiceStatusStub) Status(string) externalVoiceStatus {
	return stub.status
}

func (*publicWebExternalVoiceStatusStub) Offer(context.Context, string, externalVoiceOfferRequest, [32]byte) (externalVoiceOfferResult, error) {
	return externalVoiceOfferResult{}, errExternalVoiceUnavailable
}

func (*publicWebExternalVoiceStatusStub) Answer(context.Context, string, string, externalVoiceCallRequest, [sha256.Size]byte) (sipgateway.MutationResult, error) {
	return sipgateway.MutationResult{}, errExternalVoiceUnavailable
}

func (*publicWebExternalVoiceStatusStub) End(context.Context, string, string, externalVoiceCallRequest, [sha256.Size]byte) (sipgateway.MutationResult, error) {
	return sipgateway.MutationResult{}, errExternalVoiceUnavailable
}

func (*publicWebExternalVoiceStatusStub) Reconcile(context.Context, string, externalVoiceCallRequest, bool, bool) (sipgateway.PublicCallSnapshot, error) {
	return sipgateway.PublicCallSnapshot{}, errExternalVoiceUnavailable
}

func (*publicWebExternalVoiceStatusStub) Close() error { return nil }

func (transport *publicWebTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	status := transport.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(transport.body)),
		Request:    request,
	}, nil
}

func testPublicWebKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	publicWebKeyOnce.Do(func() {
		publicWebKey, publicWebKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if publicWebKeyErr != nil {
		t.Fatalf("generate Access test key: %v", publicWebKeyErr)
	}
	return publicWebKey
}

func testPublicWebJWKS(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kid": "key-1", "kty": "RSA", "alg": "RS256", "use": "sig",
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func testPublicWebJWT(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	now := time.Now().Unix()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "key-1", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"aud": []string{testPublicWebAudience}, "email": testPublicWebEmail,
		"exp": now + 300, "iat": now - 5, "nbf": now - 5,
		"iss": "https://" + testPublicWebTeam, "type": "app",
		"identity_nonce": "synthetic-nonce-1", "sub": testPublicWebSubject,
	})
	headerPart := base64.RawURLEncoding.EncodeToString(header)
	claimsPart := base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(headerPart + "." + claimsPart))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return headerPart + "." + claimsPart + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func newPublicWebTestApp(t *testing.T, control bool, transport http.RoundTripper) (*app, http.Handler, string) {
	t.Helper()
	allowlist := filepath.Join(t.TempDir(), "allowed-emails")
	if err := os.WriteFile(allowlist, []byte(testPublicWebEmail+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startup, err := parsePublicWebStartupConfig(publicWebFlagConfig{
		Listen: defaultPublicWebListen, Host: testPublicWebHost,
		TeamDomain: testPublicWebTeam, Audience: testPublicWebAudience,
		AllowedEmailsFile: allowlist, Control: control,
		HTTPClient: &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("parse public web config: %v", err)
	}
	instance := newDemoApp()
	if err := instance.initializeSMSStore(filepath.Join(t.TempDir(), "sms")); err != nil {
		t.Fatal(err)
	}
	if err := instance.configurePublicWebAccess(startup, filepath.Join(t.TempDir(), "operations.jsonl")); err != nil {
		t.Fatalf("configure public web: %v", err)
	}
	t.Cleanup(func() {
		_ = instance.remoteLedger.Close()
		instance.closeSMSStore()
	})
	return instance, instance.publicWebRoutes(), testPublicWebJWT(t, testPublicWebKey(t))
}

func newPublicVoiceWebTestApp(
	t *testing.T,
	transport http.RoundTripper,
) (*app, http.Handler, string, *externalVoiceRuntimeFixture, sipgateway.PublicCallSnapshot) {
	return newPublicVoiceWebTestAppWithOptions(t, transport, false)
}

func newPublicVoiceWebTestAppWithOptions(
	t *testing.T,
	transport http.RoundTripper,
	allowDial bool,
) (*app, http.Handler, string, *externalVoiceRuntimeFixture, sipgateway.PublicCallSnapshot) {
	return newPublicVoiceWebTestAppWithSeedOptions(t, transport, allowDial, true)
}

func newPublicVoiceWebTestAppWithSeedOptions(
	t *testing.T,
	transport http.RoundTripper,
	allowDial bool,
	seedIncoming bool,
) (*app, http.Handler, string, *externalVoiceRuntimeFixture, sipgateway.PublicCallSnapshot) {
	t.Helper()
	privateDir := t.TempDir()
	allowlist := filepath.Join(privateDir, "allowed-emails")
	turnSecret := filepath.Join(privateDir, "turn-secret")
	if err := os.WriteFile(allowlist, []byte(testPublicWebEmail+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(turnSecret, []byte(strings.Repeat("S", 48)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startup, err := parsePublicWebStartupConfig(publicWebFlagConfig{
		Listen: defaultPublicWebListen, Host: testPublicWebHost,
		TeamDomain: testPublicWebTeam, Audience: testPublicWebAudience,
		AllowedEmailsFile: allowlist, Control: true,
		ExternalVoice: true, TURNHost: "turn.example.com", TURNSecretFile: turnSecret,
		TURNResolveIPv4: testPublicWebTURNResolveIPv4,
		HTTPClient:      &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("parse public voice web config: %v", err)
	}
	fixture := newExternalVoiceRuntimeFixtureWithOptions(t, context.Background(), allowDial)
	var incoming sipgateway.PublicCallSnapshot
	if seedIncoming {
		incoming = fixture.incoming(t)
	}
	instance := newDemoApp()
	instance.sipVoice = fixture.runtime
	if err := instance.initializeSMSStore(filepath.Join(t.TempDir(), "sms")); err != nil {
		t.Fatal(err)
	}
	if err := instance.configurePublicWebAccess(startup, filepath.Join(t.TempDir(), "operations.jsonl")); err != nil {
		t.Fatalf("configure public voice web: %v", err)
	}
	t.Cleanup(func() {
		_ = startup.TURNIssuer.Close()
		_ = instance.remoteLedger.Close()
		instance.closeSMSStore()
	})
	return instance, instance.publicWebRoutes(), testPublicWebJWT(t, testPublicWebKey(t)), fixture, incoming
}

func newPublicDirectVoiceWebTestApp(
	t *testing.T,
	transport http.RoundTripper,
) (*app, http.Handler, string) {
	t.Helper()
	privateDir := t.TempDir()
	t.Setenv("HOME", privateDir)
	allowlist := filepath.Join(privateDir, "allowed-emails")
	turnSecret := filepath.Join(privateDir, "turn-secret")
	if err := os.WriteFile(allowlist, []byte(testPublicWebEmail+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(turnSecret, []byte(strings.Repeat("D", 48)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	startup, err := parsePublicWebStartupConfig(publicWebFlagConfig{
		Listen: defaultPublicWebListen, Host: testPublicWebHost,
		TeamDomain: testPublicWebTeam, Audience: testPublicWebAudience,
		AllowedEmailsFile: allowlist, Control: true, DirectVoice: true,
		TURNHost: "turn.example.com", TURNSecretFile: turnSecret,
		TURNResolveIPv4: testPublicWebTURNResolveIPv4,
		HTTPClient:      &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("parse public direct voice config: %v", err)
	}
	instance := newDemoApp()
	if err := instance.initializeSMSStore(filepath.Join(privateDir, "sms")); err != nil {
		t.Fatal(err)
	}
	if err := instance.configurePublicWebAccess(startup, filepath.Join(privateDir, "operations.jsonl")); err != nil {
		t.Fatalf("configure public direct voice web: %v", err)
	}
	t.Cleanup(func() {
		if instance.remoteMedia != nil {
			_ = instance.remoteMedia.Close()
		}
		_ = startup.TURNIssuer.Close()
		_ = instance.remoteLedger.Close()
		instance.closeSMSStore()
	})
	return instance, instance.publicWebRoutes(), testPublicWebJWT(t, testPublicWebKey(t))
}

func publicWebTestRequest(method, path, body, token string) *http.Request {
	request := httptest.NewRequest(method, "https://"+testPublicWebHost+path, strings.NewReader(body))
	request.Host = testPublicWebHost
	if token != "" {
		request.Header.Set(publicedge.AccessAssertionHeader, token)
	}
	return request
}

func authorizePublicWebMutation(request *http.Request, csrf, idempotencyKey string) {
	request.Header.Set("Origin", "https://"+testPublicWebHost)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-MacCellular-CSRF", csrf)
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
}

func TestPublicDirectVoiceStoresUploadedRecordingOnMac(t *testing.T) {
	key := testPublicWebKey(t)
	instance, handler, token := newPublicDirectVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}

	metadata := publicWebRecordingMetadata{
		Version: 1, ID: "recording_test_001", CallID: "call-1", Number: "10086",
		Direction: "outgoing", StartedAt: "2026-08-16T12:00:00Z", EndedAt: "2026-08-16T12:00:09Z",
		DurationSeconds: 9, MIMEType: "audio/mp4;codecs=mp4a.40.2", Extension: "m4a",
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataPart, err := writer.CreateFormField("metadata")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(metadataPart).Encode(metadata); err != nil {
		t.Fatal(err)
	}
	audioHeader := make(textproto.MIMEHeader)
	audioHeader.Set("Content-Disposition", `form-data; name="audio"; filename="call.m4a"`)
	audioHeader.Set("Content-Type", "audio/mp4;codecs=mp4a.40.2")
	audioPart, err := writer.CreatePart(audioHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audioPart.Write([]byte("synthetic-call-audio")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://"+testPublicWebHost+publicWebRecordingUploadPath, &body)
	request.Host = testPublicWebHost
	request.Header.Set(publicedge.AccessAssertionHeader, token)
	request.Header.Set("Origin", "https://"+testPublicWebHost)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("X-MacCellular-CSRF", session.CSRF)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("recording upload=%d %s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(instance.publicWeb.RecordingsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("recording files=%d, want audio plus metadata", len(entries))
	}
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), "10086") {
			t.Fatalf("recording file name %q does not identify the call peer", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("recording file mode=%o", info.Mode().Perm())
		}
	}
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, publicWebTestRequest(http.MethodGet, publicWebRecordingUploadPath, "", token))
	var listing struct {
		Version int                          `json:"version"`
		Items   []publicWebRecordingMetadata `json:"items"`
	}
	if listResponse.Code != http.StatusOK || json.Unmarshal(listResponse.Body.Bytes(), &listing) != nil ||
		listing.Version != 1 || len(listing.Items) != 1 {
		t.Fatalf("recording list=%d %s", listResponse.Code, listResponse.Body.String())
	}
	if listing.Items[0].ID != metadata.ID || listing.Items[0].AudioPath != publicWebRecordingUploadPath+"/"+metadata.ID+"/audio" {
		t.Fatalf("recording list item=%+v", listing.Items[0])
	}
	audioRequest := publicWebTestRequest(http.MethodGet, listing.Items[0].AudioPath, "", token)
	audioRequest.Header.Set("Range", "bytes=0-8")
	audioResponse := httptest.NewRecorder()
	handler.ServeHTTP(audioResponse, audioRequest)
	if audioResponse.Code != http.StatusPartialContent || audioResponse.Body.String() != "synthetic" ||
		audioResponse.Header().Get("Content-Type") != "audio/mp4" ||
		audioResponse.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("recording range=%d type=%q ranges=%q body=%q", audioResponse.Code,
			audioResponse.Header().Get("Content-Type"), audioResponse.Header().Get("Accept-Ranges"), audioResponse.Body.String())
	}
}

func TestPrimaryPublicWebRecordingsDropsShortTailForSameCall(t *testing.T) {
	items := []publicWebRecordingMetadata{
		{ID: "recording_main_001", CallID: "call-shared", StartedAt: "2026-08-17T01:00:00Z", DurationSeconds: 70},
		{ID: "recording_tail_001", CallID: "call-shared", StartedAt: "2026-08-17T01:01:11Z", DurationSeconds: 2},
		{ID: "recording_other_1", CallID: "call-other", StartedAt: "2026-08-17T02:00:00Z", DurationSeconds: 8},
	}
	got := primaryPublicWebRecordings(items)
	if len(got) != 2 || got[0].ID != "recording_other_1" || got[1].ID != "recording_main_001" {
		t.Fatalf("primary recordings=%+v", got)
	}
}

func TestPublicDirectVoiceRecordingUploadKeepsOnlyLongestFilePerCall(t *testing.T) {
	key := testPublicWebKey(t)
	instance, handler, token := newPublicDirectVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	upload := func(metadata publicWebRecordingMetadata) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		metadataPart, err := writer.CreateFormField("metadata")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(metadataPart).Encode(metadata); err != nil {
			t.Fatal(err)
		}
		audioHeader := make(textproto.MIMEHeader)
		audioHeader.Set("Content-Disposition", `form-data; name="audio"; filename="call.m4a"`)
		audioHeader.Set("Content-Type", "audio/mp4;codecs=mp4a.40.2")
		audioPart, err := writer.CreatePart(audioHeader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := audioPart.Write([]byte("synthetic-call-audio-" + metadata.ID)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "https://"+testPublicWebHost+publicWebRecordingUploadPath, &body)
		request.Host = testPublicWebHost
		request.Header.Set(publicedge.AccessAssertionHeader, token)
		request.Header.Set("Origin", "https://"+testPublicWebHost)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("X-MacCellular-CSRF", session.CSRF)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	metadata := func(id, callID, startedAt, endedAt string, duration int) publicWebRecordingMetadata {
		return publicWebRecordingMetadata{
			Version: 1, ID: id, CallID: callID, Number: "10086", Direction: "outgoing",
			StartedAt: startedAt, EndedAt: endedAt, DurationSeconds: duration,
			MIMEType: "audio/mp4;codecs=mp4a.40.2", Extension: "m4a",
		}
	}

	main := metadata("mainrec_001", "call-main-first", "2026-08-17T01:00:00Z", "2026-08-17T01:01:10Z", 70)
	if response := upload(main); response.Code != http.StatusCreated {
		t.Fatalf("main upload=%d %s", response.Code, response.Body.String())
	}
	tail := metadata("tailrec_001", main.CallID, "2026-08-17T01:01:11Z", "2026-08-17T01:01:13Z", 2)
	response := upload(tail)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deduplicated":true`) {
		t.Fatalf("tail upload=%d %s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(instance.publicWeb.RecordingsDir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("main-first recording files=%d err=%v", len(entries), err)
	}

	shortFirst := metadata("shortrec_01", "call-short-first", "2026-08-17T02:00:00Z", "2026-08-17T02:00:02Z", 2)
	if response := upload(shortFirst); response.Code != http.StatusCreated {
		t.Fatalf("short-first upload=%d %s", response.Code, response.Body.String())
	}
	longSecond := metadata("longrec_001", shortFirst.CallID, "2026-08-17T02:00:00Z", "2026-08-17T02:01:20Z", 80)
	if response := upload(longSecond); response.Code != http.StatusCreated {
		t.Fatalf("long-second upload=%d %s", response.Code, response.Body.String())
	}
	entries, err = os.ReadDir(instance.publicWeb.RecordingsDir)
	if err != nil || len(entries) != 4 {
		t.Fatalf("long-second recording files=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), shortFirst.ID[:8]) || strings.Contains(entry.Name(), tail.ID[:8]) {
			t.Fatalf("superseded recording remains: %s", entry.Name())
		}
	}
}

func TestPublicWebSessionAndRoutesAreSMSOnly(t *testing.T) {
	key := testPublicWebKey(t)
	_, handler, token := newPublicWebTestApp(t, false, &publicWebTestTransport{body: testPublicWebJWKS(t, key)})

	sessionRequest := publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	var session struct {
		Transport string   `json:"transport"`
		SMSOnly   bool     `json:"sms_only"`
		Control   bool     `json:"remote_control"`
		Actions   []string `json:"actions"`
		CSRF      string   `json:"csrf_token"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Transport != "cloudflare-access" || !session.SMSOnly || session.Control || session.CSRF == "" ||
		strings.Join(session.Actions, ",") != "sms.read,status.read" {
		t.Fatalf("unexpected public session: %+v", session)
	}
	if got := sessionResponse.Header().Get("Permissions-Policy"); !strings.Contains(got, "microphone=()") {
		t.Fatalf("SMS-only permissions policy=%q", got)
	}

	rootResponse := httptest.NewRecorder()
	handler.ServeHTTP(rootResponse, publicWebTestRequest(http.MethodGet, "/", "", token))
	if rootResponse.Code != http.StatusTemporaryRedirect || rootResponse.Header().Get("Location") != "/remote/" {
		t.Fatalf("root redirect=%d location=%q", rootResponse.Code, rootResponse.Header().Get("Location"))
	}
	for _, path := range []string{
		"/api/remote/v1/calls/dial", "/api/remote/v1/media/offers",
		remoteVoiceV2SnapshotPath, externalVoiceICECredentialsPath, remoteVoiceV2OfferPath,
		"/api/native/v1/session", "/api/status", "/api/at",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, publicWebTestRequest(http.MethodGet, path, "", token))
		if response.Code != http.StatusNotFound {
			t.Fatalf("forbidden route %s=%d, want 404", path, response.Code)
		}
	}
}

func TestPublicWebExternalVoiceUsesExactV2RoutesContextAndTURN(t *testing.T) {
	key := testPublicWebKey(t)
	instance, handler, token, fixture, incoming := newPublicVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		Identity             string   `json:"identity"`
		Transport            string   `json:"transport"`
		SMSOnly              bool     `json:"sms_only"`
		ExternalVoice        bool     `json:"external_voice"`
		ExternalVoiceVersion int      `json:"external_voice_api_version"`
		Actions              []string `json:"actions"`
		CSRF                 string   `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil {
		t.Fatalf("session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	wantActions := "calls.control,calls.hangup,calls.media,calls.read,sms.read,sms.send,status.read"
	if session.Identity != testPublicWebEmail || session.Transport != "cloudflare-access" ||
		session.SMSOnly || !session.ExternalVoice ||
		session.ExternalVoiceVersion != 2 || session.CSRF == "" || strings.Join(session.Actions, ",") != wantActions {
		t.Fatalf("unexpected public voice session: %+v", session)
	}
	if got := sessionResponse.Header().Get("Permissions-Policy"); !strings.Contains(got, "microphone=(self)") {
		t.Fatalf("voice permissions policy=%q", got)
	}

	iceResponse := httptest.NewRecorder()
	handler.ServeHTTP(iceResponse, publicWebTestRequest(http.MethodGet, externalVoiceICECredentialsPath, "", token))
	var ice struct {
		Version    int      `json:"version"`
		URLs       []string `json:"urls"`
		Username   string   `json:"username"`
		Credential string   `json:"credential"`
		ExpiresAt  string   `json:"expires_at"`
	}
	if iceResponse.Code != http.StatusOK || json.Unmarshal(iceResponse.Body.Bytes(), &ice) != nil ||
		ice.Version != 1 || len(ice.URLs) != 3 || ice.Username == "" || ice.Credential == "" ||
		ice.ExpiresAt == "" || iceResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ICE credentials=%d %s", iceResponse.Code, iceResponse.Body.String())
	}
	wantBrowserURLs := []string{
		"turn:8.8.8.8:3478?transport=udp",
		"turn:8.8.8.8:3478?transport=tcp",
		"turns:turn.example.com:443?transport=tcp",
	}
	if strings.Join(ice.URLs, "\n") != strings.Join(wantBrowserURLs, "\n") {
		t.Fatalf("browser TURN URLs=%v want=%v", ice.URLs, wantBrowserURLs)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, ice.ExpiresAt)
	if err != nil || time.Until(expiresAt) < 4*time.Minute || time.Until(expiresAt) > 5*time.Minute+5*time.Second {
		t.Fatalf("browser TURN credential is not bounded to the configured short TTL: expires_at=%q err=%v", ice.ExpiresAt, err)
	}

	offerBody := mustExternalVoiceJSON(t, syntheticExternalVoiceOffer(incoming, "synthetic_public_nonce_0001"))
	offerRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2OfferPath, offerBody, token)
	authorizePublicWebMutation(offerRequest, session.CSRF, "")
	offerResponse := httptest.NewRecorder()
	handler.ServeHTTP(offerResponse, offerRequest)
	if offerResponse.Code != http.StatusOK {
		t.Fatalf("public context offer=%d %s remote-control=%t", offerResponse.Code, offerResponse.Body.String(), instance.remoteAccess.Control)
	}
	var offer externalVoiceOfferResponse
	if err := json.Unmarshal(offerResponse.Body.Bytes(), &offer); err != nil ||
		offer.Call.Media == nil || !offer.Call.Media.Prepared {
		t.Fatalf("public context offer contract=%+v err=%v", offer, err)
	}
	fixture.runtime.mu.Lock()
	owner := fixture.runtime.owner
	fixture.runtime.mu.Unlock()
	wantOwner := fixture.runtime.ownerIdentity(testPublicWebEmail)
	if owner != wantOwner {
		t.Fatalf("public media owner token does not bind the verified Access principal: got=%q want=%q", owner, wantOwner)
	}
	fixture.mu.Lock()
	serverNetwork := cloneExternalVoiceNetwork(fixture.answerNetwork)
	fixture.mu.Unlock()
	if serverNetwork.TURNRelay == nil || len(serverNetwork.TURNRelay.URLs) != 3 ||
		serverNetwork.TURNRelay.URLs[0] != "turn:8.8.8.8:3478?transport=udp" ||
		serverNetwork.TURNRelay.URLs[1] != "turn:8.8.8.8:3478?transport=tcp" ||
		serverNetwork.TURNRelay.URLs[2] != "turns:turn.example.com:443?transport=tcp" ||
		serverNetwork.TURNRelay.Username == "" || serverNetwork.TURNRelay.Password == "" ||
		serverNetwork.TURNRelay.CredentialType != "password" ||
		len(serverNetwork.AllowedInterfaces) != 0 || len(serverNetwork.AllowedLocalCIDRs) != 0 ||
		len(serverNetwork.AllowedRemoteCIDRs) != 0 {
		t.Fatalf("public offer did not use server TURN relay-only media: %v", serverNetwork)
	}

	answerBody := mustExternalVoiceJSON(t, externalVoiceCallRequest{
		Version: externalVoiceAPISchemaVersion, Call: offer.Call.Call,
		ExpectedRevision: offer.Call.Revision, MediaLeaseID: offer.Call.Media.LeaseID,
	})
	for attempt := 0; attempt < 2; attempt++ {
		answerRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2AnswerPath, answerBody, token)
		authorizePublicWebMutation(answerRequest, session.CSRF, "public-voice-answer-command-0001")
		answerResponse := httptest.NewRecorder()
		handler.ServeHTTP(answerResponse, answerRequest)
		if answerResponse.Code != http.StatusOK || !strings.Contains(answerResponse.Body.String(), `"code":"completed"`) {
			t.Fatalf("public answer attempt %d=%d %s", attempt, answerResponse.Code, answerResponse.Body.String())
		}
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 1 {
		t.Fatalf("public answer replay executed provider %d times", got)
	}

	activeResponse := httptest.NewRecorder()
	handler.ServeHTTP(activeResponse, publicWebTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", token))
	var active externalVoiceSnapshotResponse
	if activeResponse.Code != http.StatusOK || json.Unmarshal(activeResponse.Body.Bytes(), &active) != nil ||
		active.Voice.Call == nil || active.Voice.Call.Media == nil || !active.Voice.OwnedByRequester {
		t.Fatalf("public active snapshot=%d %s", activeResponse.Code, activeResponse.Body.String())
	}
	endBody := mustExternalVoiceJSON(t, externalVoiceCallRequest{
		Version: externalVoiceAPISchemaVersion, Call: active.Voice.Call.Call,
		ExpectedRevision: active.Voice.Call.Revision, MediaLeaseID: active.Voice.Call.Media.LeaseID,
	})
	endRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2EndPath, endBody, token)
	authorizePublicWebMutation(endRequest, session.CSRF, "public-voice-end-command-0001")
	endResponse := httptest.NewRecorder()
	handler.ServeHTTP(endResponse, endRequest)
	if endResponse.Code != http.StatusOK || !strings.Contains(endResponse.Body.String(), `"code":"completed"`) {
		t.Fatalf("public end=%d %s", endResponse.Code, endResponse.Body.String())
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandEndActive); got != 1 {
		t.Fatalf("public end executed provider %d times", got)
	}

	for _, path := range []string{
		"/api/remote/v1/media/offers", "/api/remote/v1/calls/answer",
		remoteVoiceV2RejectPath + "/extra", externalVoiceICECredentialsPath + "/extra",
		"/api/native/v1/session", "/api/at",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, publicWebTestRequest(http.MethodGet, path, "", token))
		if response.Code != http.StatusNotFound {
			t.Fatalf("non-whitelisted public voice route %s=%d, want 404", path, response.Code)
		}
	}
	// The dial path is part of the external-SIP route allowlist even when this
	// fixture leaves outbound dialing disabled; the handler must return a
	// capability error, not a path-level 404.
	dialPathResponse := httptest.NewRecorder()
	handler.ServeHTTP(dialPathResponse, publicWebTestRequest(http.MethodGet, remoteVoiceV2DialPath, "", token))
	if dialPathResponse.Code == http.StatusNotFound {
		t.Fatalf("external SIP dial route was not allowlisted")
	}
}

func TestPublicWebExternalVoiceDialExecutesThroughAuthorizedRoute(t *testing.T) {
	key := testPublicWebKey(t)
	_, handler, token, fixture, _ := newPublicVoiceWebTestAppWithSeedOptions(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)}, true, false,
	)
	// Drive one provider observation so the public capability gate is connected,
	// then end that synthetic dialog before exercising outbound dialing.
	ref, err := fixture.emulator.Incoming("synthetic-dial-health-dialog")
	if err != nil {
		t.Fatal(err)
	}
	waitExternalVoiceCall(t, fixture.runtime, func(call sipgateway.PublicCallSnapshot) bool {
		return call.Phase == sipgateway.PhaseIncomingRinging
	})
	if _, err := fixture.emulator.Cancel(ref); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := fixture.runtime.Status(testPublicWebEmail)
		if status.Health == "connected" && status.Call != nil && status.Call.Phase == sipgateway.PhaseEnded {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status := fixture.runtime.Status(testPublicWebEmail); status.Health != "connected" || status.Call == nil || status.Call.Phase != sipgateway.PhaseEnded {
		t.Fatalf("synthetic dial precondition did not settle: %+v", status)
	}
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("public voice session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}

	snapshotResponse := httptest.NewRecorder()
	handler.ServeHTTP(snapshotResponse, publicWebTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", token))
	var before externalVoiceSnapshotResponse
	if snapshotResponse.Code != http.StatusOK || json.Unmarshal(snapshotResponse.Body.Bytes(), &before) != nil ||
		!before.Voice.Enabled || !before.Voice.DialEnabled || before.Voice.Call == nil ||
		before.Voice.Call.Phase != sipgateway.PhaseEnded {
		t.Fatalf("public dial capability snapshot=%d %s", snapshotResponse.Code, snapshotResponse.Body.String())
	}

	body := `{"version":2,"number":"+8613800138000"}`
	dial := func() *httptest.ResponseRecorder {
		request := publicWebTestRequest(http.MethodPost, remoteVoiceV2DialPath, body, token)
		authorizePublicWebMutation(request, session.CSRF, "public-voice-dial-command-0001")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	first := dial()
	var firstResult externalVoiceCallResponse
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &firstResult) != nil ||
		firstResult.Call.Direction != sipgateway.CallDirectionOutgoing ||
		firstResult.Call.Phase != sipgateway.PhaseIncomingRinging {
		t.Fatalf("public dial=%d %s", first.Code, first.Body.String())
	}
	second := dial()
	var secondResult externalVoiceCallResponse
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &secondResult) != nil ||
		secondResult.Call.Call != firstResult.Call.Call || secondResult.Call.Revision != firstResult.Call.Revision {
		t.Fatalf("public dial replay=%d %s first=%+v", second.Code, second.Body.String(), firstResult.Call)
	}

	conflictRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2DialPath,
		`{"version":2,"number":"+8613800138001"}`, token)
	authorizePublicWebMutation(conflictRequest, session.CSRF, "public-voice-dial-command-0001")
	conflictResponse := httptest.NewRecorder()
	handler.ServeHTTP(conflictResponse, conflictRequest)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("public dial idempotency conflict=%d %s", conflictResponse.Code, conflictResponse.Body.String())
	}
	if got := fixture.runtime.Status(testPublicWebEmail); got.Call == nil || got.Call.Direction != sipgateway.CallDirectionOutgoing {
		t.Fatalf("public dial did not publish outgoing status: %+v", got)
	}
}

func TestPublicWebDirectVoiceUsesExactIncomingV1RoutesAndTURN(t *testing.T) {
	key := testPublicWebKey(t)
	instance, handler, token := newPublicDirectVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		Transport      string   `json:"transport"`
		SMSOnly        bool     `json:"sms_only"`
		ExternalVoice  bool     `json:"external_voice"`
		MediaOffer     bool     `json:"media_offer"`
		IncomingAnswer bool     `json:"incoming_answer"`
		RescueHangup   bool     `json:"rescue_hangup"`
		Actions        []string `json:"actions"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil {
		t.Fatalf("direct session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	wantActions := "calls.control,calls.hangup,calls.media,calls.read,sms.read,sms.send,status.read"
	if session.Transport != "cloudflare-access" || session.SMSOnly || session.ExternalVoice ||
		!session.MediaOffer || !session.IncomingAnswer || !session.RescueHangup ||
		strings.Join(session.Actions, ",") != wantActions {
		t.Fatalf("unexpected direct public session: %+v", session)
	}
	if instance.remoteMedia.cfg.networkMode() != remoteMediaNetworkPublicTURN ||
		instance.remoteMedia.cfg.Interface != "" || len(instance.remoteMedia.cfg.LocalCIDRs) != 0 ||
		len(instance.remoteMedia.cfg.RemoteCIDRs) != 0 || instance.remoteMedia.cfg.UDPMin != 0 ||
		instance.remoteMedia.cfg.UDPMax != 0 ||
		!strings.HasSuffix(instance.remoteMedia.cfg.RootDir, filepath.Join("MacCellular", "remote", "pcm")) ||
		!strings.HasSuffix(instance.remoteMedia.cfg.ControlPath, filepath.Join("MacCellular", "remote", "media-control.sock")) {
		t.Fatalf("unexpected public direct media profile: %+v", instance.remoteMedia.cfg)
	}
	if got := sessionResponse.Header().Get("Permissions-Policy"); !strings.Contains(got, "microphone=(self)") {
		t.Fatalf("direct voice permissions policy=%q", got)
	}
	conflict := newDemoApp()
	conflict.remoteMedia = &remoteMediaManager{}
	if err := conflict.configurePublicWebAccess(instance.publicWeb, filepath.Join(t.TempDir(), "conflict-ledger.jsonl")); err == nil {
		t.Fatal("public direct voice accepted an existing Tailnet media manager")
	}

	iceResponse := httptest.NewRecorder()
	handler.ServeHTTP(iceResponse, publicWebTestRequest(http.MethodGet, externalVoiceICECredentialsPath, "", token))
	if iceResponse.Code != http.StatusOK || iceResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("direct ICE credentials=%d %s", iceResponse.Code, iceResponse.Body.String())
	}
	var ice struct {
		URLs []string `json:"urls"`
	}
	if json.Unmarshal(iceResponse.Body.Bytes(), &ice) != nil || len(ice.URLs) != 3 {
		t.Fatalf("direct ICE contract=%s", iceResponse.Body.String())
	}
	serverRelay, err := instance.remoteMedia.cfg.IssueTURN()
	if err != nil || serverRelay == nil || len(serverRelay.URLs) != 3 ||
		serverRelay.URLs[0] != "turn:8.8.8.8:3478?transport=udp" ||
		serverRelay.URLs[1] != "turn:8.8.8.8:3478?transport=tcp" ||
		serverRelay.URLs[2] != "turns:turn.example.com:443?transport=tcp" ||
		serverRelay.Username == "" || serverRelay.Password == "" || serverRelay.CredentialType != "password" {
		t.Fatalf("direct server relay forwarding=%+v err=%v", serverRelay, err)
	}

	for _, route := range []string{
		"/api/remote/v1/media/offers", "/api/remote/v1/calls/dial",
		"/api/remote/v1/calls/answer", "/api/remote/v1/calls/hangup",
		"/api/remote/v1/calls/dtmf",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, publicWebTestRequest(http.MethodPost, route, "{}", token))
		if response.Code == http.StatusNotFound {
			t.Fatalf("direct media-bound route %s was not registered", route)
		}
	}
	for _, route := range []string{
		"/api/remote/v1/calls/reject",
		remoteVoiceV2SnapshotPath, remoteVoiceV2OfferPath, remoteVoiceV2AnswerPath,
		remoteVoiceV2EndPath, remoteVoiceV2ReconcilePath, "/api/native/v1/session", "/api/at",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, publicWebTestRequest(http.MethodGet, route, "", token))
		if response.Code != http.StatusNotFound {
			t.Fatalf("non-whitelisted direct route %s=%d, want 404", route, response.Code)
		}
	}
}

func TestPublicDirectVoiceDoesNotAdvertiseOrAcceptTailnetMedia(t *testing.T) {
	instance, _, _ := newPublicDirectVoiceWebTestApp(
		t,
		&publicWebTestTransport{body: testPublicWebJWKS(t, testPublicWebKey(t))},
	)
	authorization := remoteAuthorization{
		Identity: testRemoteLogin,
		Actions: map[string]bool{
			"status.read": true, "calls.read": true, "calls.media": true,
			"calls.control": true, "calls.hangup": true,
		},
	}
	browser := remoteBrowserContext{Control: true, Transport: "tailscale-serve"}
	request := httptest.NewRequest(http.MethodGet, "/api/remote/v1/session", nil)
	ctx := context.WithValue(request.Context(), remoteAuthorizationContextKey{}, authorization)
	ctx = context.WithValue(ctx, remoteBrowserContextKey{}, browser)
	response := httptest.NewRecorder()
	instance.remoteSession(response, request.WithContext(ctx))
	if response.Code != http.StatusOK {
		t.Fatalf("Tailnet session status=%d body=%s", response.Code, response.Body.String())
	}
	var session struct {
		Media          string `json:"media"`
		MediaOffer     bool   `json:"media_offer"`
		IncomingAnswer bool   `json:"incoming_answer"`
		RescueHangup   bool   `json:"rescue_hangup"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.Media != "disabled" || session.MediaOffer || session.IncomingAnswer || session.RescueHangup {
		t.Fatalf("public TURN manager leaked into Tailnet session: %+v", session)
	}

	offerRequest := httptest.NewRequest(http.MethodPost, "/api/remote/v1/media/offers", strings.NewReader(`{}`))
	offerResponse := httptest.NewRecorder()
	instance.remoteMediaOffer(offerResponse, offerRequest.WithContext(ctx))
	if offerResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("Tailnet offer reached public TURN manager: status=%d body=%s", offerResponse.Code, offerResponse.Body.String())
	}
}

func TestPublicDirectVoiceDTMFBindsToCurrentActiveCall(t *testing.T) {
	instance, handler, token := newPublicDirectVoiceWebTestApp(
		t,
		&publicWebTestTransport{body: testPublicWebJWKS(t, testPublicWebKey(t))},
	)
	now := time.Now()
	instance.callMu.Lock()
	instance.callGeneration = 7
	instance.activeCall = &callRecord{
		ID: "call-current", Index: 1, Direction: "outgoing", State: "active",
		StartedAt: now, UpdatedAt: now,
	}
	instance.callMu.Unlock()

	sessionRequest := publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("direct session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}

	body := `{"call":{"call_id":"call-current","call_generation":7,"call_index":1,"call_direction":"outgoing"},"digit":"1"}`
	request := publicWebTestRequest(http.MethodPost, "/api/remote/v1/calls/dtmf", body, token)
	authorizePublicWebMutation(request, session.CSRF, "dtmf-current-call")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("direct DTMF=%d %s", response.Code, response.Body.String())
	}

	stale := publicWebTestRequest(http.MethodPost, "/api/remote/v1/calls/dtmf",
		`{"call":{"call_id":"call-old","call_generation":6,"call_index":1,"call_direction":"outgoing"},"digit":"2"}`, token)
	authorizePublicWebMutation(stale, session.CSRF, "dtmf-stale-call")
	staleResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleResponse, stale)
	if staleResponse.Code != http.StatusConflict {
		t.Fatalf("stale DTMF=%d %s", staleResponse.Code, staleResponse.Body.String())
	}
}

func TestRemoteCallControlWaitsForBriefModuleContention(t *testing.T) {
	instance := &app{}
	instance.moduleMutationMu.Lock()
	released := make(chan struct{})
	go func() {
		time.Sleep(75 * time.Millisecond)
		instance.moduleMutationMu.Unlock()
		close(released)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if !instance.lockModuleMutationFor(ctx, 500*time.Millisecond) {
		t.Fatal("brief background module operation made call control fail")
	}
	instance.moduleMutationMu.Unlock()
	<-released
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("contention wait=%s, want a bounded wait for the existing operation", elapsed)
	}
}

func TestPublicWebExternalVoiceUnknownAnswerReconcilesWithoutRetry(t *testing.T) {
	key := testPublicWebKey(t)
	_, handler, token, fixture, incoming := newPublicVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.CSRF == "" {
		t.Fatalf("public voice session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}

	offerRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2OfferPath,
		mustExternalVoiceJSON(t, syntheticExternalVoiceOffer(incoming, "synthetic_public_nonce_0002")), token)
	authorizePublicWebMutation(offerRequest, session.CSRF, "")
	offerResponse := httptest.NewRecorder()
	handler.ServeHTTP(offerResponse, offerRequest)
	var offer externalVoiceOfferResponse
	if offerResponse.Code != http.StatusOK || json.Unmarshal(offerResponse.Body.Bytes(), &offer) != nil || offer.Call.Media == nil {
		t.Fatalf("public offer=%d %s", offerResponse.Code, offerResponse.Body.String())
	}
	if err := fixture.emulator.SetNextCommandFault(sipgateway.CommandAnswerIncoming, sipgateway.CommandFault{
		Outcome: sipgateway.CommandUnknown, Apply: false,
	}); err != nil {
		t.Fatal(err)
	}
	answerBody := mustExternalVoiceJSON(t, externalVoiceCallRequest{
		Version: externalVoiceAPISchemaVersion, Call: offer.Call.Call,
		ExpectedRevision: offer.Call.Revision, MediaLeaseID: offer.Call.Media.LeaseID,
	})
	for attempt := 0; attempt < 2; attempt++ {
		answerRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2AnswerPath, answerBody, token)
		authorizePublicWebMutation(answerRequest, session.CSRF, "public-voice-answer-unknown-0001")
		answerResponse := httptest.NewRecorder()
		handler.ServeHTTP(answerResponse, answerRequest)
		if answerResponse.Code != http.StatusConflict ||
			!strings.Contains(answerResponse.Body.String(), `"code":"unknown_outcome"`) {
			t.Fatalf("public unknown answer attempt %d=%d %s", attempt, answerResponse.Code, answerResponse.Body.String())
		}
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 1 {
		t.Fatalf("public unknown answer was retried: %d", got)
	}

	for attempt := 0; attempt < 2; attempt++ {
		reconcileRequest := publicWebTestRequest(http.MethodPost, remoteVoiceV2ReconcilePath, answerBody, token)
		authorizePublicWebMutation(reconcileRequest, session.CSRF, "")
		reconcileResponse := httptest.NewRecorder()
		handler.ServeHTTP(reconcileResponse, reconcileRequest)
		if reconcileResponse.Code != http.StatusOK ||
			!strings.Contains(reconcileResponse.Body.String(), `"reconcile_required":true`) {
			t.Fatalf("public reconcile attempt %d=%d %s", attempt, reconcileResponse.Code, reconcileResponse.Body.String())
		}
	}
	if got := fixture.emulator.CommandExecutions(sipgateway.CommandAnswerIncoming); got != 1 {
		t.Fatalf("public reconcile repeated provider answer: %d", got)
	}
}

func TestPublicWebExternalVoiceConfigurationFallsBackToSMSOnlyWhenRuntimeIsUnavailable(t *testing.T) {
	key := testPublicWebKey(t)
	instance, _, token, _, _ := newPublicVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)
	instance.sipVoice = nil
	handler := instance.publicWebRoutes()

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		SMSOnly       bool     `json:"sms_only"`
		ExternalVoice bool     `json:"external_voice"`
		Actions       []string `json:"actions"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil ||
		!session.SMSOnly || session.ExternalVoice ||
		strings.Join(session.Actions, ",") != "sms.read,sms.send,status.read" {
		t.Fatalf("runtime-unavailable public session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	if got := sessionResponse.Header().Get("Permissions-Policy"); !strings.Contains(got, "microphone=()") {
		t.Fatalf("runtime-unavailable permissions policy=%q", got)
	}
	for _, path := range []string{remoteVoiceV2SnapshotPath, externalVoiceICECredentialsPath, remoteVoiceV2OfferPath} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, publicWebTestRequest(http.MethodGet, path, "", token))
		if response.Code != http.StatusNotFound {
			t.Fatalf("runtime-unavailable voice route %s=%d, want 404", path, response.Code)
		}
	}
}

func TestPublicWebExternalVoiceConnectingKeepsRoutesButNotCapabilities(t *testing.T) {
	key := testPublicWebKey(t)
	instance, _, _, _, _ := newPublicVoiceWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)
	instance.sipVoice = &publicWebExternalVoiceStatusStub{
		status: externalVoiceStatus{Enabled: true, Health: "reconnecting"},
	}
	handler := instance.publicWebRoutes()

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", testPublicWebJWT(t, key)))
	var session struct {
		SMSOnly       bool     `json:"sms_only"`
		ExternalVoice bool     `json:"external_voice"`
		Actions       []string `json:"actions"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil ||
		!session.SMSOnly || session.ExternalVoice || strings.Join(session.Actions, ",") != "sms.read,sms.send,status.read" {
		t.Fatalf("connecting external voice was advertised: %d %s", sessionResponse.Code, sessionResponse.Body.String())
	}
	if got := sessionResponse.Header().Get("Permissions-Policy"); !strings.Contains(got, "microphone=()") {
		t.Fatalf("connecting external voice enabled microphone: %q", got)
	}

	voiceRoute := httptest.NewRecorder()
	handler.ServeHTTP(voiceRoute, publicWebTestRequest(http.MethodGet, remoteVoiceV2SnapshotPath, "", testPublicWebJWT(t, key)))
	if voiceRoute.Code == http.StatusNotFound {
		t.Fatal("external voice route disappeared while supervisor was reconnecting")
	}
}

func TestPublicWebAccessErrorsAndExactHost(t *testing.T) {
	key := testPublicWebKey(t)
	_, handler, token := newPublicWebTestApp(t, false, &publicWebTestTransport{status: http.StatusServiceUnavailable})
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, publicWebTestRequest(http.MethodGet, "/remote/", "", ""))
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing assertion=%d, want 401", missing.Code)
	}
	unavailable := httptest.NewRecorder()
	handler.ServeHTTP(unavailable, publicWebTestRequest(http.MethodGet, "/remote/", "", token))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("JWKS unavailable=%d, want 503", unavailable.Code)
	}

	_, exactHandler, exactToken := newPublicWebTestApp(t, false, &publicWebTestTransport{body: testPublicWebJWKS(t, key)})
	wrongHost := publicWebTestRequest(http.MethodGet, "/remote/", "", exactToken)
	wrongHost.Host = testPublicWebHost + ":443"
	response := httptest.NewRecorder()
	exactHandler.ServeHTTP(response, wrongHost)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("host with port=%d, want 401", response.Code)
	}
}

func TestPublicWebSMSSendUsesCSRFAndPersistentIdempotency(t *testing.T) {
	key := testPublicWebKey(t)
	instance, handler, token := newPublicWebTestApp(t, true, &publicWebTestTransport{body: testPublicWebJWKS(t, key)})
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		Control bool     `json:"remote_control"`
		Actions []string `json:"actions"`
		CSRF    string   `json:"csrf_token"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if !session.Control || strings.Join(session.Actions, ",") != "sms.read,sms.send,status.read" {
		t.Fatalf("control session=%+v", session)
	}
	body := `{"phone":"synthetic-recipient","message":"synthetic-message"}`
	send := func() *httptest.ResponseRecorder {
		request := publicWebTestRequest(http.MethodPost, "/api/remote/v1/sms/send", body, token)
		request.Header.Set("Origin", "https://"+testPublicWebHost)
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-MacCellular-CSRF", session.CSRF)
		request.Header.Set("Idempotency-Key", "public-web-sms-operation-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := send(); response.Code != http.StatusOK {
		t.Fatalf("first send=%d %s", response.Code, response.Body.String())
	}
	first, err := instance.syncSMSHistory("", 100)
	if err != nil {
		t.Fatal(err)
	}
	if response := send(); response.Code != http.StatusOK {
		t.Fatalf("replay=%d %s", response.Code, response.Body.String())
	}
	second, err := instance.syncSMSHistory("", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != len(first.Messages) {
		t.Fatalf("idempotent replay added SMS: first=%d second=%d", len(first.Messages), len(second.Messages))
	}
}

func TestPublicWebConfigurationRequiresLoopbackAndMode0600Allowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowed-emails")
	if err := os.WriteFile(path, []byte(testPublicWebEmail+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := publicWebFlagConfig{
		Listen: defaultPublicWebListen, Host: testPublicWebHost,
		TeamDomain: testPublicWebTeam, Audience: testPublicWebAudience,
		AllowedEmailsFile: path,
	}
	if _, err := parsePublicWebStartupConfig(base); err == nil {
		t.Fatal("mode-0644 public allowlist was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	base.Listen = "0.0.0.0:7578"
	if _, err := parsePublicWebStartupConfig(base); err == nil {
		t.Fatal("non-loopback public listener was accepted")
	}
}

func TestPublicWebExternalVoiceConfigurationIsExplicitAndPrivate(t *testing.T) {
	privateDir := t.TempDir()
	allowlist := filepath.Join(privateDir, "allowed-emails")
	secret := filepath.Join(privateDir, "turn-secret")
	if err := os.WriteFile(allowlist, []byte(testPublicWebEmail+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte(strings.Repeat("T", 48)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := publicWebFlagConfig{
		Listen: defaultPublicWebListen, Host: testPublicWebHost,
		TeamDomain: testPublicWebTeam, Audience: testPublicWebAudience,
		AllowedEmailsFile: allowlist, Control: true,
		ExternalVoice: true, TURNHost: "turn.example.com", TURNSecretFile: secret,
	}
	if _, err := parsePublicWebStartupConfig(base); err == nil {
		t.Fatal("mode-0644 TURN secret was accepted")
	}
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	base.Control = false
	if _, err := parsePublicWebStartupConfig(base); err == nil {
		t.Fatal("external voice without public control was accepted")
	}
	base.Control = true
	base.ExternalVoice = false
	if _, err := parsePublicWebStartupConfig(base); err == nil {
		t.Fatal("TURN configuration without explicit external voice was accepted")
	}
	base.ExternalVoice = true
	base.DirectVoice = true
	if _, err := parsePublicWebStartupConfig(base); err == nil {
		t.Fatal("mutually exclusive public voice modes were accepted")
	}
	base.ExternalVoice = false
	base.DirectVoice = true
	startup, err := parsePublicWebStartupConfig(base)
	if err != nil || startup == nil || !startup.DirectVoice || startup.ExternalVoice || startup.TURNIssuer == nil {
		t.Fatalf("explicit direct public voice was rejected: startup=%+v err=%v", startup, err)
	}
	_ = startup.TURNIssuer.Close()
}
