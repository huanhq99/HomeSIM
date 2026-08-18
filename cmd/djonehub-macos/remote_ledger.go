package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

// The operation journal is deliberately small and append-only. Reaching a
// bound is an operator-visible failure instead of an excuse to forget an old
// idempotency key and accidentally repeat a modem mutation.
const (
	remoteLedgerVersion    = 1
	remoteLedgerMaxBytes   = 8 << 20
	remoteLedgerMaxLine    = 64 << 10
	remoteLedgerMaxRecords = 32768
	remoteLedgerPayloadMax = 16 << 10
)

var (
	errRemoteLedgerCorrupt      = errors.New("remote operation ledger is corrupt")
	errRemoteLedgerFull         = errors.New("remote operation ledger is full")
	errRemoteUnknownOutcome     = errors.New("remote operation outcome is unknown")
	errRemoteAuditUnavailable   = errors.New("remote operation audit is unavailable")
	errRemoteJournalUnavailable = errors.New("remote operation journal is unavailable")

	remoteLedgerKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	remoteLedgerCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
)

type remoteOperationDisposition uint8

const (
	remoteOperationExecute remoteOperationDisposition = iota + 1
	remoteOperationWait
	remoteOperationReplay
	remoteOperationConflict
	remoteOperationUnknownOutcome
)

// remoteOperationResponse retains only a status/code by default. The sole
// bounded payload exception is the opaque public external-SIP dial snapshot:
// the browser needs the newly-created call identity in the first response and
// after an idempotent replay. Payload validation below rejects every other
// route, and never accepts SDP, phone text, provider handles or credentials.
type remoteOperationResponse struct {
	Status  int
	Code    string
	Payload string
}

var remoteLedgerSafeCodes = map[string]struct{}{
	"accepted":                    {},
	"completed":                   {},
	"completed_audit_unavailable": {},
	"conflict":                    {},
	"invalid_request":             {},
	"operation_failed":            {},
	"modem_unavailable":           {},
	"service_unavailable":         {},
	"unknown_outcome":             {},
}

func (r remoteOperationResponse) validate() error {
	if r.Status < 200 || r.Status > 599 {
		return fmt.Errorf("invalid replay status %d", r.Status)
	}
	if !remoteLedgerCodePattern.MatchString(r.Code) {
		return errors.New("invalid replay code")
	}
	if _, ok := remoteLedgerSafeCodes[r.Code]; !ok {
		return errors.New("replay code is not allowlisted")
	}
	if len(r.Payload) > remoteLedgerPayloadMax {
		return errors.New("replay payload is too large")
	}
	if r.Payload != "" && (r.Status < 200 || r.Status >= 300) {
		return errors.New("replay payload requires a successful response")
	}
	return nil
}

func (r remoteOperationResponse) JSON() []byte {
	if r.Payload != "" {
		payload := []byte(r.Payload)
		if len(payload) == 0 || payload[len(payload)-1] != '\n' {
			payload = append(payload, '\n')
		}
		return payload
	}
	ok := r.Status >= 200 && r.Status < 300
	payload, _ := json.Marshal(struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}{OK: ok, Code: r.Code})
	return append(payload, '\n')
}

// writeRemoteOperationResponse is the only intended HTTP serialization path
// for journaled results. It never replays captured headers or a captured body.
func writeRemoteOperationResponse(w http.ResponseWriter, response remoteOperationResponse) error {
	if err := response.validate(); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.Status)
	_, err := w.Write(response.JSON())
	return err
}

type remoteOperationBeginResult struct {
	Disposition remoteOperationDisposition
	Operation   *remoteOperation
	Response    remoteOperationResponse
}

type remoteOperationLookupResult struct {
	State    string
	Response remoteOperationResponse
}

// remoteOperationCompletionResult separates the authoritative operation
// journal from its secondary audit trail. Once Committed is true, the modem
// result is durably replayable and callers must never report unknown outcome
// or invite a retry, even if AuditDurable is false.
type remoteOperationCompletionResult struct {
	Committed    bool
	AuditDurable bool
	Response     remoteOperationResponse
}

type remoteOperation struct {
	ledger *remoteOperationLedger
	entry  *remoteOperationLedgerEntry
}

type remoteOperationLedgerEntry struct {
	loginHash   string
	keyHash     string
	path        string
	requestHash string
	state       string
	response    remoteOperationResponse
	done        chan struct{}
	doneOnce    sync.Once
}

func (e *remoteOperationLedgerEntry) finish(state string) {
	e.state = state
	e.doneOnce.Do(func() { close(e.done) })
}

type remoteOperationLedger struct {
	mu             sync.Mutex
	operations     *os.File
	audit          *os.File
	entries        map[string]*remoteOperationLedgerEntry
	records        int
	closed         bool
	journalFailure error
	auditFailure   error
	syncOperations func(*os.File) error
	syncAudit      func(*os.File) error
	now            func() time.Time
}

type remoteLedgerRecord struct {
	Version     int    `json:"version"`
	Phase       string `json:"phase"`
	Time        string `json:"time"`
	LoginHash   string `json:"login_hash"`
	Path        string `json:"path"`
	KeyHash     string `json:"key_hash"`
	RequestHash string `json:"request_hash"`
	Status      int    `json:"status,omitempty"`
	Code        string `json:"code,omitempty"`
	Payload     string `json:"payload,omitempty"`
}

type remoteAuditRecord struct {
	Time        string `json:"time"`
	LoginHash   string `json:"login_hash"`
	Path        string `json:"path"`
	Status      string `json:"status"`
	KeyHash     string `json:"key_hash"`
	RequestHash string `json:"request_hash"`
}

// openRemoteOperationLedger opens path as the operation JSONL and
// path+".audit.jsonl" as its redacted audit trail. The parent must be a
// dedicated private directory; it is created and forced to 0700. Both files
// are forced to 0600. Any malformed or partial record makes opening fail.
func openRemoteOperationLedger(path string) (*remoteOperationLedger, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return nil, errors.New("remote operation ledger path is required")
	}
	dir := filepath.Dir(path)
	if err := secureRemoteLedgerDir(dir); err != nil {
		return nil, err
	}
	operations, err := openRemoteLedgerFile(path)
	if err != nil {
		return nil, err
	}
	auditPath := path + ".audit.jsonl"
	audit, err := openRemoteLedgerFile(auditPath)
	if err != nil {
		_ = operations.Close()
		return nil, err
	}
	ledger := &remoteOperationLedger{
		operations:     operations,
		audit:          audit,
		entries:        make(map[string]*remoteOperationLedgerEntry),
		syncOperations: func(file *os.File) error { return file.Sync() },
		syncAudit:      func(file *os.File) error { return file.Sync() },
		now:            time.Now,
	}
	if err := ledger.loadOperations(); err != nil {
		_ = ledger.Close()
		return nil, err
	}
	if err := validateRemoteAuditFile(audit, ledger.entries); err != nil {
		// The operation journal is authoritative. Keep it available for safe
		// replay even if the secondary audit is corrupt, but fail closed for
		// every new idempotency key until an operator repairs the audit.
		ledger.auditFailure = err
	} else if err := audit.Sync(); err != nil {
		// A prior process may have observed an audit fsync failure even if its
		// page-cache write is readable now. Establish durability at startup
		// before treating the audit as recovered and accepting new keys.
		ledger.auditFailure = fmt.Errorf("persist validated remote audit: %w", err)
	}
	return ledger, nil
}

func secureRemoteLedgerDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("remote ledger parent must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect remote ledger directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create remote ledger directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure remote ledger directory: %w", err)
	}
	return nil
}

func openRemoteLedgerFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("remote ledger %q must be a regular file", filepath.Base(path))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect remote ledger file: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open remote ledger file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure remote ledger file: %w", err)
	}
	return f, nil
}

func (l *remoteOperationLedger) loadOperations() error {
	records := 0
	err := scanRemoteJSONL(l.operations, func(line []byte) error {
		var record remoteLedgerRecord
		if err := decodeStrictRemoteJSON(line, &record); err != nil {
			return err
		}
		if err := validateRemoteLedgerRecord(record); err != nil {
			return err
		}
		id := record.LoginHash + "\x00" + record.KeyHash
		existing := l.entries[id]
		switch record.Phase {
		case "started":
			if existing != nil {
				return errors.New("duplicate operation start")
			}
			l.entries[id] = &remoteOperationLedgerEntry{
				loginHash: record.LoginHash, keyHash: record.KeyHash, path: record.Path,
				requestHash: record.RequestHash, state: "unknown_outcome", done: closedRemoteLedgerChannel(),
			}
		case "completed":
			if existing == nil || existing.state != "unknown_outcome" || existing.path != record.Path ||
				existing.requestHash != record.RequestHash {
				return errors.New("completion does not match one start")
			}
			response := remoteOperationResponse{Status: record.Status, Code: record.Code, Payload: record.Payload}
			if err := validateRemoteOperationResponse(record.Path, response); err != nil {
				return err
			}
			existing.state = "completed"
			existing.response = response
		default:
			return errors.New("invalid operation phase")
		}
		records++
		if records > remoteLedgerMaxRecords {
			return errRemoteLedgerFull
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errRemoteLedgerCorrupt, err)
	}
	l.records = records
	return nil
}

func validateRemoteAuditFile(file *os.File, entries map[string]*remoteOperationLedgerEntry) error {
	type auditCoverage struct {
		started   bool
		completed bool
	}
	coverage := make(map[string]auditCoverage)
	err := scanRemoteJSONL(file, func(line []byte) error {
		var record remoteAuditRecord
		if err := decodeStrictRemoteJSON(line, &record); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339Nano, record.Time); err != nil {
			return errors.New("invalid audit time")
		}
		if !validRemoteLedgerHash(record.LoginHash) || !validRemoteLedgerHash(record.KeyHash) ||
			!validRemoteLedgerHash(record.RequestHash) || !validRemoteLedgerPath(record.Path) ||
			!validRemoteAuditStatus(record.Status) {
			return errors.New("invalid audit record")
		}
		entry := entries[record.LoginHash+"\x00"+record.KeyHash]
		matchesEntry := entry != nil && entry.path == record.Path && entry.requestHash == record.RequestHash
		switch record.Status {
		case "started":
			if !matchesEntry {
				return errors.New("audit start has no authoritative operation")
			}
			item := coverage[remoteAuditCoverageKey(entry)]
			item.started = true
			coverage[remoteAuditCoverageKey(entry)] = item
		case "completed":
			if !matchesEntry || entry.state != "completed" {
				return errors.New("audit completion precedes authoritative completion")
			}
			item := coverage[remoteAuditCoverageKey(entry)]
			item.completed = true
			coverage[remoteAuditCoverageKey(entry)] = item
		case "replayed":
			if !matchesEntry || entry.state != "completed" {
				return errors.New("audit replay has no authoritative completion")
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: audit: %v", errRemoteLedgerCorrupt, err)
	}
	for _, entry := range entries {
		item := coverage[remoteAuditCoverageKey(entry)]
		if !item.started {
			return fmt.Errorf("%w: audit: missing operation start", errRemoteLedgerCorrupt)
		}
		if entry.state == "completed" && !item.completed {
			return fmt.Errorf("%w: audit: missing authoritative completion", errRemoteLedgerCorrupt)
		}
	}
	return nil
}

func remoteAuditCoverageKey(entry *remoteOperationLedgerEntry) string {
	if entry == nil {
		return ""
	}
	return entry.loginHash + "\x00" + entry.keyHash + "\x00" + entry.path + "\x00" + entry.requestHash
}

func scanRemoteJSONL(file *os.File, visit func([]byte) error) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > remoteLedgerMaxBytes {
		return errRemoteLedgerFull
	}
	if info.Size() > 0 {
		var tail [1]byte
		if _, err := file.ReadAt(tail[:], info.Size()-1); err != nil {
			return err
		}
		if tail[0] != '\n' {
			return errors.New("partial JSONL tail")
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), remoteLedgerMaxLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			return errors.New("blank JSONL record")
		}
		if err := visit(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err = file.Seek(0, io.SeekEnd)
	return err
}

func decodeStrictRemoteJSON(line []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func validateRemoteLedgerRecord(record remoteLedgerRecord) error {
	if record.Version != remoteLedgerVersion {
		return errors.New("unsupported ledger version")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.Time); err != nil {
		return errors.New("invalid ledger time")
	}
	if !validRemoteLedgerHash(record.LoginHash) || !validRemoteLedgerHash(record.KeyHash) ||
		!validRemoteLedgerHash(record.RequestHash) || !validRemoteLedgerPath(record.Path) {
		return errors.New("invalid ledger metadata")
	}
	switch record.Phase {
	case "started":
		if record.Status != 0 || record.Code != "" {
			return errors.New("start record contains a response")
		}
	case "completed":
		if err := validateRemoteOperationResponse(record.Path, remoteOperationResponse{
			Status: record.Status, Code: record.Code, Payload: record.Payload,
		}); err != nil {
			return err
		}
	default:
		return errors.New("invalid ledger phase")
	}
	return nil
}

func validateRemoteOperationResponse(path string, response remoteOperationResponse) error {
	if err := response.validate(); err != nil {
		return err
	}
	if response.Payload == "" {
		return nil
	}
	if path != remoteVoiceV2DialPath || response.Code != "completed" {
		return errors.New("replay payload is not allowed for this operation")
	}
	var payload externalVoiceCallResponse
	if err := decodeStrictRemoteJSON([]byte(response.Payload), &payload); err != nil {
		return errors.New("replay payload is not canonical external dial JSON")
	}
	if payload.Version != externalVoiceAPISchemaVersion || payload.Call.Revision == 0 ||
		payload.Call.Call.Generation == 0 || payload.Call.Direction != sipgateway.CallDirectionOutgoing ||
		payload.Call.Phase != sipgateway.PhaseIncomingRinging || payload.Call.Media != nil ||
		!validRemotePublicCallID(payload.Call.Call.PublicCallID) {
		return errors.New("replay payload is not a valid external dial snapshot")
	}
	return nil
}

func validRemotePublicCallID(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validRemoteLedgerHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func validRemoteLedgerPath(path string) bool {
	// Keep request data out of the audit path by accepting only fixed routes;
	// phone numbers and call identifiers belong in request bodies.
	if policy, ok := remoteVoiceV2Policy(path); ok {
		return policy.persistent
	}
	switch path {
	case "/api/remote/v1/sms/send",
		"/api/remote/v1/sms/refresh",
		"/api/remote/v1/calls/dial",
		"/api/remote/v1/calls/answer",
		"/api/remote/v1/calls/reject",
		"/api/remote/v1/calls/hangup",
		"/api/remote/v1/calls/dtmf",
		"/api/native/v1/sms/send":
		return true
	default:
		return false
	}
}

func validRemoteAuditStatus(status string) bool {
	switch status {
	case "started", "completed", "replayed", "conflict", "unknown_outcome":
		return true
	default:
		return false
	}
}

func closedRemoteLedgerChannel() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

// requestHash must cover the HTTP method, fixed path and complete request body.
// Begin persists and fsyncs a started record before returning Execute. A
// matching concurrent duplicate returns Wait; a completed duplicate returns
// Replay. An entry left started by a prior process returns UnknownOutcome and
// is never executed again automatically.
func (l *remoteOperationLedger) Begin(login, path, key string, requestHash [sha256.Size]byte) (remoteOperationBeginResult, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	key = strings.TrimSpace(key)
	if login == "" {
		return remoteOperationBeginResult{}, errors.New("remote login is required")
	}
	if !remoteLedgerKeyPattern.MatchString(key) {
		return remoteOperationBeginResult{}, errors.New("invalid idempotency key")
	}
	if !validRemoteLedgerPath(path) {
		return remoteOperationBeginResult{}, errors.New("invalid remote operation path")
	}
	loginHash := remoteLedgerHash(login)
	keyHash := remoteLedgerHash(key)
	requestHashHex := hex.EncodeToString(requestHash[:])
	id := loginHash + "\x00" + keyHash

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return remoteOperationBeginResult{}, os.ErrClosed
	}
	if existing := l.entries[id]; existing != nil {
		operation := &remoteOperation{ledger: l, entry: existing}
		if existing.path != path || existing.requestHash != requestHashHex {
			attempt := &remoteOperationLedgerEntry{
				loginHash: loginHash, keyHash: keyHash, path: path, requestHash: requestHashHex,
			}
			l.appendAuditBestEffortLocked(attempt, "conflict")
			return remoteOperationBeginResult{Disposition: remoteOperationConflict}, nil
		}
		switch existing.state {
		case "started":
			return remoteOperationBeginResult{Disposition: remoteOperationWait, Operation: operation}, nil
		case "completed":
			// Replay is determined solely by the authoritative operation
			// journal. A broken audit must never turn a durable completion into
			// an unknown result or a newly executable mutation.
			l.appendAuditBestEffortLocked(existing, "replayed")
			return remoteOperationBeginResult{Disposition: remoteOperationReplay, Response: existing.response}, nil
		default:
			l.appendAuditBestEffortLocked(existing, "unknown_outcome")
			return remoteOperationBeginResult{Disposition: remoteOperationUnknownOutcome}, nil
		}
	}
	if l.journalFailure != nil {
		return remoteOperationBeginResult{}, fmt.Errorf("%w: %v", errRemoteJournalUnavailable, l.journalFailure)
	}
	if l.auditFailure != nil {
		return remoteOperationBeginResult{}, fmt.Errorf("%w: %v", errRemoteAuditUnavailable, l.auditFailure)
	}
	// Reserve the second record now. Starting an operation which the record
	// bound alone would prevent us from completing would manufacture an
	// avoidable unknown outcome.
	if l.records+2 > remoteLedgerMaxRecords {
		return remoteOperationBeginResult{}, errRemoteLedgerFull
	}
	entry := &remoteOperationLedgerEntry{
		loginHash: loginHash, keyHash: keyHash, path: path, requestHash: requestHashHex,
		state: "started", done: make(chan struct{}),
	}
	record := remoteLedgerRecord{
		Version: remoteLedgerVersion, Phase: "started", Time: l.now().UTC().Format(time.RFC3339Nano),
		LoginHash: loginHash, Path: path, KeyHash: keyHash, RequestHash: requestHashHex,
	}
	if err := appendRemoteJSONL(l.operations, record, l.syncOperations); err != nil {
		// A failed fsync may still have written bytes. Reserve the key as
		// unknown in memory so an immediate retry cannot execute the modem
		// mutation under the same key.
		entry.finish("unknown_outcome")
		l.entries[id] = entry
		l.journalFailure = err
		l.appendAuditBestEffortLocked(entry, "unknown_outcome")
		return remoteOperationBeginResult{}, fmt.Errorf("persist remote operation start: %w", err)
	}
	l.records++
	l.entries[id] = entry
	if err := l.appendAuditLocked(entry, "started"); err != nil {
		l.auditFailure = err
		entry.finish("unknown_outcome")
		return remoteOperationBeginResult{}, err
	}
	return remoteOperationBeginResult{
		Disposition: remoteOperationExecute,
		Operation:   &remoteOperation{ledger: l, entry: entry},
	}, nil
}

// Lookup reports one principal/key's durable state without changing the
// journal or inviting execution. Native clients use it after response loss so
// they never need to re-submit an SMS mutation merely to learn its outcome.
func (l *remoteOperationLedger) Lookup(login, path, key string) (remoteOperationLookupResult, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	key = strings.TrimSpace(key)
	if login == "" || !remoteLedgerKeyPattern.MatchString(key) || !validRemoteLedgerPath(path) {
		return remoteOperationLookupResult{}, errors.New("invalid remote operation lookup")
	}
	id := remoteLedgerHash(login) + "\x00" + remoteLedgerHash(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return remoteOperationLookupResult{}, os.ErrClosed
	}
	entry := l.entries[id]
	if entry == nil {
		return remoteOperationLookupResult{State: "not_found"}, nil
	}
	if entry.path != path {
		return remoteOperationLookupResult{State: "conflict"}, nil
	}
	switch entry.state {
	case "started":
		return remoteOperationLookupResult{State: "in_flight"}, nil
	case "completed":
		return remoteOperationLookupResult{State: "completed", Response: entry.response}, nil
	default:
		return remoteOperationLookupResult{State: "unknown_outcome"}, nil
	}
}

// Complete first fsyncs the authoritative operation completion and only then
// appends a completed audit record. An operation-journal failure is unknown.
// An audit failure after the authoritative fsync is not unknown: the response
// remains durably replayable, while all new keys fail closed.
func (l *remoteOperationLedger) Complete(operation *remoteOperation, response remoteOperationResponse) (remoteOperationCompletionResult, error) {
	if operation == nil || operation.ledger != l || operation.entry == nil {
		return remoteOperationCompletionResult{}, errors.New("operation does not belong to this ledger")
	}
	if err := validateRemoteOperationResponse(operation.entry.path, response); err != nil {
		return remoteOperationCompletionResult{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return remoteOperationCompletionResult{}, os.ErrClosed
	}
	entry := operation.entry
	if entry.state != "started" {
		return remoteOperationCompletionResult{}, errors.New("remote operation is not in flight")
	}
	if l.journalFailure != nil {
		entry.finish("unknown_outcome")
		l.appendAuditBestEffortLocked(entry, "unknown_outcome")
		return remoteOperationCompletionResult{}, fmt.Errorf("%w: %v", errRemoteJournalUnavailable, l.journalFailure)
	}
	if l.records >= remoteLedgerMaxRecords {
		entry.finish("unknown_outcome")
		l.journalFailure = errRemoteLedgerFull
		l.appendAuditBestEffortLocked(entry, "unknown_outcome")
		return remoteOperationCompletionResult{}, errRemoteLedgerFull
	}
	record := remoteLedgerRecord{
		Version: remoteLedgerVersion, Phase: "completed", Time: l.now().UTC().Format(time.RFC3339Nano),
		LoginHash: entry.loginHash, Path: entry.path, KeyHash: entry.keyHash, RequestHash: entry.requestHash,
		Status: response.Status, Code: response.Code, Payload: response.Payload,
	}
	if err := appendRemoteJSONL(l.operations, record, l.syncOperations); err != nil {
		l.journalFailure = err
		entry.finish("unknown_outcome")
		l.appendAuditBestEffortLocked(entry, "unknown_outcome")
		return remoteOperationCompletionResult{}, fmt.Errorf("persist remote operation completion: %w", err)
	}
	l.records++
	entry.response = response
	entry.finish("completed")
	result := remoteOperationCompletionResult{Committed: true, Response: response}
	if err := l.appendAuditLocked(entry, "completed"); err != nil {
		l.auditFailure = err
		return result, errors.Join(errRemoteAuditUnavailable, err)
	}
	result.AuditDurable = true
	return result, nil
}

func (l *remoteOperationLedger) Wait(ctx context.Context, operation *remoteOperation) (remoteOperationResponse, error) {
	if operation == nil || operation.ledger != l || operation.entry == nil {
		return remoteOperationResponse{}, errors.New("operation does not belong to this ledger")
	}
	select {
	case <-operation.entry.done:
		l.mu.Lock()
		defer l.mu.Unlock()
		if operation.entry.state != "completed" {
			return remoteOperationResponse{}, errRemoteUnknownOutcome
		}
		return operation.entry.response, nil
	case <-ctx.Done():
		return remoteOperationResponse{}, ctx.Err()
	}
}

func (l *remoteOperationLedger) appendAuditLocked(entry *remoteOperationLedgerEntry, status string) error {
	if l.auditFailure != nil {
		return fmt.Errorf("%w: %v", errRemoteAuditUnavailable, l.auditFailure)
	}
	if !validRemoteAuditStatus(status) {
		return errors.New("invalid remote audit status")
	}
	record := remoteAuditRecord{
		Time: l.now().UTC().Format(time.RFC3339Nano), LoginHash: entry.loginHash, Path: entry.path,
		Status: status, KeyHash: entry.keyHash, RequestHash: entry.requestHash,
	}
	if err := appendRemoteJSONL(l.audit, record, l.syncAudit); err != nil {
		return fmt.Errorf("persist remote audit: %w", err)
	}
	return nil
}

func (l *remoteOperationLedger) appendAuditBestEffortLocked(entry *remoteOperationLedgerEntry, status string) {
	if l.auditFailure != nil {
		return
	}
	if err := l.appendAuditLocked(entry, status); err != nil {
		l.auditFailure = err
	}
}

func appendRemoteJSONL(file *os.File, value any, syncFile func(*os.File) error) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload)+1 > remoteLedgerMaxLine {
		return errors.New("remote ledger record is too large")
	}
	payload = append(payload, '\n')
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(payload)) > remoteLedgerMaxBytes {
		return errRemoteLedgerFull
	}
	if written, err := file.Write(payload); err != nil {
		return err
	} else if written != len(payload) {
		return io.ErrShortWrite
	}
	if syncFile == nil {
		return errors.New("remote ledger sync is unavailable")
	}
	return syncFile(file)
}

func remoteLedgerHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func (l *remoteOperationLedger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var result error
	if l.operations != nil {
		result = errors.Join(result, l.operations.Close())
	}
	if l.audit != nil {
		result = errors.Join(result, l.audit.Close())
	}
	for _, entry := range l.entries {
		if entry.state == "started" {
			entry.finish("unknown_outcome")
		}
	}
	return result
}
