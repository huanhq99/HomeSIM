package main

import (
	"encoding/json"
	"errors"
	"github.com/iniwex5/vohive/internal/modem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeLineDevice struct {
	atErr, simErr, iccidErr, numberErr, regErr error
	inserted                                   bool
	iccid, number                              string
	registration                               int
	numberQueries                              int
	commands                                   []string
}

func (f *fakeLineDevice) ExecuteATSilent(cmd string, _ time.Duration) (string, error) {
	f.commands = append(f.commands, cmd)
	return "OK", f.atErr
}
func (f *fakeLineDevice) QuerySIMInserted() (bool, error) { return f.inserted, f.simErr }
func (f *fakeLineDevice) QueryICCID() (string, error)     { return f.iccid, f.iccidErr }
func (f *fakeLineDevice) QueryMSISDN() (string, error) {
	f.numberQueries++
	return f.number, f.numberErr
}
func (f *fakeLineDevice) QueryRegistration() (int, string, string, string, error) {
	return f.registration, "", "", "", f.regErr
}

const fakeICCID = "89860000000000000001"

func validLineDevice() *fakeLineDevice {
	return &fakeLineDevice{inserted: true, iccid: fakeICCID, number: "+8613800138000", registration: 1}
}
func TestLineDetectionAndChangedCard(t *testing.T) {
	now := time.Now()
	f := validLineDevice()
	first := readLine(f, lineSnapshot{}, now)
	if first.Number != f.number || first.NumberState != "found" || lineState(first, true, now) != "online" {
		t.Fatalf("%+v", first)
	}
	second := readLine(f, first, now.Add(12*time.Second))
	if second.Number != first.Number || f.numberQueries != 1 {
		t.Fatal("same card not cached")
	}
	f.iccid = "89860000000000000002"
	f.number = ""
	swapped := readLine(f, second, now.Add(24*time.Second))
	if swapped.Number != "" || swapped.NumberState != "unavailable" || f.numberQueries != 2 {
		t.Fatal("old number leaked to changed card")
	}
	f.number = "+8613900139000"
	retry := readLine(f, swapped, now.Add(90*time.Second))
	if retry.Number != f.number {
		t.Fatal("missing number was never retried")
	}
	if len(f.commands) != 4 {
		t.Fatal("missing heartbeat")
	}
	for _, cmd := range f.commands {
		if cmd != "AT" {
			t.Fatal("probe mutated device")
		}
	}
}
func TestLineIdentityFailureCannotReuseNumber(t *testing.T) {
	now := time.Now()
	f := validLineDevice()
	old := readLine(f, lineSnapshot{}, now)
	f.iccidErr = errors.New("query error")
	f.number = ""
	next := readLine(f, old, now.Add(12*time.Second))
	if next.ICCID != "" || next.Number != "" {
		t.Fatal("failed identity retained a previous card")
	}
	f.simErr = errors.New("query error")
	if s := readLine(f, old, now); lineState(s, true, now) != "sim_unknown" || s.Number != "" {
		t.Fatal("SIM failure advertised healthy state")
	}
}
func TestLineStatusTransitions(t *testing.T) {
	now := time.Now()
	s := readLine(validLineDevice(), lineSnapshot{}, now)
	cases := []struct {
		name, want string
		edit       func(*lineSnapshot)
		attached   bool
		at         time.Time
	}{
		{"online", "online", func(*lineSnapshot) {}, true, now},
		{"disconnected", "offline", func(*lineSnapshot) {}, false, now},
		{"stale", "stale", func(*lineSnapshot) {}, true, now.Add(46 * time.Second)},
		{"absent", "no_sim", func(s *lineSnapshot) { s.SIMState = "absent" }, true, now},
		{"searching", "unregistered", func(s *lineSnapshot) { s.Registration = 2 }, true, now},
		{"unknown network", "network_unknown", func(s *lineSnapshot) { s.Registration = -1 }, true, now},
		{"roaming", "online", func(s *lineSnapshot) { s.Registration = 5 }, true, now},
		{"AT timeout", "offline", func(s *lineSnapshot) { s.Connected = false }, true, now},
		{"starting", "checking", func(s *lineSnapshot) { s.CheckedAt = time.Time{} }, true, now},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := s
			c.edit(&v)
			if got := lineState(v, c.attached, c.at); got != c.want {
				t.Fatalf("%s != %s", got, c.want)
			}
		})
	}
}
func lineFixture(t *testing.T) (*app, *http.Cookie) {
	a, c := organizerFixture(t)
	a.manager = new(modem.Manager)
	a.line = readLine(validLineDevice(), lineSnapshot{}, time.Now())
	a.line.Number = ""
	a.line.NumberState = "unavailable"
	return a, c
}
func TestManualNumberPersistencePriorityAndCardBinding(t *testing.T) {
	a, c := lineFixture(t)
	w := request(a, "POST", "/api/line/number", `{"iccid":"`+fakeICCID+`","number":"13800138000"}`, "http://homesim.local", c)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	b, err := openApp(a.data)
	if err != nil || b.lineNumbers[fakeICCID] != "13800138000" {
		t.Fatal("manual number not durable", err)
	}
	if info := a.lineInfo(true); info["number"] != "13800138000" || info["number_source"] != "manual" {
		t.Fatal(info)
	}
	a.line.Number = "+8613900139000"
	if a.lineInfo(true)["number_source"] != "module" {
		t.Fatal("manual overrides detection")
	}
	a.line.ICCID = "89860000000000000002"
	a.line.Number = ""
	if a.lineInfo(true)["number"] != "" {
		t.Fatal("changed SIM reused manual number")
	}
	if w = request(a, "POST", "/api/line/number", `{"iccid":"`+fakeICCID+`","number":"13800138000"}`, "http://homesim.local", c); w.Code != 409 {
		t.Fatal("stale card accepted", w.Code)
	}
	a.line.ICCID = fakeICCID
	if w = request(a, "POST", "/api/line/number", `{"iccid":"`+fakeICCID+`","number":""}`, "http://homesim.local", c); w.Code != 200 || a.lineInfo(true)["number"] != "" {
		t.Fatal("clear failed")
	}
}
func TestPrivateLineEndpointsAndFailedWrites(t *testing.T) {
	a, c := lineFixture(t)
	if w := request(a, "GET", "/api/status", "", "", nil); w.Code != 401 {
		t.Fatal("identities exposed")
	}
	body := `{"iccid":"` + fakeICCID + `","number":"13800138000"}`
	if w := request(a, "POST", "/api/line/number", body, "http://evil.invalid", c); w.Code != 403 {
		t.Fatal("origin bypass")
	}
	if w := request(a, "POST", "/api/line/number", body, "http://homesim.local", nil); w.Code != 401 {
		t.Fatal("auth bypass")
	}
	if w := request(a, "POST", "/api/line/number", strings.Replace(body, "13800138000", "AT+CMGS", 1), "http://homesim.local", c); w.Code != 400 {
		t.Fatal("invalid number accepted")
	}
	if err := os.Mkdir(filepath.Join(a.data, "lines.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if w := request(a, "POST", "/api/line/number", body, "http://homesim.local", c); w.Code != 503 || len(a.lineNumbers) != 0 {
		t.Fatal("failed write changed state")
	}
	if w := request(a, "GET", "/api/export?format=json", "", "", c); strings.Contains(w.Body.String(), fakeICCID) {
		t.Fatal("export leaked private SIM settings")
	}
	a.lineNumbers[fakeICCID] = "13800138000"
	a.line.CheckedAt = time.Now().Add(-46 * time.Second)
	info := a.lineInfo(true)
	if info["number"] != "" || info["iccid"] != "" || info["manual_allowed"] != false {
		t.Fatal("stale identities still advertised")
	}
	a.line.CheckedAt = time.Now()
	info = a.lineInfo(false)
	if info["number"] != "" || info["reg_status"] != -1 {
		t.Fatal("disconnected card appeared registered")
	}
	encoded, _ := json.Marshal(info)
	if strings.Contains(string(encoded), fakeICCID) {
		t.Fatal("disconnected identity retained")
	}
}
