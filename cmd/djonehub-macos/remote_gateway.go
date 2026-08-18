package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	remoteVoiceV2SnapshotPath  = "/api/remote/v2/voice/snapshot"
	remoteVoiceV2OfferPath     = "/api/remote/v2/voice/media/offers"
	remoteVoiceV2AnswerPath    = "/api/remote/v2/voice/calls/answer"
	remoteVoiceV2RejectPath    = "/api/remote/v2/voice/calls/reject"
	remoteVoiceV2DTMFPath      = "/api/remote/v2/voice/calls/dtmf"
	remoteVoiceV2EndPath       = "/api/remote/v2/voice/calls/end"
	remoteVoiceV2DialPath      = "/api/remote/v2/voice/calls/dial"
	remoteVoiceV2ReconcilePath = "/api/remote/v2/voice/calls/reconcile"
)

func (a *app) lockModuleMutationFor(ctx context.Context, maximumWait time.Duration) bool {
	if a.moduleMutationMu.TryLock() {
		return true
	}
	timer := time.NewTimer(maximumWait)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
			if a.moduleMutationMu.TryLock() {
				return true
			}
		}
	}
}

// remoteVoiceV2RoutePolicy is the single security and persistence contract
// for the future external SIP/WebRTC API. The paths are deliberately not
// registered until their handlers exist, but every surrounding authorization,
// CSRF and ledger gate can already fail closed against the same fixed list.
type remoteVoiceV2RoutePolicy struct {
	requiredAction   string
	additionalAction string
	requiresMedia    bool
	requireExplicit  bool
	ephemeral        bool
	persistent       bool
}

func remoteVoiceV2Policy(path string) (remoteVoiceV2RoutePolicy, bool) {
	switch path {
	case remoteVoiceV2SnapshotPath:
		return remoteVoiceV2RoutePolicy{requiredAction: "calls.read"}, true
	case remoteVoiceV2OfferPath:
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.media", additionalAction: "calls.read",
			requireExplicit: true, ephemeral: true,
		}, true
	case remoteVoiceV2AnswerPath:
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.control", additionalAction: "calls.media",
			requiresMedia: true, requireExplicit: true, persistent: true,
		}, true
	case remoteVoiceV2RejectPath:
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.control", additionalAction: "calls.read",
			requireExplicit: true, persistent: true,
		}, true
	case remoteVoiceV2DTMFPath:
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.control", additionalAction: "calls.media",
			requiresMedia: true, requireExplicit: true, persistent: true,
		}, true
	case remoteVoiceV2EndPath:
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.hangup", additionalAction: "calls.media",
			requiresMedia: true, requireExplicit: true, persistent: true,
		}, true
	case remoteVoiceV2DialPath:
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.control", additionalAction: "calls.media",
			requiresMedia: true, requireExplicit: true, persistent: true,
		}, true
	case remoteVoiceV2ReconcilePath:
		// Reconciliation is inspection-only and must never enter the durable
		// mutation ledger. Its future handler must additionally distinguish an
		// uncertain answer from an uncertain end before inspecting the provider.
		return remoteVoiceV2RoutePolicy{
			requiredAction: "calls.media", requiresMedia: true,
			requireExplicit: true, ephemeral: true,
		}, true
	default:
		return remoteVoiceV2RoutePolicy{}, false
	}
}

// remoteAccessConfig is intentionally disabled unless at least one exact
// Tailscale login is configured. The backend keeps listening on loopback; the
// only supported remote ingress is Tailscale Serve, which strips spoofed
// identity headers before adding the authenticated user's identity.
type remoteAccessConfig struct {
	AllowedLogins map[string]struct{}
	Control       bool
	Capability    string
	Host          string
}

func (a *app) configureRemoteAccess(rawLogins string, control bool, capability, host, listen string, ledgerPath ...string) {
	allowed := make(map[string]struct{})
	for _, raw := range strings.Split(rawLogins, ",") {
		login := strings.ToLower(strings.TrimSpace(raw))
		if login != "" {
			allowed[login] = struct{}{}
		}
	}
	a.remoteAccess = remoteAccessConfig{
		AllowedLogins: allowed,
		Control:       control,
		Capability:    strings.TrimSpace(capability),
		Host:          canonicalRemoteHost(host),
	}
	a.remoteListen = strings.TrimSpace(listen)
	a.remoteCSRF = newRemoteCSRFStore()
	if len(allowed) > 0 {
		if !a.remoteAccess.enabled() {
			log.Printf("remote gateway remains disabled: exact host and app capability are required")
			return
		}
		path := ""
		if len(ledgerPath) > 0 {
			path = strings.TrimSpace(ledgerPath[0])
		}
		if path == "" {
			var err error
			path, err = remoteOperationLedgerPath()
			if err != nil {
				log.Printf("remote gateway remains disabled: operation ledger path: %v", err)
				a.remoteAccess = remoteAccessConfig{}
				return
			}
		}
		ledger, err := openRemoteOperationLedger(path)
		if err != nil {
			log.Printf("remote gateway remains disabled: operation ledger: %v", err)
			a.remoteAccess = remoteAccessConfig{}
			return
		}
		a.remoteLedger = ledger
		// Do not persist the private tailnet hostname, login names or capability
		// namespace in launchd logs. The operator can inspect the explicit startup
		// flags when diagnosing configuration.
		log.Printf("remote gateway enabled for %d allowlisted Tailscale login(s); control=%t",
			len(allowed), control)
	}
}

func remoteOperationLedgerPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "MacCellular", "remote", "operations.jsonl"), nil
}

func (c remoteAccessConfig) enabled() bool {
	return len(c.AllowedLogins) > 0 && c.Capability != "" && c.Host != ""
}

type remoteAuthorization struct {
	Identity string
	Actions  map[string]bool
}

func (a remoteAuthorization) allows(action string) bool {
	return a.Actions[action] || a.Actions["*"]
}

func (c remoteAccessConfig) authorize(r *http.Request) (remoteAuthorization, bool) {
	if !c.enabled() || r == nil {
		return remoteAuthorization{}, false
	}
	host := canonicalRemoteHost(r.Host)
	// Funnel is public and does not carry identity headers. Requiring the
	// tailnet DNS suffix and an allowlisted identity makes accidental public
	// exposure fail closed.
	if host != c.Host || !strings.HasSuffix(host, ".ts.net") {
		return remoteAuthorization{}, false
	}
	loginHeaders := r.Header.Values("Tailscale-User-Login")
	capabilityHeaders := r.Header.Values("Tailscale-App-Capabilities")
	if len(loginHeaders) != 1 || len(capabilityHeaders) != 1 {
		return remoteAuthorization{}, false
	}
	login := strings.ToLower(strings.TrimSpace(loginHeaders[0]))
	if login == "" {
		return remoteAuthorization{}, false
	}
	if _, ok := c.AllowedLogins[login]; !ok {
		return remoteAuthorization{}, false
	}
	actions, err := parseRemoteCapabilityActions(capabilityHeaders[0], c.Capability)
	if err != nil || len(actions) == 0 {
		return remoteAuthorization{}, false
	}
	return remoteAuthorization{Identity: login, Actions: actions}, true
}

func canonicalRemoteHost(raw string) string {
	host := strings.TrimSpace(raw)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

func parseRemoteCapabilityActions(raw, capability string) (map[string]bool, error) {
	if len(raw) == 0 || len(raw) > 16<<10 || strings.TrimSpace(capability) == "" {
		return nil, errors.New("missing or oversized app capability header")
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil {
		return nil, fmt.Errorf("decode app capability header: %w", err)
	}
	var grants map[string][]struct {
		Actions []string `json:"actions"`
	}
	decoder := json.NewDecoder(strings.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grants); err != nil {
		return nil, fmt.Errorf("parse app capability header: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing app capability data")
	}
	actions := make(map[string]bool)
	for _, grant := range grants[capability] {
		for _, action := range grant.Actions {
			action = strings.TrimSpace(action)
			if action != "" {
				actions[action] = true
			}
		}
	}
	return actions, nil
}

func isRemoteGatewayPath(path string) bool {
	if path == "/remote" || strings.HasPrefix(path, "/remote/") ||
		strings.HasPrefix(path, "/api/remote/v1/") {
		return true
	}
	_, ok := remoteVoiceV2Policy(path)
	return ok
}

func isRemoteGatewayMutation(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/api/remote/v1/") {
		if _, ok := remoteVoiceV2Policy(r.URL.Path); !ok {
			return false
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

type remoteAuthorizationContextKey struct{}

type remoteBrowserContextKey struct{}

type remoteBrowserContext struct {
	Control   bool
	Transport string
	SMSOnly   bool
}

type remoteBrowserSecurityPolicy struct {
	Host                string
	Control             bool
	Transport           string
	SMSOnly             bool
	NotFoundUnknownPath bool
	PathAllowed         func(string) bool
	Authorize           func(*http.Request) (remoteAuthorization, error)
	AuthorizationStatus func(error) int
}

var errRemoteBrowserUnauthorized = errors.New("remote browser request is unauthorized")

func remoteAuthorizationFromRequest(r *http.Request) remoteAuthorization {
	if r == nil {
		return remoteAuthorization{}
	}
	value, _ := r.Context().Value(remoteAuthorizationContextKey{}).(remoteAuthorization)
	return value
}

func remoteBrowserContextFromRequest(r *http.Request) remoteBrowserContext {
	if r == nil {
		return remoteBrowserContext{}
	}
	value, _ := r.Context().Value(remoteBrowserContextKey{}).(remoteBrowserContext)
	return value
}

func (a *app) remoteRoutes() http.Handler {
	// Native device proof is an independent transport contract. Never weaken
	// the browser's exact Origin/CSRF checks or ask a native client to forge
	// browser-only security metadata.
	root := http.NewServeMux()
	root.Handle("/api/native/", a.nativeRoutes())
	root.Handle("/", a.remoteSecurityHeaders(a.remoteBrowserRoutes(false)))
	return root
}

func (a *app) remoteBrowserRoutes(smsOnly bool) http.Handler {
	browserMux := http.NewServeMux()
	assets, err := fs.Sub(webAssets, "remote")
	if err != nil {
		panic(fmt.Sprintf("open embedded remote client: %v", err))
	}
	browserMux.Handle("GET /remote/", http.StripPrefix("/remote/", http.FileServer(http.FS(assets))))
	browserMux.HandleFunc("GET /remote", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/remote/", http.StatusTemporaryRedirect)
	})
	if smsOnly {
		browserMux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/remote/", http.StatusTemporaryRedirect)
		})
	}
	browserMux.HandleFunc("GET /api/remote/v1/session", a.remoteSession)
	browserMux.HandleFunc("GET /api/remote/v1/snapshot", a.remoteSnapshot)
	browserMux.HandleFunc("GET /api/remote/v1/sms/sync", a.remoteSyncSMS)
	browserMux.HandleFunc("POST /api/remote/v1/sms/send", a.remoteSendSMS)
	browserMux.HandleFunc("POST /api/remote/v1/sms/refresh", a.remoteRefreshSMS)
	if smsOnly {
		return browserMux
	}
	browserMux.HandleFunc("POST /api/remote/v1/media/offers", a.remoteMediaOffer)
	browserMux.HandleFunc("POST /api/remote/v1/calls/dial", a.remoteDialCall)
	browserMux.HandleFunc("POST /api/remote/v1/calls/answer", a.remoteAnswerCall)
	browserMux.HandleFunc("POST /api/remote/v1/calls/reject", a.remoteRejectCall)
	browserMux.HandleFunc("POST /api/remote/v1/calls/hangup", a.remoteHangupCall)
	browserMux.HandleFunc("POST /api/remote/v1/calls/dtmf", a.remoteDTMFCall)
	browserMux.HandleFunc("GET "+remoteVoiceV2SnapshotPath, a.remoteVoiceV2Snapshot)
	browserMux.HandleFunc("POST "+remoteVoiceV2OfferPath, a.remoteVoiceV2Offer)
	browserMux.HandleFunc("POST "+remoteVoiceV2AnswerPath, a.remoteVoiceV2Answer)
	browserMux.HandleFunc("POST "+remoteVoiceV2EndPath, a.remoteVoiceV2End)
	browserMux.HandleFunc("POST "+remoteVoiceV2DialPath, a.remoteVoiceV2Dial)
	browserMux.HandleFunc("POST "+remoteVoiceV2ReconcilePath, a.remoteVoiceV2Reconcile)
	return browserMux
}

func (a *app) remoteSession(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	browser := remoteBrowserContextFromRequest(r)
	if a.remoteCSRF == nil {
		writeError(w, http.StatusServiceUnavailable, "remote CSRF service unavailable")
		return
	}
	smsSync, _ := a.smsStoreStatus()
	actions := make([]string, 0, len(authorization.Actions))
	for action := range authorization.Actions {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	externalVoiceVersion := 0
	if !browser.SMSOnly && a.publicWebExternalVoiceEnabled() {
		externalVoiceVersion = externalVoiceAPISchemaVersion
	}
	mediaAvailable := a.remoteMediaAvailableForBrowser(browser)
	publicVoice := browser.Transport == "cloudflare-access" && !browser.SMSOnly &&
		a.publicWebVoiceEnabled()
	pushEnabled := publicVoice && a.publicWebPushEnabled() && authorization.allows("push.manage")
	media := "webrtc-pending"
	if browser.SMSOnly || (!mediaAvailable && externalVoiceVersion == 0) {
		media = "disabled"
	}
	notificationMode := "foreground-only"
	pushAPIVersion := 0
	if pushEnabled {
		notificationMode = "web-push"
		pushAPIVersion = publicWebPushAPIVersion
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        "1.4.0",
		"identity":       authorization.Identity,
		"actions":        actions,
		"csrf_token":     a.remoteCSRF.issue(authorization.Identity, time.Now()),
		"remote_control": browser.Control,
		"transport":      browser.Transport,
		"sms_only":       browser.SMSOnly,
		"media":          media,
		// Web Push can wake the service worker to show a notification. Audio
		// capture and call control still require the PWA to return to foreground.
		"background_calls":           false,
		"notification_mode":          notificationMode,
		"public_voice":               publicVoice,
		"push_enabled":               pushEnabled,
		"push_api_version":           pushAPIVersion,
		"sms_sync":                   smsSync,
		"media_offer":                mediaAvailable,
		"incoming_answer":            mediaAvailable && a.remoteIncomingAnswer,
		"rescue_hangup":              mediaAvailable && a.remoteRescueHangup && a.remoteIncomingAnswer,
		"external_voice":             !browser.SMSOnly && a.publicWebExternalVoiceEnabled(),
		"external_voice_api_version": externalVoiceVersion,
	})
}

func (a *app) remoteSyncSMS(w http.ResponseWriter, r *http.Request) {
	cursor, limit, code, err := parseRemoteSMSSyncQuery(r)
	if err != nil {
		writeRemoteAPIError(w, http.StatusBadRequest, code, "invalid SMS sync query")
		return
	}
	result, err := a.syncSMSHistory(cursor, limit)
	if err != nil {
		switch {
		case errors.Is(err, ErrSMSCursorInvalid):
			writeRemoteAPIError(w, http.StatusBadRequest, "invalid_cursor", "SMS cursor is invalid")
		case errors.Is(err, ErrSMSCursorResetRequired):
			writeRemoteAPIError(w, http.StatusGone, "reset_required", "SMS cursor reset is required")
		default:
			writeRemoteAPIError(w, http.StatusServiceUnavailable, "sms_store_unavailable", "durable SMS history is unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseRemoteSMSSyncQuery(r *http.Request) (cursor string, limit int, code string, err error) {
	if r == nil || r.URL == nil {
		return "", 0, "invalid_query", errors.New("request URL is unavailable")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", 0, "invalid_query", err
	}
	for key, values := range query {
		if key != "cursor" && key != "limit" {
			return "", 0, "invalid_query", fmt.Errorf("unexpected query parameter %q", key)
		}
		if len(values) != 1 {
			return "", 0, "invalid_query", fmt.Errorf("query parameter %q must occur exactly once", key)
		}
	}
	if values, ok := query["cursor"]; ok {
		cursor = strings.TrimSpace(values[0])
		if len(cursor) > 256 {
			return "", 0, "invalid_cursor", errors.New("SMS cursor is oversized")
		}
	}
	if values, ok := query["limit"]; ok {
		raw := values[0]
		if raw == "" || strings.TrimSpace(raw) != raw || strings.IndexFunc(raw, func(value rune) bool {
			return value < '0' || value > '9'
		}) >= 0 {
			return "", 0, "invalid_limit", errors.New("SMS limit is invalid")
		}
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > smsStoreMaximumLimit {
			return "", 0, "invalid_limit", errors.New("SMS limit is out of range")
		}
		limit = parsed
	}
	return cursor, limit, "", nil
}

func writeRemoteAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func (a *app) remoteSnapshot(w http.ResponseWriter, r *http.Request) {
	authorization := remoteAuthorizationFromRequest(r)
	a.smsMu.RLock()
	messages := append([]receivedSMS(nil), a.sms...)
	smsLastPoll := a.smsLastPoll
	smsLastError := a.smsLastPollError
	a.smsMu.RUnlock()

	a.callMu.RLock()
	var active *callRecord
	if a.activeCall != nil {
		copy := *a.activeCall
		active = &copy
	}
	history := append([]callRecord(nil), a.callHistory...)
	callLastPoll := a.callLastPoll
	callLastError := a.callLastPollError
	callGeneration := a.callGeneration
	mediaEligible := a.callMediaEligible
	a.callMu.RUnlock()

	a.moduleVoiceMu.Lock()
	voicePhase := a.moduleVoicePhase
	voiceReady := a.moduleVoiceReady && a.moduleVoicePhase == "ready"
	voiceError := a.moduleVoiceErr
	a.moduleVoiceMu.Unlock()

	if messages == nil {
		messages = []receivedSMS{}
	}
	if history == nil {
		history = []callRecord{}
	}
	response := map[string]any{
		"generated_at": time.Now(),
		"gateway": map[string]any{
			// Keep a high-frequency remote read from mutating USB discovery
			// state. Existing hardware pollers own enumeration and AT attach.
			"connected":   a.remoteGatewayConnected(),
			"voice_ready": voiceReady,
			"voice_phase": voicePhase,
			"voice_error": map[bool]string{true: "voice_route_unavailable", false: ""}[strings.TrimSpace(voiceError) != ""],
		},
	}
	if authorization.allows("sms.read") {
		response["sms"] = map[string]any{
			"items":           messages,
			"last_poll":       smsLastPoll,
			"last_poll_error": map[bool]string{true: "sms_poll_unavailable", false: ""}[strings.TrimSpace(smsLastError) != ""],
		}
	}
	if authorization.allows("calls.read") {
		response["calls"] = map[string]any{
			"active":          active,
			"history":         history,
			"last_poll":       callLastPoll,
			"last_poll_error": map[bool]string{true: "call_poll_unavailable", false: ""}[strings.TrimSpace(callLastError) != ""],
			"call_generation": callGeneration,
			"media_eligible":  mediaEligible,
			"remote_media":    a.remoteMediaStatusSnapshot(authorization.Identity),
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) remoteGatewayConnected() bool {
	if a.demo || a.modem != nil {
		return true
	}
	a.usbATOpenMu.Lock()
	defer a.usbATOpenMu.Unlock()
	return a.usbAT != nil
}

func remoteRequiredAction(path string) string {
	if policy, ok := remoteVoiceV2Policy(path); ok {
		return policy.requiredAction
	}
	if isPublicWebPushPath(path) {
		return "push.manage"
	}
	if path == publicWebRecordingUploadPath || strings.HasPrefix(path, publicWebRecordingUploadPath+"/") {
		return "calls.media"
	}
	switch path {
	case "/api/remote/v1/sms/send":
		return "sms.send"
	case "/api/remote/v1/sms/refresh", "/api/remote/v1/sms/sync":
		return "sms.read"
	case "/api/remote/v1/calls/hangup":
		return "calls.hangup"
	case "/api/remote/v1/calls/dial", "/api/remote/v1/calls/answer",
		"/api/remote/v1/calls/reject",
		"/api/remote/v1/calls/dtmf":
		return "calls.control"
	case "/api/remote/v1/media/offers":
		return "calls.media"
	default:
		return "status.read"
	}
}

func remoteRequiresMediaCapability(path string) bool {
	if policy, ok := remoteVoiceV2Policy(path); ok {
		return policy.requiresMedia
	}
	switch path {
	case "/api/remote/v1/calls/answer", "/api/remote/v1/calls/dial",
		"/api/remote/v1/calls/hangup":
		return true
	default:
		return false
	}
}

func (a *app) remoteSecurityHeaders(next http.Handler) http.Handler {
	return a.remoteBrowserSecurityHeaders(next, remoteBrowserSecurityPolicy{
		Host:      a.remoteAccess.Host,
		Control:   a.remoteAccess.Control,
		Transport: "tailscale-serve",
		PathAllowed: func(path string) bool {
			return isRemoteGatewayPath(path)
		},
		Authorize: func(request *http.Request) (remoteAuthorization, error) {
			authorization, ok := a.remoteAccess.authorize(request)
			if !ok {
				return remoteAuthorization{}, errRemoteBrowserUnauthorized
			}
			return authorization, nil
		},
		AuthorizationStatus: func(error) int { return http.StatusForbidden },
	})
}

func (a *app) remoteBrowserSecurityHeaders(next http.Handler, policy remoteBrowserSecurityPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		microphone := "(self)"
		if policy.SMSOnly {
			microphone = "()"
		}
		w.Header().Set("Permissions-Policy", "camera=(), microphone="+microphone+", geolocation=(), payment=(), usb=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; connect-src 'self'; img-src 'self'; manifest-src 'self'; script-src 'self'; style-src 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if policy.Authorize == nil || policy.PathAllowed == nil || policy.Host == "" {
			http.Error(w, "remote browser access unavailable", http.StatusServiceUnavailable)
			return
		}
		authorization, err := policy.Authorize(r)
		if err != nil {
			status := http.StatusForbidden
			if policy.AuthorizationStatus != nil {
				status = policy.AuthorizationStatus(err)
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		if !policy.PathAllowed(r.URL.Path) {
			if policy.NotFoundUnknownPath {
				http.NotFound(w, r)
			} else {
				http.Error(w, "remote path is not available", http.StatusForbidden)
			}
			return
		}
		required := remoteRequiredAction(r.URL.Path)
		if policy, isVoiceV2 := remoteVoiceV2Policy(r.URL.Path); isVoiceV2 {
			if !remoteVoiceV2AuthorizationAllows(authorization, policy) {
				http.Error(w, "remote capability does not allow this action", http.StatusForbidden)
				return
			}
		} else if !authorization.allows(required) {
			http.Error(w, "remote capability does not allow this action", http.StatusForbidden)
			return
		}
		if remoteRequiresMediaCapability(r.URL.Path) && !authorization.allows("calls.media") {
			http.Error(w, "remote media capability is required for this call mutation", http.StatusForbidden)
			return
		}
		mutation := isRemoteGatewayMutation(r)
		if mutation {
			if !policy.Control {
				http.Error(w, "remote control is disabled", http.StatusForbidden)
				return
			}
			if !remoteMutationRequestIsSameOrigin(r, policy.Host) {
				http.Error(w, "exact HTTPS same-origin request required", http.StatusForbidden)
				return
			}
			contentType, ok := remoteSingleHeader(r.Header, "Content-Type")
			mediaType, _, err := mime.ParseMediaType(contentType)
			recordingUpload := r.Method == http.MethodPost && r.URL.Path == publicWebRecordingUploadPath
			validContentType := strings.EqualFold(mediaType, "application/json") ||
				recordingUpload && strings.EqualFold(mediaType, "multipart/form-data")
			if !ok || err != nil || !validContentType {
				http.Error(w, "supported content type required", http.StatusUnsupportedMediaType)
				return
			}
			csrf, ok := remoteSingleHeader(r.Header, "X-MacCellular-CSRF")
			if !ok || a.remoteCSRF == nil || !a.remoteCSRF.valid(authorization.Identity, csrf, time.Now()) {
				http.Error(w, "valid CSRF token required", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), remoteAuthorizationContextKey{}, authorization)
		ctx = context.WithValue(ctx, remoteBrowserContextKey{}, remoteBrowserContext{
			Control: policy.Control, Transport: policy.Transport, SMSOnly: policy.SMSOnly,
		})
		r = r.WithContext(ctx)
		if mutation && !isRemoteEphemeralMutation(r) {
			if a.remoteLedger == nil {
				http.Error(w, "remote persistent operation ledger unavailable", http.StatusServiceUnavailable)
				return
			}
			a.serveRemotePersistentOperation(authorization.Identity, next, w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func remoteVoiceV2AuthorizationAllows(authorization remoteAuthorization, policy remoteVoiceV2RoutePolicy) bool {
	allows := authorization.allows
	if policy.requireExplicit {
		allows = func(action string) bool { return authorization.Actions[action] }
	}
	return allows(policy.requiredAction) &&
		(policy.additionalAction == "" || allows(policy.additionalAction))
}

// SDP and one-use media credentials are strictly in-memory. Persisting an SDP
// offer or answer in the hardware-operation ledger would leak endpoint data and
// could replay an already-dead peer after restart. All authentication, Origin,
// CSRF and capability checks above still apply.
func isRemoteEphemeralMutation(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	if isPublicWebPushMutation(r) {
		return true
	}
	if r.Method == http.MethodPost && r.URL.Path == publicWebRecordingUploadPath {
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	if r.URL.Path == "/api/remote/v1/media/offers" {
		return true
	}
	policy, ok := remoteVoiceV2Policy(r.URL.Path)
	return ok && policy.ephemeral
}

func (a *app) serveRemotePersistentOperation(identity string, next http.Handler, w http.ResponseWriter, r *http.Request) {
	key, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(key) {
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusBadRequest, Code: "invalid_request"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusBadRequest, Code: "invalid_request"})
		return
	}
	a.serveRemotePersistentOperationBody(identity, next, w, r, body)
}

// serveRemotePersistentOperationBody is shared by the browser and native
// transports after each has independently authenticated the exact request
// bytes. The hardware ledger deliberately ignores transport credentials and
// binds only the opaque principal, fixed path, idempotency key, method and
// complete body. This lets a fresh one-use native signature safely retrieve a
// prior result without repeating a modem mutation.
func (a *app) serveRemotePersistentOperationBody(identity string, next http.Handler, w http.ResponseWriter, r *http.Request, body []byte) {
	key, ok := remoteSingleHeader(r.Header, "Idempotency-Key")
	if !ok || !remoteLedgerKeyPattern.MatchString(key) {
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusBadRequest, Code: "invalid_request"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	requestHash := sha256.Sum256(append([]byte(r.Method+"\x00"+r.URL.Path+"\x00"), body...))
	begin, err := a.remoteLedger.Begin(identity, r.URL.Path, key, requestHash)
	if err != nil {
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusServiceUnavailable, Code: "service_unavailable"})
		return
	}
	switch begin.Disposition {
	case remoteOperationReplay:
		_ = writeRemoteOperationResponse(w, begin.Response)
		return
	case remoteOperationConflict:
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusConflict, Code: "conflict"})
		return
	case remoteOperationUnknownOutcome:
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusConflict, Code: "unknown_outcome"})
		return
	case remoteOperationWait:
		result, err := a.remoteLedger.Wait(r.Context(), begin.Operation)
		if err != nil {
			_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusConflict, Code: "unknown_outcome"})
			return
		}
		_ = writeRemoteOperationResponse(w, result)
		return
	case remoteOperationExecute:
		// continue below
	default:
		writeError(w, http.StatusServiceUnavailable, "invalid remote ledger disposition")
		return
	}

	recorder := newRemoteResponseRecorder()
	next.ServeHTTP(recorder, r)
	durable := recorder.durableResponse()
	// An external-SIP dial creates the opaque call identity the browser needs
	// to negotiate media. Keep that bounded, schema-validated response in the
	// idempotency journal so a lost HTTP response can be replayed without
	// redialing. All other persistent routes intentionally retain code-only
	// responses and never persist their bodies.
	if r.URL.Path == remoteVoiceV2DialPath && durable.Status >= 200 && durable.Status < 300 {
		durable.Payload = recorder.body.String()
	}
	completion, err := a.remoteLedger.Complete(begin.Operation, durable)
	if completion.Committed {
		if err != nil || !completion.AuditDurable {
			// The hardware result is already authoritative and replayable. The
			// audit copy failed, so expose a fixed terminal code that explicitly
			// must not be retried; the ledger rejects every new key while degraded.
			log.Printf("remote operation completed but audit is unavailable: %v", err)
			_ = writeRemoteOperationResponse(w, remoteOperationResponse{
				Status: http.StatusAccepted,
				Code:   "completed_audit_unavailable",
			})
			return
		}
		_ = writeRemoteOperationResponse(w, completion.Response)
		return
	}
	if err != nil {
		// Hardware may already have observed the operation. Never claim a
		// terminal outcome or invite an automatic retry when fsync failed.
		_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusConflict, Code: "unknown_outcome"})
		return
	}
	_ = writeRemoteOperationResponse(w, remoteOperationResponse{Status: http.StatusConflict, Code: "unknown_outcome"})
}

func classifyRemoteOperationResponse(status int) remoteOperationResponse {
	switch {
	case status >= 200 && status < 300:
		return remoteOperationResponse{Status: status, Code: "completed"}
	case status == http.StatusBadRequest || status == http.StatusUnsupportedMediaType:
		return remoteOperationResponse{Status: status, Code: "invalid_request"}
	case status == http.StatusConflict:
		return remoteOperationResponse{Status: status, Code: "conflict"}
	case status == http.StatusServiceUnavailable:
		return remoteOperationResponse{Status: status, Code: "service_unavailable"}
	case status == http.StatusBadGateway:
		return remoteOperationResponse{Status: status, Code: "modem_unavailable"}
	default:
		return remoteOperationResponse{Status: max(status, http.StatusInternalServerError), Code: "operation_failed"}
	}
}

func remoteMutationRequestIsSameOrigin(r *http.Request, expectedHost string) bool {
	if r == nil || canonicalRemoteHost(r.Host) != expectedHost {
		return false
	}
	fetchSite, ok := remoteSingleHeader(r.Header, "Sec-Fetch-Site")
	if !ok || strings.ToLower(fetchSite) != "same-origin" {
		return false
	}
	originValue, ok := remoteSingleHeader(r.Header, "Origin")
	if !ok {
		return false
	}
	origin, err := url.Parse(originValue)
	return err == nil && strings.EqualFold(origin.Scheme, "https") &&
		canonicalRemoteHost(origin.Host) == expectedHost && origin.Path == "" &&
		origin.RawQuery == "" && origin.Fragment == ""
}

func remoteSingleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	return value, value != "" && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "\r\n")
}

type remoteCSRFStore struct {
	secret [32]byte
}

func newRemoteCSRFStore() *remoteCSRFStore {
	store := &remoteCSRFStore{}
	if _, err := rand.Read(store.secret[:]); err != nil {
		panic(fmt.Sprintf("initialize remote CSRF secret: %v", err))
	}
	return store
}

func (s *remoteCSRFStore) issue(identity string, now time.Time) string {
	payload := make([]byte, 24)
	binary.BigEndian.PutUint64(payload[:8], uint64(now.Add(8*time.Hour).Unix()))
	if _, err := rand.Read(payload[8:]); err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSpace(identity))))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(append(payload, mac.Sum(nil)...))
}

func (s *remoteCSRFStore) valid(identity, token string, now time.Time) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil || len(decoded) != 56 {
		return false
	}
	payload, signature := decoded[:24], decoded[24:]
	expires := int64(binary.BigEndian.Uint64(payload[:8]))
	if expires < now.Unix() || expires > now.Add(9*time.Hour).Unix() {
		return false
	}
	mac := hmac.New(sha256.New, s.secret[:])
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSpace(identity))))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return hmac.Equal(signature, mac.Sum(nil))
}

type remoteCallExpectation struct {
	CallID         string `json:"call_id"`
	CallGeneration uint64 `json:"call_generation"`
	CallIndex      int    `json:"call_index"`
	CallDirection  string `json:"call_direction"`
}

type remoteIncomingAnswerRequest struct {
	Call            remoteCallExpectation `json:"call"`
	MediaSessionID  string                `json:"media_session_id"`
	LeaseGeneration uint64                `json:"lease_generation"`
}

func (a *app) remoteSendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	a.submitRemoteSMS(w, body.Phone, body.Message)
}

// submitRemoteSMS is transport-neutral business logic. Browser and native
// middleware apply different authentication contracts before calling it, but
// both must enter the same durable pending/submitted/unknown SMS state machine.
func (a *app) submitRemoteSMS(w http.ResponseWriter, rawPhone, rawMessage string) {
	phone := strings.TrimSpace(rawPhone)
	message := strings.TrimSpace(rawMessage)
	if phone == "" || message == "" {
		writeError(w, http.StatusBadRequest, "phone and message are required")
		return
	}
	// Keep a no-store demo usable for local screenshots. Every configured
	// gateway, including tests, records pending before the simulated or real
	// modem mutation so sync and idempotency share one state machine.
	if a.demo && !a.smsStoreConfigured() {
		a.recordSMS("demo-outgoing", message, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{"sent": true, "segments": 1})
		return
	}
	outgoing, err := a.beginOutgoingSMS(phone, message, time.Now())
	if err != nil {
		writeRemoteAPIError(w, http.StatusServiceUnavailable, "sms_store_unavailable", "durable SMS history is unavailable")
		return
	}
	segments := 1
	if a.demo {
		a.recordSMS("demo-outgoing", message, time.Now())
	} else {
		segments, err = a.sendTextSMS(phone, message)
	}
	if err != nil {
		_ = a.markOutgoingSMSUnknown(outgoing.ID, segments)
		markRemoteOperationUnknownOutcome(w)
		writeRemoteAPIError(w, http.StatusAccepted, "pending_confirmation", "SMS submission outcome requires confirmation")
		return
	}
	if _, err := a.updateOutgoingSMS(outgoing.ID, "submitted", segments); err != nil {
		_ = a.markOutgoingSMSUnknown(outgoing.ID, segments)
		markRemoteOperationUnknownOutcome(w)
		writeRemoteAPIError(w, http.StatusAccepted, "pending_confirmation", "SMS submission outcome requires confirmation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "segments": segments})
}

func (a *app) remoteRefreshSMS(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if !decodeJSON(w, r, &body) {
		return
	}
	if a.demo {
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		return
	}
	if a.modem == nil {
		if err := a.pollSMSOnce(); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	} else {
		go a.modem.CheckAllSMS()
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (a *app) remoteDialCall(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, stage, message string) {
		log.Printf("remote dial refused: stage=%s", stage)
		writeError(w, status, message)
	}
	if !a.remoteMediaAvailableForBrowser(remoteBrowserContextFromRequest(r)) {
		refuse(http.StatusConflict, "browser-media-unavailable", "remote outgoing media is not enabled by the operator")
		return
	}
	var body struct {
		Number                 string `json:"number"`
		ExpectedCallGeneration uint64 `json:"expected_call_generation"`
		MediaSessionID         string `json:"media_session_id"`
		LeaseGeneration        uint64 `json:"lease_generation"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	number := normalizeDialNumber(body.Number)
	if number == "" || body.ExpectedCallGeneration == 0 ||
		!remoteMediaSessionPattern.MatchString(body.MediaSessionID) || body.LeaseGeneration == 0 {
		refuse(http.StatusBadRequest, "invalid-request", "valid number, idle generation and media lease are required")
		return
	}
	if a.remoteMedia == nil {
		refuse(http.StatusConflict, "manager-unavailable", "remote outgoing media is unavailable")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	if authorization.Identity == "" || !authorization.allows("calls.media") {
		refuse(http.StatusForbidden, "identity-unavailable", "remote media identity or capability is unavailable")
		return
	}

	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if !a.remoteMediaIdleGenerationCurrent(body.ExpectedCallGeneration) {
		refuse(http.StatusConflict, "call-generation-changed", "call state changed before outgoing media was used")
		return
	}
	if a.directCallRedialCooldownRemaining(time.Now()) > 0 {
		refuse(http.StatusConflict, "redial-cooldown", "previous call is still in the redial cooldown")
		return
	}
	mediaWaitContext, cancelMediaWait := context.WithTimeout(r.Context(), 15*time.Second)
	err := a.remoteMedia.waitOutgoingPrepared(
		mediaWaitContext,
		authorization.Identity,
		body.MediaSessionID,
		body.LeaseGeneration,
		body.ExpectedCallGeneration,
		number,
	)
	cancelMediaWait()
	if err != nil {
		log.Printf("remote dial refused: stage=media-preparation-not-current detail=%s", err.Error())
		writeError(w, http.StatusConflict, "two-way remote media preparation is not current")
		return
	}
	if !a.moduleMutationMu.TryLock() {
		refuse(http.StatusConflict, "modem-mutation-busy", "another modem mutation is in progress")
		return
	}
	defer a.moduleMutationMu.Unlock()
	locationID, identityHash, identityErr := a.readModuleIdentity()
	if identityErr != nil {
		refuse(http.StatusBadGateway, "modem-identity-unavailable", "modem identity is unavailable")
		return
	}
	if err := a.prewarmOutgoingModuleVoiceRoute(body.ExpectedCallGeneration); err != nil {
		log.Printf("remote dial audio prewarm failed: %v", err)
		refuse(http.StatusConflict, "media-route-prewarm-failed", "module audio could not be prepared before dialing")
		return
	}
	var reservation remoteMediaActionReservation
	mediaGate := func(device *usbAT, identity usbATPhysicalIdentity) error {
		reserved, err := a.remoteMedia.beginOutgoingAction(
			authorization.Identity, body.MediaSessionID, body.LeaseGeneration,
			body.ExpectedCallGeneration, number, device, identity,
		)
		if err != nil {
			log.Printf("remote dial final media gate refused: detail=%s", err.Error())
			return err
		}
		reservation = reserved
		return nil
	}
	if a.implicitUACVoice {
		_, err = a.executeImplicitUACCallWithGate(
			locationID, identityHash, "ATD"+number+";", 8*time.Second, mediaGate,
		)
	} else {
		_, err = a.armDirectQPCMVAndExecuteCallWithGate(
			locationID, identityHash, true, "ATD"+number+";", 8*time.Second, mediaGate,
		)
	}
	if err != nil {
		keep := reservation.valid() && errors.Is(err, errDirectCallCommandAmbiguous)
		a.remoteMedia.finishIncomingAction(reservation, keep)
		if keep {
			markRemoteOperationUnknownOutcome(w)
			writeJSON(w, http.StatusAccepted, map[string]bool{"pending_confirmation": true})
			return
		}
		if errors.Is(err, errDirectCallCommandRejected) {
			refuse(http.StatusBadGateway, "modem-rejected", "modem explicitly rejected the outgoing call")
			return
		}
		log.Printf("remote dial route or call command failed before acceptance")
		refuse(http.StatusConflict, "media-or-call-ownership-changed", "outgoing media or call ownership changed")
		return
	}
	a.remoteMedia.finishIncomingAction(reservation, true)
	log.Printf("remote dial accepted: number_redacted=true media_generation_bound=true")
	writeJSON(w, http.StatusOK, map[string]bool{"dialing": true})
}

func (a *app) remoteAnswerCall(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, stage, message string) {
		log.Printf("remote answer refused: stage=%s", stage)
		writeError(w, status, message)
	}
	if !a.remoteMediaAvailableForBrowser(remoteBrowserContextFromRequest(r)) {
		refuse(http.StatusConflict, "browser-media-unavailable", "remote incoming answer is not enabled by the operator")
		return
	}
	var body remoteIncomingAnswerRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validRemoteCallExpectation(body.Call) || body.Call.CallDirection != "incoming" ||
		!remoteMediaSessionPattern.MatchString(body.MediaSessionID) || body.LeaseGeneration == 0 {
		writeError(w, http.StatusBadRequest, "exact incoming call and media lease ownership are required")
		return
	}
	if !a.remoteIncomingAnswer || a.remoteMedia == nil {
		refuse(http.StatusConflict, "operator-disabled", "remote incoming answer is not enabled by the operator")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	if authorization.Identity == "" || !authorization.allows("calls.media") {
		writeError(w, http.StatusForbidden, "remote media identity or capability is unavailable")
		return
	}

	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	mediaPrecheck := func() error {
		if err := a.remoteMedia.authorizeIncomingAction(
			authorization.Identity,
			body.MediaSessionID,
			body.LeaseGeneration,
			body.Call,
		); err != nil {
			return err
		}
		return nil
	}
	if err := waitForRemoteIncomingAnswerReadiness(
		r.Context(), 12*time.Second, 200*time.Millisecond,
		func() error { return a.remoteCachedCallMatches(body.Call, []string{"incoming", "waiting"}) },
		mediaPrecheck,
	); err != nil {
		if errors.Is(err, errRemoteIncomingCallChanged) {
			refuse(http.StatusConflict, "ringing-call-changed", "ringing call ownership changed")
		} else {
			refuse(http.StatusConflict, "media-not-current", "two-way remote media preparation is not current")
		}
		return
	}
	if !a.moduleMutationMu.TryLock() {
		refuse(http.StatusConflict, "modem-busy", "another modem mutation is in progress")
		return
	}
	defer a.moduleMutationMu.Unlock()
	if err := a.remoteCachedCallMatches(body.Call, []string{"incoming", "waiting"}); err != nil {
		refuse(http.StatusConflict, "ringing-call-changed", "ringing call ownership changed")
		return
	}
	mediaGateChanged := errors.New("remote media authorization changed")
	if err := mediaPrecheck(); err != nil {
		refuse(http.StatusConflict, "media-not-current", "two-way remote media preparation is not current")
		return
	}
	var reservation remoteMediaActionReservation
	finishReservation := func(keep bool) {
		a.remoteMedia.finishIncomingAction(reservation, keep)
		reservation = remoteMediaActionReservation{}
	}
	defer finishReservation(false)
	finalMediaGate := func(device *usbAT, identity usbATPhysicalIdentity) error {
		reserved, err := a.remoteMedia.beginIncomingAction(
			authorization.Identity,
			body.MediaSessionID,
			body.LeaseGeneration,
			body.Call,
			device,
			identity,
		)
		if err != nil {
			return fmt.Errorf("%w: %v", mediaGateChanged, err)
		}
		reservation = reserved
		return nil
	}

	var response string
	var err error
	state := a.directQPCMVRouteState()
	switch {
	case a.implicitUACVoice:
		locationID, identityHash, identityErr := a.readModuleIdentity()
		if identityErr != nil {
			writeError(w, http.StatusBadGateway, "module identity is unavailable")
			return
		}
		response, err = a.executeImplicitUACCallWithGate(
			locationID,
			identityHash,
			"ATA",
			5*time.Second,
			finalMediaGate,
		)
	case state.phase == "armed" && !state.ready && state.generation == 0 &&
		state.intent.direction == "incoming" && state.intent.callID == body.Call.CallID &&
		state.intent.index == body.Call.CallIndex:
		response, err = a.executeCallOnArmedDirectQPCMVWithGate("ATA", 5*time.Second, finalMediaGate)
	case state.phase == "" || state.phase == "stopped" || state.phase == "unavailable" || state.phase == "failed":
		locationID, identityHash, identityErr := a.readModuleIdentity()
		if identityErr != nil {
			writeError(w, http.StatusBadGateway, "module identity is unavailable")
			return
		}
		response, err = a.armDirectQPCMVAndExecuteCallWithGate(
			locationID,
			identityHash,
			false,
			"ATA",
			5*time.Second,
			finalMediaGate,
		)
	default:
		refuse(http.StatusConflict, "voice-route-not-ready", "voice route ownership is not safe for this incoming call")
		return
	}
	if err != nil {
		if errors.Is(err, mediaGateChanged) {
			refuse(http.StatusConflict, "media-changed-before-answer", "two-way remote media preparation changed before answer")
			return
		}
		if reservation.valid() && !errors.Is(err, errDirectCallCommandRejected) {
			finishReservation(true)
			markRemoteOperationUnknownOutcome(w)
			writeJSON(w, http.StatusAccepted, map[string]bool{"pending_confirmation": true})
			return
		}
		finishReservation(false)
		writeError(w, http.StatusBadGateway, "module did not explicitly accept the media-gated answer")
		return
	}
	if !directQPCMVExplicitOK(response) {
		finishReservation(true)
		markRemoteOperationUnknownOutcome(w)
		writeJSON(w, http.StatusAccepted, map[string]bool{"pending_confirmation": true})
		return
	}
	a.callMu.Lock()
	a.lastAnswerAt = time.Now()
	a.callMu.Unlock()
	a.refreshCallStateAfterAcceptedAnswer()
	finishReservation(true)
	log.Printf("remote answer accepted: response_ack=ok media_generation_bound=true")
	writeJSON(w, http.StatusOK, map[string]bool{"answered": true})
}

var errRemoteIncomingCallChanged = errors.New("remote incoming call changed")

func waitForRemoteIncomingAnswerReadiness(
	ctx context.Context,
	timeout time.Duration,
	pollInterval time.Duration,
	callCurrent func() error,
	mediaCurrent func() error,
) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if callCurrent() != nil {
			return errRemoteIncomingCallChanged
		}
		if mediaCurrent() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("remote incoming media readiness timed out")
		case <-ticker.C:
		}
	}
}

func (a *app) remoteRejectCall(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusConflict, "remote call control is unavailable until a non-ABA call identity is established")
}

func (a *app) remoteHangupCall(w http.ResponseWriter, r *http.Request) {
	if !a.remoteMediaAvailableForBrowser(remoteBrowserContextFromRequest(r)) {
		writeError(w, http.StatusConflict, "remote rescue hangup is not enabled by the operator")
		return
	}
	var body remoteIncomingAnswerRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validRemoteCallExpectation(body.Call) {
		writeError(w, http.StatusBadRequest, "exact active call and media rescue ownership are required")
		return
	}
	if !a.remoteRescueHangup || !a.remoteIncomingAnswer || a.remoteMedia == nil {
		writeError(w, http.StatusConflict, "remote rescue hangup is not enabled by the operator")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	if authorization.Identity == "" || !authorization.allows("calls.media") ||
		!authorization.allows("calls.hangup") {
		writeError(w, http.StatusForbidden, "remote rescue identity or capability is unavailable")
		return
	}
	ticket := callMediaTicket{
		Generation: body.Call.CallGeneration, CallID: body.Call.CallID,
		Index: body.Call.CallIndex, Direction: body.Call.CallDirection,
	}
	if body.MediaSessionID == "" && body.LeaseGeneration == 0 {
		a.remoteCallMu.Lock()
		defer a.remoteCallMu.Unlock()
		if !a.lockModuleMutationFor(r.Context(), 3*time.Second) {
			writeError(w, http.StatusConflict, "another modem mutation is in progress")
			return
		}
		defer a.moduleMutationMu.Unlock()
		if err := a.remoteCachedCallMatches(body.Call, []string{"active"}); err != nil ||
			!a.callMediaTicketIsCurrent(ticket) {
			writeError(w, http.StatusConflict, "active call ownership changed")
			return
		}
		err := a.executeRemoteAuthenticatedHangup(ticket)
		if errors.Is(err, errRemoteRescueHangupAmbiguous) {
			markRemoteOperationUnknownOutcome(w)
			writeJSON(w, http.StatusAccepted, map[string]bool{"pending_confirmation": true})
			return
		}
		if errors.Is(err, errRemoteRescueHangupRejected) {
			writeError(w, http.StatusBadGateway, "modem explicitly rejected hangup")
			return
		}
		if err != nil {
			writeError(w, http.StatusConflict, "call ownership changed before hangup")
			return
		}
		log.Printf("remote authenticated hangup accepted: response_ack=ok exact_call_bound=true")
		writeJSON(w, http.StatusOK, map[string]bool{"hung_up": true})
		return
	}
	if body.MediaSessionID == "" || body.LeaseGeneration == 0 {
		writeError(w, http.StatusBadRequest, "media rescue fields must be provided together")
		return
	}
	if !remoteMediaSessionPattern.MatchString(body.MediaSessionID) || body.LeaseGeneration == 0 {
		writeError(w, http.StatusBadRequest, "exact active call and media rescue ownership are required")
		return
	}

	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if !a.lockModuleMutationFor(r.Context(), 3*time.Second) {
		writeError(w, http.StatusConflict, "another modem mutation is in progress")
		return
	}
	defer a.moduleMutationMu.Unlock()
	if err := a.remoteCachedCallMatches(body.Call, []string{"active"}); err != nil ||
		!a.callMediaTicketIsCurrent(ticket) {
		writeError(w, http.StatusConflict, "active call ownership changed")
		return
	}
	if err := a.remoteMedia.authorizeRescueHangup(
		authorization.Identity, body.MediaSessionID, body.LeaseGeneration, ticket,
	); err != nil {
		writeError(w, http.StatusConflict, "media-proven rescue authority is unavailable")
		return
	}
	reservation, err := a.executeRemoteRescueHangup(
		authorization.Identity, body.MediaSessionID, body.LeaseGeneration, ticket,
	)
	if err != nil {
		if reservation.valid() && errors.Is(err, errRemoteRescueHangupAmbiguous) {
			markRemoteOperationUnknownOutcome(w)
			writeJSON(w, http.StatusAccepted, map[string]bool{"pending_confirmation": true})
			return
		}
		if reservation.valid() && errors.Is(err, errRemoteRescueHangupRejected) {
			writeError(w, http.StatusBadGateway, "modem explicitly rejected the one-shot rescue hangup")
			return
		}
		writeError(w, http.StatusConflict, "rescue ownership changed before hangup")
		return
	}
	log.Printf("remote rescue hangup accepted: response_ack=ok media_generation_bound=true")
	writeJSON(w, http.StatusOK, map[string]bool{"hung_up": true})
}

func (a *app) remoteDTMFCall(w http.ResponseWriter, r *http.Request) {
	if !a.remoteMediaAvailableForBrowser(remoteBrowserContextFromRequest(r)) {
		writeError(w, http.StatusConflict, "remote DTMF is not enabled by the operator")
		return
	}
	var body struct {
		Call   remoteCallExpectation `json:"call"`
		Digit  string                `json:"digit"`
		Digits string                `json:"digits"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	digits := body.Digits
	if digits == "" {
		digits = body.Digit
	}
	if (body.Digits != "" && body.Digit != "") || !validRemoteCallExpectation(body.Call) ||
		len(digits) == 0 || len(digits) > 20 || strings.Trim(digits, "0123456789*#") != "" {
		writeError(w, http.StatusBadRequest, "exact active call and 1-20 DTMF digits are required")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	if authorization.Identity == "" || !authorization.allows("calls.control") {
		writeError(w, http.StatusForbidden, "remote call control is unavailable")
		return
	}
	a.remoteCallMu.Lock()
	defer a.remoteCallMu.Unlock()
	if !a.lockModuleMutationFor(r.Context(), 1500*time.Millisecond) {
		writeError(w, http.StatusConflict, "another modem operation is in progress")
		return
	}
	defer a.moduleMutationMu.Unlock()
	if err := a.remoteCachedCallMatches(body.Call, []string{"active"}); err != nil {
		writeError(w, http.StatusConflict, "active call changed before DTMF was sent")
		return
	}
	if err := a.requireLegacyModuleVoicePath(); err != nil {
		writeError(w, http.StatusConflict, "module voice path is unavailable")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
		return
	}
	if err := sendCallDTMFSequenceWith(a.runATCommand, digits); err != nil {
		writeError(w, http.StatusBadGateway, "modem did not accept DTMF")
		return
	}
	log.Printf("remote DTMF accepted: digit_count=%d digits_redacted=true exact_call_bound=true", len(digits))
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func validRemoteCallExpectation(expected remoteCallExpectation) bool {
	return expected.CallID != "" && expected.CallGeneration != 0 && expected.CallIndex >= 0 &&
		(expected.CallDirection == "incoming" || expected.CallDirection == "outgoing")
}

func (a *app) remoteCachedCallMatches(expected remoteCallExpectation, allowedStates []string) error {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if a.activeCall == nil || a.callGeneration != expected.CallGeneration ||
		a.activeCall.ID != expected.CallID || a.activeCall.Index != expected.CallIndex ||
		a.activeCall.Direction != expected.CallDirection {
		return errors.New("call generation no longer matches; command was not executed")
	}
	for _, state := range allowedStates {
		if a.activeCall.State == state {
			return nil
		}
	}
	return fmt.Errorf("call state %q does not allow this command", a.activeCall.State)
}

type remoteResponseRecorder struct {
	header         http.Header
	status         int
	body           bytes.Buffer
	durableOutcome *remoteOperationResponse
}

func markRemoteOperationUnknownOutcome(w http.ResponseWriter) {
	if recorder, ok := w.(*remoteResponseRecorder); ok {
		outcome := remoteOperationResponse{Status: http.StatusConflict, Code: "unknown_outcome"}
		recorder.durableOutcome = &outcome
	}
}

func newRemoteResponseRecorder() *remoteResponseRecorder {
	return &remoteResponseRecorder{header: make(http.Header)}
}

func (r *remoteResponseRecorder) Header() http.Header { return r.header }

func (r *remoteResponseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *remoteResponseRecorder) Write(payload []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(payload)
}

func (r *remoteResponseRecorder) statusCode() int {
	status := r.status
	if status == 0 {
		status = http.StatusOK
	}
	return status
}

func (r *remoteResponseRecorder) durableResponse() remoteOperationResponse {
	if r != nil && r.durableOutcome != nil {
		return *r.durableOutcome
	}
	if r == nil {
		return remoteOperationResponse{Status: http.StatusInternalServerError, Code: "operation_failed"}
	}
	return classifyRemoteOperationResponse(r.statusCode())
}
