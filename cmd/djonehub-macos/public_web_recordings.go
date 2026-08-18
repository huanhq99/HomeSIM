package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	publicWebRecordingUploadPath = "/api/remote/v1/recordings"
	publicWebRecordingMaxBytes   = 128 << 20
)

var publicWebRecordingIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type publicWebRecordingMetadata struct {
	Version         int    `json:"version"`
	ID              string `json:"id"`
	CallID          string `json:"call_id"`
	Number          string `json:"number"`
	Direction       string `json:"direction"`
	StartedAt       string `json:"started_at"`
	EndedAt         string `json:"ended_at"`
	DurationSeconds int    `json:"duration_seconds"`
	MIMEType        string `json:"mime_type"`
	Extension       string `json:"extension"`
	FileName        string `json:"file_name,omitempty"`
	StoredAt        string `json:"stored_at,omitempty"`
	AudioPath       string `json:"audio_path,omitempty"`
}

func defaultPublicWebRecordingsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MacCellular", "data", "call-recordings")
}

func (a *app) publicWebSaveRecording(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.publicWeb == nil || !a.publicWebDirectVoiceEnabled() || a.publicWeb.RecordingsDir == "" {
		writeError(w, http.StatusServiceUnavailable, "call recording storage is unavailable")
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording upload")
		return
	}
	metadataPart, err := reader.NextPart()
	if err != nil || metadataPart.FormName() != "metadata" {
		writeError(w, http.StatusBadRequest, "recording metadata is required")
		return
	}
	metadataBytes, err := io.ReadAll(io.LimitReader(metadataPart, 32<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid recording metadata")
		return
	}
	var metadata publicWebRecordingMetadata
	decoder := json.NewDecoder(strings.NewReader(string(metadataBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil || validatePublicWebRecordingMetadata(metadata) != nil {
		writeError(w, http.StatusBadRequest, "invalid recording metadata")
		return
	}
	audioPart, err := reader.NextPart()
	if err != nil || audioPart.FormName() != "audio" {
		writeError(w, http.StatusBadRequest, "recording audio is required")
		return
	}
	mediaType, _, err := mime.ParseMediaType(audioPart.Header.Get("Content-Type"))
	metadataMediaType, _, metadataMIMEErr := mime.ParseMediaType(metadata.MIMEType)
	if err != nil || metadataMIMEErr != nil || mediaType != metadataMediaType ||
		mediaType != "audio/mp4" && mediaType != "audio/webm" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported recording format")
		return
	}
	if err := os.MkdirAll(a.publicWeb.RecordingsDir, 0o700); err != nil || os.Chmod(a.publicWeb.RecordingsDir, 0o700) != nil {
		writeError(w, http.StatusServiceUnavailable, "call recording storage is unavailable")
		return
	}
	if existing, ok := a.publicWebPrimaryRecordingForCall(metadata.CallID); ok &&
		existing.DurationSeconds >= metadata.DurationSeconds {
		written, copyErr := io.Copy(io.Discard, io.LimitReader(audioPart, publicWebRecordingMaxBytes+1))
		if copyErr != nil || written == 0 || written > publicWebRecordingMaxBytes {
			writeError(w, http.StatusBadRequest, "recording audio is incomplete")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"saved": true, "file_name": existing.FileName, "deduplicated": true,
		})
		return
	}
	startedAt, _ := time.Parse(time.RFC3339Nano, metadata.StartedAt)
	baseName := fmt.Sprintf("%s_%s_%s_%s", startedAt.Local().Format("2006-01-02_15-04-05"), publicWebRecordingSubject(metadata.Number), metadata.Direction, metadata.ID[:8])
	audioName := baseName + "." + metadata.Extension
	audioPath := filepath.Join(a.publicWeb.RecordingsDir, audioName)
	temporary, err := os.CreateTemp(a.publicWeb.RecordingsDir, ".recording-*")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "call recording storage is unavailable")
		return
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	_ = temporary.Chmod(0o600)
	written, copyErr := io.Copy(temporary, io.LimitReader(audioPart, publicWebRecordingMaxBytes+1))
	if copyErr != nil || written == 0 || written > publicWebRecordingMaxBytes || temporary.Sync() != nil || temporary.Close() != nil {
		writeError(w, http.StatusBadRequest, "recording audio is incomplete")
		return
	}
	if err := os.Rename(temporaryPath, audioPath); err != nil {
		writeError(w, http.StatusServiceUnavailable, "call recording storage is unavailable")
		return
	}
	committed = true
	metadata.FileName = audioName
	metadata.StoredAt = time.Now().UTC().Format(time.RFC3339Nano)
	metadataBody, _ := json.MarshalIndent(metadata, "", "  ")
	metadataBody = append(metadataBody, '\n')
	metadataPath := filepath.Join(a.publicWeb.RecordingsDir, baseName+".json")
	if err := writePublicWebRecordingMetadata(metadataPath, metadataBody); err != nil {
		_ = os.Remove(audioPath)
		writeError(w, http.StatusServiceUnavailable, "call recording metadata could not be saved")
		return
	}
	a.pruneSupersededPublicWebRecordings(metadata)
	writeJSON(w, http.StatusCreated, map[string]any{"saved": true, "file_name": audioName})
}

func (a *app) publicWebPrimaryRecordingForCall(callID string) (publicWebRecordingMetadata, bool) {
	items, err := a.publicWebRecordingItems()
	if err != nil {
		return publicWebRecordingMetadata{}, false
	}
	for _, item := range items {
		if item.CallID == callID {
			return item, true
		}
	}
	return publicWebRecordingMetadata{}, false
}

func (a *app) pruneSupersededPublicWebRecordings(primary publicWebRecordingMetadata) {
	entries, err := os.ReadDir(a.publicWeb.RecordingsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		metadataPath := filepath.Join(a.publicWeb.RecordingsDir, entry.Name())
		body, err := os.ReadFile(metadataPath)
		if err != nil || len(body) > 32<<10 {
			continue
		}
		var candidate publicWebRecordingMetadata
		if json.Unmarshal(body, &candidate) != nil || candidate.ID == primary.ID ||
			candidate.CallID != primary.CallID || candidate.DurationSeconds > primary.DurationSeconds ||
			candidate.FileName == "" || filepath.Base(candidate.FileName) != candidate.FileName {
			continue
		}
		_ = os.Remove(filepath.Join(a.publicWeb.RecordingsDir, candidate.FileName))
		_ = os.Remove(metadataPath)
	}
}

func (a *app) publicWebListRecordings(w http.ResponseWriter, _ *http.Request) {
	items, err := a.publicWebRecordingItems()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "call recordings are unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": 1, "items": items})
}

func (a *app) publicWebPlayRecording(w http.ResponseWriter, r *http.Request) {
	recordingID := r.PathValue("recordingID")
	if !publicWebRecordingIDPattern.MatchString(recordingID) {
		http.NotFound(w, r)
		return
	}
	items, err := a.publicWebRecordingItems()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "call recordings are unavailable")
		return
	}
	for _, metadata := range items {
		if metadata.ID != recordingID {
			continue
		}
		path := filepath.Join(a.publicWeb.RecordingsDir, metadata.FileName)
		file, err := os.Open(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		mediaType, _, _ := mime.ParseMediaType(metadata.MIMEType)
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Disposition", `inline; filename="call-recording.`+metadata.Extension+`"`)
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, metadata.FileName, info.ModTime(), file)
		return
	}
	http.NotFound(w, r)
}

func (a *app) publicWebRecordingItems() ([]publicWebRecordingMetadata, error) {
	if a == nil || a.publicWeb == nil || !a.publicWebDirectVoiceEnabled() || a.publicWeb.RecordingsDir == "" {
		return nil, errors.New("recordings unavailable")
	}
	entries, err := os.ReadDir(a.publicWeb.RecordingsDir)
	if errors.Is(err, os.ErrNotExist) {
		return []publicWebRecordingMetadata{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]publicWebRecordingMetadata, 0, len(entries)/2)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(a.publicWeb.RecordingsDir, entry.Name()))
		if err != nil || len(body) > 32<<10 {
			continue
		}
		var metadata publicWebRecordingMetadata
		if json.Unmarshal(body, &metadata) != nil || validatePublicWebRecordingMetadata(metadata) != nil ||
			metadata.FileName == "" || filepath.Base(metadata.FileName) != metadata.FileName {
			continue
		}
		info, err := os.Stat(filepath.Join(a.publicWeb.RecordingsDir, metadata.FileName))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		metadata.AudioPath = publicWebRecordingUploadPath + "/" + metadata.ID + "/audio"
		items = append(items, metadata)
	}
	return primaryPublicWebRecordings(items), nil
}

func primaryPublicWebRecordings(items []publicWebRecordingMetadata) []publicWebRecordingMetadata {
	primary := make(map[string]publicWebRecordingMetadata, len(items))
	for _, item := range items {
		key := item.CallID
		if key == "" {
			key = item.ID
		}
		current, exists := primary[key]
		if !exists || item.DurationSeconds > current.DurationSeconds {
			primary[key] = item
		}
	}
	result := make([]publicWebRecordingMetadata, 0, len(primary))
	for _, item := range primary {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StartedAt > result[j].StartedAt })
	return result
}

func publicWebRecordingSubject(number string) string {
	number = strings.TrimSpace(number)
	var subject strings.Builder
	for index, character := range number {
		if character >= '0' && character <= '9' {
			subject.WriteRune(character)
		} else if character == '+' && index == 0 {
			subject.WriteString("plus")
		}
		if subject.Len() >= 32 {
			break
		}
	}
	if subject.Len() == 0 {
		return "unknown"
	}
	return subject.String()
}

func validatePublicWebRecordingMetadata(metadata publicWebRecordingMetadata) error {
	if metadata.Version != 1 || !publicWebRecordingIDPattern.MatchString(metadata.ID) ||
		len(metadata.CallID) == 0 || len(metadata.CallID) > 128 || len(metadata.Number) > 64 ||
		metadata.Direction != "incoming" && metadata.Direction != "outgoing" ||
		metadata.DurationSeconds < 0 || metadata.DurationSeconds > 24*60*60 {
		return errors.New("invalid recording metadata")
	}
	startedAt, startErr := time.Parse(time.RFC3339Nano, metadata.StartedAt)
	endedAt, endErr := time.Parse(time.RFC3339Nano, metadata.EndedAt)
	if startErr != nil || endErr != nil || endedAt.Before(startedAt) {
		return errors.New("invalid recording timestamps")
	}
	mediaType, _, err := mime.ParseMediaType(metadata.MIMEType)
	if err != nil {
		return errors.New("invalid recording MIME type")
	}
	switch mediaType {
	case "audio/mp4":
		if metadata.Extension != "m4a" {
			return errors.New("invalid recording extension")
		}
	case "audio/webm":
		if metadata.Extension != "webm" {
			return errors.New("invalid recording extension")
		}
	default:
		return errors.New("invalid recording MIME type")
	}
	return nil
}

func writePublicWebRecordingMetadata(path string, body []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".metadata-*")
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
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
