//go:build !darwin || !cgo

package main

import (
	"errors"
	"net/http"
)

// Module-side voice route control is macOS-only for now.

func moduleVoiceUSBIdentity(*usbAT) (uint16, uint16, bool) { return 0, 0, false }

func (a *app) kickModuleVoice() {}

func (a *app) prewarmIncomingModuleVoiceRoute(remoteMediaSource) error { return nil }

func (a *app) kickIncomingModuleVoicePrewarm(callRecord, uint64) {}

func (a *app) ensureModuleVoiceRoute() error {
	return errors.New("模块语音路由仅在 macOS 版本可用")
}

func (a *app) ensureModuleVoiceRouteForCall(callMediaTicket) error {
	return errors.New("模块语音路由仅在 macOS 版本可用")
}

func (a *app) stopModuleVoiceRoute() {}

func (a *app) stopModuleVoiceRouteChecked() error { return nil }

func (a *app) stopModuleVoiceRouteForceChecked() error { return nil }

func (a *app) stopModuleVoiceRouteForCall(generation uint64) error {
	return a.stopModuleVoiceRouteForCallWith(generation, func() error { return nil })
}

func (a *app) confirmModuleVoiceCall(callMediaTicket) error {
	return errors.New("模块语音路由仅在 macOS 版本可用")
}

func (a *app) voiceStatus() map[string]any {
	return map[string]any{"ready": false, "detail": "macOS only"}
}

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStartAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "需要明确 confirm 才会启动模块语音路由")
		return
	}
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if _, ok := a.currentCallMediaTicket(); !ok {
		writeError(w, http.StatusConflict, "只有恰好一路 active 语音通话时才能启动路由")
		return
	}
	writeError(w, http.StatusBadGateway, "模块语音路由仅在 macOS 版本可用")
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}
