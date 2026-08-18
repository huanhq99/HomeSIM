package publicedgeserver

import (
	"bufio"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/publicedge"
	"golang.org/x/net/websocket"
)

const (
	// ProductionExactHost is the only production HTTP authority for this
	// service. Config.ExactHost remains injectable so a real TLS test server can
	// exercise the same exact-host check without DNS or network access.
	ProductionExactHost = "phone.example.com"

	MobileUIPath    = "/"
	MobileUICSSPath = "/assets/public-mobile-v1.css"
	MobileUIJSPath  = "/assets/public-mobile-v1.js"
	SnapshotAPIPath = "/api/public/v1/state/snapshot"
	HealthPath      = "/healthz"

	defaultHandshakeTimeout = 10 * time.Second
	defaultReadTimeout      = 45 * time.Second
	defaultWriteTimeout     = 5 * time.Second
	defaultMaxConnections   = 32

	maxHandshakeTimeout = 30 * time.Second
	maxReadTimeout      = 5 * time.Minute
	maxWriteTimeout     = 30 * time.Second
	maxConnections      = 256
)

const mobileUICSP = "default-src 'none'; base-uri 'none'; connect-src 'self'; font-src 'none'; " +
	"form-action 'none'; frame-ancestors 'none'; img-src 'none'; manifest-src 'none'; media-src 'none'; " +
	"object-src 'none'; script-src 'self'; script-src-attr 'none'; style-src 'self'; " +
	"style-src-attr 'none'; worker-src 'none'"

// Keeping the complete browser slice inside this package prevents an external
// CDN, analytics tag, font host, or mutable deployment directory from entering
// the authenticated read-only surface.
//
//go:embed assets/index.html
var mobileUIHTML string

//go:embed assets/public-mobile-v1.css
var mobileUICSS string

//go:embed assets/public-mobile-v1.js
var mobileUIJS string

var (
	ErrInvalidConfig  = errors.New("public edge server configuration is invalid")
	ErrUnauthorized   = errors.New("public edge HTTP request is unauthorized")
	errWrongFrameType = errors.New("public edge WebSocket frame is not text")
)

// Clock supplies protocol time. Socket deadlines deliberately use the local
// monotonic clock so a wall-clock adjustment or a deterministic test clock
// cannot accidentally make network I/O unbounded.
type Clock func() time.Time

// HTTPAuth is the sole authorization boundary for the public read-only HTTP
// endpoint. Implementations must derive identity from their own verified
// credential (for example, a locally verified Access assertion), never from a
// cookie, Origin, forwarding, source-IP, or Tailscale header by itself.
type HTTPAuth interface {
	AuthorizeHTTP(*http.Request) error
}

// HTTPAuthFunc adapts a callback to HTTPAuth.
type HTTPAuthFunc func(*http.Request) error

func (authorize HTTPAuthFunc) AuthorizeHTTP(request *http.Request) error {
	if authorize == nil {
		return ErrUnauthorized
	}
	return authorize(request)
}

// Config contains only immutable server policy and injected verification
// dependencies. Zero timeouts receive bounded defaults.
type Config struct {
	Registry         *publicedge.Registry
	ExactHost        string
	Clock            Clock
	HTTPAuth         HTTPAuth
	HandshakeTimeout time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxConnections   int
}

type normalizedConfig struct {
	registry         *publicedge.Registry
	exactHost        string
	clock            Clock
	httpAuth         HTTPAuth
	handshakeTimeout time.Duration
	readTimeout      time.Duration
	writeTimeout     time.Duration
	maxConnections   int
}

// SnapshotResponse is the complete public state response. Snapshot is nil
// until the current connection incarnation publishes its first valid snapshot.
// A disconnected gateway leaves its last snapshot available but marks it
// offline, allowing clients to display it only as stale evidence.
type SnapshotResponse struct {
	Version       uint64                      `json:"version"`
	GatewayOnline bool                        `json:"gateway_online"`
	ReceivedAt    int64                       `json:"received_at"`
	Snapshot      *publicedge.SnapshotPayload `json:"snapshot"`
}

// HealthResponse intentionally reveals process liveness only. Authenticated
// snapshot callers already receive the coarse online bit; exposing it here
// would turn an unauthenticated health probe into an activity oracle.
type HealthResponse struct {
	Status string `json:"status"`
}

// Server is an http.Handler for the fixed public-edge surface. Gateway reads
// and writes happen synchronously on one goroutine per connection: there is no
// application message queue, and therefore no unbounded queue to exhaust or
// reorder.
type Server struct {
	config normalizedConfig

	mu                 sync.RWMutex
	closed             bool
	inFlight           int
	connections        map[net.Conn]struct{}
	activeConnectionID string
	gatewayOnline      bool
	receivedAt         int64
	snapshot           *publicedge.SnapshotPayload
	wait               sync.WaitGroup
}

// New constructs a fail-closed public-edge handler.
func New(config Config) (*Server, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return &Server{
		config:      normalized,
		connections: make(map[net.Conn]struct{}),
	}, nil
}

func normalizeConfig(config Config) (normalizedConfig, error) {
	if config.Registry == nil || config.HTTPAuth == nil {
		return normalizedConfig{}, fmt.Errorf("%w: registry and HTTP auth are required", ErrInvalidConfig)
	}
	if config.ExactHost == "" {
		config.ExactHost = ProductionExactHost
	}
	if len(config.ExactHost) > 255 || strings.TrimSpace(config.ExactHost) != config.ExactHost ||
		strings.ContainsAny(config.ExactHost, "/\\\r\n\t") {
		return normalizedConfig{}, fmt.Errorf("%w: exact host", ErrInvalidConfig)
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Clock().IsZero() {
		return normalizedConfig{}, fmt.Errorf("%w: clock", ErrInvalidConfig)
	}

	handshakeTimeout, err := boundedDuration(
		config.HandshakeTimeout, defaultHandshakeTimeout, maxHandshakeTimeout,
	)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("%w: handshake timeout", ErrInvalidConfig)
	}
	readTimeout, err := boundedDuration(config.ReadTimeout, defaultReadTimeout, maxReadTimeout)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("%w: read timeout", ErrInvalidConfig)
	}
	writeTimeout, err := boundedDuration(config.WriteTimeout, defaultWriteTimeout, maxWriteTimeout)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("%w: write timeout", ErrInvalidConfig)
	}
	if config.MaxConnections == 0 {
		config.MaxConnections = defaultMaxConnections
	}
	if config.MaxConnections < 1 || config.MaxConnections > maxConnections {
		return normalizedConfig{}, fmt.Errorf("%w: connection bound", ErrInvalidConfig)
	}
	return normalizedConfig{
		registry: config.Registry, exactHost: config.ExactHost, clock: config.Clock,
		httpAuth: config.HTTPAuth, handshakeTimeout: handshakeTimeout,
		readTimeout: readTimeout, writeTimeout: writeTimeout,
		maxConnections: config.MaxConnections,
	}, nil
}

func boundedDuration(value, fallback, maximum time.Duration) (time.Duration, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < time.Millisecond || value > maximum {
		return 0, ErrInvalidConfig
	}
	return value, nil
}

// ServeHTTP exposes exactly six routes. It does not use a broad ServeMux, so
// encoded, normalized, or query-bearing path aliases never reach a handler.
func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if server == nil || request == nil || request.URL == nil {
		writeEmptyStatus(response, http.StatusServiceUnavailable)
		return
	}
	setCommonHeaders(response.Header())
	if server.isClosed() {
		writeEmptyStatus(response, http.StatusServiceUnavailable)
		return
	}
	if request.Host != server.config.exactHost || request.URL.RawPath != "" ||
		request.URL.RawQuery != "" || request.URL.ForceQuery {
		writeEmptyStatus(response, http.StatusNotFound)
		return
	}

	switch request.URL.Path {
	case publicedge.GatewayWebSocketPath:
		server.serveGateway(response, request)
	case MobileUIPath:
		server.serveMobileUI(response, request, "text/html; charset=utf-8", mobileUIHTML)
	case MobileUICSSPath:
		server.serveMobileUI(response, request, "text/css; charset=utf-8", mobileUICSS)
	case MobileUIJSPath:
		server.serveMobileUI(response, request, "text/javascript; charset=utf-8", mobileUIJS)
	case SnapshotAPIPath:
		server.serveSnapshot(response, request)
	case HealthPath:
		server.serveHealth(response, request)
	default:
		writeEmptyStatus(response, http.StatusNotFound)
	}
}

func (server *Server) serveMobileUI(
	response http.ResponseWriter,
	request *http.Request,
	contentType string,
	content string,
) {
	setMobileUIHeaders(response.Header())
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeEmptyStatus(response, http.StatusMethodNotAllowed)
		return
	}
	if !server.authorizeHTTP(response, request) {
		return
	}
	response.Header().Set("Content-Type", contentType)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(content))
}

func (server *Server) serveGateway(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeEmptyStatus(response, http.StatusMethodNotAllowed)
		return
	}
	protocolHeaders := request.Header.Values("Sec-WebSocket-Protocol")
	if len(protocolHeaders) != 1 || protocolHeaders[0] != publicedge.GatewayWebSocketSubprotocol {
		writeEmptyStatus(response, http.StatusBadRequest)
		return
	}
	if !exactGatewayOrigin(request.Header, server.config.exactHost) ||
		forbiddenGatewayIdentityHeader(request.Header) {
		writeEmptyStatus(response, http.StatusBadRequest)
		return
	}
	if _, ok := response.(http.Hijacker); !ok {
		// x/net/websocket requires HTTP/1.x hijacking. Reject unsupported
		// transports instead of allowing its server adapter to panic.
		writeEmptyStatus(response, http.StatusBadRequest)
		return
	}
	if !server.acquireConnectionSlot() {
		writeEmptyStatus(response, http.StatusServiceUnavailable)
		return
	}
	defer server.releaseConnectionSlot()

	// x/net/websocket hijacks before it validates the handshake or invokes its
	// Handler. Intercepting that exact point closes the otherwise-untracked
	// upgrade gap: Close can terminate and wait for a peer even while the
	// library is writing its 101 response.
	upgrade := &gatewayUpgradeWriter{ResponseWriter: response, server: server}
	defer upgrade.untrack()
	websocket.Server{
		Handshake: func(config *websocket.Config, handshakeRequest *http.Request) error {
			if handshakeRequest.Method != http.MethodGet || handshakeRequest.Host != server.config.exactHost ||
				handshakeRequest.URL == nil || handshakeRequest.URL.Path != publicedge.GatewayWebSocketPath ||
				handshakeRequest.URL.RawPath != "" || handshakeRequest.URL.RawQuery != "" ||
				handshakeRequest.URL.ForceQuery || len(config.Protocol) != 1 ||
				config.Protocol[0] != publicedge.GatewayWebSocketSubprotocol ||
				!exactGatewayOrigin(handshakeRequest.Header, server.config.exactHost) ||
				forbiddenGatewayIdentityHeader(handshakeRequest.Header) {
				return publicedge.ErrUnauthorized
			}
			// Echo only the one selected protocol. Forwarding metadata is allowed
			// for Cloudflare Tunnel compatibility but never enters authorization;
			// the signed P-256 protocol remains the gateway identity boundary.
			config.Protocol = []string{publicedge.GatewayWebSocketSubprotocol}
			return nil
		},
		Handler: server.handleGateway,
	}.ServeHTTP(upgrade, request)
}

func exactGatewayOrigin(headers http.Header, exactHost string) bool {
	want := "https://" + exactHost
	origin, count := "", 0
	for name, values := range headers {
		if !strings.EqualFold(name, "Origin") {
			continue
		}
		count += len(values)
		if len(values) == 1 {
			origin = values[0]
		}
	}
	return count == 1 && origin == want
}

func forbiddenGatewayIdentityHeader(headers http.Header) bool {
	for name := range headers {
		normalized := strings.ToLower(name)
		switch normalized {
		case "cookie", "cf-access-authenticated-user-email", "cf-access-user":
			return true
		}
		if strings.HasPrefix(normalized, "tailscale-") ||
			strings.HasPrefix(normalized, "x-tailscale-") {
			return true
		}
	}
	return false
}

// gatewayUpgradeWriter records the raw connection at the instant x/net's
// WebSocket adapter hijacks it. The adapter hijacks before validating or
// invoking its Handler, so tracking only *websocket.Conn in handleGateway
// leaves a shutdown gap in which Close cannot interrupt the upgrade.
type gatewayUpgradeWriter struct {
	http.ResponseWriter
	server     *Server
	connection net.Conn
	tracked    bool
}

func (writer *gatewayUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffered, err := writer.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	writer.connection = connection
	writer.tracked = writer.server.trackConnection(connection)
	if !writer.tracked {
		_ = connection.SetDeadline(time.Now())
		_ = connection.Close()
	}
	return connection, buffered, nil
}

func (writer *gatewayUpgradeWriter) untrack() {
	if writer == nil || !writer.tracked {
		return
	}
	writer.server.untrackConnection(writer.connection)
	writer.tracked = false
}

func (server *Server) serveSnapshot(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeEmptyStatus(response, http.StatusMethodNotAllowed)
		return
	}
	if !server.authorizeHTTP(response, request) {
		return
	}
	writeJSON(response, http.StatusOK, server.currentSnapshot())
}

func (server *Server) authorizeHTTP(response http.ResponseWriter, request *http.Request) bool {
	if err := server.config.httpAuth.AuthorizeHTTP(request); err != nil {
		status, code := http.StatusUnauthorized, "unauthorized"
		if errors.Is(err, publicedge.ErrAccessUnavailable) {
			status, code = http.StatusServiceUnavailable, "unavailable"
		}
		writeJSON(response, status, struct {
			Error string `json:"error"`
		}{Error: code})
		return false
	}
	return true
}

func (server *Server) serveHealth(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet {
		response.Header().Set("Allow", http.MethodGet)
		writeEmptyStatus(response, http.StatusMethodNotAllowed)
		return
	}
	writeJSON(response, http.StatusOK, HealthResponse{Status: "ok"})
}

func (server *Server) handleGateway(connection *websocket.Conn) {
	connection.MaxPayloadBytes = publicedge.MaxFrameBytes
	connectionID, err := newConnectionID()
	if err != nil {
		return
	}
	if err := setReadWindow(connection, server.config.handshakeTimeout); err != nil {
		return
	}
	hello, err := receiveFrame(connection)
	if err != nil {
		return
	}
	now := server.config.clock()
	if now.IsZero() {
		return
	}
	session, challenge, err := server.config.registry.Open(connectionID, hello, now)
	if err != nil {
		return
	}
	defer func() {
		session.Close()
		server.markOffline(connectionID)
	}()
	if err := server.sendFrame(connection, challenge); err != nil {
		return
	}

	if err := setReadWindow(connection, server.config.handshakeTimeout); err != nil {
		return
	}
	authenticate, err := receiveFrame(connection)
	if err != nil {
		return
	}
	now = server.config.clock()
	if now.IsZero() {
		return
	}
	authenticated, err := session.Authenticate(authenticate, now)
	if err != nil {
		return
	}
	if !server.claimActive(connectionID) {
		return
	}
	// Authentication already acquired the registry lease. A failed or
	// ambiguous acknowledgement write therefore terminates that lease.
	if err := server.sendFrame(connection, authenticated); err != nil {
		return
	}

	for {
		if err := setReadWindow(connection, server.config.readTimeout); err != nil {
			return
		}
		frame, err := receiveFrame(connection)
		if err != nil {
			return
		}
		// This first public slice serves only coarse status. Requiring every
		// application frame to be a fresh snapshot prevents heartbeats/events
		// from keeping an arbitrarily old cache marked online.
		if frame.Type != publicedge.FrameStateSnapshot {
			return
		}
		now = server.config.clock()
		if now.IsZero() || session.AcceptGateway(frame, now) != nil {
			return
		}
		server.acceptSnapshot(connectionID, frame, now)
	}
}

// setReadWindow also bounds x/net/websocket's automatic Pong write. The
// library emits that control frame inside Receive, outside sendFrame; carrying
// the same finite window onto the write side prevents either an expired prior
// acknowledgement deadline or an unbounded control-frame write.
func setReadWindow(connection *websocket.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := connection.SetReadDeadline(deadline); err != nil {
		return err
	}
	return connection.SetWriteDeadline(deadline)
}

func receiveFrame(connection *websocket.Conn) (publicedge.Frame, error) {
	var data []byte
	if err := textFrameCodec.Receive(connection, &data); err != nil {
		return publicedge.Frame{}, err
	}
	return publicedge.DecodeFrame(data)
}

func (server *Server) sendFrame(connection *websocket.Conn, frame publicedge.Frame) error {
	data, err := publicedge.EncodeFrame(frame)
	if err != nil {
		return err
	}
	if err := connection.SetWriteDeadline(time.Now().Add(server.config.writeTimeout)); err != nil {
		return err
	}
	return textFrameCodec.Send(connection, data)
}

var textFrameCodec = websocket.Codec{
	Marshal: func(value any) ([]byte, byte, error) {
		data, ok := value.([]byte)
		if !ok || len(data) == 0 || len(data) > publicedge.MaxFrameBytes {
			return nil, websocket.UnknownFrame, websocket.ErrNotSupported
		}
		return data, websocket.TextFrame, nil
	},
	Unmarshal: func(data []byte, payloadType byte, destination any) error {
		if payloadType != websocket.TextFrame {
			return errWrongFrameType
		}
		output, ok := destination.(*[]byte)
		if !ok {
			return websocket.ErrNotSupported
		}
		*output = append((*output)[:0], data...)
		return nil
	},
}

func newConnectionID() (string, error) {
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("create connection incarnation: %w", err)
	}
	return "ws-" + hex.EncodeToString(randomBytes[:]), nil
}

func (server *Server) acceptSnapshot(connectionID string, frame publicedge.Frame, now time.Time) {
	var snapshot publicedge.SnapshotPayload
	// DecodeFrame already performed strict, duplicate-free, canonical payload
	// validation. This second decode only copies the typed value.
	if err := json.Unmarshal(frame.Payload, &snapshot); err != nil {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed || server.activeConnectionID != connectionID {
		return
	}
	server.gatewayOnline = true
	server.receivedAt = now.Unix()
	server.snapshot = &snapshot
}

// claimActive globally serializes the one coarse public state cache even if a
// caller accidentally configures Registry with more than one gateway key. The
// protocol Registry independently enforces one incarnation per gateway ID;
// this additional claim prevents two IDs from racing into one anonymous API.
func (server *Server) claimActive(connectionID string) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed || server.activeConnectionID != "" {
		return false
	}
	server.activeConnectionID = connectionID
	// Never present a prior connection's snapshot as live for a fresh lease.
	server.gatewayOnline = false
	server.receivedAt = 0
	server.snapshot = nil
	return true
}

func (server *Server) markOffline(connectionID string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.activeConnectionID != connectionID {
		return
	}
	server.activeConnectionID = ""
	server.gatewayOnline = false
}

func (server *Server) currentSnapshot() SnapshotResponse {
	server.mu.RLock()
	defer server.mu.RUnlock()
	response := SnapshotResponse{
		Version: publicedge.ProtocolVersion, GatewayOnline: server.gatewayOnline,
		ReceivedAt: server.receivedAt,
	}
	if server.snapshot != nil {
		copy := *server.snapshot
		response.Snapshot = &copy
	}
	return response
}

func (server *Server) acquireConnectionSlot() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	// Once one authenticated incarnation owns the anonymous public cache, a
	// later HTTP upgrade cannot legitimately replace it. Rejecting it before
	// hijack prevents unauthenticated peers from consuming the remaining slots.
	if server.closed || server.activeConnectionID != "" || server.inFlight >= server.config.maxConnections {
		return false
	}
	server.inFlight++
	server.wait.Add(1)
	return true
}

func (server *Server) releaseConnectionSlot() {
	server.mu.Lock()
	server.inFlight--
	server.mu.Unlock()
	server.wait.Done()
}

func (server *Server) trackConnection(connection net.Conn) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed {
		return false
	}
	server.connections[connection] = struct{}{}
	return true
}

func (server *Server) untrackConnection(connection net.Conn) {
	server.mu.Lock()
	delete(server.connections, connection)
	server.mu.Unlock()
}

func (server *Server) isClosed() bool {
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.closed
}

// Close terminates every challenged or authenticated WebSocket, releases each
// protocol lease, and waits for all WebSocket handlers to return. Deadlines are
// forced before Close so a peer that stopped reading cannot delay shutdown.
func (server *Server) Close() error {
	if server == nil {
		return nil
	}
	server.mu.Lock()
	if !server.closed {
		server.closed = true
		server.gatewayOnline = false
		server.activeConnectionID = ""
	}
	connections := make([]net.Conn, 0, len(server.connections))
	for connection := range server.connections {
		connections = append(connections, connection)
	}
	server.mu.Unlock()

	for _, connection := range connections {
		_ = connection.SetDeadline(time.Now())
		_ = connection.Close()
	}
	server.wait.Wait()
	return nil
}

func setCommonHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}

func setMobileUIHeaders(header http.Header) {
	setCommonHeaders(header)
	header.Set("Content-Security-Policy", mobileUICSP)
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Permissions-Policy", "accelerometer=(), autoplay=(), camera=(), display-capture=(), fullscreen=(), geolocation=(), gyroscope=(), hid=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-create=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), usb=(), web-share=(), xr-spatial-tracking=()")
	header.Set("X-Frame-Options", "DENY")
	header.Set("X-Robots-Tag", "noindex, nofollow, noarchive, nosnippet")
}

func writeEmptyStatus(response http.ResponseWriter, status int) {
	response.Header().Set("Content-Length", "0")
	response.WriteHeader(status)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeEmptyStatus(response, http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write(data)
}

type redactedConfig struct {
	Registry         string        `json:"registry"`
	ExactHost        string        `json:"exact_host"`
	Clock            string        `json:"clock"`
	HTTPAuth         string        `json:"http_auth"`
	HandshakeTimeout time.Duration `json:"handshake_timeout"`
	ReadTimeout      time.Duration `json:"read_timeout"`
	WriteTimeout     time.Duration `json:"write_timeout"`
	MaxConnections   int           `json:"max_connections"`
}

func (config Config) redacted() redactedConfig {
	return redactedConfig{
		Registry: redactConfigured(config.Registry != nil), ExactHost: config.ExactHost,
		Clock: redactConfigured(config.Clock != nil), HTTPAuth: redactConfigured(config.HTTPAuth != nil),
		HandshakeTimeout: config.HandshakeTimeout, ReadTimeout: config.ReadTimeout,
		WriteTimeout: config.WriteTimeout, MaxConnections: config.MaxConnections,
	}
}

func (config Config) MarshalJSON() ([]byte, error) { return json.Marshal(config.redacted()) }

func (config Config) String() string {
	data, err := config.MarshalJSON()
	if err != nil {
		return "publicedgeserver.Config{redacted}"
	}
	return string(data)
}

func (config Config) GoString() string { return config.String() }

func redactConfigured(configured bool) string {
	if configured {
		return "[configured]"
	}
	return ""
}

type redactedServer struct {
	Closed        bool   `json:"closed"`
	ExactHost     string `json:"exact_host"`
	InFlight      int    `json:"in_flight"`
	GatewayOnline bool   `json:"gateway_online"`
	HasSnapshot   bool   `json:"has_snapshot"`
}

func (server *Server) MarshalJSON() ([]byte, error) {
	if server == nil {
		return json.Marshal(redactedServer{Closed: true})
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	return json.Marshal(redactedServer{
		Closed: server.closed, ExactHost: server.config.exactHost, InFlight: server.inFlight,
		GatewayOnline: server.gatewayOnline, HasSnapshot: server.snapshot != nil,
	})
}

func (server *Server) String() string {
	data, err := server.MarshalJSON()
	if err != nil {
		return "publicedgeserver.Server{redacted}"
	}
	return string(data)
}

func (server *Server) GoString() string { return server.String() }
