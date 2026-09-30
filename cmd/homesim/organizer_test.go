package main

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func organizerFixture(t *testing.T) (*app, *http.Cookie) {
	t.Helper()
	a, err := openApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Cookie{Name: "homesim", Value: "synthetic-organizer-session"}
	a.sessions[c.Value] = session{Expires: time.Now().Add(time.Hour)}
	a.auth = credentials{Username: "fixture", Hash: "synthetic-private-hash"}
	a.receive("10086", "synthetic test message", time.Now())
	return a, c
}

func TestOrganizerPersistentAndPartialUpdates(t *testing.T) {
	a, c := organizerFixture(t)
	for _, b := range []string{`{"peer":"10086","name":"移动","starred":true}`, `{"peer":"10086","archived":true}`} {
		w := request(a, "POST", "/api/peers", b, "http://homesim.local", c)
		if w.Code != 200 {
			t.Fatalf("update: %d %s", w.Code, w.Body)
		}
	}
	ids, _ := json.Marshal(map[string]any{"ids": []string{a.messages[0].ID}, "read": true})
	if w := request(a, "POST", "/api/messages/read", string(ids), "http://homesim.local", c); w.Code != 200 {
		t.Fatalf("read: %d", w.Code)
	}
	reopened, err := openApp(a.data)
	if err != nil {
		t.Fatal(err)
	}
	if p := reopened.peers["10086"]; p.Name != "移动" || !p.Starred || !p.Archived {
		t.Fatalf("lost partial updates: %+v", p)
	}
	if !reopened.messages[0].Read {
		t.Fatal("read state not durable")
	}
	if w := request(a, "POST", "/api/peers", `{"peer":"10086","archived":false}`, "http://homesim.local", c); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if a.peers["10086"].Archived || !a.peers["10086"].Starred || len(a.messages) != 1 {
		t.Fatal("restore lost messages or metadata")
	}
}

func TestReadSnapshotDoesNotConsumeNewArrivals(t *testing.T) {
	a, c := organizerFixture(t)
	id := a.messages[0].ID
	a.receive("10086", "new arrival", time.Now().Add(time.Second))
	payload, _ := json.Marshal(map[string]any{"ids": []string{id}, "read": true})
	if w := request(a, "POST", "/api/messages/read", string(payload), "http://homesim.local", c); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if !a.messages[0].Read || a.messages[1].Read {
		t.Fatal("snapshot marked a later arrival read")
	}
	payload, _ = json.Marshal(map[string]any{"ids": []string{id, "nonexistent"}, "read": false})
	if w := request(a, "POST", "/api/messages/read", string(payload), "http://homesim.local", c); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if !a.messages[0].Read {
		t.Fatal("invalid batch applied partially")
	}
}

func TestOrganizerRequiresAuthenticationAndOrigin(t *testing.T) {
	a, c := organizerFixture(t)
	for _, path := range []string{"/api/peers", "/api/export?format=json", "/api/export?format=csv"} {
		if w := request(a, "GET", path, "", "", nil); w.Code != 401 {
			t.Fatalf("private endpoint exposed: %s %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/peers", "/api/messages/read", "/api/messages/estimate"} {
		if w := request(a, "POST", path, `{}`, "http://evil.invalid", c); w.Code != 403 {
			t.Fatalf("CSRF %s", path)
		}
	}
	for _, b := range []string{`{"peer":"../../auth","name":"x"}`, `{"peer":"10086","name":"bad\nname"}`, `{"peer":"10086"}`} {
		if w := request(a, "POST", "/api/peers", b, "http://homesim.local", c); w.Code != 400 {
			t.Fatalf("invalid metadata accepted %s", b)
		}
	}
}

func TestFailedOrganizerWritePreservesMemory(t *testing.T) {
	a, c := organizerFixture(t)
	if err := os.Mkdir(filepath.Join(a.data, "peers.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if w := request(a, "POST", "/api/peers", `{"peer":"10086","starred":true}`, "http://homesim.local", c); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if a.peers["10086"].Starred {
		t.Fatal("failed disk write changed in-memory state")
	}
}

func TestExportExcludesCredentialsAndEscapesFormulas(t *testing.T) {
	a, c := organizerFixture(t)
	a.messages[0].Content = "\t=HYPERLINK(\"https://invalid.example\")"
	a.peers["10086"] = peerInfo{Name: "+formula"}
	w := request(a, "GET", "/api/export?format=json", "", "", c)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("export missing attachment")
	}
	for _, secret := range []string{a.setupToken, a.auth.Hash, "setup_token", "password", "auth.json"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("export leaked %s", secret)
		}
	}
	var data struct {
		Messages []message
		Peers    map[string]peerInfo
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Messages) != 1 || data.Peers["10086"].Name != "+formula" {
		t.Fatal("missing exported data")
	}
	w = request(a, "GET", "/api/export?format=csv", "", "", c)
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff")))
	table, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if table[1][2] != "'+formula" || !strings.HasPrefix(table[1][6], "'\t=") {
		t.Fatal("spreadsheet formula injection not escaped")
	}
}

func TestActualEncodingEstimatesWithoutModem(t *testing.T) {
	a, c := organizerFixture(t)
	for _, tt := range []struct {
		body     string
		segments int
	}{{"", 0}, {strings.Repeat("a", 160), 1}, {strings.Repeat("a", 161), 2}, {strings.Repeat("中", 70), 1}, {strings.Repeat("中", 71), 2}} {
		b, _ := json.Marshal(map[string]string{"message": tt.body})
		w := request(a, "POST", "/api/messages/estimate", string(b), "http://homesim.local", c)
		var got struct{ Characters, Segments int }
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Segments != tt.segments || got.Characters != len([]rune(tt.body)) {
			t.Fatalf("estimate: %+v want %d", got, tt.segments)
		}
	}
	if len(a.messages) != 1 {
		t.Fatal("estimate transmitted or persisted SMS")
	}
}

func TestLegacyStoreMigrationKeepsAccountAndSMS(t *testing.T) {
	dir := t.TempDir()
	legacy := `[{"id":"legacy","peer":"10086","content":"old SMS","direction":"incoming","timestamp":"2026-09-01T10:00:00Z","status":"received"}]`
	if err := os.WriteFile(filepath.Join(dir, "messages.json"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(dir, "auth.json"), credentials{Username: "existing", Hash: "private-legacy-hash"}); err != nil {
		t.Fatal(err)
	}
	a, err := openApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.auth.Username != "existing" || a.auth.Hash != "private-legacy-hash" || a.messages[0].Content != "old SMS" || a.messages[0].Read || a.peers == nil {
		t.Fatal("legacy data damaged")
	}
}
