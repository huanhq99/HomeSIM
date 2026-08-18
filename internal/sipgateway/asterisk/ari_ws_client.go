package asterisk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
	"golang.org/x/net/websocket"
)

// ariRESTRequest and ariRESTResponse implement Asterisk's native ARI REST over
// WebSocket protocol. Keeping every live REST request on the already-verified
// event connection is the incarnation boundary: once that socket is lost the
// client is terminal and never falls back to HTTP or opens a replacement event
// socket.
type ariRESTRequest struct {
	Type          string `json:"type"`
	TransactionID string `json:"transaction_id"`
	RequestID     string `json:"request_id"`
	Method        string `json:"method"`
	URI           string `json:"uri"`
}

type ariRESTResponse struct {
	Type          string `json:"type"`
	TransactionID string `json:"transaction_id"`
	RequestID     string `json:"request_id"`
	StatusCode    int    `json:"status_code"`
	ReasonPhrase  string `json:"reason_phrase"`
	URI           string `json:"uri"`
	ContentType   string `json:"content_type"`
	MessageBody   string `json:"message_body"`
	Timestamp     string `json:"timestamp"`
	AsteriskID    string `json:"asterisk_id"`
	Application   string `json:"application"`
}

type ariWireEnvelope struct {
	Type         string `json:"type"`
	AsteriskID   string `json:"asterisk_id"`
	Application  string `json:"application"`
	Timestamp    string `json:"timestamp"`
	RequestID    string `json:"request_id"`
	URI          string `json:"uri"`
	StatusCode   int    `json:"status_code"`
	MessageBody  string `json:"message_body"`
	ContentType  string `json:"content_type"`
	ReasonPhrase string `json:"reason_phrase"`
}

type ariWSPending struct {
	transactionID string
	uri           string
	response      chan ariRESTResponse
}

const maxARIWSPending = 64

type ariWSClient struct {
	conn           *websocket.Conn
	application    string
	entityID       string
	transactionID  string
	maxBody        int64
	requestTimeout time.Duration

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]ariWSPending
	events  chan ariEvent
	done    chan struct{}
	closed  bool
	err     error
	onFail  func(error)

	requestCounter atomic.Uint64
	closeOnce      sync.Once
	readerDone     chan struct{}
}

func newARIWSClient(conn *websocket.Conn, cfg normalizedConfig, entityID string) (*ariWSClient, error) {
	if conn == nil || !validOpaque(entityID, 1, 256) {
		return nil, ErrConfiguration
	}
	random := make([]byte, 24)
	if _, err := io.ReadFull(cfg.idReader, random); err != nil {
		return nil, ErrConfiguration
	}
	client := &ariWSClient{
		conn: conn, application: cfg.application, entityID: entityID,
		transactionID: "dj1tx_" + base64.RawURLEncoding.EncodeToString(random),
		maxBody:       cfg.maxHTTPBodyBytes, requestTimeout: cfg.httpTimeout,
		pending: make(map[string]ariWSPending), events: make(chan ariEvent, maxCleanStartupEvents),
		done: make(chan struct{}), readerDone: make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

func (c *ariWSClient) setFailureHandler(handler func(error)) {
	c.mu.Lock()
	c.onFail = handler
	closed, err := c.closed, c.err
	c.mu.Unlock()
	if closed && handler != nil && !errors.Is(err, sipgateway.ErrClosed) {
		go handler(err)
	}
}

func (c *ariWSClient) do(
	ctx context.Context,
	method string,
	segments []string,
	query url.Values,
) (httpResponse, error) {
	if err := contextErr(ctx); err != nil {
		return httpResponse{}, err
	}
	ctx, cancel := withMaxTimeout(ctx, c.requestTimeout)
	defer cancel()
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodDelete {
		return httpResponse{}, ErrProtocol
	}
	uri, err := ariRESTURI(segments, query)
	if err != nil {
		return httpResponse{}, err
	}
	requestID := c.transactionID + "_" + strconv.FormatUint(c.requestCounter.Add(1), 36)
	pending := ariWSPending{
		transactionID: c.transactionID, uri: uri,
		response: make(chan ariRESTResponse, 1),
	}
	c.mu.Lock()
	if c.closed {
		terminal := c.terminalErrorLocked()
		c.mu.Unlock()
		return httpResponse{}, terminal
	}
	if _, duplicate := c.pending[requestID]; duplicate || len(c.pending) >= maxARIWSPending {
		c.mu.Unlock()
		c.fail(errors.Join(sipgateway.ErrRecoveryRequired, ErrProtocol))
		return httpResponse{}, sipgateway.ErrRecoveryRequired
	}
	c.pending[requestID] = pending
	c.mu.Unlock()

	request := ariRESTRequest{
		Type: "RESTRequest", TransactionID: c.transactionID,
		RequestID: requestID, Method: method, URI: uri,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		c.fail(errors.Join(sipgateway.ErrRecoveryRequired, ErrProtocol))
		return httpResponse{}, sipgateway.ErrRecoveryRequired
	}
	c.writeMu.Lock()
	if deadline, ok := ctx.Deadline(); !ok || c.conn.SetWriteDeadline(deadline) != nil {
		c.writeMu.Unlock()
		c.fail(errors.Join(sipgateway.ErrRecoveryRequired, ErrTransport))
		return httpResponse{}, sipgateway.ErrRecoveryRequired
	}
	err = websocket.Message.Send(c.conn, string(payload))
	c.writeMu.Unlock()
	if err != nil {
		c.fail(errors.Join(sipgateway.ErrRecoveryRequired, stableWebsocketError(ctx, err)))
		return httpResponse{}, sipgateway.ErrRecoveryRequired
	}

	select {
	case <-ctx.Done():
		// Once bytes were written, a missing response is ambiguous even for a GET:
		// close the one incarnation-bound socket and never accept a late response.
		c.fail(errors.Join(sipgateway.ErrRecoveryRequired, ctx.Err()))
		return httpResponse{}, ctx.Err()
	case <-c.done:
		return httpResponse{}, c.terminalError()
	case response := <-pending.response:
		select {
		case <-c.done:
			return httpResponse{}, c.terminalError()
		default:
		}
		return httpResponse{status: response.StatusCode, body: []byte(response.MessageBody)}, nil
	}
}

func (c *ariWSClient) inspectPBX(
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
	identity, err := parsePBXInfoResponse(response, expectedVersion, expectedEntityID, expectedStartupTime)
	if err != nil {
		failure := errors.Join(sipgateway.ErrRecoveryRequired, err)
		c.fail(failure)
		return pbxIdentity{}, failure
	}
	return identity, nil
}

func (c *ariWSClient) nextEvent(ctx context.Context) (ariEvent, error) {
	if err := contextErr(ctx); err != nil {
		return ariEvent{}, err
	}
	select {
	case <-c.done:
		return ariEvent{}, c.terminalError()
	default:
	}
	select {
	case <-ctx.Done():
		return ariEvent{}, ctx.Err()
	case <-c.done:
		return ariEvent{}, c.terminalError()
	case event := <-c.events:
		select {
		case <-c.done:
			return ariEvent{}, c.terminalError()
		default:
			return event, nil
		}
	}
}

func (c *ariWSClient) readLoop() {
	defer close(c.readerDone)
	for {
		var message framedMessage
		if err := receiveFrame.Receive(c.conn, &message); err != nil {
			c.fail(errors.Join(sipgateway.ErrRecoveryRequired, stableWebsocketError(context.Background(), err)))
			return
		}
		if message.payloadType != websocket.TextFrame || len(message.data) == 0 {
			c.fail(wsProtocolFailure("non-text-frame"))
			return
		}
		var envelope ariWireEnvelope
		if err := decodeOneJSON(message.data, &envelope); err != nil ||
			!validOpaque(envelope.Type, 1, 128) ||
			!validOpaque(envelope.AsteriskID, 1, 256) || envelope.AsteriskID != c.entityID ||
			envelope.Application != c.application || !validAsteriskTime(envelope.Timestamp) {
			c.fail(wsProtocolFailure("invalid-envelope"))
			return
		}
		if envelope.Type == "RESTResponse" {
			var response ariRESTResponse
			if err := decodeOneJSON(message.data, &response); err != nil ||
				response.Type != "RESTResponse" || !validOpaque(response.RequestID, 1, 256) ||
				!validOpaque(response.TransactionID, 1, 256) ||
				!validRESTURI(response.URI) || response.StatusCode < 100 || response.StatusCode > 599 ||
				!validHTTPMetadata(response.ReasonPhrase, 1, 256) || int64(len(response.MessageBody)) > c.maxBody ||
				(response.ContentType != "" && !validHTTPMetadata(response.ContentType, 1, 256)) {
				c.fail(wsProtocolFailure("invalid-rest-response"))
				return
			}
			c.mu.Lock()
			pending, ok := c.pending[response.RequestID]
			uriMatches := ok && equivalentRESTResponseURI(response.URI, pending.uri)
			if ok && response.TransactionID == pending.transactionID && uriMatches {
				delete(c.pending, response.RequestID)
			}
			c.mu.Unlock()
			if !ok {
				c.fail(wsProtocolFailure("unknown-rest-request"))
				return
			}
			if response.TransactionID != pending.transactionID {
				c.fail(wsProtocolFailure("rest-transaction-mismatch"))
				return
			}
			if !uriMatches {
				stage := "rest-uri-mismatch"
				if equivalentRESTPath(response.URI, pending.uri) {
					stage = "rest-query-mismatch"
				}
				c.fail(wsProtocolFailure(stage))
				return
			}
			pending.response <- response
			continue
		}
		if envelope.Type == "RESTRequest" {
			c.fail(wsProtocolFailure("inbound-rest-request"))
			return
		}
		if envelope.Type == "ApplicationReplaced" || envelope.Type == "ApplicationUnregistered" {
			c.fail(errors.Join(sipgateway.ErrRecoveryRequired, ErrProtocol))
			return
		}
		var event ariEvent
		if err := decodeOneJSON(message.data, &event); err != nil {
			c.fail(wsProtocolFailure("invalid-event"))
			return
		}
		select {
		case c.events <- event:
		default:
			c.fail(errors.Join(sipgateway.ErrRecoveryRequired, sipgateway.ErrObservationQueueFull))
			return
		}
	}
}

func wsProtocolFailure(stage string) error {
	return errors.Join(sipgateway.ErrRecoveryRequired, ErrProtocol, errors.New("asterisk: websocket "+stage))
}

func (c *ariWSClient) fail(err error) {
	if err == nil {
		err = errors.Join(sipgateway.ErrRecoveryRequired, ErrProtocol)
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.err = err
		handler := c.onFail
		close(c.done)
		c.mu.Unlock()
		_ = c.conn.Close()
		if handler != nil && !errors.Is(err, sipgateway.ErrClosed) {
			go handler(err)
		}
	})
}

func (c *ariWSClient) close() {
	if c == nil {
		return
	}
	c.fail(sipgateway.ErrClosed)
	<-c.readerDone
}

func (c *ariWSClient) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminalErrorLocked()
}

func (c *ariWSClient) terminalErrorLocked() error {
	if c.err != nil {
		return c.err
	}
	return sipgateway.ErrClosed
}

func ariRESTURI(segments []string, query url.Values) (string, error) {
	if len(segments) == 0 {
		return "", ErrProtocol
	}
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		if !validOpaque(segment, 1, 256) {
			return "", ErrProtocol
		}
		escaped = append(escaped, url.PathEscape(segment))
	}
	uri := strings.Join(escaped, "/")
	if len(query) != 0 {
		uri += "?" + query.Encode()
	}
	if !validRESTURI(uri) {
		return "", ErrProtocol
	}
	return uri, nil
}

func validRESTURI(uri string) bool {
	if !validOpaque(uri, 1, 4096) || strings.HasPrefix(uri, "/") || strings.Contains(uri, "#") {
		return false
	}
	parsed, err := url.ParseRequestURI("/" + uri)
	return err == nil && parsed != nil && parsed.Path != "" && parsed.Host == "" && parsed.User == nil
}

// Asterisk echoes the REST resource path but may omit the original request's
// query string from RESTResponse. Correlation remains exact on the random
// request and transaction IDs. If Asterisk does echo a query it must be
// semantically identical after independent parsing and canonical encoding.
func equivalentRESTResponseURI(responseURI, requestURI string) bool {
	responseCanonical, responseOK := canonicalRESTURI(responseURI)
	requestCanonical, requestOK := canonicalRESTURI(requestURI)
	if !responseOK || !requestOK {
		return false
	}
	if responseCanonical == requestCanonical {
		return true
	}
	responseParsed, err := url.ParseRequestURI("/" + responseURI)
	return err == nil && responseParsed.RawQuery == "" && equivalentRESTPath(responseURI, requestURI)
}

func equivalentRESTPath(left, right string) bool {
	leftPath, leftOK := canonicalRESTPath(left)
	rightPath, rightOK := canonicalRESTPath(right)
	return leftOK && rightOK && leftPath == rightPath
}

func canonicalRESTURI(uri string) (string, bool) {
	if !validRESTURI(uri) {
		return "", false
	}
	parsed, err := url.ParseRequestURI("/" + uri)
	if err != nil || parsed == nil || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}
	canonical, ok := canonicalRESTPath(uri)
	if !ok {
		return "", false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", false
	}
	if encoded := query.Encode(); encoded != "" {
		canonical += "?" + encoded
	}
	return canonical, true
}

func canonicalRESTPath(uri string) (string, bool) {
	if !validRESTURI(uri) {
		return "", false
	}
	parsed, err := url.ParseRequestURI("/" + uri)
	if err != nil || parsed == nil || parsed.Host != "" || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}
	rawPath := strings.TrimPrefix(parsed.EscapedPath(), "/")
	rawSegments := strings.Split(rawPath, "/")
	canonicalSegments := make([]string, 0, len(rawSegments))
	for _, rawSegment := range rawSegments {
		segment, err := url.PathUnescape(rawSegment)
		if err != nil || !validOpaque(segment, 1, 256) || strings.Contains(segment, "/") {
			return "", false
		}
		canonicalSegments = append(canonicalSegments, url.PathEscape(segment))
	}
	return strings.Join(canonicalSegments, "/"), true
}

func validHTTPMetadata(value string, minLength, maxLength int) bool {
	if value != strings.TrimSpace(value) || len(value) < minLength || len(value) > maxLength {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}
