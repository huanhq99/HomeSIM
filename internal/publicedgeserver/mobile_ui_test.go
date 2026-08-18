package publicedgeserver

import (
	"bytes"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iniwex5/vohive/internal/publicedge"
)

func requestMobileUI(
	t *testing.T,
	harness *serverTestHarness,
	method string,
	path string,
	authorized bool,
) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, harness.httpServer.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = ProductionExactHost
	if authorized {
		request.Header.Set("Authorization", testBearerToken)
	}
	response, err := harness.httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, body
}

func TestMobileUIIsAuthenticatedExactReadOnlySurface(t *testing.T) {
	harness := newServerTestHarness(t)

	response, body := requestMobileUI(t, harness, http.MethodGet, MobileUIPath, true)
	if response.StatusCode != http.StatusOK ||
		response.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
		response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("Content-Security-Policy") != mobileUICSP ||
		response.Header.Get("Referrer-Policy") != "no-referrer" ||
		response.Header.Get("X-Content-Type-Options") != "nosniff" ||
		response.Header.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
		response.Header.Get("Cross-Origin-Resource-Policy") != "same-origin" ||
		response.Header.Get("X-Frame-Options") != "DENY" ||
		response.Header.Get("X-Robots-Tag") != "noindex, nofollow, noarchive, nosnippet" {
		t.Fatalf("mobile UI headers/status: status=%d headers=%v", response.StatusCode, response.Header)
	}
	permissions := response.Header.Get("Permissions-Policy")
	for _, disabled := range []string{"camera=()", "microphone=()", "geolocation=()", "usb=()"} {
		if !strings.Contains(permissions, disabled) {
			t.Fatalf("mobile UI Permissions-Policy did not disable %q: %q", disabled, permissions)
		}
	}

	html := string(body)
	for _, required := range []string{
		`lang="zh-CN"`,
		`name="viewport"`,
		`href="` + MobileUICSSPath + `"`,
		`src="` + MobileUIJSPath + `"`,
		`只读界面`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("mobile UI omitted required contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"<button", "<form", "<input", "<textarea", "<select", "<a ",
		"<iframe", "<video", "<audio", "<style", "javascript:", "http://", "https://",
		"/api/remote", "7576", "7577", "phone_number", "sms_body", "message_text",
	} {
		if strings.Contains(strings.ToLower(html), strings.ToLower(forbidden)) {
			t.Fatalf("mobile UI contains forbidden surface %q", forbidden)
		}
	}
}

func TestMobileUIAssetsAreEmbeddedAuthenticatedAndStateOnly(t *testing.T) {
	harness := newServerTestHarness(t)
	for _, asset := range []struct {
		path        string
		contentType string
	}{
		{path: MobileUICSSPath, contentType: "text/css; charset=utf-8"},
		{path: MobileUIJSPath, contentType: "text/javascript; charset=utf-8"},
	} {
		response, body := requestMobileUI(t, harness, http.MethodGet, asset.path, true)
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != asset.contentType ||
			response.Header.Get("Cache-Control") != "no-store" || len(body) == 0 {
			t.Fatalf("asset %q status=%d type=%q cache=%q bytes=%d", asset.path,
				response.StatusCode, response.Header.Get("Content-Type"),
				response.Header.Get("Cache-Control"), len(body))
		}
		unauthorized, unauthorizedBody := requestMobileUI(t, harness, http.MethodGet, asset.path, false)
		if unauthorized.StatusCode != http.StatusUnauthorized ||
			string(unauthorizedBody) != `{"error":"unauthorized"}` {
			t.Fatalf("unauthorized asset %q status=%d body=%s", asset.path,
				unauthorized.StatusCode, unauthorizedBody)
		}
	}

	source := mobileUIJS
	for _, required := range []string{
		`var SNAPSHOT_PATH = "` + SnapshotAPIPath + `"`,
		`credentials: "same-origin"`,
		`cache: "no-store"`,
		`redirect: "manual"`,
		`documentObject.visibilityState === "hidden"`,
		`activeRequest.abort()`,
		`response.status`,
		`parseSnapshotText`,
		`MAX_RESPONSE_BYTES`,
		`MAX_BACKOFF_MS`,
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("mobile UI script omitted contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "indexedDB", "document.cookie", "cookieStore",
		"sendBeacon", "WebSocket", "EventSource", "XMLHttpRequest", "eval(", "new Function",
		"response.text(",
		"innerHTML", "insertAdjacentHTML", "http://", "https://", "/api/remote", "7576", "7577",
		"phone_number", "sms_body", "message_text", "sdp", "candidate",
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("mobile UI script contains forbidden capability %q", forbidden)
		}
	}
	if strings.Contains(mobileUICSS, "url(") || strings.Contains(mobileUICSS, "@import") {
		t.Fatal("mobile UI CSS may not load external or additional resources")
	}
}

func TestMobileUIAccessFailuresAndRoutesFailClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		authError  error
		wantStatus int
		wantBody   string
	}{
		{name: "unauthorized", authError: publicedge.ErrAccessUnauthorized,
			wantStatus: http.StatusUnauthorized, wantBody: `{"error":"unauthorized"}`},
		{name: "verification unavailable", authError: publicedge.ErrAccessUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantBody: `{"error":"unavailable"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newServerTestHarness(t)
			harness.server.config.httpAuth = HTTPAuthFunc(func(*http.Request) error {
				return test.authError
			})
			response, body := requestMobileUI(t, harness, http.MethodGet, MobileUIPath, false)
			if response.StatusCode != test.wantStatus || string(body) != test.wantBody ||
				response.Header.Get("Cache-Control") != "no-store" ||
				response.Header.Get("Content-Security-Policy") != mobileUICSP {
				t.Fatalf("auth failure status=%d cache=%q csp=%q body=%s", response.StatusCode,
					response.Header.Get("Cache-Control"), response.Header.Get("Content-Security-Policy"), body)
			}
		})
	}

	harness := newServerTestHarness(t)
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
		response, body := requestMobileUI(t, harness, method, MobileUIPath, true)
		if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet ||
			len(body) != 0 {
			t.Fatalf("method %s status=%d allow=%q body=%q", method, response.StatusCode,
				response.Header.Get("Allow"), body)
		}
	}
	for _, path := range []string{
		"/?refresh=1",
		"/index.html",
		"/assets/public-mobile-v1.js?version=1",
		"/assets/../assets/public-mobile-v1.js",
		"/api/public/v1/state/snapshot/",
	} {
		response, _ := requestMobileUI(t, harness, http.MethodGet, path, true)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("non-exact UI path %q returned %d", path, response.StatusCode)
		}
	}

	request, _ := http.NewRequest(http.MethodGet, harness.httpServer.URL+MobileUIPath, nil)
	request.Host = ProductionExactHost + ":443"
	request.Header.Set("Authorization", testBearerToken)
	response, err := harness.httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("host with port returned %d", response.StatusCode)
	}
}

func TestMobileUIPureJavaScriptContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable")
	}
	command := exec.Command(node, "--test", filepath.Join("assets", "public-mobile-v1.test.js"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Node.js mobile UI contract failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("pass 8")) || !bytes.Contains(output, []byte("fail 0")) {
		t.Fatalf("unexpected Node.js contract summary:\n%s", output)
	}
}
