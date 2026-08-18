package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
)

const nativeGatewayMaxBody = 16 << 10

type nativePrincipalContextKey struct{}
type nativeSMSSendContextKey struct{}

type nativeSMSSendRequest struct {
	Version     int    `json:"version"`
	OperationID string `json:"operation_id"`
	Phone       string `json:"phone"`
	Message     string `json:"message"`
}

type nativeLocalTicketRequest struct {
	Confirm bool     `json:"confirm"`
	Owner   string   `json:"owner,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

type nativeLocalRevokeRequest struct {
	Confirm  bool   `json:"confirm"`
	DeviceID string `json:"device_id"`
}

func (a *app) configureNativeGateway(root string, enrollmentOnce bool) error {
	if a == nil || !a.remoteAccess.enabled() {
		if enrollmentOnce {
			return errors.New("native enrollment requires the authenticated remote gateway")
		}
		return nil
	}
	store, err := openNativeDeviceStore(root)
	if err != nil {
		return err
	}
	a.nativeDeviceStore = store
	a.nativeEnrollment = newNativeEnrollmentManager(store, enrollmentOnce)
	a.nativeAuth = newNativeAuthManager(store)
	return nil
}

func (a *app) closeNativeGateway() {
	if a == nil {
		return
	}
	store := a.nativeDeviceStore
	a.nativeDeviceStore = nil
	a.nativeEnrollment = nil
	a.nativeAuth = nil
	if store != nil {
		_ = store.Close()
	}
}

func (a *app) nativeRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/native/v1/enrollments", a.nativeEnrollmentAPI)
	mux.HandleFunc("POST /api/native/v1/auth/challenges", a.nativeChallengeAPI)
	mux.HandleFunc("GET /api/native/v1/session", a.nativeSessionAPI)
	mux.HandleFunc("GET /api/native/v1/sms/sync", a.nativeSMSSyncAPI)
	mux.HandleFunc("GET /api/native/v1/sms/operations", a.nativeSMSOperationAPI)
	mux.HandleFunc("POST /api/native/v1/sms/send", a.nativeSMSSendAPI)
	mux.HandleFunc("/api/native/", http.NotFound)
	return a.nativeSecurityHeaders(mux)
}

func (a *app) nativeSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if r == nil || r.URL == nil || !strings.HasPrefix(r.URL.Path, "/api/native/v1/") {
			http.NotFound(w, r)
			return
		}
		// Native requests use a device-held key. Browser security metadata is
		// rejected instead of being forged by a native HTTP stack.
		if nativeHeaderPresent(r.Header, "Origin") ||
			nativeHeaderPresent(r.Header, "Sec-Fetch-Site") ||
			nativeHeaderPresent(r.Header, "X-MacCellular-CSRF") {
			writeNativeError(w, http.StatusForbidden, "native_transport_required")
			return
		}
		authorization, ok := a.remoteAccess.authorize(r)
		if !ok {
			writeNativeError(w, http.StatusForbidden, "tailscale_auth_required")
			return
		}
		if r.Method == http.MethodPost && !nativeJSONRequest(r) {
			writeNativeError(w, http.StatusUnsupportedMediaType, "json_required")
			return
		}
		ctx := context.WithValue(r.Context(), remoteAuthorizationContextKey{}, authorization)
		r = r.WithContext(ctx)
		switch r.URL.Path {
		case "/api/native/v1/enrollments":
			if !authorization.allows("devices.enroll") {
				writeNativeError(w, http.StatusForbidden, "capability_denied")
				return
			}
		case "/api/native/v1/auth/challenges":
			// The requested target determines the action in the handler.
		case "/api/native/v1/session", "/api/native/v1/sms/sync", "/api/native/v1/sms/operations", "/api/native/v1/sms/send":
			if a.nativeAuth == nil {
				writeNativeError(w, http.StatusServiceUnavailable, "native_auth_unavailable")
				return
			}
			var signedBody []byte
			if r.URL.Path == "/api/native/v1/sms/send" {
				if !a.remoteAccess.Control {
					writeNativeError(w, http.StatusForbidden, "native_control_disabled")
					return
				}
				var err error
				signedBody, err = readNativeJSONBody(w, r)
				if err != nil {
					writeNativeError(w, http.StatusBadRequest, "invalid_request")
					return
				}
			}
			principal, err := a.nativeAuth.Verify(r, authorization.Identity, r.Host, authorization.Actions, signedBody)
			if err != nil {
				writeNativeError(w, http.StatusForbidden, "device_proof_invalid")
				return
			}
			ctx = context.WithValue(r.Context(), nativePrincipalContextKey{}, principal)
			if r.URL.Path == "/api/native/v1/sms/send" {
				request, err := validateNativeSMSSendRequest(signedBody, r.Header)
				if err != nil {
					writeNativeError(w, http.StatusBadRequest, "invalid_request")
					return
				}
				ctx = context.WithValue(ctx, nativeSMSSendContextKey{}, request)
			}
			r = r.WithContext(ctx)
			if r.URL.Path == "/api/native/v1/sms/send" {
				if a.remoteLedger == nil {
					writeNativeError(w, http.StatusServiceUnavailable, "operation_ledger_unavailable")
					return
				}
				a.serveRemotePersistentOperationBody(
					nativeLedgerPrincipal(principal), next, w, r, signedBody,
				)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) nativeEnrollmentAPI(w http.ResponseWriter, r *http.Request) {
	if a.nativeEnrollment == nil {
		writeNativeError(w, http.StatusServiceUnavailable, "native_enrollment_unavailable")
		return
	}
	body, err := readNativeJSONBody(w, r)
	if err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var discriminator struct {
		Step string `json:"step"`
	}
	if err := json.Unmarshal(body, &discriminator); err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	switch discriminator.Step {
	case "prepare":
		var request nativeEnrollmentPrepareRequest
		if err := decodeStrictJSON(body, &request); err != nil {
			writeNativeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		response, err := a.nativeEnrollment.Prepare(authorization.Identity, r.Host, request)
		if err != nil {
			writeNativeEnrollmentFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	case "complete":
		var request nativeEnrollmentCompleteRequest
		if err := decodeStrictJSON(body, &request); err != nil {
			writeNativeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		response, err := a.nativeEnrollment.Complete(authorization.Identity, r.Host, request)
		if err != nil {
			writeNativeEnrollmentFailure(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, response)
	default:
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
	}
}

func (a *app) nativeChallengeAPI(w http.ResponseWriter, r *http.Request) {
	if a.nativeAuth == nil {
		writeNativeError(w, http.StatusServiceUnavailable, "native_auth_unavailable")
		return
	}
	body, err := readNativeJSONBody(w, r)
	if err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var request nativeAuthChallengeRequest
	if err := decodeStrictJSON(body, &request); err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if request.Method == http.MethodPost && request.RequestTarget == "/api/native/v1/sms/send" && !a.remoteAccess.Control {
		writeNativeError(w, http.StatusForbidden, "challenge_denied")
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	response, err := a.nativeAuth.Issue(authorization.Identity, r.Host, authorization.Actions, request)
	if err != nil {
		writeNativeError(w, http.StatusForbidden, "challenge_denied")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) nativeSessionAPI(w http.ResponseWriter, r *http.Request) {
	principal, ok := nativePrincipalFromRequest(r)
	if !ok || !principal.allows("status.read") {
		writeNativeError(w, http.StatusForbidden, "device_proof_invalid")
		return
	}
	smsSync, _ := a.smsStoreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   1,
		"device_id": principal.DeviceID,
		"algorithm": "ES256",
		"actions":   sortedNativeActions(principal.Actions),
		"transport": "tailscale-serve-device-proof",
		"sms_sync":  smsSync && principal.allows("sms.read"),
		"sms_send":  smsSync && a.remoteAccess.Control && a.remoteLedger != nil && principal.allows("sms.send"),
	})
}

func (a *app) nativeSMSSyncAPI(w http.ResponseWriter, r *http.Request) {
	principal, ok := nativePrincipalFromRequest(r)
	if !ok || !principal.allows("sms.read") {
		writeNativeError(w, http.StatusForbidden, "device_proof_invalid")
		return
	}
	cursor, limit, code, err := parseRemoteSMSSyncQuery(r)
	if err != nil || limit < 1 || limit > nativeSMSSyncMaximumLimit {
		if code == "" {
			code = "invalid_limit"
		}
		writeNativeError(w, http.StatusBadRequest, code)
		return
	}
	result, err := a.syncSMSHistory(cursor, limit)
	if err != nil {
		switch {
		case errors.Is(err, ErrSMSCursorInvalid):
			writeNativeError(w, http.StatusBadRequest, "invalid_cursor")
		case errors.Is(err, ErrSMSCursorResetRequired):
			writeNativeError(w, http.StatusGone, "reset_required")
		default:
			writeNativeError(w, http.StatusServiceUnavailable, "sms_store_unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) nativeSMSSendAPI(w http.ResponseWriter, r *http.Request) {
	principal, ok := nativePrincipalFromRequest(r)
	request, requestOK := r.Context().Value(nativeSMSSendContextKey{}).(nativeSMSSendRequest)
	if !ok || !principal.allows("sms.send") || !requestOK {
		writeNativeError(w, http.StatusForbidden, "device_proof_invalid")
		return
	}
	// The browser and native clients share the same durable SMS state machine.
	// The surrounding native middleware has already bound the exact body to a
	// one-use device signature and to the persistent operation ledger.
	a.submitRemoteSMS(w, request.Phone, request.Message)
}

func (a *app) nativeSMSOperationAPI(w http.ResponseWriter, r *http.Request) {
	principal, ok := nativePrincipalFromRequest(r)
	if !ok || !principal.allows("sms.send") {
		writeNativeError(w, http.StatusForbidden, "device_proof_invalid")
		return
	}
	if a.remoteLedger == nil {
		writeNativeError(w, http.StatusServiceUnavailable, "operation_ledger_unavailable")
		return
	}
	operationID := r.URL.Query().Get("operation_id")
	if !validOpaqueID(operationID, "nsm_") {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := a.remoteLedger.Lookup(
		nativeLedgerPrincipal(principal), "/api/native/v1/sms/send", operationID,
	)
	if err != nil {
		writeNativeError(w, http.StatusServiceUnavailable, "operation_ledger_unavailable")
		return
	}
	response := struct {
		Version     int    `json:"version"`
		OperationID string `json:"operation_id"`
		State       string `json:"state"`
		HTTPStatus  int    `json:"http_status,omitempty"`
		Code        string `json:"code,omitempty"`
	}{Version: 1, OperationID: operationID, State: result.State}
	if result.State == "completed" {
		response.HTTPStatus = result.Response.Status
		response.Code = result.Response.Code
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) nativeLocalTicketAPI(w http.ResponseWriter, r *http.Request) {
	localNativeHeaders(w)
	if nativeHeaderPresent(r.Header, "Origin") || nativeHeaderPresent(r.Header, "Sec-Fetch-Site") ||
		!nativeJSONRequest(r) {
		writeNativeError(w, http.StatusForbidden, "local_cli_required")
		return
	}
	if a.nativeEnrollment == nil {
		writeNativeError(w, http.StatusServiceUnavailable, "native_enrollment_unavailable")
		return
	}
	body, err := readNativeJSONBody(w, r)
	if err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var request nativeLocalTicketRequest
	if err := decodeStrictJSON(body, &request); err != nil || !request.Confirm {
		writeNativeError(w, http.StatusBadRequest, "confirmation_required")
		return
	}
	owner, err := a.nativeEnrollmentOwner(request.Owner)
	if err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_owner")
		return
	}
	scopes := request.Scopes
	if len(scopes) == 0 {
		scopes = []string{"sms.read", "status.read"}
	}
	response, err := a.nativeEnrollment.Issue(owner, a.remoteAccess.Host, scopes)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, ErrNativeEnrollmentDisabled) {
			status = http.StatusForbidden
		}
		writeNativeError(w, status, "ticket_unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (a *app) nativeLocalRevokeAPI(w http.ResponseWriter, r *http.Request) {
	localNativeHeaders(w)
	if nativeHeaderPresent(r.Header, "Origin") || nativeHeaderPresent(r.Header, "Sec-Fetch-Site") ||
		!nativeJSONRequest(r) {
		writeNativeError(w, http.StatusForbidden, "local_cli_required")
		return
	}
	if a.nativeDeviceStore == nil {
		writeNativeError(w, http.StatusServiceUnavailable, "native_store_unavailable")
		return
	}
	body, err := readNativeJSONBody(w, r)
	if err != nil {
		writeNativeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var request nativeLocalRevokeRequest
	if err := decodeStrictJSON(body, &request); err != nil || !request.Confirm || !validNativeDeviceID(request.DeviceID) {
		writeNativeError(w, http.StatusBadRequest, "confirmation_required")
		return
	}
	if _, err := a.nativeDeviceStore.Revoke(request.DeviceID); err != nil {
		writeNativeError(w, http.StatusConflict, "revoke_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "code": "revoked"})
}

func (a *app) nativeEnrollmentOwner(raw string) (string, error) {
	owner := strings.ToLower(strings.TrimSpace(raw))
	if owner == "" {
		if len(a.remoteAccess.AllowedLogins) != 1 {
			return "", errors.New("owner is required when multiple logins are configured")
		}
		for login := range a.remoteAccess.AllowedLogins {
			owner = login
		}
	}
	if _, ok := a.remoteAccess.AllowedLogins[owner]; !ok {
		return "", errors.New("owner is not allowlisted")
	}
	return normalizeNativeOwner(owner)
}

func nativePrincipalFromRequest(r *http.Request) (nativePrincipal, bool) {
	if r == nil {
		return nativePrincipal{}, false
	}
	principal, ok := r.Context().Value(nativePrincipalContextKey{}).(nativePrincipal)
	return principal, ok
}

func nativeLedgerPrincipal(principal nativePrincipal) string {
	// remoteOperationLedger hashes this value before writing it. Hash the
	// case-sensitive device id first because Begin canonicalizes its historical
	// login parameter to lower case.
	return "native-v1:" + principal.Owner + ":" + remoteLedgerHash(principal.DeviceID)
}

func validateNativeSMSSendRequest(body []byte, header http.Header) (nativeSMSSendRequest, error) {
	var request nativeSMSSendRequest
	if err := decodeStrictJSON(body, &request); err != nil || request.Version != 1 ||
		!validOpaqueID(request.OperationID, "nsm_") {
		return nativeSMSSendRequest{}, errors.New("invalid native SMS request")
	}
	idempotencyKey, ok := nativeSingleHeader(header, "Idempotency-Key")
	if !ok || idempotencyKey != request.OperationID {
		return nativeSMSSendRequest{}, errors.New("native SMS operation id mismatch")
	}
	phone := strings.TrimSpace(request.Phone)
	message := strings.TrimSpace(request.Message)
	if phone == "" || message == "" || phone != request.Phone || message != request.Message ||
		strings.ContainsRune(phone, 0) || strings.ContainsRune(message, 0) ||
		len(phone) > 256 || len(message) > 8<<10 {
		return nativeSMSSendRequest{}, errors.New("native SMS payload is invalid")
	}
	return request, nil
}

func nativeJSONRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func nativeHeaderPresent(header http.Header, name string) bool {
	return len(header.Values(name)) != 0
}

func readNativeJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, errors.New("native request body is missing")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, nativeGatewayMaxBody))
	if err != nil || len(body) == 0 {
		return nil, errors.New("native request body is invalid")
	}
	return body, nil
}

func writeNativeEnrollmentFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNativeDeviceStoreClosed), errors.Is(err, ErrNativeDeviceStoreDegraded),
		errors.Is(err, ErrSMSStoreCommitUncertain):
		writeNativeError(w, http.StatusServiceUnavailable, "native_store_unavailable")
	default:
		writeNativeError(w, http.StatusForbidden, "enrollment_denied")
	}
}

func writeNativeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": "native request refused", "code": code})
}

func localNativeHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func sortedNativeScopes(scopes []string) []string {
	result := append([]string(nil), scopes...)
	sort.Strings(result)
	return result
}
