//go:build asterisk_integration

package asterisk

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAsterisk22RESTOverEventWebSocketLive is deliberately excluded from the
// normal test suite. With the explicit build tag it fails, rather than skips,
// unless every synthetic loopback-PBX input is provided.
func TestAsterisk22RESTOverEventWebSocketLive(t *testing.T) {
	baseURL := requiredIntegrationEnv(t, "MACCELLULAR_ASTERISK_INTEGRATION_URL")
	username := requiredIntegrationEnv(t, "MACCELLULAR_ASTERISK_INTEGRATION_USERNAME")
	password := readIntegrationPassword(t, requiredIntegrationEnv(t, "MACCELLULAR_ASTERISK_INTEGRATION_PASSWORD_FILE"))
	expectedVersion := requiredIntegrationEnv(t, "MACCELLULAR_ASTERISK_INTEGRATION_VERSION")
	expectedEntityID := requiredIntegrationEnv(t, "MACCELLULAR_ASTERISK_INTEGRATION_ENTITY_ID")
	application := requiredIntegrationEnv(t, "MACCELLULAR_ASTERISK_INTEGRATION_APPLICATION")

	normalized, err := normalizeConfig(Config{
		GatewayID: "synthetic-integration-gateway", BaseURL: baseURL,
		Application: application, IncomingArgument: "incoming",
		IncomingContext: "from-cellular", IncomingEndpoint: "cellular-gateway",
		IncomingPolicyID: "djonehub-incoming-v1", Username: username, Password: password,
		ExpectedVersion: expectedVersion, ExpectedEntityID: expectedEntityID,
		HTTPTimeout: 5 * time.Second, MaxHTTPBodyBytes: defaultMaxHTTPBodyBytes,
	})
	password = ""
	if err != nil {
		t.Fatalf("integration configuration rejected: %v", err)
	}
	defer zeroRecoverySecret(&normalized)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpClient := newARIHTTPClient(normalized)
	defer httpClient.close()
	pbx, err := httpClient.inspectPBX(ctx, expectedVersion, expectedEntityID, "")
	if err != nil {
		t.Fatalf("initial PBX identity: %v", err)
	}
	eventWS, err := httpClient.dialEvents(ctx, application)
	if err != nil {
		t.Fatalf("event WebSocket: %v", err)
	}
	ari, err := newARIWSClient(eventWS, normalized, pbx.EntityID)
	if err != nil {
		_ = eventWS.Close()
		t.Fatalf("REST-over-WebSocket client: %v", err)
	}
	defer ari.close()
	if _, err := ari.inspectPBX(ctx, pbx.Version, pbx.EntityID, pbx.StartupTime); err != nil {
		t.Fatalf("same-WebSocket PBX identity: %v", err)
	}

	missingID := integrationOpaqueID(t, "dj1int_missing_")
	response, err := ari.do(ctx, http.MethodGet, []string{"channels", missingID}, nil)
	if err != nil || response.status != http.StatusNotFound {
		t.Fatalf("same-WebSocket missing channel status=%d err=%v", response.status, err)
	}

	bridgeID := integrationOpaqueID(t, "dj1int_bridge_")
	created := false
	defer func() {
		if created {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_, _ = ari.do(cleanupCtx, http.MethodDelete, []string{"bridges", bridgeID}, nil)
		}
	}()
	response, err = ari.do(ctx, http.MethodPost, []string{"bridges", bridgeID}, url.Values{"type": {"mixing"}})
	if err != nil || !status2xx(response.status) {
		t.Fatalf("same-WebSocket bridge create status=%d err=%v", response.status, err)
	}
	created = true
	var bridge ariBridge
	if err := decodeOneJSON(response.body, &bridge); err != nil || bridge.ID != bridgeID || len(bridge.Channels) != 0 {
		t.Fatalf("same-WebSocket bridge create contract invalid")
	}
	response, err = ari.do(ctx, http.MethodGet, []string{"bridges", bridgeID}, nil)
	if err != nil || response.status != http.StatusOK || decodeOneJSON(response.body, &bridge) != nil || bridge.ID != bridgeID {
		t.Fatalf("same-WebSocket bridge inspect status=%d err=%v", response.status, err)
	}
	response, err = ari.do(ctx, http.MethodDelete, []string{"bridges", bridgeID}, nil)
	if err != nil || !status2xx(response.status) {
		t.Fatalf("same-WebSocket bridge delete status=%d err=%v", response.status, err)
	}
	created = false
	response, err = ari.do(ctx, http.MethodGet, []string{"bridges", bridgeID}, nil)
	if err != nil || response.status != http.StatusNotFound {
		t.Fatalf("same-WebSocket deleted bridge status=%d err=%v", response.status, err)
	}
	select {
	case <-ari.done:
		t.Fatalf("REST-over-WebSocket became terminal: %v", ari.terminalError())
	default:
	}
}

func requiredIntegrationEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("explicit integration input %s is required", name)
	}
	return value
}

func readIntegrationPassword(t *testing.T, path string) string {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatal("integration password path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > 1024 {
		t.Fatal("integration password file contract rejected")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("integration password file could not be read")
	}
	defer func() {
		for index := range content {
			content[index] = 0
		}
	}()
	password := strings.TrimSuffix(string(content), "\n")
	if password == "" || strings.ContainsAny(password, "\r\n") {
		t.Fatal("integration password must contain one non-empty line")
	}
	return password
}

func integrationOpaqueID(t *testing.T, prefix string) string {
	t.Helper()
	randomBytes := make([]byte, 18)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatal("integration random identifier unavailable")
	}
	return prefix + base64.RawURLEncoding.EncodeToString(randomBytes)
}
