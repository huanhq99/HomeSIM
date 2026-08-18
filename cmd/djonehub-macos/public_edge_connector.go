package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/publicedge"
	"github.com/iniwex5/vohive/internal/sipgateway"
	"golang.org/x/net/websocket"
)

const (
	publicEdgeHost                   = "phone.example.com"
	publicEdgeHeartbeatInterval      = 15 * time.Second
	publicEdgeMaximumHeartbeat       = 30 * time.Second
	publicEdgeDialTimeout            = 10 * time.Second
	publicEdgeWriteTimeout           = 10 * time.Second
	publicEdgeControlWriteWindow     = publicEdgeMaximumHeartbeat + publicEdgeWriteTimeout
	publicEdgeHandshakeTimeout       = 15 * time.Second
	publicEdgeReconnectMinimum       = time.Second
	publicEdgeReconnectMaximum       = 30 * time.Second
	publicEdgeKeyMaximumBytes        = 8 << 10
	publicEdgeAccessMaximumBytes     = 512
	publicEdgeHandshakeHeaderMaximum = 64 << 10
)

var (
	errPublicEdgeConfiguration = errors.New("public edge configuration is invalid")
	errPublicEdgeTransport     = errors.New("public edge transport is unavailable")
	errPublicEdgeProtocol      = errors.New("public edge protocol failed")

	publicEdgeAccessValuePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,512}$`)
	publicEdgeGatewayIDPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)
	publicEdgeTextMessage        = websocket.Codec{
		Marshal: func(value any) ([]byte, byte, error) {
			text, ok := value.(string)
			if !ok {
				return nil, websocket.UnknownFrame, errPublicEdgeProtocol
			}
			return []byte(text), websocket.TextFrame, nil
		},
		Unmarshal: func(data []byte, payloadType byte, target any) error {
			text, ok := target.(*string)
			if !ok || payloadType != websocket.TextFrame {
				return errPublicEdgeProtocol
			}
			*text = string(data)
			return nil
		},
	}
)

type publicEdgeFlagConfig struct {
	URL                    string
	GatewayID              string
	PrivateKeyFile         string
	AccessClientIDFile     string
	AccessClientSecretFile string
}

type publicEdgeStartupConfig struct {
	endpoint               *url.URL
	gatewayID              string
	privateKeyFile         string
	accessClientIDFile     string
	accessClientSecretFile string
}

func (publicEdgeStartupConfig) String() string   { return "publicEdgeStartupConfig{redacted}" }
func (publicEdgeStartupConfig) GoString() string { return "publicEdgeStartupConfig{redacted}" }

func parsePublicEdgeStartupConfig(flags publicEdgeFlagConfig) (*publicEdgeStartupConfig, error) {
	values := []string{
		flags.URL, flags.GatewayID, flags.PrivateKeyFile,
		flags.AccessClientIDFile, flags.AccessClientSecretFile,
	}
	configured := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != len(values) {
		return nil, errPublicEdgeConfiguration
	}

	rawURL := strings.TrimSpace(flags.URL)
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Scheme != "wss" || endpoint.User != nil ||
		endpoint.Host != publicEdgeHost || endpoint.Hostname() != publicEdgeHost || endpoint.Port() != "" ||
		endpoint.Path != publicedge.GatewayWebSocketPath || endpoint.RawPath != "" ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.String() != rawURL {
		return nil, errPublicEdgeConfiguration
	}
	if strings.TrimSpace(flags.GatewayID) != flags.GatewayID ||
		!publicEdgeGatewayIDPattern.MatchString(flags.GatewayID) {
		return nil, errPublicEdgeConfiguration
	}
	for _, path := range []string{
		flags.PrivateKeyFile, flags.AccessClientIDFile, flags.AccessClientSecretFile,
	} {
		if !filepath.IsAbs(path) || strings.TrimSpace(path) != path {
			return nil, errPublicEdgeConfiguration
		}
	}

	return &publicEdgeStartupConfig{
		endpoint: endpoint, gatewayID: flags.GatewayID,
		privateKeyFile:         flags.PrivateKeyFile,
		accessClientIDFile:     flags.AccessClientIDFile,
		accessClientSecretFile: flags.AccessClientSecretFile,
	}, nil
}

type publicEdgeCoarseState struct {
	Service publicedge.ServiceState
	Call    publicedge.CallState
	SMS     publicedge.SMSState
}

type publicEdgeConnectorConfig struct {
	Endpoint           *url.URL
	GatewayID          string
	PrivateKey         *ecdsa.PrivateKey
	AccessClientID     []byte
	AccessClientSecret []byte
	Source             func() publicEdgeCoarseState
	Now                func() time.Time
	IDReader           io.Reader
	HeartbeatInterval  time.Duration
	ReconnectMinimum   time.Duration
	ReconnectMaximum   time.Duration
	Dial               func(context.Context, *url.URL, []byte, []byte) (*websocket.Conn, error)
}

func (publicEdgeConnectorConfig) String() string   { return "publicEdgeConnectorConfig{redacted}" }
func (publicEdgeConnectorConfig) GoString() string { return "publicEdgeConnectorConfig{redacted}" }

func (publicEdgeConnectorConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Configuration string `json:"configuration"`
	}{Configuration: "redacted"})
}

type publicEdgeConnector struct {
	mu sync.Mutex
	wg sync.WaitGroup

	endpoint           *url.URL
	gatewayID          string
	privateKey         *ecdsa.PrivateKey
	accessClientID     []byte
	accessClientSecret []byte
	source             func() publicEdgeCoarseState
	now                func() time.Time
	idReader           io.Reader
	heartbeatInterval  time.Duration
	reconnectMinimum   time.Duration
	reconnectMaximum   time.Duration
	dial               func(context.Context, *url.URL, []byte, []byte) (*websocket.Conn, error)
	bootID             string

	started   bool
	closed    bool
	cancel    context.CancelFunc
	closeDone chan struct{}
}

func (*publicEdgeConnector) String() string   { return "publicEdgeConnector{redacted}" }
func (*publicEdgeConnector) GoString() string { return "publicEdgeConnector{redacted}" }

func newPublicEdgeConnector(config publicEdgeConnectorConfig) (*publicEdgeConnector, error) {
	if !validPublicEdgeEndpoint(config.Endpoint) ||
		!publicEdgeGatewayIDPattern.MatchString(config.GatewayID) || config.PrivateKey == nil || config.Source == nil ||
		!validPublicEdgeP256PrivateKey(config.PrivateKey) ||
		!validPublicEdgeAccessValue(config.AccessClientID) ||
		!validPublicEdgeAccessValue(config.AccessClientSecret) {
		return nil, errPublicEdgeConfiguration
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.IDReader == nil {
		config.IDReader = rand.Reader
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = publicEdgeHeartbeatInterval
	}
	if config.ReconnectMinimum == 0 {
		config.ReconnectMinimum = publicEdgeReconnectMinimum
	}
	if config.ReconnectMaximum == 0 {
		config.ReconnectMaximum = publicEdgeReconnectMaximum
	}
	if config.HeartbeatInterval < 10*time.Millisecond || config.HeartbeatInterval > publicEdgeMaximumHeartbeat ||
		config.ReconnectMinimum < 10*time.Millisecond ||
		config.ReconnectMaximum < config.ReconnectMinimum || config.ReconnectMaximum > 5*time.Minute {
		return nil, errPublicEdgeConfiguration
	}
	if config.Dial == nil {
		config.Dial = dialPublicEdgeWebSocket
	}
	bootID, err := newPublicEdgeOpaqueID("boot_", config.IDReader)
	if err != nil {
		return nil, errPublicEdgeConfiguration
	}
	endpoint := *config.Endpoint
	connector := &publicEdgeConnector{
		endpoint: &endpoint, gatewayID: config.GatewayID,
		privateKey:         clonePublicEdgePrivateKey(config.PrivateKey),
		accessClientID:     append([]byte(nil), config.AccessClientID...),
		accessClientSecret: append([]byte(nil), config.AccessClientSecret...),
		source:             config.Source, now: config.Now, idReader: config.IDReader,
		heartbeatInterval: config.HeartbeatInterval,
		reconnectMinimum:  config.ReconnectMinimum, reconnectMaximum: config.ReconnectMaximum,
		dial: config.Dial, bootID: bootID, closeDone: make(chan struct{}),
	}
	return connector, nil
}

func (connector *publicEdgeConnector) Start(parent context.Context) error {
	if connector == nil || parent == nil {
		return errPublicEdgeConfiguration
	}
	connector.mu.Lock()
	defer connector.mu.Unlock()
	if connector.closed || connector.started {
		return errPublicEdgeConfiguration
	}
	ctx, cancel := context.WithCancel(parent)
	connector.cancel = cancel
	connector.started = true
	connector.wg.Add(1)
	go connector.run(ctx)
	return nil
}

func (connector *publicEdgeConnector) run(ctx context.Context) {
	defer connector.wg.Done()
	backoff := connector.reconnectMinimum
	for ctx.Err() == nil {
		err := connector.runConnection(ctx, func() { backoff = connector.reconnectMinimum })
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// Do not log transport errors: lower layers may embed URLs or remote
			// response details. A fixed error code is sufficient for operators.
			log.Printf("public edge disconnected: error_code=public_edge_unavailable")
		}
		if !waitPublicEdgeBackoff(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, connector.reconnectMaximum)
	}
}

func (connector *publicEdgeConnector) runConnection(ctx context.Context, onAuthenticated func()) error {
	connectionID, err := newPublicEdgeOpaqueID("conn_", connector.idReader)
	if err != nil {
		return errPublicEdgeProtocol
	}
	session, err := publicedge.NewGatewaySession(publicedge.GatewaySessionConfig{
		ConnectionID: connectionID, GatewayID: connector.gatewayID, BootID: connector.bootID,
		PrivateKey: connector.privateKey,
	})
	if err != nil {
		return errPublicEdgeProtocol
	}
	defer session.Close()

	connection, err := connector.dial(
		ctx, connector.endpoint, connector.accessClientID, connector.accessClientSecret,
	)
	if err != nil {
		return errPublicEdgeTransport
	}
	connection.MaxPayloadBytes = publicedge.MaxFrameBytes
	connectionContext, cancelConnection := context.WithCancel(ctx)
	closeCallbackDone := make(chan struct{})
	stopClose := context.AfterFunc(connectionContext, func() {
		defer close(closeCallbackDone)
		hardClosePublicEdgeWebSocket(connection)
	})
	var readerDone chan struct{}
	defer func() {
		cancelConnection()
		if !stopClose() {
			<-closeCallbackDone
		} else {
			hardClosePublicEdgeWebSocket(connection)
		}
		if readerDone != nil {
			<-readerDone
		}
	}()

	now := connector.now()
	hello, err := session.Hello(now)
	if err != nil || sendPublicEdgeFrame(connection, hello, now.Add(publicEdgeWriteTimeout)) != nil {
		return errPublicEdgeProtocol
	}
	if err := connection.SetReadDeadline(now.Add(publicEdgeHandshakeTimeout)); err != nil {
		return errPublicEdgeTransport
	}
	challenge, err := receivePublicEdgeFrame(connection)
	if err != nil {
		return errPublicEdgeProtocol
	}
	authenticate, err := session.AcceptChallenge(connectionID, challenge, connector.now())
	if err != nil || sendPublicEdgeFrame(connection, authenticate, connector.now().Add(publicEdgeWriteTimeout)) != nil {
		return errPublicEdgeProtocol
	}
	acknowledgement, err := receivePublicEdgeFrame(connection)
	if err != nil || session.AcceptAuthenticated(connectionID, acknowledgement, connector.now()) != nil {
		return errPublicEdgeProtocol
	}
	if onAuthenticated != nil {
		onAuthenticated()
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return errPublicEdgeTransport
	}

	lastState := connector.source()
	revision := uint64(1)
	eventHighWater := uint64(0)
	if err := connector.sendSnapshot(connection, session, connectionID, lastState, revision, eventHighWater); err != nil {
		return err
	}

	ticker := time.NewTicker(connector.heartbeatInterval)
	defer ticker.Stop()
	type inboundResult struct {
		frame publicedge.Frame
		err   error
	}
	inbound := make(chan inboundResult, 1)
	readerDone = make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			frame, receiveErr := receivePublicEdgeFrame(connection)
			select {
			case inbound <- inboundResult{frame: frame, err: receiveErr}:
			case <-connectionContext.Done():
				return
			}
			if receiveErr != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-inbound:
			if result.err != nil || session.AcceptOperationProbe(connectionID, result.frame, connector.now()) != nil {
				return errPublicEdgeProtocol
			}
			// This first public slice has no operation lookup source. Even a
			// correctly signed probe is therefore terminal rather than becoming
			// an accidental command or arbitrary-data tunnel.
			return errPublicEdgeProtocol
		case <-ticker.C:
			current := connector.source()
			if current != lastState {
				if revision == publicedge.MaxWireCounter {
					return errPublicEdgeProtocol
				}
				revision++
				lastState = current
			}
			if err := connector.sendSnapshot(connection, session, connectionID, current, revision, eventHighWater); err != nil {
				return err
			}
		}
	}
}

func (connector *publicEdgeConnector) sendSnapshot(
	connection *websocket.Conn,
	session *publicedge.GatewaySession,
	connectionID string,
	state publicEdgeCoarseState,
	revision, eventHighWater uint64,
) error {
	now := connector.now()
	frame, err := session.Snapshot(connectionID, publicedge.SnapshotPayload{
		ObservedAt: now.Unix(), Revision: revision, EventHighWater: eventHighWater,
		Service: state.Service, Call: state.Call, SMS: state.SMS,
	}, now)
	if err != nil || sendPublicEdgeFrame(connection, frame, now.Add(publicEdgeWriteTimeout)) != nil {
		return errPublicEdgeProtocol
	}
	return nil
}

func (connector *publicEdgeConnector) Close() error {
	if connector == nil {
		return nil
	}
	connector.mu.Lock()
	if connector.closed {
		closeDone := connector.closeDone
		connector.mu.Unlock()
		if closeDone != nil {
			<-closeDone
		}
		return nil
	}
	connector.closed = true
	cancel := connector.cancel
	connector.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	connector.wg.Wait()
	connector.mu.Lock()
	zeroPublicEdgeBytes(connector.accessClientID)
	zeroPublicEdgeBytes(connector.accessClientSecret)
	connector.accessClientID = nil
	connector.accessClientSecret = nil
	if connector.privateKey != nil && connector.privateKey.D != nil {
		connector.privateKey.D.SetInt64(0)
	}
	connector.privateKey = nil
	if connector.closeDone != nil {
		close(connector.closeDone)
	}
	connector.mu.Unlock()
	return nil
}

func dialPublicEdgeWebSocket(
	ctx context.Context,
	endpoint *url.URL,
	accessClientID, accessClientSecret []byte,
) (*websocket.Conn, error) {
	if ctx == nil || endpoint == nil || endpoint.Scheme != "wss" || endpoint.Host != publicEdgeHost ||
		endpoint.Path != publicedge.GatewayWebSocketPath || !validPublicEdgeAccessValue(accessClientID) ||
		!validPublicEdgeAccessValue(accessClientSecret) {
		return nil, errPublicEdgeConfiguration
	}
	return dialPublicEdgeWebSocketWithTransport(
		ctx, endpoint, accessClientID, accessClientSecret,
		publicEdgeDialTimeout, openPublicEdgeTLSTransport,
	)
}

type publicEdgeTransportOpener func(context.Context, *url.URL) (net.Conn, error)

func openPublicEdgeTLSTransport(ctx context.Context, endpoint *url.URL) (net.Conn, error) {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	rawConnection, err := dialer.DialContext(
		ctx, "tcp", net.JoinHostPort(endpoint.Hostname(), "443"),
	)
	if err != nil {
		return nil, err
	}
	tlsConnection := tls.Client(rawConnection, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: endpoint.Hostname(),
	})
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		_ = rawConnection.Close()
		return nil, err
	}
	return tlsConnection, nil
}

func dialPublicEdgeWebSocketWithTransport(
	ctx context.Context,
	endpoint *url.URL,
	accessClientID, accessClientSecret []byte,
	timeout time.Duration,
	openTransport publicEdgeTransportOpener,
) (*websocket.Conn, error) {
	if ctx == nil || openTransport == nil || timeout <= 0 || timeout > publicEdgeDialTimeout ||
		!validPublicEdgeEndpoint(endpoint) || !validPublicEdgeAccessValue(accessClientID) ||
		!validPublicEdgeAccessValue(accessClientSecret) {
		return nil, errPublicEdgeConfiguration
	}
	dialContext, cancelDial := context.WithTimeout(ctx, timeout)
	defer cancelDial()
	transport, err := openTransport(dialContext, endpoint)
	if err != nil {
		return nil, errPublicEdgeTransport
	}
	capture := &publicEdgeHandshakeCaptureConn{Conn: transport}
	origin := "https://" + endpoint.Host
	config, err := websocket.NewConfig(endpoint.String(), origin)
	if err != nil {
		_ = capture.Close()
		return nil, errPublicEdgeConfiguration
	}
	config.Protocol = []string{publicedge.GatewayWebSocketSubprotocol}
	config.Header = make(http.Header)
	config.Header.Set("CF-Access-Client-Id", string(accessClientID))
	config.Header.Set("CF-Access-Client-Secret", string(accessClientSecret))
	if deadline, ok := dialContext.Deadline(); ok {
		if err := capture.SetDeadline(deadline); err != nil {
			_ = capture.Close()
			return nil, errPublicEdgeTransport
		}
	}
	deadlineCallbackDone := make(chan struct{})
	stopDeadlineCallback := context.AfterFunc(dialContext, func() {
		defer close(deadlineCallbackDone)
		_ = capture.SetDeadline(time.Now())
	})
	connection, err := websocket.NewClient(config, capture)
	config.Header.Del("CF-Access-Client-Id")
	config.Header.Del("CF-Access-Client-Secret")
	if !stopDeadlineCallback() {
		<-deadlineCallbackDone
	}
	if err != nil {
		_ = capture.Close()
		if errors.Is(err, errPublicEdgeProtocol) || capture.didOverflow() {
			return nil, errPublicEdgeProtocol
		}
		return nil, errPublicEdgeTransport
	}
	if dialContext.Err() != nil {
		_ = connection.Close()
		return nil, errPublicEdgeTransport
	}
	if !capture.consumeSelectedProtocol(publicedge.GatewayWebSocketSubprotocol) {
		_ = connection.Close()
		return nil, errPublicEdgeProtocol
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_ = connection.Close()
		return nil, errPublicEdgeTransport
	}
	connection.MaxPayloadBytes = publicedge.MaxFrameBytes
	return connection, nil
}

type publicEdgeHandshakeCaptureConn struct {
	net.Conn
	mu       sync.Mutex
	header   []byte
	complete bool
	overflow bool
}

func (connection *publicEdgeHandshakeCaptureConn) Read(destination []byte) (int, error) {
	read, err := connection.Conn.Read(destination)
	if read > 0 {
		accepted, captureErr := connection.capture(destination[:read])
		if captureErr != nil {
			_ = connection.Conn.SetDeadline(time.Now())
			_ = connection.Conn.Close()
			return accepted, captureErr
		}
	}
	return read, err
}

func (connection *publicEdgeHandshakeCaptureConn) capture(data []byte) (int, error) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.complete {
		return len(data), nil
	}
	if connection.overflow {
		return 0, errPublicEdgeProtocol
	}
	remaining := publicEdgeHandshakeHeaderMaximum - len(connection.header)
	if remaining <= 0 {
		connection.overflow = true
		return 0, errPublicEdgeProtocol
	}
	accepted := len(data)
	if len(data) > remaining {
		accepted = remaining
	}
	connection.header = append(connection.header, data[:accepted]...)
	if end := bytes.Index(connection.header, []byte("\r\n\r\n")); end >= 0 {
		connection.header = connection.header[:end+4]
		connection.complete = true
		return len(data), nil
	}
	if len(connection.header) >= publicEdgeHandshakeHeaderMaximum || accepted < len(data) {
		connection.overflow = true
		return accepted, errPublicEdgeProtocol
	}
	return len(data), nil
}

func (connection *publicEdgeHandshakeCaptureConn) didOverflow() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.overflow
}

func (connection *publicEdgeHandshakeCaptureConn) consumeSelectedProtocol(expected string) bool {
	connection.mu.Lock()
	header := append([]byte(nil), connection.header...)
	complete := connection.complete
	overflow := connection.overflow
	zeroPublicEdgeBytes(connection.header)
	connection.header = nil
	connection.mu.Unlock()
	defer zeroPublicEdgeBytes(header)
	if !complete || overflow || len(header) == 0 {
		return false
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), nil)
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	protocols := response.Header.Values("Sec-WebSocket-Protocol")
	return len(protocols) == 1 && protocols[0] == expected
}

func hardClosePublicEdgeWebSocket(connection *websocket.Conn) {
	if connection == nil {
		return
	}
	// SetDeadline reaches the underlying net.Conn without taking x/net's write
	// mutex. It therefore interrupts an automatic Pong already blocked while
	// holding that mutex before Close attempts to send its own control frame.
	_ = connection.SetDeadline(time.Now())
	_ = connection.Close()
}

func sendPublicEdgeFrame(connection *websocket.Conn, frame publicedge.Frame, deadline time.Time) error {
	if connection == nil {
		return errPublicEdgeTransport
	}
	payload, err := publicedge.EncodeFrame(frame)
	if err != nil || connection.SetWriteDeadline(deadline) != nil {
		return errPublicEdgeProtocol
	}
	sendErr := publicEdgeTextMessage.Send(connection, string(payload))
	controlDeadlineErr := connection.SetWriteDeadline(time.Now().Add(publicEdgeControlWriteWindow))
	if sendErr != nil || controlDeadlineErr != nil {
		return errPublicEdgeTransport
	}
	return nil
}

func receivePublicEdgeFrame(connection *websocket.Conn) (publicedge.Frame, error) {
	if connection == nil {
		return publicedge.Frame{}, errPublicEdgeTransport
	}
	var payload string
	if err := publicEdgeTextMessage.Receive(connection, &payload); err != nil {
		if errors.Is(err, errPublicEdgeProtocol) {
			return publicedge.Frame{}, errPublicEdgeProtocol
		}
		return publicedge.Frame{}, errPublicEdgeTransport
	}
	frame, err := publicedge.DecodeFrame([]byte(payload))
	if err != nil {
		return publicedge.Frame{}, errPublicEdgeProtocol
	}
	return frame, nil
}

func loadPublicEdgePrivateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := readExternalVoicePasswordFile(path, publicEdgeKeyMaximumBytes)
	if err != nil {
		return nil, errPublicEdgeConfiguration
	}
	defer zeroPublicEdgeBytes(data)
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errPublicEdgeConfiguration
	}
	defer zeroPublicEdgeBytes(block.Bytes)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errPublicEdgeConfiguration
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || !validPublicEdgeP256PrivateKey(key) {
		return nil, errPublicEdgeConfiguration
	}
	return key, nil
}

func loadPublicEdgeAccessValue(path string) ([]byte, error) {
	data, err := readExternalVoicePasswordFile(path, publicEdgeAccessMaximumBytes)
	if err != nil {
		return nil, errPublicEdgeConfiguration
	}
	trimmed := strings.TrimSuffix(string(data), "\n")
	zeroPublicEdgeBytes(data)
	if !publicEdgeAccessValuePattern.MatchString(trimmed) {
		return nil, errPublicEdgeConfiguration
	}
	return []byte(trimmed), nil
}

func validPublicEdgeAccessValue(value []byte) bool {
	return len(value) >= 8 && len(value) <= publicEdgeAccessMaximumBytes &&
		publicEdgeAccessValuePattern.Match(value)
}

func validPublicEdgeP256PrivateKey(key *ecdsa.PrivateKey) bool {
	if key == nil || key.Curve != elliptic.P256() || key.D == nil || key.D.Sign() <= 0 ||
		key.D.Cmp(key.Curve.Params().N) >= 0 || key.X == nil || key.Y == nil ||
		!key.Curve.IsOnCurve(key.X, key.Y) {
		return false
	}
	x, y := key.Curve.ScalarBaseMult(key.D.Bytes())
	return x.Cmp(key.X) == 0 && y.Cmp(key.Y) == 0
}

func clonePublicEdgePrivateKey(key *ecdsa.PrivateKey) *ecdsa.PrivateKey {
	if !validPublicEdgeP256PrivateKey(key) {
		return nil
	}
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(), X: new(big.Int).Set(key.X), Y: new(big.Int).Set(key.Y),
		},
		D: new(big.Int).Set(key.D),
	}
}

func validPublicEdgeEndpoint(endpoint *url.URL) bool {
	return endpoint != nil && endpoint.Scheme == "wss" && endpoint.User == nil &&
		endpoint.Host == publicEdgeHost && endpoint.Hostname() == publicEdgeHost && endpoint.Port() == "" &&
		endpoint.Path == publicedge.GatewayWebSocketPath && endpoint.RawPath == "" &&
		endpoint.RawQuery == "" && !endpoint.ForceQuery && endpoint.Fragment == "" && endpoint.Opaque == "" &&
		endpoint.String() == publicEdgeEndpoint()
}

func newPublicEdgeOpaqueID(prefix string, reader io.Reader) (string, error) {
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", err
	}
	encoded := prefix + base64.RawURLEncoding.EncodeToString(buffer)
	zeroPublicEdgeBytes(buffer)
	return encoded, nil
}

func zeroPublicEdgeBytes(data []byte) {
	for index := range data {
		data[index] = 0
	}
}

func waitPublicEdgeBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *app) configurePublicEdgeStartup(config *publicEdgeStartupConfig) {
	a.publicEdgeStartup = config
}

func (a *app) startPublicEdge(parent context.Context) error {
	if a == nil || a.publicEdgeStartup == nil {
		return nil
	}
	privateKey, err := loadPublicEdgePrivateKey(a.publicEdgeStartup.privateKeyFile)
	if err != nil {
		return err
	}
	defer privateKey.D.SetInt64(0)
	clientID, err := loadPublicEdgeAccessValue(a.publicEdgeStartup.accessClientIDFile)
	if err != nil {
		return err
	}
	defer zeroPublicEdgeBytes(clientID)
	clientSecret, err := loadPublicEdgeAccessValue(a.publicEdgeStartup.accessClientSecretFile)
	if err != nil {
		return err
	}
	defer zeroPublicEdgeBytes(clientSecret)
	connector, err := newPublicEdgeConnector(publicEdgeConnectorConfig{
		Endpoint: a.publicEdgeStartup.endpoint, GatewayID: a.publicEdgeStartup.gatewayID,
		PrivateKey: privateKey, AccessClientID: clientID, AccessClientSecret: clientSecret,
		Source: a.publicEdgeCoarseState,
	})
	if err != nil {
		return err
	}
	if err := connector.Start(parent); err != nil {
		_ = connector.Close()
		return err
	}
	a.publicEdge = connector
	return nil
}

func (a *app) publicEdgeCoarseState() publicEdgeCoarseState {
	state := publicEdgeCoarseState{
		Service: publicedge.ServiceReady,
		Call:    publicedge.CallUnavailable,
		SMS:     publicedge.SMSUnavailable,
	}
	if a.demo {
		state.SMS = publicedge.SMSReady
	} else if available, code := a.smsStoreStatus(); available {
		state.SMS = publicedge.SMSReady
	} else if code != "" {
		state.SMS = publicedge.SMSDegraded
	}

	if a.sipVoice != nil {
		voice := a.sipVoice.Status("")
		if voice.RecoveryRequired || voice.Health == "manual_recovery_required" {
			state.Call = publicedge.CallRecoveryRequired
		} else if voice.Call == nil {
			if voice.Health == "connected" {
				state.Call = publicedge.CallIdle
			}
		} else if voice.Call.ReconcileRequired {
			state.Call = publicedge.CallRecoveryRequired
		} else {
			switch voice.Call.Phase {
			case sipgateway.PhaseIncomingRinging, sipgateway.PhaseMediaPreparing, sipgateway.PhaseMediaReady:
				state.Call = publicedge.CallRinging
			case sipgateway.PhaseAnswerPending:
				state.Call = publicedge.CallConnecting
			case sipgateway.PhaseActiveUnverified, sipgateway.PhaseActiveTransportVerified,
				sipgateway.PhaseActiveUnmanaged, sipgateway.PhaseEnding:
				state.Call = publicedge.CallActive
			case sipgateway.PhaseReconciling:
				state.Call = publicedge.CallRecoveryRequired
			case sipgateway.PhaseEnded:
				state.Call = publicedge.CallIdle
			}
		}
	}
	device := a.snapshotDeviceState()
	if state.Call == publicedge.CallRecoveryRequired {
		state.Service = publicedge.ServiceRecoveryRequired
	} else if state.SMS != publicedge.SMSReady || (!a.demo && device.DiscoveryError != "") {
		state.Service = publicedge.ServiceDegraded
	}
	return state
}

func (a *app) closePublicEdge() {
	if a == nil || a.publicEdge == nil {
		return
	}
	_ = a.publicEdge.Close()
	a.publicEdge = nil
}

func publicEdgeEndpoint() string {
	return fmt.Sprintf("wss://%s%s", publicEdgeHost, publicedge.GatewayWebSocketPath)
}
