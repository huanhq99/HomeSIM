package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	nativeAuthChallengeTTL        = 60 * time.Second
	nativeAuthMaxChallengesDevice = 8
	nativeSMSSyncMaximumLimit     = 10
	nativeEmptyBodySHA256         = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var ErrNativeAuthentication = errors.New("native device authentication failed")

type nativeAuthChallengeRequest struct {
	Version       int    `json:"version"`
	DeviceID      string `json:"device_id"`
	Method        string `json:"method"`
	RequestTarget string `json:"request_target"`
	BodySHA256    string `json:"body_sha256"`
}

type nativeAuthChallengeResponse struct {
	Version      int       `json:"version"`
	ChallengeID  string    `json:"challenge_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	SigningInput string    `json:"signing_input"`
}

type nativeAuthChallenge struct {
	id            string
	deviceID      string
	owner         string
	host          string
	method        string
	requestTarget string
	bodySHA256    string
	action        string
	expiresAt     time.Time
	signingInput  string
}

type nativePrincipal struct {
	DeviceID string
	Owner    string
	Actions  map[string]bool
}

func (p nativePrincipal) allows(action string) bool {
	return p.Actions[action] || p.Actions["*"]
}

type nativeAuthManager struct {
	mu         sync.Mutex
	store      *nativeDeviceStore
	challenges map[string]nativeAuthChallenge
	now        func() time.Time
	randomID   func(string, int) (string, error)
}

func (*nativeAuthManager) String() string   { return "nativeAuthManager{redacted}" }
func (*nativeAuthManager) GoString() string { return "nativeAuthManager{redacted}" }

func newNativeAuthManager(store *nativeDeviceStore) *nativeAuthManager {
	return &nativeAuthManager{
		store:      store,
		challenges: make(map[string]nativeAuthChallenge),
		now:        time.Now,
		randomID:   randomOpaqueID,
	}
}

func (m *nativeAuthManager) Issue(owner, host string, actions map[string]bool, request nativeAuthChallengeRequest) (nativeAuthChallengeResponse, error) {
	if m == nil || m.store == nil {
		return nativeAuthChallengeResponse{}, ErrNativeAuthentication
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativeAuthChallengeResponse{}, ErrNativeAuthentication
	}
	host = canonicalRemoteHost(host)
	method, target, action, err := validateNativeTarget(request.Version, request.Method, request.RequestTarget, request.BodySHA256)
	if err != nil || !nativeActionsAllow(actions, action) {
		return nativeAuthChallengeResponse{}, ErrNativeAuthentication
	}
	record, ok, err := m.store.Lookup(request.DeviceID, owner)
	if err != nil || !ok || !record.allows(action) {
		return nativeAuthChallengeResponse{}, ErrNativeAuthentication
	}
	challengeID, err := m.randomID("nch_", 16)
	if err != nil {
		return nativeAuthChallengeResponse{}, err
	}
	now := m.now().UTC()
	challenge := nativeAuthChallenge{
		id:            challengeID,
		deviceID:      record.DeviceID,
		owner:         owner,
		host:          host,
		method:        method,
		requestTarget: target,
		bodySHA256:    request.BodySHA256,
		action:        action,
		expiresAt:     now.Add(nativeAuthChallengeTTL),
	}
	challenge.signingInput = nativeRequestSigningInput(m.store, challenge)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupExpiredLocked(now)
	count := 0
	for _, existing := range m.challenges {
		if existing.deviceID == record.DeviceID {
			count++
		}
	}
	if count >= nativeAuthMaxChallengesDevice {
		return nativeAuthChallengeResponse{}, errors.New("native authentication challenge limit reached")
	}
	m.challenges[challenge.id] = challenge
	return nativeAuthChallengeResponse{
		Version:      1,
		ChallengeID:  challenge.id,
		ExpiresAt:    challenge.expiresAt,
		SigningInput: challenge.signingInput,
	}, nil
}

func (m *nativeAuthManager) Verify(r *http.Request, owner, host string, actions map[string]bool, body []byte) (nativePrincipal, error) {
	if m == nil || m.store == nil || r == nil || r.URL == nil {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	host = canonicalRemoteHost(host)
	deviceID, ok := nativeSingleHeader(r.Header, "X-MacCellular-Device-ID")
	if !ok || !validNativeDeviceID(deviceID) {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	challengeID, ok := nativeSingleHeader(r.Header, "X-MacCellular-Challenge-ID")
	if !ok || !validOpaqueID(challengeID, "nch_") {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	signatureValue, ok := nativeSingleHeader(r.Header, "X-MacCellular-Signature")
	if !ok {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureValue)
	if err != nil || len(signature) == 0 || len(signature) > 256 {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	// Native signed requests use a fixed-length body. Reject unknown-length or
	// transfer-encoded requests so the bytes verified here are exactly the bytes
	// later consumed by the mutation handler and persistent operation ledger.
	if len(body) > nativeGatewayMaxBody || r.ContentLength != int64(len(body)) || len(r.TransferEncoding) != 0 {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	requestTarget := r.URL.RequestURI()
	bodyDigest := sha256.Sum256(body)
	bodySHA256 := hex.EncodeToString(bodyDigest[:])
	_, canonicalTarget, action, err := validateNativeTarget(1, r.Method, requestTarget, bodySHA256)
	if err != nil || canonicalTarget != requestTarget || !nativeActionsAllow(actions, action) {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	idempotencyKey := ""
	if r.Method == http.MethodPost {
		var ok bool
		idempotencyKey, ok = nativeSingleHeader(r.Header, "Idempotency-Key")
		if !ok || !remoteLedgerKeyPattern.MatchString(idempotencyKey) {
			return nativePrincipal{}, ErrNativeAuthentication
		}
	} else if len(r.Header.Values("Idempotency-Key")) != 0 {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	now := m.now().UTC()
	m.mu.Lock()
	m.cleanupExpiredLocked(now)
	challenge, exists := m.challenges[challengeID]
	if !exists || challenge.deviceID != deviceID || challenge.owner != owner || challenge.host != host {
		m.mu.Unlock()
		return nativePrincipal{}, ErrNativeAuthentication
	}
	// A challenge is burned before signature verification. A malformed proof
	// cannot be used as an online oracle or retried concurrently.
	delete(m.challenges, challengeID)
	m.mu.Unlock()
	if !now.Before(challenge.expiresAt) || challenge.method != r.Method ||
		challenge.requestTarget != requestTarget || challenge.bodySHA256 != bodySHA256 ||
		challenge.action != action {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	record, exists, err := m.store.Lookup(deviceID, owner)
	if err != nil || !exists || !record.allows(action) {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	publicKey, err := record.publicKey()
	if err != nil {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	digest := sha256.Sum256([]byte(challenge.signingInput))
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	intersection := make(map[string]bool)
	for _, scope := range record.Scopes {
		if nativeActionsAllow(actions, scope) {
			intersection[scope] = true
		}
	}
	if !intersection[action] {
		return nativePrincipal{}, ErrNativeAuthentication
	}
	return nativePrincipal{DeviceID: record.DeviceID, Owner: owner, Actions: intersection}, nil
}

func (m *nativeAuthManager) cleanupExpiredLocked(now time.Time) {
	for id, challenge := range m.challenges {
		if !now.Before(challenge.expiresAt) {
			delete(m.challenges, id)
		}
	}
}

func validateNativeTarget(version int, method, requestTarget, bodySHA256 string) (string, string, string, error) {
	if version != 1 || (method != http.MethodGet && method != http.MethodPost) ||
		len(requestTarget) == 0 || len(requestTarget) > 1024 || !validLowerHexSHA256(bodySHA256) {
		return "", "", "", ErrNativeAuthentication
	}
	parsed, err := url.ParseRequestURI(requestTarget)
	if err != nil || parsed.IsAbs() || parsed.Fragment != "" || parsed.RawPath != "" || parsed.RequestURI() != requestTarget {
		return "", "", "", ErrNativeAuthentication
	}
	switch parsed.Path {
	case "/api/native/v1/session":
		if method != http.MethodGet || bodySHA256 != nativeEmptyBodySHA256 || parsed.RawQuery != "" {
			return "", "", "", ErrNativeAuthentication
		}
		return method, requestTarget, "status.read", nil
	case "/api/native/v1/sms/sync":
		if method != http.MethodGet || bodySHA256 != nativeEmptyBodySHA256 {
			return "", "", "", ErrNativeAuthentication
		}
		dummy := &http.Request{URL: parsed}
		_, limit, _, err := parseRemoteSMSSyncQuery(dummy)
		if err != nil || limit < 1 || limit > nativeSMSSyncMaximumLimit {
			return "", "", "", ErrNativeAuthentication
		}
		return method, requestTarget, "sms.read", nil
	case "/api/native/v1/sms/send":
		if method != http.MethodPost || bodySHA256 == nativeEmptyBodySHA256 || parsed.RawQuery != "" {
			return "", "", "", ErrNativeAuthentication
		}
		return method, requestTarget, "sms.send", nil
	case "/api/native/v1/sms/operations":
		if method != http.MethodGet || bodySHA256 != nativeEmptyBodySHA256 {
			return "", "", "", ErrNativeAuthentication
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil || len(query) != 1 || len(query["operation_id"]) != 1 {
			return "", "", "", ErrNativeAuthentication
		}
		operationID := query.Get("operation_id")
		if !validOpaqueID(operationID, "nsm_") ||
			requestTarget != "/api/native/v1/sms/operations?operation_id="+operationID {
			return "", "", "", ErrNativeAuthentication
		}
		return method, requestTarget, "sms.send", nil
	default:
		return "", "", "", ErrNativeAuthentication
	}
}

func nativeRequestSigningInput(store *nativeDeviceStore, challenge nativeAuthChallenge) string {
	return strings.Join([]string{
		"MACCELLULAR-NATIVE-REQUEST-V1",
		"challenge_id=" + challenge.id,
		"device_id=" + challenge.deviceID,
		"method=" + challenge.method,
		"request_target=" + challenge.requestTarget,
		"body_sha256=" + challenge.bodySHA256,
		"host=" + challenge.host,
		"owner_binding=" + store.ownerMAC(challenge.owner),
		"action=" + challenge.action,
		fmt.Sprintf("expires_unix=%d", challenge.expiresAt.Unix()),
	}, "\n") + "\n"
}

func nativeSingleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	return value, value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\r\n")
}

func nativeActionsAllow(actions map[string]bool, action string) bool {
	if action == "sms.send" {
		// A paid/irreversible native mutation must be named explicitly in the
		// current App Cap. A broad wildcard is sufficient for legacy reads, but
		// must not silently activate SMS sending after an enrollment upgrade.
		return actions[action]
	}
	return action != "" && (actions[action] || actions["*"])
}

func validLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sortedNativeActions(actions map[string]bool) []string {
	result := make([]string, 0, len(actions))
	for action, allowed := range actions {
		if allowed {
			result = append(result, action)
		}
	}
	sort.Strings(result)
	return result
}
