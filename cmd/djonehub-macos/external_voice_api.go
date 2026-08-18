package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

const (
	externalVoiceOfferBodyLimit     = 96 << 10
	externalVoiceCallBodyLimit      = 16 << 10
	externalVoiceMutationTimeout    = 30 * time.Second
	externalVoiceReconcileTimeout   = 15 * time.Second
	externalVoiceAPISchemaVersion   = 2
	externalVoiceICECredentialsPath = "/api/remote/v2/voice/media/ice-credentials"
)

type externalVoiceSnapshotResponse struct {
	Version int                 `json:"version"`
	Voice   externalVoiceStatus `json:"voice"`
}

type externalVoiceOfferResponse struct {
	Version   int                           `json:"version"`
	Call      sipgateway.PublicCallSnapshot `json:"call"`
	AnswerSDP string                        `json:"answer_sdp"`
}

type externalVoiceCallResponse struct {
	Version int                           `json:"version"`
	Call    sipgateway.PublicCallSnapshot `json:"call"`
}

func (a *app) remoteVoiceV2Snapshot(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	if authorization.Identity == "" || !authorization.allows("calls.read") {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice snapshot is unavailable")
		return
	}
	status := externalVoiceStatus{Enabled: false, Health: "disabled"}
	if a.sipVoice != nil {
		status = a.sipVoice.Status(authorization.Identity)
	}
	writeJSON(w, http.StatusOK, externalVoiceSnapshotResponse{
		Version: externalVoiceAPISchemaVersion,
		Voice:   status,
	})
}

func (a *app) remoteVoiceV2ICECredentials(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if authorization.Identity == "" || authorization.Actions["calls.read"] != true ||
		authorization.Actions["calls.media"] != true || browser.Transport != "cloudflare-access" ||
		browser.SMSOnly || !a.publicWebVoiceEnabled() {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "public voice ICE credentials are unavailable")
		return
	}
	body, err := issuePublicWebBrowserTURN(r.Context(), a.publicWeb)
	if err != nil {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "ice_unavailable", "public voice ICE credentials are unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (a *app) remoteVoiceV2Offer(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.read"] || !authorization.Actions["calls.media"] {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice media is unavailable")
		return
	}
	if a.sipVoice == nil {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	var request externalVoiceOfferRequest
	body, ok := readExternalVoiceJSON(w, r, externalVoiceOfferBodyLimit, &request)
	if !ok {
		return
	}
	if browser.Transport == "cloudflare-access" {
		if !a.publicWebExternalVoiceEnabled() {
			writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
			return
		}
		relay, err := issuePublicWebServerRelay(r.Context(), a.publicWeb)
		if err != nil {
			writeRemoteAPIError(w, http.StatusServiceUnavailable, "ice_unavailable", "external voice ICE credentials are unavailable")
			return
		}
		request.turnRelay = &remotevoice.TURNRelayConfig{
			URLs: append([]string(nil), relay.URLs...), Username: relay.Username, Password: relay.Password,
			CredentialType: remotevoice.TURNCredentialTypePassword,
		}
	} else if browser.Transport != "tailscale-serve" {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice media is unavailable")
		return
	}
	result, err := a.sipVoice.Offer(r.Context(), authorization.Identity, request, sha256.Sum256(body))
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceOfferResponse{
		Version: externalVoiceAPISchemaVersion,
		Call:    result.Call, AnswerSDP: result.AnswerSDP,
	})
}

func (a *app) remoteVoiceV2Answer(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.control"] || !authorization.Actions["calls.media"] {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice answer is unavailable")
		return
	}
	if a.sipVoice == nil {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	commandID, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(commandID) {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return
	}
	var request externalVoiceCallRequest
	body, ok := readExternalVoiceJSON(w, r, externalVoiceCallBodyLimit, &request)
	if !ok {
		return
	}
	ctx, cancel := externalVoiceServerOperationContext(r, externalVoiceMutationTimeout)
	defer cancel()
	result, err := a.sipVoice.Answer(ctx, authorization.Identity, commandID, request, sha256.Sum256(body))
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceCallResponse{
		Version: externalVoiceAPISchemaVersion,
		Call:    result.Call,
	})
}

func (a *app) remoteVoiceV2Reject(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.control"] || !authorization.Actions["calls.read"] {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice reject is unavailable")
		return
	}
	controller, ok := a.sipVoice.(externalVoiceControlService)
	if !ok {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	commandID, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(commandID) {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return
	}
	var request externalVoiceRejectRequest
	body, ok := readExternalVoiceJSON(w, r, externalVoiceCallBodyLimit, &request)
	if !ok {
		return
	}
	ctx, cancel := externalVoiceServerOperationContext(r, externalVoiceMutationTimeout)
	defer cancel()
	result, err := controller.Reject(ctx, authorization.Identity, commandID, request, sha256.Sum256(body))
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceCallResponse{Version: externalVoiceAPISchemaVersion, Call: result.Call})
}

func (a *app) remoteVoiceV2DTMF(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.control"] || !authorization.Actions["calls.media"] {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice DTMF is unavailable")
		return
	}
	controller, ok := a.sipVoice.(externalVoiceControlService)
	if !ok {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	commandID, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(commandID) {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return
	}
	var request externalVoiceDTMFRequest
	body, ok := readExternalVoiceJSON(w, r, externalVoiceCallBodyLimit, &request)
	if !ok {
		return
	}
	ctx, cancel := externalVoiceServerOperationContext(r, externalVoiceMutationTimeout)
	defer cancel()
	result, err := controller.DTMF(ctx, authorization.Identity, commandID, request, sha256.Sum256(body))
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceCallResponse{Version: externalVoiceAPISchemaVersion, Call: result.Call})
}

func (a *app) remoteVoiceV2End(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.hangup"] || !authorization.Actions["calls.media"] {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice end is unavailable")
		return
	}
	if a.sipVoice == nil {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	commandID, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(commandID) {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return
	}
	var request externalVoiceCallRequest
	body, ok := readExternalVoiceJSON(w, r, externalVoiceCallBodyLimit, &request)
	if !ok {
		return
	}
	ctx, cancel := externalVoiceServerOperationContext(r, externalVoiceMutationTimeout)
	defer cancel()
	result, err := a.sipVoice.End(ctx, authorization.Identity, commandID, request, sha256.Sum256(body))
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceCallResponse{
		Version: externalVoiceAPISchemaVersion,
		Call:    result.Call,
	})
}

func (a *app) remoteVoiceV2Dial(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.control"] || !authorization.Actions["calls.read"] ||
		!authorization.Actions["calls.media"] {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice dialing is unavailable")
		return
	}
	dialer, ok := a.sipVoice.(externalVoiceDialService)
	if !ok {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	commandID, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(commandID) {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return
	}
	var request externalVoiceDialRequest
	body, ok := readExternalVoiceJSON(w, r, externalVoiceCallBodyLimit, &request)
	if !ok {
		return
	}
	ctx, cancel := externalVoiceServerOperationContext(r, externalVoiceMutationTimeout)
	defer cancel()
	call, err := dialer.Dial(ctx, authorization.Identity, commandID, request, sha256.Sum256(body))
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceCallResponse{Version: externalVoiceAPISchemaVersion, Call: call})
}

func (a *app) remoteVoiceV2Reconcile(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	allowControl := authorization.Actions["calls.control"]
	allowHangup := authorization.Actions["calls.hangup"]
	if !browser.Control || authorization.Identity == "" ||
		!authorization.Actions["calls.media"] || (!allowControl && !allowHangup) {
		writeRemoteAPIError(w, http.StatusForbidden, "forbidden", "external voice reconciliation is unavailable")
		return
	}
	if a.sipVoice == nil {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
		return
	}
	var request externalVoiceCallRequest
	if _, ok := readExternalVoiceJSON(w, r, externalVoiceCallBodyLimit, &request); !ok {
		return
	}
	ctx, cancel := externalVoiceServerOperationContext(r, externalVoiceReconcileTimeout)
	defer cancel()
	result, err := a.sipVoice.Reconcile(ctx, authorization.Identity, request, allowControl, allowHangup)
	if err != nil {
		writeExternalVoiceAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, externalVoiceCallResponse{
		Version: externalVoiceAPISchemaVersion,
		Call:    result,
	})
}

func readExternalVoiceJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) ([]byte, bool) {
	if r == nil || r.Body == nil || limit <= 0 {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil || len(body) == 0 || decodeStrictRemoteJSON(body, target) != nil {
		writeExternalVoiceAPIError(w, errExternalVoiceInvalid)
		return nil, false
	}
	return body, true
}

func externalVoiceServerOperationContext(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.Background()
	if r != nil {
		base = context.WithoutCancel(r.Context())
	}
	return context.WithTimeout(base, timeout)
}

func writeExternalVoiceAPIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sipgateway.ErrCommandOutcomeUnknown):
		markRemoteOperationUnknownOutcome(w)
		writeRemoteAPIError(w, http.StatusConflict, "unknown_outcome", "external voice outcome requires reconciliation")
	case errors.Is(err, errExternalVoiceInvalid):
		writeRemoteAPIError(w, http.StatusBadRequest, "invalid_request", "external voice request is invalid")
	case errors.Is(err, errExternalVoiceStale):
		writeRemoteAPIError(w, http.StatusConflict, "stale_call", "external voice call changed")
	case errors.Is(err, errExternalVoiceConflict):
		writeRemoteAPIError(w, http.StatusConflict, "conflict", "external voice ownership changed")
	case errors.Is(err, errExternalVoiceNotReady):
		writeRemoteAPIError(w, http.StatusConflict, "media_not_ready", "external voice media is not ready")
	case errors.Is(err, errExternalVoiceClosed), errors.Is(err, errExternalVoiceUnavailable):
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
	default:
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "voice_unavailable", "external voice provider is unavailable")
	}
}
