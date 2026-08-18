// Package turnauth issues bounded, time-limited coturn REST credentials.
//
// The shared secret is never returned to callers. Wire encoding is explicit so
// ordinary JSON/log formatting cannot accidentally disclose a live credential.
package turnauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // TURN REST authentication requires HMAC-SHA1.
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	credentialRandomBytes = 16
	minimumSecretBytes    = 32
	maximumSecretBytes    = 4096
	minimumTTL            = time.Minute
	maximumTTL            = 10 * time.Minute
)

var (
	ErrInvalidConfig = errors.New("turnauth: invalid configuration")
	ErrClosed        = errors.New("turnauth: issuer is closed")
)

// Config is deliberately small: callers configure one exact DNS name and the
// public listener ports that clients are allowed to use.
type Config struct {
	Host    string
	UDPPort uint16
	TLSPort uint16
	TTL     time.Duration
	Secret  []byte
	Now     func() time.Time
	Random  io.Reader
}

func (Config) String() string   { return "turnauth.Config{redacted}" }
func (Config) GoString() string { return "turnauth.Config{redacted}" }
func (Config) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

// Issuer owns a private copy of the TURN REST shared secret.
type Issuer struct {
	mu      sync.RWMutex
	host    string
	udpPort uint16
	tlsPort uint16
	ttl     time.Duration
	secret  []byte
	now     func() time.Time
	random  io.Reader
	closed  bool
}

// Credentials intentionally has no exported fields. Use EncodeJSON only at the
// authenticated response boundary.
type Credentials struct {
	urls       []string
	username   string
	credential string
	expiresAt  time.Time
}

// ServerRelayCredential is the process-local view used by the server-side
// WebRTC peer. The browser receives the full transport list through
// Credentials.EncodeJSON, while the server uses the direct UDP endpoint plus
// the TLS-over-TCP fallback. The plaintext TCP URL remains browser-only.
type ServerRelayCredential struct {
	URLs      []string  `json:"-"`
	Username  string    `json:"-"`
	Password  string    `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

func (ServerRelayCredential) String() string   { return "turnauth.ServerRelayCredential{redacted}" }
func (ServerRelayCredential) GoString() string { return "turnauth.ServerRelayCredential{redacted}" }
func (ServerRelayCredential) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

type wireCredentials struct {
	Version    int      `json:"version"`
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	ExpiresAt  string   `json:"expires_at"`
}

func New(config Config) (*Issuer, error) {
	host := canonicalDNSName(config.Host)
	if host == "" || config.UDPPort == 0 || config.TLSPort == 0 ||
		config.UDPPort == config.TLSPort || config.TTL < minimumTTL || config.TTL > maximumTTL ||
		len(config.Secret) < minimumSecretBytes || len(config.Secret) > maximumSecretBytes {
		return nil, ErrInvalidConfig
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	return &Issuer{
		host: host, udpPort: config.UDPPort, tlsPort: config.TLSPort,
		ttl: config.TTL, secret: append([]byte(nil), config.Secret...), now: now, random: random,
	}, nil
}

func canonicalDNSName(value string) string {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if value == "" || len(value) > 253 || net.ParseIP(value) != nil || strings.ContainsAny(value, ":/[]@?#") {
		return ""
	}
	labels := strings.Split(value, ".")
	if len(labels) < 2 {
		return ""
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return ""
			}
		}
	}
	return value
}

// Issue creates one opaque, time-limited credential. It contains no device,
// phone, login, call, or gateway identifier.
func (issuer *Issuer) Issue() (Credentials, error) {
	if issuer == nil {
		return Credentials{}, ErrClosed
	}
	// Serialize access to the injected entropy source as well as Close. The
	// production crypto/rand.Reader is concurrency-safe, but the contract must
	// not silently require every test or platform reader to be so.
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	if issuer.closed || len(issuer.secret) == 0 {
		return Credentials{}, ErrClosed
	}
	now := issuer.now().UTC()
	if now.IsZero() {
		return Credentials{}, ErrInvalidConfig
	}
	expiresAt := now.Add(issuer.ttl)
	if !expiresAt.After(now) || expiresAt.Unix() <= 0 {
		return Credentials{}, ErrInvalidConfig
	}
	randomBytes := make([]byte, credentialRandomBytes)
	if _, err := io.ReadFull(issuer.random, randomBytes); err != nil {
		return Credentials{}, errors.New("turnauth: credential entropy is unavailable")
	}
	username := strconv.FormatInt(expiresAt.Unix(), 10) + ":" + base64.RawURLEncoding.EncodeToString(randomBytes)
	mac := hmac.New(sha1.New, issuer.secret)
	_, _ = mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return Credentials{
		urls: []string{
			"turn:" + issuer.host + ":" + strconv.Itoa(int(issuer.udpPort)) + "?transport=udp",
			"turn:" + issuer.host + ":" + strconv.Itoa(int(issuer.udpPort)) + "?transport=tcp",
			"turns:" + issuer.host + ":" + strconv.Itoa(int(issuer.tlsPort)) + "?transport=tcp",
		},
		username: username, credential: credential, expiresAt: expiresAt,
	}, nil
}

// EncodeJSON is the only supported wire serialization for live credentials.
func (credentials Credentials) EncodeJSON() ([]byte, error) {
	if len(credentials.urls) != 3 || credentials.username == "" || credentials.credential == "" || credentials.expiresAt.IsZero() {
		return nil, errors.New("turnauth: empty credentials")
	}
	return json.Marshal(wireCredentials{
		Version: 1, URLs: append([]string(nil), credentials.urls...),
		Username: credentials.username, Credential: credentials.credential,
		ExpiresAt: credentials.expiresAt.UTC().Format(time.RFC3339),
	})
}

// EncodeJSONWithResolvedIPv4 is the browser wire form for hosts where a local
// transparent proxy synthesizes DNS answers for non-HTTP protocols. Plaintext
// TURN UDP/TCP use one separately verified public IPv4 address; TURNS keeps
// the configured DNS name so its certificate is still verified normally.
func (credentials Credentials) EncodeJSONWithResolvedIPv4(ipv4 string) ([]byte, error) {
	urls, err := credentials.urlsWithResolvedIPv4(ipv4)
	if err != nil {
		return nil, err
	}
	return json.Marshal(wireCredentials{
		Version: 1, URLs: urls, Username: credentials.username, Credential: credentials.credential,
		ExpiresAt: credentials.expiresAt.UTC().Format(time.RFC3339),
	})
}

// ServerRelay returns a private copy of the short-lived material needed by the
// server-side relay-only peer. It deliberately omits plaintext TURN-over-TCP;
// the server falls back directly from UDP to authenticated TLS on TCP 443.
func (credentials Credentials) ServerRelay() (ServerRelayCredential, error) {
	if len(credentials.urls) != 3 || credentials.username == "" ||
		credentials.credential == "" || credentials.expiresAt.IsZero() {
		return ServerRelayCredential{}, errors.New("turnauth: empty credentials")
	}
	udpHost, udpEndpoint, ok := issuedTURNEndpoint(credentials.urls[0], "turn", "udp")
	if !ok {
		return ServerRelayCredential{}, errors.New("turnauth: invalid server relay credentials")
	}
	tcpHost, tcpEndpoint, ok := issuedTURNEndpoint(credentials.urls[1], "turn", "tcp")
	if !ok || tcpHost != udpHost || tcpEndpoint != udpEndpoint {
		return ServerRelayCredential{}, errors.New("turnauth: invalid server relay credentials")
	}
	tlsHost, _, ok := issuedTURNEndpoint(credentials.urls[2], "turns", "tcp")
	if !ok || tlsHost != udpHost {
		return ServerRelayCredential{}, errors.New("turnauth: invalid server relay credentials")
	}
	return ServerRelayCredential{
		URLs: []string{credentials.urls[0], credentials.urls[2]}, Username: credentials.username,
		Password: credentials.credential, ExpiresAt: credentials.expiresAt,
	}, nil
}

// ServerRelayWithResolvedIPv4 keeps certificate validation on the original
// TURNS DNS name while allowing the server-side peer to reach plaintext TURN
// UDP/TCP through one already verified public IPv4 address. This is useful on
// hosts whose local transparent proxy returns synthetic DNS addresses. Media
// remains DTLS-SRTP encrypted end to end; only the client-to-TURN transport
// fallback is plaintext TCP.
func (credentials Credentials) ServerRelayWithResolvedIPv4(ipv4 string) (ServerRelayCredential, error) {
	base, err := credentials.ServerRelay()
	if err != nil {
		return ServerRelayCredential{}, err
	}
	urls, err := credentials.urlsWithResolvedIPv4(ipv4)
	if err != nil {
		return ServerRelayCredential{}, err
	}
	base.URLs = urls
	return base, nil
}

func (credentials Credentials) urlsWithResolvedIPv4(ipv4 string) ([]string, error) {
	if len(credentials.urls) != 3 || credentials.username == "" ||
		credentials.credential == "" || credentials.expiresAt.IsZero() {
		return nil, errors.New("turnauth: empty credentials")
	}
	ip := net.ParseIP(ipv4)
	if ip == nil || ip.To4() == nil || !publicTURNIPv4(ip.To4()) {
		return nil, errors.New("turnauth: invalid server relay address")
	}
	_, udpEndpoint, ok := issuedTURNEndpoint(credentials.urls[0], "turn", "udp")
	if !ok {
		return nil, errors.New("turnauth: invalid server relay credentials")
	}
	_, port, splitErr := net.SplitHostPort(udpEndpoint)
	if splitErr != nil {
		return nil, errors.New("turnauth: invalid server relay credentials")
	}
	endpoint := net.JoinHostPort(ip.To4().String(), port)
	return []string{
		"turn:" + endpoint + "?transport=udp",
		"turn:" + endpoint + "?transport=tcp",
		credentials.urls[2],
	}, nil
}

func publicTURNIPv4(ip net.IP) bool {
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() ||
		ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	first, second, third := ip[0], ip[1], ip[2]
	if first == 0 || first >= 224 || first == 100 && second >= 64 && second <= 127 ||
		first == 192 && second == 0 && (third == 0 || third == 2) ||
		first == 198 && (second == 18 || second == 19 || second == 51 && third == 100) ||
		first == 203 && second == 0 && third == 113 {
		return false
	}
	return true
}

func issuedTURNEndpoint(raw, scheme, transport string) (string, string, bool) {
	prefix := scheme + ":"
	if raw == "" || raw != strings.TrimSpace(raw) || !strings.HasPrefix(raw, prefix) ||
		strings.ContainsAny(raw, "@/#\t\r\n ") {
		return "", "", false
	}
	endpoint, query, ok := strings.Cut(strings.TrimPrefix(raw, prefix), "?")
	if !ok || query != "transport="+transport || strings.Contains(endpoint, "?") {
		return "", "", false
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || canonicalDNSName(host) == "" {
		return "", "", false
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return "", "", false
	}
	return canonicalDNSName(host), net.JoinHostPort(canonicalDNSName(host), port), true
}

// ExpiresAt allows the caller to schedule a refresh without exposing secrets.
func (credentials Credentials) ExpiresAt() time.Time { return credentials.expiresAt }

func (Credentials) String() string   { return "turnauth.Credentials{redacted}" }
func (Credentials) GoString() string { return "turnauth.Credentials{redacted}" }
func (Credentials) MarshalJSON() ([]byte, error) {
	return []byte(`{"redacted":true}`), nil
}

func (issuer *Issuer) String() string   { return "turnauth.Issuer{redacted}" }
func (issuer *Issuer) GoString() string { return "turnauth.Issuer{redacted}" }

// Close destroys the issuer's owned secret copy. It is idempotent.
func (issuer *Issuer) Close() error {
	if issuer == nil {
		return nil
	}
	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	if issuer.closed {
		return nil
	}
	for index := range issuer.secret {
		issuer.secret[index] = 0
	}
	issuer.secret = nil
	issuer.closed = true
	return nil
}
