package main

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// All serial reads use Manager's command queue, never a second serial owner.
type lineDevice interface {
	ExecuteATSilent(string, time.Duration) (string, error)
	QuerySIMInserted() (bool, error)
	QueryICCID() (string, error)
	QueryMSISDN() (string, error)
	QueryRegistration() (int, string, string, string, error)
}
type lineSnapshot struct {
	Connected       bool
	SIMState        string
	ICCID           string
	Number          string
	NumberState     string
	Registration    int
	CheckedAt       time.Time
	NumberCheckedAt time.Time
}

var iccidPattern = regexp.MustCompile(`^[0-9]{18,22}$`)
var lineNumberPattern = regexp.MustCompile(`^\+?[0-9]{5,20}$`)

func cleanICCID(v string) string {
	v = strings.TrimSuffix(strings.TrimSpace(v), "F")
	if !iccidPattern.MatchString(v) {
		return ""
	}
	return v
}
func readLine(d lineDevice, previous lineSnapshot, now time.Time) lineSnapshot {
	s := lineSnapshot{SIMState: "unknown", NumberState: "unavailable", Registration: -1, CheckedAt: now}
	if _, err := d.ExecuteATSilent("AT", 2*time.Second); err != nil {
		return s
	}
	s.Connected = true
	inserted, err := d.QuerySIMInserted()
	if err != nil {
		return s
	}
	if !inserted {
		s.SIMState = "absent"
		return s
	}
	s.SIMState = "identified"
	if v, err := d.QueryICCID(); err == nil {
		s.ICCID = cleanICCID(v)
	}
	if st, _, _, _, err := d.QueryRegistration(); err == nil {
		s.Registration = st
	}
	// A number is only reused after re-reading the same card's identity.
	if s.ICCID != "" && s.ICCID == previous.ICCID && now.Sub(previous.NumberCheckedAt) < time.Minute {
		s.Number = previous.Number
		s.NumberState = previous.NumberState
		s.NumberCheckedAt = previous.NumberCheckedAt
	} else {
		s.NumberCheckedAt = now
		v, err := d.QueryMSISDN()
		switch {
		case err != nil:
			s.NumberState = "query_failed"
		case lineNumberPattern.MatchString(strings.TrimSpace(v)):
			s.Number = strings.TrimSpace(v)
			s.NumberState = "found"
		}
	}
	return s
}
func (a *app) updateLine(d lineDevice, force bool) {
	a.mu.Lock()
	previous := a.line
	a.mu.Unlock()
	if force {
		previous.NumberCheckedAt = time.Time{}
	}
	s := readLine(d, previous, time.Now())
	s.CheckedAt = time.Now()
	a.modemMu.RLock()
	defer a.modemMu.RUnlock()
	if a.manager == nil || lineDevice(a.manager) != d {
		return
	}
	a.mu.Lock()
	a.line = s
	a.mu.Unlock()
}
func lineState(s lineSnapshot, attached bool, now time.Time) string {
	if !attached {
		return "offline"
	}
	if s.CheckedAt.IsZero() {
		return "checking"
	}
	if now.Sub(s.CheckedAt) > 45*time.Second {
		return "stale"
	}
	if !s.Connected {
		return "offline"
	}
	if s.SIMState == "absent" {
		return "no_sim"
	}
	if s.SIMState != "identified" {
		return "sim_unknown"
	}
	if s.Registration == 1 || s.Registration == 5 {
		return "online"
	}
	if s.Registration < 0 {
		return "network_unknown"
	}
	return "unregistered"
}
func (a *app) lineInfo(attached bool) map[string]any {
	a.mu.Lock()
	s := a.line
	a.mu.Unlock()
	account := a.accountApp()
	account.mu.Lock()
	defer account.mu.Unlock()
	state := lineState(s, attached, time.Now())
	number, source := "", "unavailable"
	// Do not advertise an old card or its manual number while probes are stale.
	identityCurrent := attached && s.Connected && !s.CheckedAt.IsZero() && time.Since(s.CheckedAt) <= 45*time.Second && s.SIMState == "identified"
	iccid := ""
	manual := ""
	if identityCurrent {
		iccid = s.ICCID
		manual = account.lineNumbers[iccid]
		if s.Number != "" {
			number, source = s.Number, "module"
		} else if iccid != "" && manual != "" {
			number, source = manual, "manual"
		}
	}
	if !identityCurrent {
		s.SIMState = "unknown"
		s.Registration = -1
	}
	age := int64(-1)
	if !s.CheckedAt.IsZero() {
		age = int64(time.Since(s.CheckedAt).Seconds())
		if age < 0 {
			age = 0
		}
	}
	return map[string]any{"state": state, "module_connected": attached && s.Connected && age >= 0 && age <= 45, "sim_state": s.SIMState, "reg_status": s.Registration, "iccid": iccid, "number": number, "number_source": source, "number_detection": s.NumberState, "manual_number": manual, "checked_at": s.CheckedAt, "age_seconds": age, "manual_allowed": identityCurrent && iccid != ""}
}
func (a *app) setLineNumber(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ICCID  string `json:"iccid"`
		Number string `json:"number"`
	}
	if !body(w, r, &b) {
		return
	}
	b.Number = strings.TrimSpace(b.Number)
	if !iccidPattern.MatchString(b.ICCID) || (b.Number != "" && !lineNumberPattern.MatchString(b.Number)) {
		fail(w, 400, "请填写有效手机号，或留空移除手动号码")
		return
	}
	attached := a.getModem() != nil
	a.mu.Lock()
	s := a.line
	a.mu.Unlock()
	if !attached || !s.Connected || s.SIMState != "identified" || s.ICCID != b.ICCID || s.CheckedAt.IsZero() || time.Since(s.CheckedAt) > 45*time.Second {
		fail(w, 409, "SIM 已变化或状态过期，请重新检测后再保存")
		return
	}
	account := a.accountApp()
	account.mu.Lock()
	defer account.mu.Unlock()
	next := make(map[string]string, len(account.lineNumbers)+1)
	for k, v := range account.lineNumbers {
		next[k] = v
	}
	if b.Number == "" {
		delete(next, b.ICCID)
	} else {
		next[b.ICCID] = b.Number
	}
	if err := atomicJSON(filepath.Join(account.data, "lines.json"), next); err != nil {
		fail(w, 503, "号码未保存，请检查 NAS 空间")
		return
	}
	account.lineNumbers = next
	reply(w, 200, map[string]bool{"ok": true})
}
