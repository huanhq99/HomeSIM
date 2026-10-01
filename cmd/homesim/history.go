package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type callRecord struct {
	ID        string     `json:"id"`
	ModemID   string     `json:"modem_id"`
	Number    string     `json:"number"`
	Direction int        `json:"direction"`
	Started   time.Time  `json:"started"`
	Answered  *time.Time `json:"answered,omitempty"`
	Ended     *time.Time `json:"ended,omitempty"`
	Result    string     `json:"result"`
}
type diagnosticEvent struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}
type callHistory struct {
	mu     sync.Mutex
	rows   []callRecord
	events []diagnosticEvent
}

func (a *app) loadHistory() error {
	if b, err := os.ReadFile(filepath.Join(a.data, "events.json")); err == nil {
		if err = json.Unmarshal(b, &a.history.events); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := os.ReadFile(filepath.Join(a.data, "calls.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &a.history.rows); err != nil {
		return err
	}
	changed := false
	now := time.Now()
	for i := range a.history.rows {
		if a.history.rows[i].Ended == nil {
			a.history.rows[i].Ended = &now
			a.history.rows[i].Result = "interrupted"
			changed = true
		}
	}
	if changed {
		return atomicJSON(filepath.Join(a.data, "calls.json"), a.history.rows)
	}
	return nil
}
func reconcileCalls(previous []callRecord, calls []call, now time.Time) []callRecord {
	next := append([]callRecord{}, previous...)
	seen := map[string]bool{}
	for _, c := range calls {
		key := c.ID
		seen[key] = true
		index := -1
		for i := range next {
			if next[i].Ended == nil && next[i].ModemID == key {
				index = i
				break
			}
		}
		if index < 0 {
			next = append(next, callRecord{ID: uuid.NewString(), ModemID: key, Number: c.Number, Direction: c.Direction, Started: now, Result: "ongoing"})
			index = len(next) - 1
		}
		row := &next[index]
		if c.Number != "" {
			row.Number = c.Number
		}
		if c.State == 0 && row.Answered == nil {
			t := now
			row.Answered = &t
		}
	}
	for i := range next {
		r := &next[i]
		if r.Ended == nil && !seen[r.ModemID] {
			t := now
			r.Ended = &t
			r.Result = "outgoing"
			if r.Answered != nil {
				r.Result = "completed"
			} else if r.Direction == 1 {
				r.Result = "missed"
			}
		}
	}
	if len(next) > 1000 {
		next = next[len(next)-1000:]
	}
	return next
}
func (a *app) recordCalls(calls []call) {
	h := &a.history
	h.mu.Lock()
	incoming := []callRecord{}
	missed := false
	defer func() {
		h.mu.Unlock()
		for _, row := range incoming {
			a.notify("家信 · 有来电", row.Number)
			a.pushIncoming(row)
		}
		if missed {
			a.notify("家信 · 未接来电", "打开通话记录查看详情。")
		}
	}()
	next := reconcileCalls(h.rows, calls, time.Now())
	old, _ := json.Marshal(h.rows)
	fresh, _ := json.Marshal(next)
	if string(old) == string(fresh) {
		return
	}
	if atomicJSON(filepath.Join(a.data, "calls.json"), next) != nil {
		return
	}
	known := map[string]callRecord{}
	ringing := map[string]bool{}
	for _, current := range calls {
		ringing[current.ID] = current.State == 4 || current.State == 5
	}
	for _, row := range h.rows {
		known[row.ID] = row
	}
	for _, row := range next {
		old, exists := known[row.ID]
		if !exists && row.Direction == 1 && row.Answered == nil && row.Ended == nil && ringing[row.ModemID] {
			incoming = append(incoming, row)
		}
		if row.Result == "missed" && old.Result != "missed" {
			missed = true
		}
	}
	h.rows = next
}
func (a *app) watchCalls(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m := a.getModem()
			if m == nil {
				continue
			}
			if c, err := a.currentCalls(); err == nil {
				a.modemMu.RLock()
				if a.manager == m {
					a.recordCalls(c)
				}
				a.modemMu.RUnlock()
			}
		}
	}
}
func (a *app) callRecords(w http.ResponseWriter, r *http.Request) {
	a.history.mu.Lock()
	rows := append([]callRecord{}, a.history.rows...)
	a.history.mu.Unlock()
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	reply(w, 200, rows)
}

// Event messages are fixed operational text. Never log SMS, numbers or credentials.
func (a *app) addEvent(kind, message string) {
	a.history.mu.Lock()
	defer a.history.mu.Unlock()
	a.history.events = append(a.history.events, diagnosticEvent{time.Now(), kind, message})
	if len(a.history.events) > 200 {
		a.history.events = a.history.events[len(a.history.events)-200:]
	}
	_ = atomicJSON(filepath.Join(a.data, "events.json"), a.history.events)
}
func (a *app) interruptCalls() {
	a.history.mu.Lock()
	defer a.history.mu.Unlock()
	now := time.Now()
	next := append([]callRecord{}, a.history.rows...)
	for i := range next {
		if next[i].Ended == nil {
			next[i].Ended = &now
			next[i].Result = "interrupted"
		}
	}
	if atomicJSON(filepath.Join(a.data, "calls.json"), next) == nil {
		a.history.rows = next
	}
}
func (a *app) diagnostics(w http.ResponseWriter, r *http.Request) {
	a.history.mu.Lock()
	events := append([]diagnosticEvent{}, a.history.events...)
	a.history.mu.Unlock()
	audio := a.audioName()
	a.voiceMu.Lock()
	v := a.voice
	a.voiceMu.Unlock()
	ready := false
	state := "idle"
	var captured, played uint64
	if v != nil {
		v.mu.Lock()
		ready = v.connected
		state = v.pc.ConnectionState().String()
		v.mu.Unlock()
		captured, played = v.captured.Load(), v.played.Load()
	}
	reply(w, 200, map[string]any{"device_id": a.deviceID, "line": a.lineInfo(a.getModem() != nil), "audio_device": audio, "audio_connected": ready, "ice_state": state, "captured_frames": captured, "played_frames": played, "turn_configured": len(voiceICE().ICEServers) > 0, "events": events})
}
