package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSMSStorageClearResult(t *testing.T) {
	result := smsStorageClearResult{Memory: "SM", Before: 2, After: 0}
	if result.Memory != "SM" || result.Before != 2 || result.After != 0 {
		t.Fatalf("unexpected SMS clear result: %#v", result)
	}
}

func TestClearModuleSMSDemoReportsBothStores(t *testing.T) {
	a := &app{demo: true}
	recorder := httptest.NewRecorder()

	a.clearModuleSMS(recorder, httptest.NewRequest(
		http.MethodPost, "/api/sms/clear-module", bytes.NewBufferString(`{"confirm":true}`),
	))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response struct {
		Cleared  bool     `json:"cleared"`
		Storages []string `json:"storages"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Cleared || len(response.Storages) != 2 || response.Storages[0] != "SM" || response.Storages[1] != "ME" {
		t.Fatalf("unexpected clear response: %#v", response)
	}
}

func TestSMSAutoCleanupCannotBeEnabled(t *testing.T) {
	a := &app{}
	recorder := httptest.NewRecorder()
	a.updateSMSSettings(recorder, httptest.NewRequest(
		http.MethodPatch, "/api/sms/settings", bytes.NewBufferString(`{"auto_cleanup_me":true}`),
	))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("enable auto cleanup status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if a.smsAutoCleanupME {
		t.Fatal("auto cleanup became enabled")
	}

	recorder = httptest.NewRecorder()
	a.updateSMSSettings(recorder, httptest.NewRequest(
		http.MethodPatch, "/api/sms/settings", bytes.NewBufferString(`{"auto_cleanup_me":false}`),
	))
	if recorder.Code != http.StatusOK {
		t.Fatalf("disable auto cleanup status = %d, want %d", recorder.Code, http.StatusOK)
	}
}
