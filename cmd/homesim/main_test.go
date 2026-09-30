package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func request(a *app, method, path, payload, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://homesim.local"+path, strings.NewReader(payload))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}
func TestAuthenticationAndCSRF(t *testing.T) {
	a, err := openApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if w := request(a, "GET", "/api/messages", "", "", nil); w.Code != 401 {
		t.Fatalf("unprotected messages %d", w.Code)
	}
	b, _ := json.Marshal(map[string]string{"username": "tester", "password": "test-only-passphrase", "setup_token": a.setupToken})
	if w := request(a, "POST", "/api/auth", string(b), "https://evil.invalid", nil); w.Code != 403 {
		t.Fatal("cross-origin enrollment allowed")
	}
	w := request(a, "POST", "/api/auth", string(b), "http://homesim.local", nil)
	if w.Code != 200 {
		t.Fatalf("setup %d %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	c := cookies[0]
	if w := request(a, "GET", "/api/messages", "", "", c); w.Code != 200 {
		t.Fatal("session failed")
	}
	if w := request(a, "POST", "/api/messages/send", `{}`, "http://evil.invalid", c); w.Code != 403 {
		t.Fatal("CSRF send allowed")
	}
	if w := request(a, "POST", "/api/at", `{}`, "http://homesim.local", c); w.Code != 404 {
		t.Fatal("arbitrary AT exposed")
	}
	w = request(a, "DELETE", "/api/auth", "", "http://homesim.local", c)
	if w.Code != 200 {
		t.Fatal("logout failed")
	}
	if w := request(a, "GET", "/api/messages", "", "", c); w.Code != 401 {
		t.Fatal("logged-out session valid")
	}
}
func TestPersistenceDedupeAndInterruptedSend(t *testing.T) {
	dir := t.TempDir()
	a, err := openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now()
	a.receive("10086", "验证码 123456", ts)
	a.receive("10086", "验证码 123456", ts)
	if len(a.messages) != 1 {
		t.Fatal("SMS duplicated")
	}
	a.messages = append(a.messages, message{ID: "pending-test", Direction: "outgoing", Status: "pending"})
	if err = atomicJSON(filepath.Join(dir, "messages.json"), a.messages); err != nil {
		t.Fatal(err)
	}
	a, err = openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.messages) != 2 || a.messages[1].Status != "unknown" {
		t.Fatal("interrupted SMS must never auto-retry")
	}
	if a.messages[0].Code != "123456" {
		t.Fatal("verification code missing")
	}
	if err = os.WriteFile(filepath.Join(dir, "messages.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = openApp(dir); err == nil {
		t.Fatal("corrupt store silently discarded")
	}
}
func TestAudioCodecAndCallParser(t *testing.T) {
	for _, x := range []int16{-30000, -2000, -500, 0, 500, 2000, 30000} {
		got := muToLinear(linearToMu(x))
		delta := int(got) - int(x)
		if delta < 0 {
			delta = -delta
		}
		if delta > 1500 {
			t.Fatalf("G711 roundtrip %d %d", x, got)
		}
	}
	rows := parseCalls("\r\n+CLCC: 1,1,4,0,0,\"+8613800000000\",145\r\nOK\r\n")
	if len(rows) != 1 || rows[0].State != 4 || rows[0].Number != "+8613800000000" {
		t.Fatal("incoming call parsing")
	}
}
