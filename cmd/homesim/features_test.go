package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLineRoutingIsolation(t *testing.T) {
	root, err := openApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := openApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second.root = root
	root.messages = []message{{ID: "one", Content: "primary"}}
	second.messages = []message{{ID: "two", Content: "second"}}
	root.registry = &deviceRegistry{apps: map[string]*app{"secondary": second}}
	root.sessions["test"] = session{time.Now().Add(time.Hour)}
	cookie := &http.Cookie{Name: "homesim", Value: "test"}
	w := request(root, "GET", "/api/messages?device_id=secondary", "", "", cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "second") || strings.Contains(w.Body.String(), "primary") {
		t.Fatal("wrong line data", w.Body)
	}
	if w = request(root, "GET", "/api/messages?device_id=missing", "", "", cookie); w.Code != 404 {
		t.Fatal("unknown line silently fell back")
	}
	if w = request(root, "GET", "/api/messages?device_id=secondary", "", "", nil); w.Code != 401 {
		t.Fatal("child route bypassed auth")
	}
}
func TestCallHistoryTransitions(t *testing.T) {
	now := time.Now()
	incoming := []call{{ID: "1", Direction: 1, State: 4, Number: "10086"}}
	rows := reconcileCalls(nil, incoming, now)
	if len(rows) != 1 || rows[0].Result != "ongoing" {
		t.Fatal(rows)
	}
	id := rows[0].ID
	rows = reconcileCalls(rows, incoming, now.Add(time.Second))
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatal("duplicate ring")
	}
	rows = reconcileCalls(rows, nil, now.Add(4*time.Second))
	if rows[0].Result != "missed" || rows[0].Ended == nil {
		t.Fatal(rows)
	}
	incoming[0].State = 0
	rows = reconcileCalls(rows, incoming, now.Add(time.Minute))
	if len(rows) != 2 || rows[1].ID == id || rows[1].Answered == nil {
		t.Fatal("modem call ID reused old history")
	}
	rows = reconcileCalls(rows, nil, now.Add(2*time.Minute))
	if rows[1].Result != "completed" {
		t.Fatal(rows)
	}
}
func TestInterruptedHistoryIsNotMissed(t *testing.T) {
	a, _ := openApp(t.TempDir())
	rows := []callRecord{{ID: "test", Started: time.Now(), Result: "ongoing"}}
	if err := atomicJSON(filepath.Join(a.data, "calls.json"), rows); err != nil {
		t.Fatal(err)
	}
	if err := a.loadHistory(); err != nil {
		t.Fatal(err)
	}
	if a.history.rows[0].Result != "interrupted" {
		t.Fatal("restart fabricated missed call")
	}
}
func TestPortAndProxyValidation(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "/dev/ttyUSBx", "/dev/ttyUSB0/../../etc/passwd", "/dev/serial/by-id/../../random", "/dev/ttyUSB2\n"} {
		if validDevice(deviceConfig{"test", "test", p, "disabled", true}) {
			t.Fatal("unsafe port", p)
		}
	}
	if !validDevice(deviceConfig{"test", "test", "/host-dev/serial/by-path/pci-0-usb-2:1.2-port0", "hw:1,0", true}) {
		t.Fatal("stable path rejected")
	}
	c := proxyConfig{Enabled: true, Mode: "socks5", Listen: "192.168.6.181", Port: 1080, Interface: "usb0", Username: "test", Password: "test-only-password"}
	if !validProxy(c, []string{"usb0"}) {
		t.Fatal("valid config rejected")
	}
	if validProxy(c, []string{"eth0"}) {
		t.Fatal("proxy allowed unrelated NAS interface")
	}
	c.Listen = "0.0.0.0"
	if validProxy(c, []string{"usb0"}) {
		t.Fatal("proxy allowed wildcard public listener")
	}
}
func TestSocksAuthenticationAndTarget(t *testing.T) {
	cfg := proxyConfig{Username: "u", Password: "password"}
	input := []byte{5, 1, 2, 1, 1, 'u', 8}
	input = append(input, []byte("password")...)
	input = append(input, 5, 1, 0, 3, 11)
	input = append(input, []byte("example.com")...)
	input = append(input, 1, 187)
	reader := bytes.NewReader(input)
	var out bytes.Buffer
	if err := socksHandshake(reader, &out, cfg); err != nil {
		t.Fatal(err)
	}
	target, err := socksTarget(reader)
	if err != nil || target != "example.com:443" {
		t.Fatal(target, err)
	}
	if err = socksHandshake(bytes.NewReader([]byte{5, 1, 0}), &out, cfg); err == nil {
		t.Fatal("SOCKS allowed unauthenticated client")
	}
}
func TestNativeTokenOriginAndRevocation(t *testing.T) {
	a, _ := openApp(t.TempDir())
	token := randomID()
	row := nativeRegistration{ID: "test", Hash: tokenHash(token), Expires: time.Now().Add(time.Hour)}
	atomicJSON(filepath.Join(a.data, "native-devices.json"), []nativeRegistration{row})
	r := httptest.NewRequest("GET", "http://homesim.local/api/devices", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("valid native token rejected")
	}
	r = httptest.NewRequest("DELETE", "http://homesim.local/api/native/register", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("native mutation skipped origin check")
	}
	r.Header.Set("Origin", "http://homesim.local")
	w = httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 || a.nativeAuthenticated(r) {
		t.Fatal("native credential not revoked")
	}
}
func TestNotificationSecretsNotReturned(t *testing.T) {
	a, _ := openApp(t.TempDir())
	atomicJSON(filepath.Join(a.data, "notifications.json"), notifyConfig{Channel: "bark", URL: "https://example.com/private-key", Secret: "private-secret"})
	w := httptest.NewRecorder()
	a.notificationConfig(w, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(w.Body.String(), "private-") {
		t.Fatal("notification secret disclosed")
	}
}
func TestCanceledDeviceContextDoesNotStartSecondOwner(t *testing.T) {
	a, _ := openApp(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	a.registry = &deviceRegistry{ctx: ctx, apps: map[string]*app{"primary": a}, cancels: map[string]context.CancelFunc{"primary": cancel}, done: map[string]chan struct{}{"primary": done}}
	if err := a.startDeviceLocked(deviceConfig{ID: "primary"}); err == nil {
		t.Fatal("started another serial owner while previous one was stopping")
	}
}
func TestNotificationConfigBoundToExplicitOptIn(t *testing.T) {
	a, _ := openApp(t.TempDir())
	var c notifyConfig
	b, _ := os.ReadFile(filepath.Join(a.data, "notifications.json"))
	_ = json.Unmarshal(b, &c)
	if c.Enabled {
		t.Fatal("notifications automatically enabled")
	}
}
