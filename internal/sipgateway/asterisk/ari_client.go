package asterisk

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

type pbxIdentity struct {
	EntityID    string
	Version     string
	StartupTime string
}

type ariInfo struct {
	System struct {
		EntityID string `json:"entity_id"`
		Version  string `json:"version"`
	} `json:"system"`
	Status struct {
		StartupTime    string `json:"startup_time"`
		LastReloadTime string `json:"last_reload_time"`
	} `json:"status"`
}

type ariHTTPClient struct {
	baseURL     *url.URL
	username    string
	password    string
	client      *http.Client
	maxBody     int64
	httpTimeout time.Duration
	dialer      *net.Dialer
	tlsConfig   *tls.Config
}

type httpResponse struct {
	status int
	body   []byte
}

func newARIHTTPClient(cfg normalizedConfig) *ariHTTPClient {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           cfg.dialer.DialContext,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       0,
		TLSHandshakeTimeout:   cfg.httpTimeout,
		ResponseHeaderTimeout: cfg.httpTimeout,
		ExpectContinueTimeout: 0,
		TLSClientConfig:       cfg.tlsConfig,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.httpTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &ariHTTPClient{
		baseURL: cfg.baseURL, username: cfg.username, password: cfg.password,
		client: client, maxBody: cfg.maxHTTPBodyBytes, httpTimeout: cfg.httpTimeout,
		dialer:    cfg.dialer,
		tlsConfig: cfg.tlsConfig,
	}
}

func (c *ariHTTPClient) close() {
	if transport, ok := c.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func (c *ariHTTPClient) inspectPBX(
	ctx context.Context,
	expectedVersion string,
	expectedEntityID string,
	expectedStartupTime string,
) (pbxIdentity, error) {
	response, err := c.do(ctx, http.MethodGet, []string{"asterisk", "info"}, url.Values{
		"only": {"system,status"},
	})
	if err != nil {
		return pbxIdentity{}, err
	}
	return parsePBXInfoResponse(response, expectedVersion, expectedEntityID, expectedStartupTime)
}

func parsePBXInfoResponse(
	response httpResponse,
	expectedVersion string,
	expectedEntityID string,
	expectedStartupTime string,
) (pbxIdentity, error) {
	if response.status != http.StatusOK {
		return pbxIdentity{}, ErrTransport
	}
	var info ariInfo
	if err := decodeExactJSON(response.body, &info); err != nil ||
		!validOpaque(info.System.EntityID, 1, 256) || !validOpaque(info.System.Version, 1, 128) ||
		!validAsteriskTime(info.Status.StartupTime) {
		return pbxIdentity{}, ErrProtocol
	}
	identity := pbxIdentity{
		EntityID: info.System.EntityID, Version: info.System.Version,
		StartupTime: info.Status.StartupTime,
	}
	if identity.Version != expectedVersion ||
		(expectedEntityID != "" && identity.EntityID != expectedEntityID) ||
		(expectedStartupTime != "" && identity.StartupTime != expectedStartupTime) {
		return pbxIdentity{}, ErrProtocol
	}
	return identity, nil
}

func (c *ariHTTPClient) do(
	ctx context.Context,
	method string,
	segments []string,
	query url.Values,
) (httpResponse, error) {
	if err := contextErr(ctx); err != nil {
		return httpResponse{}, err
	}
	target := appendURLSegments(c.baseURL, segments...)
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return httpResponse{}, ErrProtocol
	}
	request.SetBasicAuth(c.username, c.password)
	request.Header.Set("Accept", "application/json")
	// A new connection for every request prevents net/http's stale pooled
	// connection recovery from turning one logical mutation into two writes.
	request.Close = true
	response, err := c.client.Do(request)
	if err != nil {
		if contextErr(ctx) != nil {
			return httpResponse{}, ctx.Err()
		}
		return httpResponse{}, ErrTransport
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil {
		return httpResponse{status: response.StatusCode}, ErrTransport
	}
	if int64(len(body)) > c.maxBody {
		return httpResponse{status: response.StatusCode}, ErrProtocol
	}
	return httpResponse{status: response.StatusCode, body: body}, nil
}

func (c *ariHTTPClient) dialEvents(ctx context.Context, application string) (*websocket.Conn, error) {
	query := url.Values{"app": {application}, "subscribeAll": {"false"}}
	return c.dialWebsocket(ctx, appendURLSegments(c.baseURL, "events"), query, "", true, maxARIEventBytes)
}

func (c *ariHTTPClient) dialMedia(ctx context.Context, connectionID string) (*websocket.Conn, error) {
	base := *c.baseURL
	base.Path = ""
	base.RawPath = ""
	return c.dialWebsocket(
		ctx,
		appendURLSegments(&base, "media", connectionID),
		nil,
		"media",
		true,
		maxMediaMessageBytes,
	)
}

func (c *ariHTTPClient) dialWebsocket(
	ctx context.Context,
	target *url.URL,
	query url.Values,
	protocol string,
	withAuthorization bool,
	maxPayload int,
) (*websocket.Conn, error) {
	location := *target
	switch location.Scheme {
	case "http":
		location.Scheme = "ws"
	case "https":
		location.Scheme = "wss"
	default:
		return nil, ErrConfiguration
	}
	location.RawQuery = query.Encode()
	origin := *c.baseURL
	origin.Path = "/"
	origin.RawPath = ""
	origin.RawQuery = ""
	config, err := websocket.NewConfig(location.String(), origin.String())
	if err != nil {
		return nil, ErrProtocol
	}
	if protocol != "" {
		config.Protocol = []string{protocol}
	}
	if withAuthorization {
		encoded := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
		config.Header.Set("Authorization", "Basic "+encoded)
	}
	config.Dialer = c.dialer
	config.TlsConfig = cloneTLSConfig(c.tlsConfig)

	// DialContext on x/net/websocket performs its own bounded TCP/TLS dial. Its
	// error embeds the URL, so never return or wrap that error.
	connection, err := config.DialContext(ctx)
	if err != nil {
		if contextErr(ctx) != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTransport
	}
	connection.MaxPayloadBytes = maxPayload
	return connection, nil
}

func appendURLSegments(base *url.URL, segments ...string) *url.URL {
	copyURL := *base
	escapedPath := strings.TrimRight(copyURL.EscapedPath(), "/")
	for _, segment := range segments {
		escapedPath += "/" + url.PathEscape(segment)
	}
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		// All callers pass either constants, random URL-safe identifiers, or
		// already validated provider identifiers; keep this branch fail-closed.
		decodedPath = "/invalid"
		escapedPath = ""
	}
	copyURL.Path = decodedPath
	copyURL.RawPath = escapedPath
	return &copyURL
}

func cloneTLSConfig(config *tls.Config) *tls.Config {
	if config == nil {
		return nil
	}
	return config.Clone()
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return ErrConfiguration
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func withMaxTimeout(ctx context.Context, maximum time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= maximum {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, maximum)
}

func status2xx(status int) bool { return status >= 200 && status < 300 }

func status4xx(status int) bool { return status >= 400 && status < 500 }

func stableWebsocketError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if contextErr(ctx) != nil {
		return ctx.Err()
	}
	if errors.Is(err, websocket.ErrFrameTooLarge) {
		return ErrProtocol
	}
	return ErrTransport
}
