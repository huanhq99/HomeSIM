package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const callHistoryStoreVersion = 1

type callHistoryStoreFile struct {
	Version int          `json:"version"`
	Calls   []callRecord `json:"calls"`
}

func defaultCallHistoryStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MacCellular", "data", "call-history.json")
}

func (a *app) initializeCallHistoryStore(path string) error {
	if a == nil {
		return errors.New("call history is unavailable")
	}
	if path == "" {
		path = defaultCallHistoryStorePath()
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("call history path must be an absolute clean path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	a.callHistoryPath = path
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		a.callHistory = callHistoryFromRecordingMetadata(defaultPublicWebRecordingsRoot())
		if len(a.callHistory) > 0 {
			return a.persistCallHistory()
		}
		return nil
	}
	if err != nil {
		return err
	}
	var stored callHistoryStoreFile
	if err := json.Unmarshal(body, &stored); err != nil || stored.Version != callHistoryStoreVersion {
		return errors.New("call history file is invalid")
	}
	if len(stored.Calls) > 100 {
		stored.Calls = stored.Calls[:100]
	}
	a.callHistory = append([]callRecord(nil), stored.Calls...)
	return nil
}

func (a *app) persistCallHistory() error {
	if a == nil || a.callHistoryPath == "" {
		return nil
	}
	a.callMu.RLock()
	calls := append([]callRecord(nil), a.callHistory...)
	path := a.callHistoryPath
	a.callMu.RUnlock()
	body, err := json.MarshalIndent(callHistoryStoreFile{Version: callHistoryStoreVersion, Calls: calls}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".call-history-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func callHistoryFromRecordingMetadata(directory string) []callRecord {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	result := make([]callRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		var metadata publicWebRecordingMetadata
		if json.Unmarshal(body, &metadata) != nil || validatePublicWebRecordingMetadata(metadata) != nil {
			continue
		}
		startedAt, startErr := time.Parse(time.RFC3339Nano, metadata.StartedAt)
		endedAt, endErr := time.Parse(time.RFC3339Nano, metadata.EndedAt)
		if startErr != nil || endErr != nil {
			continue
		}
		result = append(result, callRecord{
			ID: metadata.CallID, Index: -1, Direction: metadata.Direction, State: "ended",
			Number: metadata.Number, StartedAt: startedAt, UpdatedAt: endedAt, EndedAt: &endedAt,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt.After(result[j].StartedAt) })
	if len(result) > 100 {
		result = result[:100]
	}
	return result
}
