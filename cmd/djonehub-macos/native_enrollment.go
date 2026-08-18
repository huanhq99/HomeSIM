package main

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	nativeEnrollmentTicketTTL  = 5 * time.Minute
	nativeEnrollmentPendingTTL = 60 * time.Second
)

var (
	ErrNativeEnrollmentDisabled = errors.New("native enrollment is disabled")
	ErrNativeEnrollmentConsumed = errors.New("native enrollment ticket was already issued or consumed")
	ErrNativeEnrollmentInvalid  = errors.New("native enrollment proof is invalid")
)

type nativeEnrollmentTicketResponse struct {
	Version      int       `json:"version"`
	TicketID     string    `json:"ticket_id"`
	TicketSecret string    `json:"ticket_secret"`
	ExpiresAt    time.Time `json:"expires_at"`
	Host         string    `json:"host"`
	Scopes       []string  `json:"scopes"`
}

func (nativeEnrollmentTicketResponse) String() string {
	return "nativeEnrollmentTicketResponse{redacted}"
}
func (nativeEnrollmentTicketResponse) GoString() string {
	return "nativeEnrollmentTicketResponse{redacted}"
}

type nativeEnrollmentPrepareRequest struct {
	Step          string `json:"step"`
	TicketID      string `json:"ticket_id"`
	TicketSecret  string `json:"ticket_secret"`
	Algorithm     string `json:"algorithm"`
	PublicKeySPKI string `json:"public_key_spki"`
}

func (nativeEnrollmentPrepareRequest) String() string {
	return "nativeEnrollmentPrepareRequest{redacted}"
}
func (nativeEnrollmentPrepareRequest) GoString() string {
	return "nativeEnrollmentPrepareRequest{redacted}"
}

type nativeEnrollmentPrepareResponse struct {
	Version      int       `json:"version"`
	EnrollmentID string    `json:"enrollment_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	SigningInput string    `json:"signing_input"`
}

type nativeEnrollmentCompleteRequest struct {
	Step         string `json:"step"`
	EnrollmentID string `json:"enrollment_id"`
	Signature    string `json:"signature"`
}

type nativeEnrollmentCompleteResponse struct {
	Version   int       `json:"version"`
	DeviceID  string    `json:"device_id"`
	Algorithm string    `json:"algorithm"`
	Scopes    []string  `json:"scopes"`
	Enrolled  time.Time `json:"enrolled_at"`
}

type nativeEnrollmentTicket struct {
	id         string
	secretHash [32]byte
	owner      string
	host       string
	scopes     []string
	expiresAt  time.Time
}

type nativePendingEnrollment struct {
	id           string
	owner        string
	host         string
	scopes       []string
	spki         []byte
	publicKey    *ecdsa.PublicKey
	deviceID     string
	expiresAt    time.Time
	signingInput string
}

type nativeEnrollmentManager struct {
	mu          sync.Mutex
	store       *nativeDeviceStore
	allowIssue  bool
	issued      bool
	ticket      *nativeEnrollmentTicket
	pending     map[string]nativePendingEnrollment
	now         func() time.Time
	randomID    func(string, int) (string, error)
	randomBytes func(int) ([]byte, error)
}

func (*nativeEnrollmentManager) String() string   { return "nativeEnrollmentManager{redacted}" }
func (*nativeEnrollmentManager) GoString() string { return "nativeEnrollmentManager{redacted}" }

func newNativeEnrollmentManager(store *nativeDeviceStore, allowIssue bool) *nativeEnrollmentManager {
	return &nativeEnrollmentManager{
		store:       store,
		allowIssue:  allowIssue,
		pending:     make(map[string]nativePendingEnrollment),
		now:         time.Now,
		randomID:    randomOpaqueID,
		randomBytes: nativeRandomBytes,
	}
}

func (m *nativeEnrollmentManager) Issue(owner, host string, scopes []string) (nativeEnrollmentTicketResponse, error) {
	if m == nil || m.store == nil {
		return nativeEnrollmentTicketResponse{}, ErrNativeEnrollmentDisabled
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativeEnrollmentTicketResponse{}, err
	}
	host = canonicalRemoteHost(host)
	if host == "" || !strings.HasSuffix(host, ".ts.net") {
		return nativeEnrollmentTicketResponse{}, errors.New("native enrollment host is invalid")
	}
	scopes, err = normalizeNativeScopes(scopes)
	if err != nil {
		return nativeEnrollmentTicketResponse{}, err
	}
	ticketID, err := m.randomID("enr_", 16)
	if err != nil {
		return nativeEnrollmentTicketResponse{}, err
	}
	secret, err := m.randomBytes(32)
	if err != nil {
		return nativeEnrollmentTicketResponse{}, err
	}
	secretHash := sha256.Sum256(secret)
	now := m.now().UTC()
	ticket := &nativeEnrollmentTicket{
		id:         ticketID,
		secretHash: secretHash,
		owner:      owner,
		host:       host,
		scopes:     scopes,
		expiresAt:  now.Add(nativeEnrollmentTicketTTL),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.allowIssue {
		return nativeEnrollmentTicketResponse{}, ErrNativeEnrollmentDisabled
	}
	if m.issued {
		return nativeEnrollmentTicketResponse{}, ErrNativeEnrollmentConsumed
	}
	m.issued = true
	m.ticket = ticket
	return nativeEnrollmentTicketResponse{
		Version:      1,
		TicketID:     ticket.id,
		TicketSecret: base64.RawURLEncoding.EncodeToString(secret),
		ExpiresAt:    ticket.expiresAt,
		Host:         ticket.host,
		Scopes:       append([]string(nil), ticket.scopes...),
	}, nil
}

func (m *nativeEnrollmentManager) Prepare(owner, host string, request nativeEnrollmentPrepareRequest) (nativeEnrollmentPrepareResponse, error) {
	if m == nil || m.store == nil {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentDisabled
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	host = canonicalRemoteHost(host)
	if request.Step != "prepare" || !validOpaqueID(request.TicketID, "enr_") || request.Algorithm != "ES256" {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	secret, err := base64.RawURLEncoding.DecodeString(request.TicketSecret)
	if err != nil || len(secret) != 32 {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	spki, err := base64.RawURLEncoding.DecodeString(request.PublicKeySPKI)
	if err != nil {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	publicKey, err := parseNativeP256PublicKey(spki)
	if err != nil {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	canonical, err := x509MarshalNativePublicKey(publicKey)
	if err != nil || !hmac.Equal(canonical, spki) {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	enrollmentID, err := m.randomID("npe_", 16)
	if err != nil {
		return nativeEnrollmentPrepareResponse{}, err
	}
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupExpiredLocked(now)
	ticket := m.ticket
	if ticket == nil || ticket.id != request.TicketID || ticket.owner != owner || ticket.host != host ||
		!now.Before(ticket.expiresAt) || !hmac.Equal(ticket.secretHash[:], sha256Bytes(secret)) {
		return nativeEnrollmentPrepareResponse{}, ErrNativeEnrollmentInvalid
	}
	// Preparing burns the one-use secret even if the response is lost. The
	// operator must restart with a fresh explicit opt-in rather than replay a
	// bearer whose delivery outcome is unknown.
	m.ticket = nil
	pending := nativePendingEnrollment{
		id:        enrollmentID,
		owner:     owner,
		host:      host,
		scopes:    append([]string(nil), ticket.scopes...),
		spki:      append([]byte(nil), spki...),
		publicKey: publicKey,
		deviceID:  nativeDeviceID(spki),
		expiresAt: now.Add(nativeEnrollmentPendingTTL),
	}
	pending.signingInput = nativeEnrollmentSigningInput(m.store, pending)
	m.pending[pending.id] = pending
	return nativeEnrollmentPrepareResponse{
		Version:      1,
		EnrollmentID: pending.id,
		ExpiresAt:    pending.expiresAt,
		SigningInput: pending.signingInput,
	}, nil
}

func (m *nativeEnrollmentManager) Complete(owner, host string, request nativeEnrollmentCompleteRequest) (nativeEnrollmentCompleteResponse, error) {
	if m == nil || m.store == nil {
		return nativeEnrollmentCompleteResponse{}, ErrNativeEnrollmentDisabled
	}
	owner, err := normalizeNativeOwner(owner)
	if err != nil {
		return nativeEnrollmentCompleteResponse{}, ErrNativeEnrollmentInvalid
	}
	host = canonicalRemoteHost(host)
	if request.Step != "complete" || !validOpaqueID(request.EnrollmentID, "npe_") {
		return nativeEnrollmentCompleteResponse{}, ErrNativeEnrollmentInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(request.Signature)
	if err != nil || len(signature) == 0 || len(signature) > 256 {
		return nativeEnrollmentCompleteResponse{}, ErrNativeEnrollmentInvalid
	}
	now := m.now().UTC()
	m.mu.Lock()
	m.cleanupExpiredLocked(now)
	pending, ok := m.pending[request.EnrollmentID]
	if !ok || pending.owner != owner || pending.host != host || !now.Before(pending.expiresAt) {
		m.mu.Unlock()
		return nativeEnrollmentCompleteResponse{}, ErrNativeEnrollmentInvalid
	}
	delete(m.pending, request.EnrollmentID)
	m.mu.Unlock()
	digest := sha256.Sum256([]byte(pending.signingInput))
	if !ecdsa.VerifyASN1(pending.publicKey, digest[:], signature) {
		return nativeEnrollmentCompleteResponse{}, ErrNativeEnrollmentInvalid
	}
	record, _, err := m.store.Enroll(pending.owner, pending.spki, pending.scopes)
	if err != nil {
		return nativeEnrollmentCompleteResponse{}, err
	}
	return nativeEnrollmentCompleteResponse{
		Version:   1,
		DeviceID:  record.DeviceID,
		Algorithm: record.Algorithm,
		Scopes:    append([]string(nil), record.Scopes...),
		Enrolled:  record.EnrolledAt,
	}, nil
}

func (m *nativeEnrollmentManager) cleanupExpiredLocked(now time.Time) {
	if m.ticket != nil && !now.Before(m.ticket.expiresAt) {
		m.ticket = nil
	}
	for id, pending := range m.pending {
		if !now.Before(pending.expiresAt) {
			delete(m.pending, id)
		}
	}
}

func nativeEnrollmentSigningInput(store *nativeDeviceStore, pending nativePendingEnrollment) string {
	return strings.Join([]string{
		"MACCELLULAR-NATIVE-ENROLLMENT-V1",
		"enrollment_id=" + pending.id,
		"device_id=" + pending.deviceID,
		"host=" + pending.host,
		"owner_binding=" + store.ownerMAC(pending.owner),
		"scopes=" + strings.Join(pending.scopes, ","),
		fmt.Sprintf("expires_unix=%d", pending.expiresAt.Unix()),
	}, "\n") + "\n"
}

func nativeRandomBytes(count int) ([]byte, error) {
	if count <= 0 || count > 1024 {
		return nil, errors.New("invalid random byte count")
	}
	value := make([]byte, count)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func x509MarshalNativePublicKey(publicKey *ecdsa.PublicKey) ([]byte, error) {
	return x509.MarshalPKIXPublicKey(publicKey)
}
