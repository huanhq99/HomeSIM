package asterisk

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	defaultEventCapacity      = 64
	defaultMediaQueueCapacity = 32
	defaultMaxCommands        = 4096
	defaultHTTPTimeout        = 5 * time.Second
	defaultPrepareTimeout     = 10 * time.Second
	defaultConfirmTimeout     = 3 * time.Second
	defaultIOTimeout          = 5 * time.Second
	defaultMaxHTTPBodyBytes   = 1 << 20
	maxARIEventBytes          = 1 << 20
	maxMediaMessageBytes      = 65_500
	pcmuFrameBytes            = 160
)

var (
	// ErrConfiguration is returned for an incomplete or unsafe adapter setup.
	ErrConfiguration = errors.New("asterisk: invalid configuration")
	// ErrInsecureEndpoint means plaintext transport was configured for a
	// non-loopback host.
	ErrInsecureEndpoint = errors.New("asterisk: TLS or loopback endpoint required")
	// ErrTransport is a stable, credential-free network failure.
	ErrTransport = errors.New("asterisk: transport failure")
	// ErrProtocol is a stable, credential-free response or event violation.
	ErrProtocol = errors.New("asterisk: protocol violation")
)

// Config describes one inbound ARI client. BaseURL must end at the ARI root,
// normally https://pbx.example/ari or http://127.0.0.1:8088/ari. Plain HTTP is
// accepted only for a literal loopback address or localhost.
//
// Username and Password are sent only in an Authorization header. They are
// never accepted in BaseURL and are redacted by String and GoString.
type Config struct {
	GatewayID        string `json:"-"`
	BaseURL          string `json:"-"`
	Application      string `json:"-"`
	IncomingArgument string `json:"-"`
	IncomingContext  string `json:"-"`
	IncomingEndpoint string `json:"-"`
	// OutgoingEndpoint is an optional, explicitly configured PJSIP endpoint
	// used for browser-authorized outbound calls. Empty keeps dial disabled.
	OutgoingEndpoint string `json:"-"`
	Username         string `json:"-"`
	Password         string `json:"-"`
	// ExpectedVersion exactly pins system.version returned by
	// GET /asterisk/info. The caller must supply the deployment version; the
	// adapter deliberately has no built-in Asterisk version default.
	ExpectedVersion string `json:"-"`
	// IncomingPolicyID exactly pins the dialplan-provided
	// DJONEHUB_POLICY_ID channel variable for every incoming call.
	IncomingPolicyID string `json:"-"`
	// ExpectedEntityID optionally pins one PBX. ExpectedStartupTime additionally
	// pins one exact PBX incarnation and may only be set with ExpectedEntityID.
	ExpectedEntityID    string `json:"-"`
	ExpectedStartupTime string `json:"-"`
	// RequireCleanStartup establishes a PBX-clock admission cutoff around a
	// channel enumeration and rejects every channel created at or before that
	// cutoff. ARI UserEvents are anchors, not cross-topic ordering barriers. This
	// check performs no call mutation.
	RequireCleanStartup bool `json:"-"`
	// RecoverySecret optionally adds protocol_id to recovery identity as a
	// keyed digest. It must remain stable across process restarts. When absent,
	// recovery remains available but deliberately omits protocol_id.
	RecoverySecret     []byte           `json:"-"`
	TLSConfig          *tls.Config      `json:"-"`
	Dialer             *net.Dialer      `json:"-"`
	IDReader           io.Reader        `json:"-"`
	Now                func() time.Time `json:"-"`
	EventCapacity      int
	MediaQueueCapacity int
	MaxCommands        int
	HTTPTimeout        time.Duration
	PrepareTimeout     time.Duration
	ConfirmTimeout     time.Duration
	IOTimeout          time.Duration
	MaxHTTPBodyBytes   int64
}

func (Config) String() string   { return "asterisk.Config{redacted}" }
func (Config) GoString() string { return "asterisk.Config{redacted}" }

// ValidateConfig checks every static identity, transport and bound without
// opening ARI HTTP or WebSocket connections. Callers can therefore reject an
// unsafe startup configuration before entering a reconnect loop.
func ValidateConfig(cfg Config) error {
	normalized, err := normalizeConfig(cfg)
	zeroRecoverySecret(&normalized)
	return err
}

type normalizedConfig struct {
	gatewayID           string
	baseURL             *url.URL
	application         string
	incomingArgument    string
	incomingContext     string
	incomingEndpoint    string
	outgoingEndpoint    string
	username            string
	password            string
	expectedVersion     string
	incomingPolicyID    string
	expectedEntityID    string
	expectedStartupTime string
	requireCleanStartup bool
	recoverySecret      []byte
	tlsConfig           *tls.Config
	dialer              *net.Dialer
	idReader            io.Reader
	now                 func() time.Time
	eventCapacity       int
	mediaQueueCapacity  int
	maxCommands         int
	httpTimeout         time.Duration
	prepareTimeout      time.Duration
	confirmTimeout      time.Duration
	ioTimeout           time.Duration
	maxHTTPBodyBytes    int64
}

func normalizeConfig(cfg Config) (normalizedConfig, error) {
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return normalizedConfig{}, ErrConfiguration
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/ari"
	}
	if parsed.Path != "/ari" {
		return normalizedConfig{}, ErrConfiguration
	}
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !isLiteralLoopback(parsed.Hostname()) {
			return normalizedConfig{}, ErrInsecureEndpoint
		}
	} else if cfg.TLSConfig != nil && cfg.TLSConfig.InsecureSkipVerify &&
		!isLiteralLoopback(parsed.Hostname()) {
		return normalizedConfig{}, ErrInsecureEndpoint
	}
	if !validToken(cfg.GatewayID, 1, 64) || !validToken(cfg.Application, 1, 64) ||
		!validToken(defaultString(cfg.IncomingArgument, "incoming"), 1, 64) ||
		!validToken(cfg.IncomingContext, 1, 64) || !validToken(cfg.IncomingEndpoint, 1, 64) ||
		(cfg.OutgoingEndpoint != "" && !validToken(cfg.OutgoingEndpoint, 1, 64)) ||
		!validOpaque(cfg.ExpectedVersion, 1, 128) || !validToken(cfg.IncomingPolicyID, 1, 64) ||
		cfg.Username == "" || len(cfg.Username) > 256 || strings.Contains(cfg.Username, ":") ||
		cfg.Password == "" || len(cfg.Password) > 1024 {
		return normalizedConfig{}, ErrConfiguration
	}
	if (cfg.ExpectedEntityID != "" && !validOpaque(cfg.ExpectedEntityID, 1, 256)) ||
		(cfg.ExpectedStartupTime != "" &&
			(cfg.ExpectedEntityID == "" || !validAsteriskTime(cfg.ExpectedStartupTime))) ||
		(len(cfg.RecoverySecret) != 0 && (len(cfg.RecoverySecret) < 32 || len(cfg.RecoverySecret) > 64)) {
		return normalizedConfig{}, ErrConfiguration
	}

	n := normalizedConfig{
		gatewayID: cfg.GatewayID, baseURL: parsed, application: cfg.Application,
		incomingArgument: defaultString(cfg.IncomingArgument, "incoming"),
		incomingContext:  cfg.IncomingContext, incomingEndpoint: cfg.IncomingEndpoint,
		outgoingEndpoint: cfg.OutgoingEndpoint,
		username:         cfg.Username, password: cfg.Password,
		expectedVersion: cfg.ExpectedVersion, incomingPolicyID: cfg.IncomingPolicyID,
		expectedEntityID: cfg.ExpectedEntityID, expectedStartupTime: cfg.ExpectedStartupTime,
		requireCleanStartup: cfg.RequireCleanStartup,
		recoverySecret:      append([]byte(nil), cfg.RecoverySecret...),
		tlsConfig:           cfg.TLSConfig, dialer: cfg.Dialer, idReader: cfg.IDReader, now: cfg.Now,
		eventCapacity: cfg.EventCapacity, mediaQueueCapacity: cfg.MediaQueueCapacity,
		maxCommands: cfg.MaxCommands, httpTimeout: cfg.HTTPTimeout,
		prepareTimeout: cfg.PrepareTimeout, confirmTimeout: cfg.ConfirmTimeout,
		ioTimeout: cfg.IOTimeout, maxHTTPBodyBytes: cfg.MaxHTTPBodyBytes,
	}
	if n.idReader == nil {
		n.idReader = rand.Reader
	}
	if n.now == nil {
		n.now = time.Now
	}
	if n.dialer == nil {
		n.dialer = &net.Dialer{Timeout: defaultHTTPTimeout, KeepAlive: -1}
	}
	if n.tlsConfig != nil {
		n.tlsConfig = n.tlsConfig.Clone()
	}
	if n.eventCapacity == 0 {
		n.eventCapacity = defaultEventCapacity
	}
	if n.mediaQueueCapacity == 0 {
		n.mediaQueueCapacity = defaultMediaQueueCapacity
	}
	if n.maxCommands == 0 {
		n.maxCommands = defaultMaxCommands
	}
	if n.httpTimeout == 0 {
		n.httpTimeout = defaultHTTPTimeout
	}
	if n.prepareTimeout == 0 {
		n.prepareTimeout = defaultPrepareTimeout
	}
	if n.confirmTimeout == 0 {
		n.confirmTimeout = defaultConfirmTimeout
	}
	if n.ioTimeout == 0 {
		n.ioTimeout = defaultIOTimeout
	}
	if n.maxHTTPBodyBytes == 0 {
		n.maxHTTPBodyBytes = defaultMaxHTTPBodyBytes
	}
	if n.eventCapacity < 1 || n.eventCapacity > 4096 ||
		n.mediaQueueCapacity < 1 || n.mediaQueueCapacity > 4096 ||
		n.maxCommands < 1 || n.maxCommands > 65_536 ||
		!validDuration(n.httpTimeout) || !validDuration(n.prepareTimeout) ||
		!validDuration(n.confirmTimeout) || !validDuration(n.ioTimeout) ||
		n.maxHTTPBodyBytes < 1 || n.maxHTTPBodyBytes > 8<<20 {
		zeroRecoverySecret(&n)
		return normalizedConfig{}, ErrConfiguration
	}
	return n, nil
}

func validAsteriskTime(value string) bool {
	_, ok := parseAsteriskTime(value)
	return ok
}

func parseAsteriskTime(value string) (time.Time, bool) {
	if !validOpaque(value, 1, 128) {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func zeroRecoverySecret(cfg *normalizedConfig) {
	if cfg == nil {
		return
	}
	for index := range cfg.recoverySecret {
		cfg.recoverySecret[index] = 0
	}
}

func isLiteralLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validDuration(value time.Duration) bool {
	return value >= 10*time.Millisecond && value <= 5*time.Minute
}

func validToken(value string, minLength, maxLength int) bool {
	if len(value) < minLength || len(value) > maxLength {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validOpaque(value string, minLength, maxLength int) bool {
	if value != strings.TrimSpace(value) || len(value) < minLength || len(value) > maxLength {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
