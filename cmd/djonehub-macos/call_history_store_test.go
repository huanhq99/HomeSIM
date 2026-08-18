package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCallHistoryPersistsAnsweredAndMissedCallsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "call-history.json")
	started := time.Date(2026, 8, 16, 20, 1, 0, 0, time.Local)
	ended := started.Add(12 * time.Second)
	first := &app{}
	if err := first.initializeCallHistoryStore(path); err != nil {
		t.Fatal(err)
	}
	first.callHistory = []callRecord{
		{ID: "missed-1", Index: 1, Direction: "incoming", State: "incoming", Number: "10086", StartedAt: started, UpdatedAt: ended, EndedAt: &ended, Missed: true},
		{ID: "outgoing-1", Index: 2, Direction: "outgoing", State: "active", Number: "10010", StartedAt: started.Add(-time.Minute), UpdatedAt: ended.Add(-time.Minute), EndedAt: &ended},
	}
	if err := first.persistCallHistory(); err != nil {
		t.Fatal(err)
	}

	second := &app{}
	if err := second.initializeCallHistoryStore(path); err != nil {
		t.Fatal(err)
	}
	if len(second.callHistory) != 2 || !second.callHistory[0].Missed || second.callHistory[0].Number != "10086" || second.callHistory[1].Direction != "outgoing" {
		t.Fatalf("restored history=%+v", second.callHistory)
	}
}

func TestCallHistoryBootstrapsFromExistingMacRecording(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	directory := defaultPublicWebRecordingsRoot()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := publicWebRecordingMetadata{
		Version: 1, ID: "recording_bootstrap_1", CallID: "call-recorded", Number: "18900001111",
		Direction: "outgoing", StartedAt: "2026-08-16T12:00:00Z", EndedAt: "2026-08-16T12:00:21Z",
		DurationSeconds: 21, MIMEType: "audio/mp4", Extension: "m4a",
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "recorded.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	instance := &app{}
	if err := instance.initializeCallHistoryStore(""); err != nil {
		t.Fatal(err)
	}
	if len(instance.callHistory) != 1 || instance.callHistory[0].ID != "call-recorded" || instance.callHistory[0].Number != "18900001111" {
		t.Fatalf("bootstrapped history=%+v", instance.callHistory)
	}
	if _, err := os.Stat(defaultCallHistoryStorePath()); err != nil {
		t.Fatalf("persisted bootstrap history: %v", err)
	}
}
