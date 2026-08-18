package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/iniwex5/vohive/internal/sipgateway"
)

const (
	publicWebPushAPIVersion                 = 1
	publicWebPushMaximumEndpointBytes       = 2048
	publicWebPushMaximumStoreBytes    int64 = 256 << 10
	publicWebPushMaximumPerPrincipal        = 4
	publicWebPushMaximumTotal               = 32
	publicWebPushQueueDepth                 = 16
	publicWebPushWorkerCount                = 2
	publicWebPushDeliveryTimeout            = 8 * time.Second
	publicWebPushRetryMaximumAttempts       = 3
	publicWebPushRetryFirstDelay            = 250 * time.Millisecond
	publicWebPushDialAttemptTimeout         = 2 * time.Second
	publicWebPushMaximumDialAddresses       = 8
	publicWebPushTTL                        = 45
	publicWebPushOpaqueIDBytes              = 24
	publicWebPushMaximumDedupeEntries       = 4096
	publicWebPushDNSAttemptTimeout          = 2 * time.Second
	publicWebPushConfigPath                 = "/api/remote/v1/push/config"
	publicWebPushSubscriptionsPath          = "/api/remote/v1/push/subscriptions"
	publicWebPushEndpointHashHeader         = "X-MacCellular-Push-Endpoint-SHA256"
)

var publicWebPushDNSResolvers = [...]string{
	"119.29.29.29:53",
	"223.5.5.5:53",
	"1.1.1.1:53",
}

var (
	publicWebPushOpaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)
	publicWebPushHashPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	publicWebPushDNSLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	publicWebPushExpiration      = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,15})$`)

	errPublicWebPushInvalid       = errors.New("public web push request is invalid")
	errPublicWebPushUnavailable   = errors.New("public web push is unavailable")
	errPublicWebPushLimit         = errors.New("public web push subscription limit reached")
	errPublicWebPushConflict      = errors.New("public web push subscription conflicts with another principal")
	errPublicWebPushDelivery      = errors.New("public web push delivery failed")
	errPublicWebPushUnsafeAddress = errors.New("public web push address is not public")
)

var publicWebIncomingCallPayload = []byte(`{"version":1,"type":"incoming_call"}`)

// URL-safe base64 without padding for "incoming-call". Apple validates that
// Topic is decodable base64url rather than merely checking its character set.
const publicWebIncomingCallTopic = "aW5jb21pbmctY2FsbA"

type publicWebPushFlagConfig struct {
	Enabled           bool
	PrivateKeyFile    string
	Subject           string
	SubscriptionsFile string
}

type publicWebPushStartupConfig struct {
	PrivateKeyFile    string
	Subject           string
	SubscriptionsFile string
	AllowedPrincipals []string
	Sender            publicWebPushSender
	Now               func() time.Time
	Random            io.Reader
}

func (publicWebPushStartupConfig) String() string {
	return "publicWebPushStartupConfig{redacted}"
}

func (publicWebPushStartupConfig) GoString() string {
	return "publicWebPushStartupConfig{redacted}"
}

type publicWebPushSubscriptionRequest struct {
	Endpoint       string
	ExpirationTime *int64
	P256DH         string
	Auth           string
}

func (publicWebPushSubscriptionRequest) String() string {
	return "publicWebPushSubscriptionRequest{redacted}"
}

func (publicWebPushSubscriptionRequest) GoString() string {
	return "publicWebPushSubscriptionRequest{redacted}"
}

type publicWebPushDiskSubscription struct {
	ID             string `json:"id"`
	PrincipalHash  string `json:"principal_hash"`
	EndpointHash   string `json:"endpoint_hash"`
	Endpoint       string `json:"endpoint"`
	ExpirationTime *int64 `json:"expiration_time"`
	P256DH         string `json:"p256dh"`
	Auth           string `json:"auth"`
	CreatedAtMS    int64  `json:"created_at_ms"`
	UpdatedAtMS    int64  `json:"updated_at_ms"`
}

func (publicWebPushDiskSubscription) String() string {
	return "publicWebPushDiskSubscription{redacted}"
}

func (publicWebPushDiskSubscription) GoString() string {
	return "publicWebPushDiskSubscription{redacted}"
}

type publicWebPushDiskStore struct {
	Version       int                             `json:"version"`
	Subscriptions []publicWebPushDiskSubscription `json:"subscriptions"`
}

type publicWebPushSender interface {
	Send(context.Context, publicWebPushDiskSubscription, []byte) (int, error)
}

type publicWebPushDeliveryClient struct {
	privateKey string
	publicKey  string
	subject    string
	httpClient webpush.HTTPClient
}

type publicWebPushProviderError struct {
	reason string
}

func (e *publicWebPushProviderError) Error() string {
	return "public web push provider rejected the request"
}

func publicWebPushProviderFailureReason(err error) string {
	var providerError *publicWebPushProviderError
	if !errors.As(err, &providerError) || providerError == nil {
		return ""
	}
	return providerError.reason
}

func (*publicWebPushDeliveryClient) String() string {
	return "publicWebPushDeliveryClient{redacted}"
}

func (*publicWebPushDeliveryClient) GoString() string {
	return "publicWebPushDeliveryClient{redacted}"
}

func (c *publicWebPushDeliveryClient) Send(
	ctx context.Context,
	subscription publicWebPushDiskSubscription,
	payload []byte,
) (int, error) {
	if c == nil || c.httpClient == nil || ctx == nil {
		return 0, errPublicWebPushDelivery
	}
	response, err := webpush.SendNotificationWithContext(ctx, append([]byte(nil), payload...), &webpush.Subscription{
		Endpoint: subscription.Endpoint,
		Keys:     webpush.Keys{P256dh: subscription.P256DH, Auth: subscription.Auth},
	}, &webpush.Options{
		HTTPClient: c.httpClient, Subscriber: c.subject, Topic: publicWebIncomingCallTopic,
		TTL: publicWebPushTTL, Urgency: webpush.UrgencyHigh,
		VAPIDPublicKey: c.publicKey, VAPIDPrivateKey: c.privateKey,
	})
	if err != nil || response == nil || response.Body == nil {
		return 0, errPublicWebPushDelivery
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if readErr != nil {
		return response.StatusCode, errPublicWebPushDelivery
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, nil
	}
	var providerResponse struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &providerResponse) == nil &&
		regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`).MatchString(providerResponse.Reason) {
		return response.StatusCode, &publicWebPushProviderError{reason: providerResponse.Reason}
	}
	return response.StatusCode, errPublicWebPushDelivery
}

type publicWebPushEvent struct {
	dedupeKey string
}

type publicWebPushManager struct {
	mu sync.Mutex

	path       string
	root       string
	lockFile   *os.File
	publicKey  string
	sender     publicWebPushSender
	now        func() time.Time
	random     io.Reader
	allowed    map[string]struct{}
	subs       []publicWebPushDiskSubscription
	dedupe     map[string]struct{}
	dedupeFIFO []string
	queue      chan publicWebPushEvent
	ctx        context.Context
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	retryWait  func(context.Context, time.Duration) bool
	pending    int
	idle       chan struct{}
	closed     bool
}

func (*publicWebPushManager) String() string   { return "publicWebPushManager{redacted}" }
func (*publicWebPushManager) GoString() string { return "publicWebPushManager{redacted}" }

func parsePublicWebPushStartupConfig(
	flags publicWebPushFlagConfig,
	allowedPrincipals []string,
) (*publicWebPushStartupConfig, error) {
	configured := strings.TrimSpace(flags.PrivateKeyFile) != "" ||
		strings.TrimSpace(flags.Subject) != "" || strings.TrimSpace(flags.SubscriptionsFile) != ""
	if !flags.Enabled {
		if configured {
			return nil, errors.New("public web push files require -public-web-push")
		}
		return nil, nil
	}
	privatePath := strings.TrimSpace(flags.PrivateKeyFile)
	storePath := strings.TrimSpace(flags.SubscriptionsFile)
	subject := strings.TrimSpace(flags.Subject)
	if privatePath != flags.PrivateKeyFile || storePath != flags.SubscriptionsFile ||
		subject != flags.Subject || !cleanAbsolutePath(privatePath) || !cleanAbsolutePath(storePath) ||
		privatePath == storePath || len(allowedPrincipals) == 0 {
		return nil, errors.New("public web push configuration is incomplete")
	}
	if _, err := validatePublicWebPushSubject(subject); err != nil {
		return nil, err
	}
	return &publicWebPushStartupConfig{
		PrivateKeyFile: privatePath, Subject: subject, SubscriptionsFile: storePath,
		AllowedPrincipals: append([]string(nil), allowedPrincipals...),
	}, nil
}

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validatePublicWebPushSubject(subject string) (string, error) {
	if len(subject) == 0 || len(subject) > 256 || subject != strings.TrimSpace(subject) ||
		strings.ContainsAny(subject, "\x00\r\n") {
		return "", errors.New("public web push VAPID subject is invalid")
	}
	parsed, err := url.Parse(subject)
	if err != nil || parsed == nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.User != nil {
		return "", errors.New("public web push VAPID subject is invalid")
	}
	switch parsed.Scheme {
	case "mailto":
		address := strings.TrimPrefix(subject, "mailto:")
		if parsed.Opaque == "" || parsed.Opaque != address || !validPublicWebPushEmail(address) {
			return "", errors.New("public web push VAPID subject is invalid")
		}
		// webpush-go expects a bare address and adds the mailto: scheme itself.
		return address, nil
	case "https":
		if parsed.Opaque != "" || parsed.Host == "" ||
			!validPublicWebPushPublicHost(parsed.Hostname()) || parsed.Port() != "" && parsed.Port() != "443" {
			return "", errors.New("public web push VAPID subject is invalid")
		}
		return subject, nil
	default:
		return "", errors.New("public web push VAPID subject is invalid")
	}
}

func validPublicWebPushEmail(address string) bool {
	if len(address) < 3 || len(address) > 254 || strings.Count(address, "@") != 1 ||
		strings.ContainsAny(address, " <>\t") {
		return false
	}
	parts := strings.SplitN(strings.ToLower(address), "@", 2)
	return parts[0] != "" && validPublicWebPushPublicHost(parts[1])
}

func readPublicWebPushVAPIDPrivateKey(path string) (privateKey, publicKey string, err error) {
	data, err := readExternalVoicePasswordFile(path, 128)
	if err != nil {
		return "", "", err
	}
	defer func() {
		for index := range data {
			data[index] = 0
		}
	}()
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	privateKey = string(data)
	if len(privateKey) != 43 || !publicWebPushHashPattern.MatchString(privateKey) {
		return "", "", errors.New("VAPID private key must be one exact base64url line")
	}
	raw, err := base64.RawURLEncoding.DecodeString(privateKey)
	if err != nil || len(raw) != 32 {
		return "", "", errors.New("VAPID private key is invalid")
	}
	defer func() {
		for index := range raw {
			raw[index] = 0
		}
	}()
	key, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		return "", "", errors.New("VAPID private key is invalid")
	}
	publicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	if len(publicKey) != 87 {
		return "", "", errors.New("VAPID public key derivation failed")
	}
	return privateKey, publicKey, nil
}

func newPublicWebPushManager(config *publicWebPushStartupConfig) (*publicWebPushManager, error) {
	if config == nil || !cleanAbsolutePath(config.PrivateKeyFile) ||
		!cleanAbsolutePath(config.SubscriptionsFile) || len(config.AllowedPrincipals) == 0 {
		return nil, errPublicWebPushUnavailable
	}
	privateKey, publicKey, err := readPublicWebPushVAPIDPrivateKey(config.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("public web push VAPID key: %w", err)
	}
	subscriber, err := validatePublicWebPushSubject(config.Subject)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(config.SubscriptionsFile)
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, fmt.Errorf("public web push store directory: %w", err)
	}
	lockFile, err := openSMSStoreLock(filepath.Join(root, "subscriptions.lock"))
	if err != nil {
		return nil, fmt.Errorf("public web push store lock: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	sender := config.Sender
	if sender == nil {
		sender = &publicWebPushDeliveryClient{
			privateKey: privateKey, publicKey: publicKey, subject: subscriber,
			httpClient: newPublicWebPushHTTPClient(),
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	manager := &publicWebPushManager{
		path: config.SubscriptionsFile, root: root, lockFile: lockFile,
		publicKey: publicKey, sender: sender, now: now, random: randomSource,
		allowed: make(map[string]struct{}, len(config.AllowedPrincipals)),
		dedupe:  make(map[string]struct{}), queue: make(chan publicWebPushEvent, publicWebPushQueueDepth),
		ctx: ctx, cancel: cancel, idle: idle, retryWait: waitPublicWebPushRetry,
	}
	for _, principal := range config.AllowedPrincipals {
		if normalized := normalizePublicWebPushPrincipal(principal); normalized != "" {
			manager.allowed[publicWebPushPrincipalHash(normalized)] = struct{}{}
		}
	}
	if len(manager.allowed) == 0 {
		_ = lockFile.Close()
		cancel()
		return nil, errPublicWebPushUnavailable
	}
	if err := manager.load(); err != nil {
		_ = lockFile.Close()
		cancel()
		return nil, err
	}
	for index := 0; index < publicWebPushWorkerCount; index++ {
		manager.workers.Add(1)
		go manager.worker()
	}
	return manager, nil
}

func (m *publicWebPushManager) load() error {
	data, err := readPrivateRegular(m.path, publicWebPushMaximumStoreBytes)
	if errors.Is(err, os.ErrNotExist) {
		m.mu.Lock()
		err = m.persistLocked(nil)
		m.mu.Unlock()
		return err
	}
	if err != nil {
		return fmt.Errorf("public web push store read: %w", err)
	}
	var disk publicWebPushDiskStore
	if err := decodePublicWebPushJSON(data, &disk); err != nil || disk.Version != publicWebPushAPIVersion ||
		len(disk.Subscriptions) > publicWebPushMaximumTotal {
		return errors.New("public web push store is invalid")
	}
	seenIDs := make(map[string]struct{}, len(disk.Subscriptions))
	seenEndpoints := make(map[string]string, len(disk.Subscriptions))
	perPrincipal := make(map[string]int)
	filtered := make([]publicWebPushDiskSubscription, 0, len(disk.Subscriptions))
	changed := false
	for _, subscription := range disk.Subscriptions {
		if err := validatePublicWebPushDiskSubscription(subscription, m.now()); err != nil {
			return errors.New("public web push store is invalid")
		}
		if _, duplicate := seenIDs[subscription.ID]; duplicate {
			return errors.New("public web push store is invalid")
		}
		if _, duplicate := seenEndpoints[subscription.EndpointHash]; duplicate {
			return errors.New("public web push store is invalid")
		}
		seenIDs[subscription.ID] = struct{}{}
		seenEndpoints[subscription.EndpointHash] = subscription.PrincipalHash
		perPrincipal[subscription.PrincipalHash]++
		if perPrincipal[subscription.PrincipalHash] > publicWebPushMaximumPerPrincipal {
			return errors.New("public web push store is invalid")
		}
		if _, allowed := m.allowed[subscription.PrincipalHash]; !allowed ||
			subscription.ExpirationTime != nil && *subscription.ExpirationTime <= m.now().UnixMilli() {
			changed = true
			continue
		}
		filtered = append(filtered, subscription)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if changed {
		if err := m.persistLocked(filtered); err != nil {
			return err
		}
	}
	m.subs = clonePublicWebPushSubscriptions(filtered)
	return nil
}

func decodePublicWebPushJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errPublicWebPushInvalid
	}
	return nil
}

func validatePublicWebPushDiskSubscription(subscription publicWebPushDiskSubscription, now time.Time) error {
	if !publicWebPushOpaqueIDPattern.MatchString(subscription.ID) ||
		!publicWebPushHashPattern.MatchString(subscription.PrincipalHash) ||
		!publicWebPushHashPattern.MatchString(subscription.EndpointHash) ||
		subscription.EndpointHash != publicWebPushEndpointHash(subscription.Endpoint) ||
		subscription.CreatedAtMS <= 0 || subscription.UpdatedAtMS < subscription.CreatedAtMS ||
		subscription.UpdatedAtMS > now.Add(5*time.Minute).UnixMilli() {
		return errPublicWebPushInvalid
	}
	request := publicWebPushSubscriptionRequest{
		Endpoint: subscription.Endpoint, ExpirationTime: subscription.ExpirationTime,
		P256DH: subscription.P256DH, Auth: subscription.Auth,
	}
	return validatePublicWebPushSubscription(request, now, true)
}

func (m *publicWebPushManager) persistLocked(subscriptions []publicWebPushDiskSubscription) error {
	copy := clonePublicWebPushSubscriptions(subscriptions)
	sort.Slice(copy, func(i, j int) bool {
		if copy[i].PrincipalHash != copy[j].PrincipalHash {
			return copy[i].PrincipalHash < copy[j].PrincipalHash
		}
		return copy[i].ID < copy[j].ID
	})
	data, err := json.Marshal(publicWebPushDiskStore{Version: publicWebPushAPIVersion, Subscriptions: copy})
	if err != nil {
		return errPublicWebPushUnavailable
	}
	data = append(data, '\n')
	if int64(len(data)) > publicWebPushMaximumStoreBytes {
		return errPublicWebPushUnavailable
	}
	if err := atomicPrivateWrite(m.root, m.path, data, syncSMSStoreDirectory); err != nil {
		return fmt.Errorf("public web push store persist: %w", err)
	}
	return nil
}

func (m *publicWebPushManager) PublicKey() string {
	if m == nil {
		return ""
	}
	return m.publicKey
}

func (m *publicWebPushManager) Lookup(identity, endpointHash string) (string, bool) {
	principalHash := publicWebPushPrincipalHash(normalizePublicWebPushPrincipal(identity))
	if principalHash == "" || !publicWebPushHashPattern.MatchString(endpointHash) {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", false
	}
	if _, allowed := m.allowed[principalHash]; !allowed {
		return "", false
	}
	for _, subscription := range m.subs {
		if subscription.PrincipalHash == principalHash && subscription.EndpointHash == endpointHash {
			return subscription.ID, true
		}
	}
	return "", false
}

func (m *publicWebPushManager) Upsert(
	identity string,
	request publicWebPushSubscriptionRequest,
) (string, error) {
	normalized := normalizePublicWebPushPrincipal(identity)
	principalHash := publicWebPushPrincipalHash(normalized)
	if principalHash == "" || validatePublicWebPushSubscription(request, m.now(), false) != nil {
		return "", errPublicWebPushInvalid
	}
	endpointHash := publicWebPushEndpointHash(request.Endpoint)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errPublicWebPushUnavailable
	}
	if _, allowed := m.allowed[principalHash]; !allowed {
		return "", errPublicWebPushUnavailable
	}
	next := clonePublicWebPushSubscriptions(m.subs)
	principalCount := 0
	for index := range next {
		subscription := &next[index]
		if subscription.PrincipalHash == principalHash {
			principalCount++
		}
		if subscription.EndpointHash != endpointHash {
			continue
		}
		if subscription.PrincipalHash != principalHash {
			return "", errPublicWebPushConflict
		}
		subscription.Endpoint = request.Endpoint
		subscription.ExpirationTime = clonePublicWebPushExpiration(request.ExpirationTime)
		subscription.P256DH = request.P256DH
		subscription.Auth = request.Auth
		subscription.UpdatedAtMS = m.now().UnixMilli()
		if err := m.persistLocked(next); err != nil {
			return "", errPublicWebPushUnavailable
		}
		m.subs = next
		return subscription.ID, nil
	}
	if principalCount >= publicWebPushMaximumPerPrincipal || len(next) >= publicWebPushMaximumTotal {
		return "", errPublicWebPushLimit
	}
	id, err := m.newOpaqueIDLocked()
	if err != nil {
		return "", errPublicWebPushUnavailable
	}
	nowMS := m.now().UnixMilli()
	next = append(next, publicWebPushDiskSubscription{
		ID: id, PrincipalHash: principalHash, EndpointHash: endpointHash,
		Endpoint: request.Endpoint, ExpirationTime: clonePublicWebPushExpiration(request.ExpirationTime),
		P256DH: request.P256DH, Auth: request.Auth, CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	})
	if err := m.persistLocked(next); err != nil {
		return "", errPublicWebPushUnavailable
	}
	m.subs = next
	return id, nil
}

func (m *publicWebPushManager) Delete(identity, id string) error {
	principalHash := publicWebPushPrincipalHash(normalizePublicWebPushPrincipal(identity))
	if principalHash == "" || !publicWebPushOpaqueIDPattern.MatchString(id) {
		return errPublicWebPushInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errPublicWebPushUnavailable
	}
	if _, allowed := m.allowed[principalHash]; !allowed {
		return errPublicWebPushUnavailable
	}
	next := make([]publicWebPushDiskSubscription, 0, len(m.subs))
	found := false
	for _, subscription := range m.subs {
		if subscription.ID == id && subscription.PrincipalHash == principalHash {
			found = true
			continue
		}
		next = append(next, subscription)
	}
	if !found {
		return nil
	}
	if err := m.persistLocked(next); err != nil {
		return errPublicWebPushUnavailable
	}
	m.subs = next
	return nil
}

func (m *publicWebPushManager) newOpaqueIDLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		buffer := make([]byte, publicWebPushOpaqueIDBytes)
		if _, err := io.ReadFull(m.random, buffer); err != nil {
			return "", err
		}
		id := base64.RawURLEncoding.EncodeToString(buffer)
		collision := false
		for _, subscription := range m.subs {
			if subscription.ID == id {
				collision = true
				break
			}
		}
		if !collision {
			return id, nil
		}
	}
	return "", errPublicWebPushUnavailable
}

func (m *publicWebPushManager) enqueueIncoming(dedupeKey string) bool {
	if m == nil || dedupeKey == "" || len(dedupeKey) > 256 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	if _, duplicate := m.dedupe[dedupeKey]; duplicate {
		return false
	}
	m.dedupe[dedupeKey] = struct{}{}
	m.dedupeFIFO = append(m.dedupeFIFO, dedupeKey)
	select {
	case m.queue <- publicWebPushEvent{dedupeKey: dedupeKey}:
		if len(m.dedupeFIFO) > publicWebPushMaximumDedupeEntries {
			oldest := m.dedupeFIFO[0]
			m.dedupeFIFO = m.dedupeFIFO[1:]
			delete(m.dedupe, oldest)
		}
		if m.pending == 0 {
			m.idle = make(chan struct{})
		}
		m.pending++
		return true
	default:
		delete(m.dedupe, dedupeKey)
		m.dedupeFIFO = m.dedupeFIFO[:len(m.dedupeFIFO)-1]
		log.Printf("public web push queue full: notification_dropped=true")
		return false
	}
}

func (m *publicWebPushManager) worker() {
	defer m.workers.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case event := <-m.queue:
			m.deliver(event)
			m.finishPending()
		}
	}
}

func (m *publicWebPushManager) deliver(_ publicWebPushEvent) {
	m.mu.Lock()
	nowMS := m.now().UnixMilli()
	subscriptions := make([]publicWebPushDiskSubscription, 0, len(m.subs))
	expired := make(map[string]struct{})
	for _, subscription := range m.subs {
		if _, allowed := m.allowed[subscription.PrincipalHash]; !allowed {
			expired[subscription.ID] = struct{}{}
			continue
		}
		if subscription.ExpirationTime != nil && *subscription.ExpirationTime <= nowMS {
			expired[subscription.ID] = struct{}{}
			continue
		}
		subscriptions = append(subscriptions, subscription)
	}
	m.mu.Unlock()

	delivered, failed := 0, 0
	remaining := subscriptions
	for attempt := 1; attempt <= publicWebPushRetryMaximumAttempts && len(remaining) > 0; attempt++ {
		results := m.sendPublicWebPushBatch(remaining)
		next := make([]publicWebPushDiskSubscription, 0, len(results))
		for _, result := range results {
			provider := publicWebPushProvider(result.subscription.Endpoint)
			reason := publicWebPushProviderFailureReason(result.err)
			if reason == "" {
				reason = "none"
			}
			switch {
			case result.err == nil && result.status >= 200 && result.status < 300:
				delivered++
				log.Printf("public web push endpoint result: provider=%s status=%d outcome=delivered", provider, result.status)
			case result.status == http.StatusNotFound || result.status == http.StatusGone:
				expired[result.subscription.ID] = struct{}{}
				log.Printf("public web push endpoint result: provider=%s status=%d outcome=revoked", provider, result.status)
			case publicWebPushDeliveryRetryable(result.status, result.err) &&
				attempt < publicWebPushRetryMaximumAttempts:
				next = append(next, result.subscription)
				log.Printf("public web push endpoint result: provider=%s status=%d outcome=retrying", provider, result.status)
			default:
				failed++
				log.Printf("public web push endpoint result: provider=%s status=%d reason=%s outcome=failed",
					provider, result.status, reason)
			}
		}
		if len(next) == 0 {
			break
		}
		delay := publicWebPushRetryFirstDelay << (attempt - 1)
		if m.retryWait == nil || !m.retryWait(m.ctx, delay) {
			failed += len(next)
			break
		}
		remaining = next
	}
	if len(expired) > 0 {
		m.removeRevoked(expired)
	}
	log.Printf("public web push delivery complete: delivered=%d revoked=%d failed=%d",
		delivered, len(expired), failed)
}

type publicWebPushDeliveryResult struct {
	subscription publicWebPushDiskSubscription
	status       int
	err          error
}

func (m *publicWebPushManager) sendPublicWebPushBatch(
	subscriptions []publicWebPushDiskSubscription,
) []publicWebPushDeliveryResult {
	results := make(chan publicWebPushDeliveryResult, len(subscriptions))
	var sends sync.WaitGroup
	for _, subscription := range subscriptions {
		subscription := subscription
		sends.Add(1)
		go func() {
			defer sends.Done()
			ctx, cancel := context.WithTimeout(m.ctx, publicWebPushDeliveryTimeout)
			defer cancel()
			status, err := m.sender.Send(ctx, subscription, append([]byte(nil), publicWebIncomingCallPayload...))
			results <- publicWebPushDeliveryResult{subscription: subscription, status: status, err: err}
		}()
	}
	sends.Wait()
	close(results)
	collected := make([]publicWebPushDeliveryResult, 0, len(subscriptions))
	for result := range results {
		collected = append(collected, result)
	}
	return collected
}

func publicWebPushProvider(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "unknown"
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "web.push.apple.com":
		return "apple"
	case host == "fcm.googleapis.com":
		return "fcm"
	default:
		return "other"
	}
}

func publicWebPushDeliveryRetryable(status int, err error) bool {
	if err != nil || status == 0 || status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

func waitPublicWebPushRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *publicWebPushManager) removeRevoked(ids map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	next := make([]publicWebPushDiskSubscription, 0, len(m.subs))
	for _, subscription := range m.subs {
		if _, remove := ids[subscription.ID]; remove {
			continue
		}
		next = append(next, subscription)
	}
	if len(next) == len(m.subs) {
		return
	}
	if err := m.persistLocked(next); err != nil {
		log.Printf("public web push revoked-subscription cleanup failed: persistence_unavailable=true")
		return
	}
	m.subs = next
}

func (m *publicWebPushManager) finishPending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending > 0 {
		m.pending--
	}
	if m.pending == 0 {
		select {
		case <-m.idle:
		default:
			close(m.idle)
		}
	}
}

func (m *publicWebPushManager) idleChannel() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.idle
}

func (m *publicWebPushManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	lockFile := m.lockFile
	m.lockFile = nil
	m.cancel()
	m.mu.Unlock()
	m.workers.Wait()
	if lockFile != nil {
		return lockFile.Close()
	}
	return nil
}

func validatePublicWebPushSubscription(
	request publicWebPushSubscriptionRequest,
	now time.Time,
	allowExpired bool,
) error {
	if err := validatePublicWebPushEndpoint(request.Endpoint); err != nil {
		return errPublicWebPushInvalid
	}
	p256dh, err := base64.RawURLEncoding.DecodeString(request.P256DH)
	if err != nil || len(p256dh) != 65 || len(request.P256DH) != 87 {
		return errPublicWebPushInvalid
	}
	if _, err := ecdh.P256().NewPublicKey(p256dh); err != nil {
		return errPublicWebPushInvalid
	}
	auth, err := base64.RawURLEncoding.DecodeString(request.Auth)
	if err != nil || len(auth) != 16 || len(request.Auth) != 22 {
		return errPublicWebPushInvalid
	}
	if request.ExpirationTime != nil && (*request.ExpirationTime <= 0 ||
		!allowExpired && *request.ExpirationTime <= now.UnixMilli()) {
		return errPublicWebPushInvalid
	}
	return nil
}

func validatePublicWebPushEndpoint(endpoint string) error {
	if len(endpoint) == 0 || len(endpoint) > publicWebPushMaximumEndpointBytes ||
		endpoint != strings.TrimSpace(endpoint) || strings.ContainsAny(endpoint, "\x00\r\n") {
		return errPublicWebPushInvalid
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || parsed.Hostname() == "" ||
		parsed.Port() != "" && parsed.Port() != "443" || !validPublicWebPushPublicHost(parsed.Hostname()) {
		return errPublicWebPushInvalid
	}
	return nil
}

func validPublicWebPushPublicHost(host string) bool {
	host = strings.ToLower(host)
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "%\x00\r\n") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return publicWebPushIPIsPublic(ip)
	}
	if strings.HasSuffix(host, ".") {
		host = strings.TrimSuffix(host, ".")
	}
	if !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{
		".localhost", ".local", ".internal", ".lan", ".home", ".arpa",
		".example", ".invalid", ".test",
	} {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if !publicWebPushDNSLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func publicWebPushIPIsPublic(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsUnspecified() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsMulticast() {
		return false
	}
	for _, prefix := range publicWebPushReservedPrefixes() {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func publicWebPushReservedPrefixes() []netip.Prefix {
	// net.IP.IsPrivate deliberately excludes several non-public ranges. Push
	// endpoints must also reject CGNAT/Tailscale, benchmark, documentation and
	// future/reserved addresses before any dial is attempted.
	values := []string{
		"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"2001:db8::/32", "2001:10::/28", "3fff::/20",
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func normalizePublicWebPushPrincipal(identity string) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if identity == "" || len(identity) > 320 || strings.ContainsAny(identity, "\x00\r\n") {
		return ""
	}
	return identity
}

func publicWebPushPrincipalHash(identity string) string {
	if identity == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(identity))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func publicWebPushEndpointHash(endpoint string) string {
	digest := sha256.Sum256([]byte(endpoint))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func clonePublicWebPushExpiration(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func clonePublicWebPushSubscriptions(values []publicWebPushDiskSubscription) []publicWebPushDiskSubscription {
	copy := append([]publicWebPushDiskSubscription(nil), values...)
	for index := range copy {
		copy[index].ExpirationTime = clonePublicWebPushExpiration(copy[index].ExpirationTime)
	}
	return copy
}

func newPublicWebPushHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.DialContext = publicWebPushDialContext
	return &http.Client{
		Transport: transport, Timeout: publicWebPushDeliveryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func publicWebPushDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !validPublicWebPushDialPort(port) {
		return nil, errPublicWebPushUnsafeAddress
	}
	resolver := newPublicWebPushResolver()
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, errPublicWebPushDelivery
	}
	for _, resolved := range addresses {
		if !publicWebPushIPIsPublic(resolved.IP) {
			return nil, errPublicWebPushUnsafeAddress
		}
	}
	dialer := net.Dialer{KeepAlive: 30 * time.Second}
	return dialPublicWebPushAddresses(ctx, network, port, addresses, dialer.DialContext)
}

func newPublicWebPushResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo:     true,
		StrictErrors: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if network != "udp" && network != "udp4" && network != "tcp" && network != "tcp4" {
				return nil, errPublicWebPushDelivery
			}
			var lastErr error
			for _, resolverAddress := range publicWebPushDNSResolvers {
				attemptCtx, cancel := context.WithTimeout(ctx, publicWebPushDNSAttemptTimeout)
				connection, err := (&net.Dialer{}).DialContext(attemptCtx, network, resolverAddress)
				cancel()
				if err == nil && connection != nil {
					return connection, nil
				}
				if connection != nil {
					_ = connection.Close()
				}
				lastErr = err
				if ctx.Err() != nil {
					break
				}
			}
			if lastErr == nil {
				lastErr = errPublicWebPushDelivery
			}
			return nil, lastErr
		},
	}
}

type publicWebPushDialFunc func(context.Context, string, string) (net.Conn, error)

func dialPublicWebPushAddresses(
	ctx context.Context,
	network string,
	port string,
	addresses []net.IPAddr,
	dial publicWebPushDialFunc,
) (net.Conn, error) {
	if ctx == nil || dial == nil || len(addresses) == 0 {
		return nil, errPublicWebPushDelivery
	}
	for _, candidate := range publicWebPushDialOrder(addresses) {
		attemptCtx, cancel := context.WithTimeout(ctx, publicWebPushDialAttemptTimeout)
		connection, err := dial(attemptCtx, network, net.JoinHostPort(candidate.IP.String(), port))
		cancel()
		if err == nil && connection != nil {
			return connection, nil
		}
		if connection != nil {
			_ = connection.Close()
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errPublicWebPushDelivery
}

func publicWebPushDialOrder(addresses []net.IPAddr) []net.IPAddr {
	limit := min(len(addresses), publicWebPushMaximumDialAddresses)
	ordered := make([]net.IPAddr, 0, limit)
	ordered = append(ordered, addresses[0])
	firstIsV4 := addresses[0].IP.To4() != nil
	opposite := -1
	for index := 1; limit > 1 && index < len(addresses); index++ {
		if (addresses[index].IP.To4() != nil) != firstIsV4 {
			opposite = index
			ordered = append(ordered, addresses[index])
			break
		}
	}
	for index := 1; index < len(addresses) && len(ordered) < limit; index++ {
		if index != opposite {
			ordered = append(ordered, addresses[index])
		}
	}
	return ordered
}

func validPublicWebPushDialPort(port string) bool {
	value, err := strconv.Atoi(port)
	return err == nil && value == 443
}

func (a *app) publicWebPushEnabled() bool {
	return a.publicWebPushConfigured() && a.publicWebVoiceEnabled()
}

func (a *app) publicWebPushConfigured() bool {
	return a != nil && a.publicWeb != nil && a.publicWeb.Push != nil && a.publicWebPush != nil
}

func (a *app) notifyPublicWebQDCIncoming(generation uint64) {
	if generation == 0 || !a.publicWebPushEnabled() || !a.publicWeb.DirectVoice {
		return
	}
	a.publicWebPush.enqueueIncoming("qdc:" + strconv.FormatUint(generation, 10))
}

func (a *app) notifyPublicWebExternalIncoming(call sipgateway.PublicCallRef) {
	// This callback is invoked by the exact external runtime that observed the
	// incoming generation. Do not require a.sipVoice to have been published yet:
	// its supervisor can receive the first event just before startExternalVoice
	// stores that pointer on app.
	if call.Generation == 0 || call.PublicCallID == "" || a == nil || a.publicWeb == nil ||
		a.publicWebPush == nil || a.publicWeb.Push == nil || !a.publicWeb.Control ||
		!a.publicWeb.ExternalVoice || a.publicWeb.TURNIssuer == nil {
		return
	}
	a.publicWebPush.enqueueIncoming("sip:" + call.PublicCallID + ":" + strconv.FormatUint(call.Generation, 10))
}

func (a *app) publicWebPushConfig(w http.ResponseWriter, r *http.Request) {
	if !a.publicWebPushEnabled() {
		http.NotFound(w, r)
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	response := map[string]any{
		"version": publicWebPushAPIVersion, "enabled": true,
		"vapid_public_key": a.publicWebPush.PublicKey(),
	}
	values := r.Header.Values(publicWebPushEndpointHashHeader)
	if len(values) > 1 || len(values) == 1 &&
		(values[0] != strings.TrimSpace(values[0]) || !publicWebPushHashPattern.MatchString(values[0])) {
		writeError(w, http.StatusBadRequest, "invalid push endpoint hash")
		return
	}
	if len(values) == 1 {
		if id, ok := a.publicWebPush.Lookup(authorization.Identity, values[0]); ok {
			response["subscription_id"] = id
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) publicWebPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if !a.publicWebPushEnabled() {
		http.NotFound(w, r)
		return
	}
	request, ok := decodePublicWebPushSubscribeRequest(w, r)
	if !ok {
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	id, err := a.publicWebPush.Upsert(authorization.Identity, request)
	if err != nil {
		switch {
		case errors.Is(err, errPublicWebPushLimit), errors.Is(err, errPublicWebPushConflict):
			writeError(w, http.StatusConflict, "push subscription unavailable")
		case errors.Is(err, errPublicWebPushInvalid):
			writeError(w, http.StatusBadRequest, "invalid push subscription")
		default:
			writeError(w, http.StatusServiceUnavailable, "push subscription store unavailable")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": publicWebPushAPIVersion, "subscription_id": id})
}

func decodePublicWebPushSubscribeRequest(w http.ResponseWriter, r *http.Request) (publicWebPushSubscriptionRequest, bool) {
	type keys struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	}
	type subscription struct {
		Endpoint       string          `json:"endpoint"`
		ExpirationTime json.RawMessage `json:"expirationTime"`
		Keys           keys            `json:"keys"`
	}
	var body struct {
		Version      int          `json:"version"`
		Subscription subscription `json:"subscription"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push subscription")
		return publicWebPushSubscriptionRequest{}, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF || body.Version != publicWebPushAPIVersion ||
		len(body.Subscription.ExpirationTime) == 0 {
		writeError(w, http.StatusBadRequest, "invalid push subscription")
		return publicWebPushSubscriptionRequest{}, false
	}
	var expiration *int64
	if string(body.Subscription.ExpirationTime) != "null" {
		raw := string(body.Subscription.ExpirationTime)
		if !publicWebPushExpiration.MatchString(raw) {
			writeError(w, http.StatusBadRequest, "invalid push subscription")
			return publicWebPushSubscriptionRequest{}, false
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid push subscription")
			return publicWebPushSubscriptionRequest{}, false
		}
		expiration = &value
	}
	request := publicWebPushSubscriptionRequest{
		Endpoint: body.Subscription.Endpoint, ExpirationTime: expiration,
		P256DH: body.Subscription.Keys.P256DH, Auth: body.Subscription.Keys.Auth,
	}
	if validatePublicWebPushSubscription(request, time.Now(), false) != nil {
		writeError(w, http.StatusBadRequest, "invalid push subscription")
		return publicWebPushSubscriptionRequest{}, false
	}
	return request, true
}

func (a *app) publicWebPushDelete(w http.ResponseWriter, r *http.Request) {
	if !a.publicWebPushEnabled() {
		http.NotFound(w, r)
		return
	}
	if !decodePublicWebPushEmptyRequest(w, r) {
		return
	}
	id := r.PathValue("opaqueID")
	if !publicWebPushOpaqueIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	authorization := remoteAuthorizationFromRequest(r)
	if err := a.publicWebPush.Delete(authorization.Identity, id); err != nil {
		if errors.Is(err, errPublicWebPushInvalid) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "push subscription store unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodePublicWebPushEmptyRequest(w http.ResponseWriter, r *http.Request) bool {
	var body struct{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push subscription deletion")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid push subscription deletion")
		return false
	}
	return true
}

func isPublicWebPushPath(path string) bool {
	if path == publicWebPushConfigPath || path == publicWebPushSubscriptionsPath {
		return true
	}
	prefix := publicWebPushSubscriptionsPath + "/"
	return strings.HasPrefix(path, prefix) &&
		publicWebPushOpaqueIDPattern.MatchString(strings.TrimPrefix(path, prefix))
}

func isPublicWebPushMutation(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	if r.Method == http.MethodPost && r.URL.Path == publicWebPushSubscriptionsPath {
		return true
	}
	return r.Method == http.MethodDelete && isPublicWebPushPath(r.URL.Path) &&
		r.URL.Path != publicWebPushConfigPath && r.URL.Path != publicWebPushSubscriptionsPath
}
