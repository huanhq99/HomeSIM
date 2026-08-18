package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/turnauth"
)

type publicWebPushTestSend struct {
	Endpoint string
	Payload  string
}

type publicWebPushTestSender struct {
	mu        sync.Mutex
	sends     []publicWebPushTestSend
	status    map[string]int
	err       error
	started   chan struct{}
	release   <-chan struct{}
	startOnce sync.Once
}

type publicWebPushTestResult struct {
	status int
	err    error
}

type publicWebPushSequenceSender struct {
	mu        sync.Mutex
	responses map[string][]publicWebPushTestResult
	calls     map[string]int
}

func (s *publicWebPushSequenceSender) Send(
	_ context.Context,
	subscription publicWebPushDiskSubscription,
	_ []byte,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
	index := s.calls[subscription.Endpoint]
	s.calls[subscription.Endpoint] = index + 1
	responses := s.responses[subscription.Endpoint]
	if len(responses) == 0 {
		return http.StatusCreated, nil
	}
	if index >= len(responses) {
		index = len(responses) - 1
	}
	return responses[index].status, responses[index].err
}

func (s *publicWebPushSequenceSender) callCount(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[endpoint]
}

func (s *publicWebPushTestSender) Send(
	ctx context.Context,
	subscription publicWebPushDiskSubscription,
	payload []byte,
) (int, error) {
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s.mu.Lock()
	s.sends = append(s.sends, publicWebPushTestSend{Endpoint: subscription.Endpoint, Payload: string(payload)})
	status := s.status[subscription.Endpoint]
	err := s.err
	s.mu.Unlock()
	if status == 0 {
		status = http.StatusCreated
	}
	return status, err
}

func (s *publicWebPushTestSender) snapshot() []publicWebPushTestSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]publicWebPushTestSend(nil), s.sends...)
}

func testPublicWebPushPrivateKey() string {
	raw := make([]byte, 32)
	raw[len(raw)-1] = 1
	return base64.RawURLEncoding.EncodeToString(raw)
}

func testPublicWebPushKeys(t *testing.T, scalar byte) (string, string) {
	t.Helper()
	raw := make([]byte, 32)
	raw[len(raw)-1] = scalar
	privateKey, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	p256dh := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes())
	auth := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{scalar}, 16))
	return p256dh, auth
}

func testPublicWebPushRequest(t *testing.T, endpoint string, scalar byte) publicWebPushSubscriptionRequest {
	t.Helper()
	p256dh, auth := testPublicWebPushKeys(t, scalar)
	return publicWebPushSubscriptionRequest{Endpoint: endpoint, P256DH: p256dh, Auth: auth}
}

func newPublicWebPushTestManager(
	t *testing.T,
	sender publicWebPushSender,
	allowed ...string,
) (*publicWebPushManager, *publicWebPushStartupConfig) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "push")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "vapid-private-key")
	if err := os.WriteFile(keyPath, []byte(testPublicWebPushPrivateKey()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(allowed) == 0 {
		allowed = []string{testPublicWebEmail}
	}
	config := &publicWebPushStartupConfig{
		PrivateKeyFile: keyPath, Subject: "mailto:push@example.com",
		SubscriptionsFile: filepath.Join(root, "subscriptions.json"),
		AllowedPrincipals: append([]string(nil), allowed...), Sender: sender,
	}
	manager, err := newPublicWebPushManager(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, config
}

func waitPublicWebPushIdle(t *testing.T, manager *publicWebPushManager) {
	t.Helper()
	select {
	case <-manager.idleChannel():
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for public Web Push dispatcher")
	}
}

func newPublicPushWebTestApp(
	t *testing.T,
	transport http.RoundTripper,
	sender publicWebPushSender,
) (*app, http.Handler, string) {
	t.Helper()
	privateDir := t.TempDir()
	t.Setenv("HOME", privateDir)
	allowlist := filepath.Join(privateDir, "allowed-emails")
	turnSecret := filepath.Join(privateDir, "turn-secret")
	vapidPrivate := filepath.Join(privateDir, "vapid-private")
	for path, content := range map[string]string{
		allowlist:    testPublicWebEmail + "\n",
		turnSecret:   strings.Repeat("D", 48) + "\n",
		vapidPrivate: testPublicWebPushPrivateKey() + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	startup, err := parsePublicWebStartupConfig(publicWebFlagConfig{
		Listen: defaultPublicWebListen, Host: testPublicWebHost,
		TeamDomain: testPublicWebTeam, Audience: testPublicWebAudience,
		AllowedEmailsFile: allowlist, Control: true, DirectVoice: true,
		TURNHost: "turn.example.com", TURNSecretFile: turnSecret,
		TURNResolveIPv4: testPublicWebTURNResolveIPv4,
		Push: publicWebPushFlagConfig{
			Enabled: true, PrivateKeyFile: vapidPrivate,
			Subject:           "mailto:push@example.com",
			SubscriptionsFile: filepath.Join(privateDir, "push", "subscriptions.json"),
		},
		HTTPClient: &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("parse public push config: %v", err)
	}
	startup.Push.Sender = sender
	instance := newDemoApp()
	if err := instance.initializeSMSStore(filepath.Join(privateDir, "sms")); err != nil {
		t.Fatal(err)
	}
	if err := instance.configurePublicWebAccess(startup, filepath.Join(privateDir, "operations.jsonl")); err != nil {
		t.Fatalf("configure public push: %v", err)
	}
	t.Cleanup(func() {
		_ = instance.publicWebPush.Close()
		if instance.remoteMedia != nil {
			_ = instance.remoteMedia.Close()
		}
		_ = startup.TURNIssuer.Close()
		_ = instance.remoteLedger.Close()
		instance.closeSMSStore()
	})
	return instance, instance.publicWebRoutes(), testPublicWebJWT(t, testPublicWebKey(t))
}

func testPublicWebPushSubscribeBody(t *testing.T, request publicWebPushSubscriptionRequest) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"version": publicWebPushAPIVersion,
		"subscription": map[string]any{
			"endpoint": request.Endpoint, "expirationTime": nil,
			"keys": map[string]string{"p256dh": request.P256DH, "auth": request.Auth},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestPublicWebPushRoutesRequireAccessActionSameOriginAndCSRF(t *testing.T) {
	sender := &publicWebPushTestSender{}
	key := testPublicWebKey(t)
	instance, handler, token := newPublicPushWebTestApp(
		t, &publicWebTestTransport{body: testPublicWebJWKS(t, key)}, sender,
	)

	unauthorized := httptest.NewRecorder()
	request := publicWebTestRequest(http.MethodGet, publicWebPushConfigPath, "", "")
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized push config=%d", unauthorized.Code)
	}

	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		CSRF            string   `json:"csrf_token"`
		Actions         []string `json:"actions"`
		PublicVoice     bool     `json:"public_voice"`
		PushEnabled     bool     `json:"push_enabled"`
		PushAPIVersion  int      `json:"push_api_version"`
		BackgroundCalls bool     `json:"background_calls"`
		Mode            string   `json:"notification_mode"`
	}
	if sessionResponse.Code != http.StatusOK || json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil ||
		!session.PublicVoice || !session.PushEnabled || session.PushAPIVersion != 1 ||
		session.BackgroundCalls || session.Mode != "web-push" ||
		!slicesContainExact(session.Actions, "push.manage") || session.CSRF == "" {
		t.Fatalf("unexpected push session=%d %s", sessionResponse.Code, sessionResponse.Body.String())
	}

	configResponse := httptest.NewRecorder()
	handler.ServeHTTP(configResponse, publicWebTestRequest(http.MethodGet, publicWebPushConfigPath, "", token))
	var config map[string]any
	if configResponse.Code != http.StatusOK || json.Unmarshal(configResponse.Body.Bytes(), &config) != nil ||
		config["version"] != float64(1) || config["enabled"] != true || config["vapid_public_key"] == "" {
		t.Fatalf("push config=%d %s", configResponse.Code, configResponse.Body.String())
	}
	for _, forbidden := range []string{"endpoint", "p256dh", "auth", testPublicWebPushPrivateKey()} {
		if strings.Contains(configResponse.Body.String(), forbidden) {
			t.Fatalf("push config leaked %q: %s", forbidden, configResponse.Body.String())
		}
	}

	subscription := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/synthetic-token", 2)
	body := testPublicWebPushSubscribeBody(t, subscription)
	missingCSRF := publicWebTestRequest(http.MethodPost, publicWebPushSubscriptionsPath, body, token)
	missingCSRF.Header.Set("Origin", "https://"+testPublicWebHost)
	missingCSRF.Header.Set("Sec-Fetch-Site", "same-origin")
	missingCSRF.Header.Set("Content-Type", "application/json")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("push subscribe without CSRF=%d", missingResponse.Code)
	}

	wrongOrigin := publicWebTestRequest(http.MethodPost, publicWebPushSubscriptionsPath, body, token)
	authorizePublicWebMutation(wrongOrigin, session.CSRF, "")
	wrongOrigin.Header.Set("Origin", "https://attacker.example.net")
	wrongResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongResponse, wrongOrigin)
	if wrongResponse.Code != http.StatusForbidden {
		t.Fatalf("push subscribe wrong origin=%d", wrongResponse.Code)
	}

	unknownField := strings.TrimSuffix(body, "}") + `,"message":"forbidden"}`
	unknownRequest := publicWebTestRequest(http.MethodPost, publicWebPushSubscriptionsPath, unknownField, token)
	authorizePublicWebMutation(unknownRequest, session.CSRF, "")
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknownRequest)
	if unknownResponse.Code != http.StatusBadRequest || strings.Contains(unknownResponse.Body.String(), subscription.Endpoint) {
		t.Fatalf("unknown push field=%d %s", unknownResponse.Code, unknownResponse.Body.String())
	}

	subscribe := publicWebTestRequest(http.MethodPost, publicWebPushSubscriptionsPath, body, token)
	authorizePublicWebMutation(subscribe, session.CSRF, "")
	subscribeResponse := httptest.NewRecorder()
	handler.ServeHTTP(subscribeResponse, subscribe)
	var subscribed struct {
		Version int    `json:"version"`
		ID      string `json:"subscription_id"`
	}
	if subscribeResponse.Code != http.StatusOK || json.Unmarshal(subscribeResponse.Body.Bytes(), &subscribed) != nil ||
		subscribed.Version != 1 || !publicWebPushOpaqueIDPattern.MatchString(subscribed.ID) {
		t.Fatalf("push subscribe=%d %s", subscribeResponse.Code, subscribeResponse.Body.String())
	}

	hashRequest := publicWebTestRequest(http.MethodGet, publicWebPushConfigPath, "", token)
	hashRequest.Header.Set(publicWebPushEndpointHashHeader, publicWebPushEndpointHash(subscription.Endpoint))
	hashResponse := httptest.NewRecorder()
	handler.ServeHTTP(hashResponse, hashRequest)
	if hashResponse.Code != http.StatusOK || !strings.Contains(hashResponse.Body.String(), subscribed.ID) ||
		strings.Contains(hashResponse.Body.String(), subscription.Endpoint) {
		t.Fatalf("push subscription recovery=%d %s", hashResponse.Code, hashResponse.Body.String())
	}

	duplicateHash := publicWebTestRequest(http.MethodGet, publicWebPushConfigPath, "", token)
	duplicateHash.Header.Add(publicWebPushEndpointHashHeader, publicWebPushEndpointHash(subscription.Endpoint))
	duplicateHash.Header.Add(publicWebPushEndpointHashHeader, publicWebPushEndpointHash(subscription.Endpoint))
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicateHash)
	if duplicateResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate endpoint hash=%d", duplicateResponse.Code)
	}

	secretField := "https://fcm.googleapis.com/fcm/send/must-not-leak"
	invalidDelete := publicWebTestRequest(http.MethodDelete,
		publicWebPushSubscriptionsPath+"/"+subscribed.ID, `{"`+secretField+`":true}`, token)
	authorizePublicWebMutation(invalidDelete, session.CSRF, "")
	invalidDeleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidDeleteResponse, invalidDelete)
	if invalidDeleteResponse.Code != http.StatusBadRequest ||
		strings.Contains(invalidDeleteResponse.Body.String(), secretField) {
		t.Fatalf("invalid push delete=%d %s", invalidDeleteResponse.Code, invalidDeleteResponse.Body.String())
	}

	deleteRequest := publicWebTestRequest(http.MethodDelete,
		publicWebPushSubscriptionsPath+"/"+subscribed.ID, `{}`, token)
	authorizePublicWebMutation(deleteRequest, session.CSRF, "")
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("push delete=%d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if _, found := instance.publicWebPush.Lookup(testPublicWebEmail, publicWebPushEndpointHash(subscription.Endpoint)); found {
		t.Fatal("deleted push subscription remained visible")
	}

	for _, path := range []string{
		"/api/remote/v1/push/send", publicWebPushSubscriptionsPath + "/bad-id",
		publicWebPushSubscriptionsPath + "/" + subscribed.ID + "/extra",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, publicWebTestRequest(http.MethodPost, path, `{}`, token))
		if response.Code != http.StatusNotFound {
			t.Fatalf("non-whitelisted push route %s=%d", path, response.Code)
		}
	}
}

func slicesContainExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestPublicWebPushDefaultOffAndConfigurationIsExplicit(t *testing.T) {
	if normalized, err := validatePublicWebPushSubject("https://phone.example.com"); err != nil ||
		normalized != "https://phone.example.com" {
		t.Fatalf("origin VAPID subject=%q err=%v", normalized, err)
	}
	if config, err := parsePublicWebPushStartupConfig(publicWebPushFlagConfig{}, []string{testPublicWebEmail}); err != nil || config != nil {
		t.Fatalf("default push config=%v err=%v", config, err)
	}
	if _, err := parsePublicWebPushStartupConfig(publicWebPushFlagConfig{
		PrivateKeyFile: "/private/vapid",
	}, []string{testPublicWebEmail}); err == nil {
		t.Fatal("push private key silently enabled push")
	}
	path := filepath.Join(t.TempDir(), "private-key")
	if err := os.WriteFile(path, []byte(testPublicWebPushPrivateKey()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := &publicWebPushStartupConfig{
		PrivateKeyFile: path, Subject: "mailto:push@example.com",
		SubscriptionsFile: filepath.Join(t.TempDir(), "subscriptions.json"),
		AllowedPrincipals: []string{testPublicWebEmail}, Sender: &publicWebPushTestSender{},
	}
	if _, err := newPublicWebPushManager(config); err == nil {
		t.Fatal("mode-0644 VAPID private key was accepted")
	}
	key := testPublicWebKey(t)
	_, handler, token := newPublicWebTestApp(
		t, true, &publicWebTestTransport{body: testPublicWebJWKS(t, key)},
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, publicWebTestRequest(http.MethodGet, publicWebPushConfigPath, "", token))
	if response.Code != http.StatusNotFound {
		t.Fatalf("default-off push route=%d, want 404", response.Code)
	}
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, publicWebTestRequest(http.MethodGet, "/api/remote/v1/session", "", token))
	var session struct {
		PublicVoice     bool `json:"public_voice"`
		PushEnabled     bool `json:"push_enabled"`
		PushAPIVersion  int  `json:"push_api_version"`
		BackgroundCalls bool `json:"background_calls"`
	}
	if json.Unmarshal(sessionResponse.Body.Bytes(), &session) != nil || session.PublicVoice ||
		session.PushEnabled || session.PushAPIVersion != 0 || session.BackgroundCalls {
		t.Fatalf("default-off session=%s", sessionResponse.Body.String())
	}
}

func TestPublicWebPushPersistencePrincipalPartitionAndLimits(t *testing.T) {
	sender := &publicWebPushTestSender{}
	manager, config := newPublicWebPushTestManager(t, sender, testPublicWebEmail, "second@example.com")
	first := testPublicWebPushRequest(t, "https://updates.push.services.mozilla.com/wpush/v2/synthetic-1", 2)
	id, err := manager.Upsert(testPublicWebEmail, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Lookup("second@example.com", publicWebPushEndpointHash(first.Endpoint)); ok {
		t.Fatal("subscription crossed verified-principal partition")
	}
	if err := manager.Delete("second@example.com", id); err != nil {
		t.Fatal(err)
	}
	if got, ok := manager.Lookup(testPublicWebEmail, publicWebPushEndpointHash(first.Endpoint)); !ok || got != id {
		t.Fatalf("cross-principal delete affected owner: id=%q ok=%v", got, ok)
	}
	for index := 2; index <= publicWebPushMaximumPerPrincipal; index++ {
		request := testPublicWebPushRequest(t,
			"https://updates.push.services.mozilla.com/wpush/v2/synthetic-"+string(rune('0'+index)), byte(index+1))
		if _, err := manager.Upsert(testPublicWebEmail, request); err != nil {
			t.Fatalf("upsert %d: %v", index, err)
		}
	}
	overLimit := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/over-limit", 9)
	if _, err := manager.Upsert(testPublicWebEmail, overLimit); !errors.Is(err, errPublicWebPushLimit) {
		t.Fatalf("per-principal limit error=%v", err)
	}
	info, err := os.Stat(config.SubscriptionsFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("subscription store mode=%v err=%v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Dir(config.SubscriptionsFile))
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("subscription directory mode=%v err=%v", dirInfo, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newPublicWebPushManager(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok := reopened.Lookup(testPublicWebEmail, publicWebPushEndpointHash(first.Endpoint)); !ok || got != id {
		t.Fatalf("reopened lookup id=%q ok=%v", got, ok)
	}
	entries, err := os.ReadDir(filepath.Dir(config.SubscriptionsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("atomic store left temporary file %q", entry.Name())
		}
	}
}

func TestPublicWebPushPersistedDuplicateEndpointFailsClosed(t *testing.T) {
	manager, config := newPublicWebPushTestManager(t, &publicWebPushTestSender{})
	request := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/persisted-duplicate", 2)
	if _, err := manager.Upsert(testPublicWebEmail, request); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(config.SubscriptionsFile)
	if err != nil {
		t.Fatal(err)
	}
	var disk publicWebPushDiskStore
	if err := json.Unmarshal(data, &disk); err != nil || len(disk.Subscriptions) != 1 {
		t.Fatalf("decode persisted store: subscriptions=%d err=%v", len(disk.Subscriptions), err)
	}
	duplicate := disk.Subscriptions[0]
	duplicate.ID = strings.Repeat("A", 32)
	if duplicate.ID == disk.Subscriptions[0].ID {
		duplicate.ID = strings.Repeat("B", 32)
	}
	disk.Subscriptions = append(disk.Subscriptions, duplicate)
	data, err = json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SubscriptionsFile, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := newPublicWebPushManager(config)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("persisted duplicate endpoint hash was accepted")
	}
}

func TestPublicWebPushRevokedPrincipalIsPurgedOnLoad(t *testing.T) {
	manager, config := newPublicWebPushTestManager(
		t, &publicWebPushTestSender{}, testPublicWebEmail, "revoked@example.com",
	)
	retained := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/retained", 2)
	revoked := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/revoked-principal", 3)
	if _, err := manager.Upsert(testPublicWebEmail, retained); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Upsert("revoked@example.com", revoked); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	config.AllowedPrincipals = []string{testPublicWebEmail}
	reopened, err := newPublicWebPushManager(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.Lookup("revoked@example.com", publicWebPushEndpointHash(revoked.Endpoint)); ok {
		t.Fatal("revoked principal subscription survived reload")
	}
	if _, ok := reopened.Lookup(testPublicWebEmail, publicWebPushEndpointHash(retained.Endpoint)); !ok {
		t.Fatal("allowed principal subscription was removed")
	}
	data, err := os.ReadFile(config.SubscriptionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), revoked.Endpoint) {
		t.Fatal("revoked principal capability remained in durable store")
	}
}

func TestPublicWebPushRejectsUnsafeOrMalformedSubscriptions(t *testing.T) {
	p256dh, auth := testPublicWebPushKeys(t, 2)
	tests := []publicWebPushSubscriptionRequest{
		{Endpoint: "http://fcm.googleapis.com/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://user:pass@fcm.googleapis.com/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://fcm.googleapis.com/send/x#fragment", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://127.0.0.1/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://10.0.0.1/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://100.64.0.1/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://[::1]/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://push.local/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://push.example/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://fcm.googleapis.com:8443/send/x", P256DH: p256dh, Auth: auth},
		{Endpoint: "https://fcm.googleapis.com/send/x", P256DH: "bad", Auth: auth},
		{Endpoint: "https://fcm.googleapis.com/send/x", P256DH: p256dh, Auth: "bad"},
	}
	for index, request := range tests {
		if err := validatePublicWebPushSubscription(request, time.Now(), false); err == nil {
			t.Fatalf("unsafe subscription %d accepted: %s", index, request)
		}
	}
	valid := publicWebPushSubscriptionRequest{
		Endpoint: "https://fcm.googleapis.com/fcm/send/x?token=opaque", P256DH: p256dh, Auth: auth,
	}
	if err := validatePublicWebPushSubscription(valid, time.Now(), false); err != nil {
		t.Fatalf("valid push subscription rejected: %v", err)
	}
}

func TestPublicWebPushExactGenerationDedupeFixedPayloadAndNonBlockingQDCTrigger(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	sender := &publicWebPushTestSender{started: started, release: release}
	manager, _ := newPublicWebPushTestManager(t, sender)
	request := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/dedupe", 2)
	if _, err := manager.Upsert(testPublicWebEmail, request); err != nil {
		t.Fatal(err)
	}
	application := &app{
		publicWeb: &publicWebStartupConfig{
			Control: true, DirectVoice: true, TURNIssuer: &turnauth.Issuer{},
			Push: &publicWebPushStartupConfig{},
		},
		publicWebPush: manager,
		remoteMedia: &remoteMediaManager{cfg: remoteMediaRuntimeConfig{IssueTURN: func() (*remotevoice.TURNRelayConfig, error) {
			return nil, errors.New("unused")
		}}},
		remoteIncomingAnswer: true, remoteRescueHangup: true,
	}
	application.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
	}}, time.Now())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("QDC incoming trigger did not enqueue Web Push")
	}
	// Repeated observations for the same exact generation must neither block
	// the CLCC path nor enqueue another delivery.
	returned := make(chan struct{})
	go func() {
		application.applyCallPoll([]parsedCall{{
			Index: 1, Direction: "incoming", State: "incoming", Number: "10086",
		}}, time.Now().Add(time.Second))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("QDC call poll blocked on Web Push delivery")
	}
	close(release)
	waitPublicWebPushIdle(t, manager)
	sends := sender.snapshot()
	if len(sends) != 1 || sends[0].Payload != string(publicWebIncomingCallPayload) ||
		strings.Contains(sends[0].Payload, "10086") || strings.Contains(sends[0].Payload, "number") {
		t.Fatalf("unexpected incoming payload/dedupe: %+v", sends)
	}
}

func TestPublicWebPushRetriesOnlyTransientlyFailedSubscriptions(t *testing.T) {
	retryEndpoint := "https://fcm.googleapis.com/fcm/send/retry-once"
	stableEndpoint := "https://updates.push.services.mozilla.com/wpush/v2/stable"
	sender := &publicWebPushSequenceSender{responses: map[string][]publicWebPushTestResult{
		retryEndpoint: {
			{status: http.StatusServiceUnavailable},
			{status: http.StatusCreated},
		},
		stableEndpoint: {{status: http.StatusCreated}},
	}}
	manager, _ := newPublicWebPushTestManager(t, sender)
	manager.retryWait = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	for index, endpoint := range []string{retryEndpoint, stableEndpoint} {
		if _, err := manager.Upsert(testPublicWebEmail,
			testPublicWebPushRequest(t, endpoint, byte(index+2))); err != nil {
			t.Fatal(err)
		}
	}
	if !manager.enqueueIncoming("qdc:retry") {
		t.Fatal("transient retry event was not queued")
	}
	waitPublicWebPushIdle(t, manager)
	if got := sender.callCount(retryEndpoint); got != 2 {
		t.Fatalf("transient endpoint sends=%d, want 2", got)
	}
	if got := sender.callCount(stableEndpoint); got != 1 {
		t.Fatalf("successful endpoint was repeated %d times", got)
	}
}

func TestPublicWebPushQueueFullDoesNotConsumeGenerationDedupe(t *testing.T) {
	idle := make(chan struct{})
	close(idle)
	manager := &publicWebPushManager{
		dedupe: make(map[string]struct{}), queue: make(chan publicWebPushEvent, 1), idle: idle,
	}
	manager.queue <- publicWebPushEvent{dedupeKey: "occupied"}
	if manager.enqueueIncoming("qdc:queue-retry") {
		t.Fatal("full queue unexpectedly accepted event")
	}
	if _, consumed := manager.dedupe["qdc:queue-retry"]; consumed || len(manager.dedupeFIFO) != 0 {
		t.Fatal("full queue consumed the generation dedupe key")
	}
	<-manager.queue
	if !manager.enqueueIncoming("qdc:queue-retry") {
		t.Fatal("same generation could not be queued after capacity returned")
	}
}

func TestPublicWebPushExternalIncomingTriggerUsesExactPublicGeneration(t *testing.T) {
	sender := &publicWebPushTestSender{}
	manager, _ := newPublicWebPushTestManager(t, sender)
	if _, err := manager.Upsert(testPublicWebEmail,
		testPublicWebPushRequest(t, "https://updates.push.services.mozilla.com/wpush/v2/external", 2)); err != nil {
		t.Fatal(err)
	}
	fixture := newExternalVoiceRuntimeFixture(t)
	application := &app{
		publicWeb: &publicWebStartupConfig{
			Control: true, ExternalVoice: true, TURNIssuer: &turnauth.Issuer{},
			Push: &publicWebPushStartupConfig{},
		},
		// Leave sipVoice unpublished to cover the startup race where the
		// supervisor observes a first incoming event before app stores it.
		publicWebPush: manager,
	}
	fixture.runtime.mu.Lock()
	fixture.runtime.incomingNotifier = application.notifyPublicWebExternalIncoming
	fixture.runtime.mu.Unlock()
	incoming := fixture.incoming(t)
	application.notifyPublicWebExternalIncoming(incoming.Call)
	waitPublicWebPushIdle(t, manager)
	if sends := sender.snapshot(); len(sends) != 1 || sends[0].Payload != string(publicWebIncomingCallPayload) {
		t.Fatalf("external incoming push=%+v", sends)
	}
}

func TestPublicWebPushRevokedCleanupPersistsAndLogsNoCapabilities(t *testing.T) {
	endpoint := "https://fcm.googleapis.com/fcm/send/revoked-secret-endpoint"
	p256dh, auth := testPublicWebPushKeys(t, 2)
	sensitiveError := errors.New("synthetic error " + endpoint + " " + p256dh + " " + auth)
	sender := &publicWebPushTestSender{
		status: map[string]int{endpoint: http.StatusGone}, err: sensitiveError,
	}
	manager, config := newPublicWebPushTestManager(t, sender)
	if _, err := manager.Upsert(testPublicWebEmail, publicWebPushSubscriptionRequest{
		Endpoint: endpoint, P256DH: p256dh, Auth: auth,
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	if !manager.enqueueIncoming("qdc:17") {
		t.Fatal("revoked cleanup event was not queued")
	}
	waitPublicWebPushIdle(t, manager)
	if sends := sender.snapshot(); len(sends) != 1 {
		t.Fatalf("revoked subscription was retried %d times", len(sends))
	}
	for _, secret := range []string{endpoint, p256dh, auth, sensitiveError.Error()} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("push log leaked capability %q: %s", secret, logs.String())
		}
	}
	if _, ok := manager.Lookup(testPublicWebEmail, publicWebPushEndpointHash(endpoint)); ok {
		t.Fatal("HTTP 410 subscription was not removed")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newPublicWebPushManager(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.Lookup(testPublicWebEmail, publicWebPushEndpointHash(endpoint)); ok {
		t.Fatal("revoked subscription cleanup was not durable")
	}
	data, err := os.ReadFile(config.SubscriptionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), endpoint) || strings.Contains(string(data), p256dh) || strings.Contains(string(data), auth) {
		t.Fatalf("revoked capability remained in store: %s", data)
	}
}

type publicWebPushTestHTTPClient struct {
	mu           sync.Mutex
	request      *http.Request
	body         []byte
	status       int
	responseBody string
}

func (c *publicWebPushTestHTTPClient) Do(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.request = request.Clone(context.Background())
	c.body = append([]byte(nil), body...)
	c.mu.Unlock()
	status := c.status
	if status == 0 {
		status = http.StatusCreated
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(c.responseBody))}, nil
}

func TestPublicWebPushMatureSenderEncryptsFixedPayloadWithoutNetwork(t *testing.T) {
	privateKey := testPublicWebPushPrivateKey()
	raw, _ := base64.RawURLEncoding.DecodeString(privateKey)
	key, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := &publicWebPushTestHTTPClient{}
	sender := &publicWebPushDeliveryClient{
		privateKey: privateKey,
		publicKey:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		subject:    "push@example.com",
		httpClient: client,
	}
	request := testPublicWebPushRequest(t, "https://fcm.googleapis.com/fcm/send/no-network", 2)
	status, err := sender.Send(context.Background(), publicWebPushDiskSubscription{
		Endpoint: request.Endpoint, P256DH: request.P256DH, Auth: request.Auth,
	}, publicWebIncomingCallPayload)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("sender status=%d err=%v", status, err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.request == nil || client.request.URL.String() != request.Endpoint ||
		client.request.Header.Get("Content-Encoding") != "aes128gcm" ||
		client.request.Header.Get("Urgency") != "high" ||
		client.request.Header.Get("Topic") != publicWebIncomingCallTopic ||
		client.request.Header.Get("Authorization") == "" ||
		bytes.Contains(client.body, publicWebIncomingCallPayload) {
		t.Fatalf("unexpected Web Push request headers=%v body_len=%d", client.request.Header, len(client.body))
	}
}

func TestPublicWebPushSenderReturnsBoundedProviderReason(t *testing.T) {
	privateKey := testPublicWebPushPrivateKey()
	raw, _ := base64.RawURLEncoding.DecodeString(privateKey)
	key, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	client := &publicWebPushTestHTTPClient{
		status:       http.StatusBadRequest,
		responseBody: `{"reason":"BadJwtToken"}`,
	}
	sender := &publicWebPushDeliveryClient{
		privateKey: privateKey,
		publicKey:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		subject:    "https://phone.example.com",
		httpClient: client,
	}
	request := testPublicWebPushRequest(t, "https://web.push.apple.com/synthetic", 7)
	status, sendErr := sender.Send(context.Background(), publicWebPushDiskSubscription{
		Endpoint: request.Endpoint, P256DH: request.P256DH, Auth: request.Auth,
	}, publicWebIncomingCallPayload)
	if status != http.StatusBadRequest || publicWebPushProviderFailureReason(sendErr) != "BadJwtToken" {
		t.Fatalf("status=%d reason=%q", status, publicWebPushProviderFailureReason(sendErr))
	}
	client.responseBody = `{"reason":"secret marker with spaces"}`
	_, sendErr = sender.Send(context.Background(), publicWebPushDiskSubscription{
		Endpoint: request.Endpoint, P256DH: request.P256DH, Auth: request.Auth,
	}, publicWebIncomingCallPayload)
	if publicWebPushProviderFailureReason(sendErr) != "" {
		t.Fatal("unbounded provider response was exposed")
	}
}

func TestPublicWebPushEndpointHashIsExactSHA256Base64URL(t *testing.T) {
	endpoint := "https://fcm.googleapis.com/fcm/send/exact"
	digest := sha256.Sum256([]byte(endpoint))
	want := base64.RawURLEncoding.EncodeToString(digest[:])
	if got := publicWebPushEndpointHash(endpoint); got != want || len(got) != 43 {
		t.Fatalf("endpoint hash=%q want=%q", got, want)
	}
}

func TestPublicWebPushDialFallsBackAcrossVerifiedAddressFamilies(t *testing.T) {
	addresses := []net.IPAddr{
		{IP: net.ParseIP("2001:4860:4860::8888")},
		{IP: net.ParseIP("2001:4860:4860::8844")},
		{IP: net.ParseIP("8.8.8.8")},
	}
	var attempts []string
	var peer net.Conn
	connection, err := dialPublicWebPushAddresses(
		context.Background(), "tcp", "443", addresses,
		func(_ context.Context, _, address string) (net.Conn, error) {
			attempts = append(attempts, address)
			if len(attempts) == 1 {
				return nil, errors.New("synthetic IPv6 route unavailable")
			}
			local, remote := net.Pipe()
			peer = remote
			return local, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	defer peer.Close()
	if len(attempts) != 2 || attempts[0] != "[2001:4860:4860::8888]:443" ||
		attempts[1] != "8.8.8.8:443" {
		t.Fatalf("dial attempts=%v", attempts)
	}
}

func TestPublicWebPushResolverEndpointsAreExactPublicIPv4(t *testing.T) {
	if len(publicWebPushDNSResolvers) != 3 {
		t.Fatalf("resolver count=%d", len(publicWebPushDNSResolvers))
	}
	seen := make(map[string]struct{}, len(publicWebPushDNSResolvers))
	for _, endpoint := range publicWebPushDNSResolvers {
		host, port, err := net.SplitHostPort(endpoint)
		if err != nil || port != "53" || !publicWebPushIPIsPublic(net.ParseIP(host)) {
			t.Fatal("unsafe public DNS resolver endpoint")
		}
		if _, duplicate := seen[endpoint]; duplicate {
			t.Fatal("duplicate public DNS resolver endpoint")
		}
		seen[endpoint] = struct{}{}
	}
	resolver := newPublicWebPushResolver()
	if resolver == nil || !resolver.PreferGo || !resolver.StrictErrors {
		t.Fatal("public push resolver is not fail-closed")
	}
}
