package publicedgeserver

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/publicedge"
	"golang.org/x/net/websocket"
)

const (
	testGatewayID   = "gateway-1"
	testBearerToken = "Bearer verified-test-assertion"
)

type atomicTestClock struct{ unix atomic.Int64 }

type blockingHijackResponse struct {
	header     http.Header
	connection net.Conn
	hijacked   chan struct{}
	once       sync.Once
}

func (response *blockingHijackResponse) Header() http.Header   { return response.header }
func (*blockingHijackResponse) Write(data []byte) (int, error) { return len(data), nil }
func (*blockingHijackResponse) WriteHeader(int)                {}

func (response *blockingHijackResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	response.once.Do(func() { close(response.hijacked) })
	return response.connection, bufio.NewReadWriter(
		bufio.NewReader(response.connection), bufio.NewWriter(response.connection),
	), nil
}

func newAtomicTestClock(now time.Time) *atomicTestClock {
	clock := &atomicTestClock{}
	clock.unix.Store(now.Unix())
	return clock
}

func (clock *atomicTestClock) Now() time.Time { return time.Unix(clock.unix.Load(), 0) }

func (clock *atomicTestClock) Advance(duration time.Duration) {
	clock.unix.Add(int64(duration / time.Second))
}

type serverTestHarness struct {
	server      *Server
	httpServer  *httptest.Server
	registry    *publicedge.Registry
	clock       *atomicTestClock
	gatewayKey  *ecdsa.PrivateKey
	httpClient  *http.Client
	listener    string
	closed      atomic.Bool
	cleanupOnce sync.Once
}

func newServerTestHarness(t *testing.T) *serverTestHarness {
	t.Helper()
	gatewayKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return newServerTestHarnessWithKeys(t, map[string]*ecdsa.PrivateKey{
		testGatewayID: gatewayKey,
	})
}

func newServerTestHarnessWithKeys(
	t *testing.T,
	gatewayKeys map[string]*ecdsa.PrivateKey,
) *serverTestHarness {
	t.Helper()
	publicKeys := make(map[string]*ecdsa.PublicKey, len(gatewayKeys))
	for gatewayID, privateKey := range gatewayKeys {
		publicKeys[gatewayID] = &privateKey.PublicKey
	}
	registry, err := publicedge.NewRegistry(publicedge.RegistryConfig{
		GatewayKeys: publicKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := newAtomicTestClock(time.Now().UTC().Truncate(time.Second))
	server, err := New(Config{
		Registry: registry, ExactHost: ProductionExactHost, Clock: clock.Now,
		HTTPAuth: HTTPAuthFunc(func(request *http.Request) error {
			if request.Header.Get("Authorization") != testBearerToken {
				return ErrUnauthorized
			}
			return nil
		}),
		HandshakeTimeout: 2 * time.Second,
		ReadTimeout:      2 * time.Second,
		WriteTimeout:     2 * time.Second,
		MaxConnections:   8,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.StartTLS()
	harness := &serverTestHarness{
		server: server, httpServer: httpServer, registry: registry, clock: clock,
		gatewayKey: gatewayKeys[testGatewayID], httpClient: httpServer.Client(),
		listener: httpServer.Listener.Addr().String(),
	}
	t.Cleanup(harness.cleanup)
	return harness
}

func (harness *serverTestHarness) cleanup() {
	harness.cleanupOnce.Do(func() {
		_ = harness.server.Close()
		harness.closed.Store(true)
		harness.httpServer.Close()
	})
}

func (harness *serverTestHarness) dialGateway(t *testing.T, host, subprotocol string, headers http.Header) (*websocket.Conn, error) {
	return harness.dialGatewayWithOrigin(
		t, host, "https://"+ProductionExactHost, subprotocol, headers,
	)
}

func (harness *serverTestHarness) dialGatewayWithOrigin(
	t *testing.T,
	host, origin, subprotocol string,
	headers http.Header,
) (*websocket.Conn, error) {
	t.Helper()
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	raw, err := tls.DialWithDialer(dialer, "tcp", harness.listener, &tls.Config{
		MinVersion: tls.VersionTLS12,
		// This is an in-process httptest certificate, not a production trust
		// decision. The connection is still real TLS over a real TCP socket.
		InsecureSkipVerify: true, //nolint:gosec
	})
	if err != nil {
		return nil, err
	}
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
	config, err := websocket.NewConfig("wss://"+host+publicedge.GatewayWebSocketPath, origin)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	config.Protocol = []string{subprotocol}
	if headers != nil {
		config.Header = headers.Clone()
	}
	connection, err := websocket.NewClient(config, raw)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, nil
}

type authenticatedGateway struct {
	connection   *websocket.Conn
	session      *publicedge.GatewaySession
	connectionID string
	lease        publicedge.Lease
}

func (gateway *authenticatedGateway) close() {
	if gateway == nil {
		return
	}
	if gateway.connection != nil {
		_ = gateway.connection.Close()
	}
	if gateway.session != nil {
		gateway.session.Close()
	}
}

func (harness *serverTestHarness) authenticateGateway(t *testing.T, bootID string) *authenticatedGateway {
	return harness.authenticateGatewayAs(t, testGatewayID, harness.gatewayKey, bootID)
}

func (harness *serverTestHarness) authenticateGatewayAs(
	t *testing.T,
	gatewayID string,
	gatewayKey *ecdsa.PrivateKey,
	bootID string,
) *authenticatedGateway {
	t.Helper()
	connection, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol,
		http.Header{
			"Forwarded":          []string{"for=127.0.0.1;proto=https"},
			"Cf-Connecting-Ip":   []string{"203.0.113.9"},
			"Cf-Ray":             []string{"synthetic-ray"},
			"X-Forwarded-For":    []string{"203.0.113.9"},
			"X-Forwarded-Proto":  []string{"https"},
			"X-Forwarded-Host":   []string{ProductionExactHost},
			"X-Original-Forward": []string{"ignored"},
		},
	)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	connectionID := "client-" + bootID
	session, err := publicedge.NewGatewaySession(publicedge.GatewaySessionConfig{
		ConnectionID: connectionID, GatewayID: gatewayID, BootID: bootID,
		PrivateKey: gatewayKey,
	})
	if err != nil {
		_ = connection.Close()
		t.Fatalf("new gateway session: %v", err)
	}
	now := harness.clock.Now()
	hello, err := session.Hello(now)
	if err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, connection, hello)
	challenge := receiveTestFrame(t, connection)
	authenticate, err := session.AcceptChallenge(connectionID, challenge, now)
	if err != nil {
		t.Fatalf("accept challenge: %v", err)
	}
	sendTestFrame(t, connection, authenticate)
	authenticated := receiveTestFrame(t, connection)
	if err := session.AcceptAuthenticated(connectionID, authenticated, now); err != nil {
		t.Fatalf("accept authenticated: %v", err)
	}
	lease, err := session.Lease()
	if err != nil {
		t.Fatal(err)
	}
	return &authenticatedGateway{
		connection: connection, session: session, connectionID: connectionID, lease: lease,
	}
}

func sendTestFrame(t *testing.T, connection *websocket.Conn, frame publicedge.Frame) {
	t.Helper()
	data, err := publicedge.EncodeFrame(frame)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := connection.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(connection, string(data)); err != nil {
		t.Fatalf("send frame: %v", err)
	}
}

func receiveTestFrame(t *testing.T, connection *websocket.Conn) publicedge.Frame {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var data string
	if err := websocket.Message.Receive(connection, &data); err != nil {
		t.Fatalf("receive frame: %v", err)
	}
	frame, err := publicedge.DecodeFrame([]byte(data))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return frame
}

func (harness *serverTestHarness) requestSnapshot(t *testing.T, authorized bool, headers http.Header) (SnapshotResponse, *http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, harness.httpServer.URL+SnapshotAPIPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = ProductionExactHost
	if headers != nil {
		request.Header = headers.Clone()
	}
	if authorized {
		request.Header.Set("Authorization", testBearerToken)
	}
	response, err := harness.httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot SnapshotResponse
	if response.StatusCode == http.StatusOK {
		decodeStrictTestJSON(t, body, &snapshot)
	}
	return snapshot, response, body
}

func decodeStrictTestJSON(t *testing.T, data []byte, destination any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode strict JSON: %v; body=%s", err, data)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON contains trailing value: %v", err)
	}
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitForOffline(t *testing.T, harness *serverTestHarness) {
	t.Helper()
	waitFor(t, func() bool {
		state, response, _ := harness.requestSnapshot(t, true, nil)
		return response.StatusCode == http.StatusOK && !state.GatewayOnline
	}, "gateway to become offline")
}

func publishSnapshot(t *testing.T, harness *serverTestHarness, gateway *authenticatedGateway, revision uint64) publicedge.Frame {
	t.Helper()
	now := harness.clock.Now()
	frame, err := gateway.session.Snapshot(gateway.connectionID, publicedge.SnapshotPayload{
		ObservedAt: now.Unix(), Revision: revision, EventHighWater: 0,
		Service: publicedge.ServiceReady, Call: publicedge.CallIdle, SMS: publicedge.SMSReady,
	}, now)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	sendTestFrame(t, gateway.connection, frame)
	return frame
}

func TestRealWSSGatewayPublishesAuthenticatedCoarseSnapshot(t *testing.T) {
	harness := newServerTestHarness(t)
	gateway := harness.authenticateGateway(t, "boot-success")
	defer gateway.close()

	publishSnapshot(t, harness, gateway, 1)
	waitFor(t, func() bool {
		state, response, body := harness.requestSnapshot(t, true, nil)
		if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" ||
			response.Header.Get("Content-Type") != "application/json" {
			return false
		}
		if bytes.Contains(body, []byte(testGatewayID)) || bytes.Contains(body, []byte("boot-success")) {
			t.Fatalf("snapshot leaked connection identity: %s", body)
		}
		return state.GatewayOnline && state.ReceivedAt == harness.clock.Now().Unix() &&
			state.Snapshot != nil && state.Snapshot.Revision == 1 &&
			state.Snapshot.Service == publicedge.ServiceReady &&
			state.Snapshot.Call == publicedge.CallIdle && state.Snapshot.SMS == publicedge.SMSReady
	}, "published snapshot")

	healthRequest, _ := http.NewRequest(http.MethodGet, harness.httpServer.URL+HealthPath, nil)
	healthRequest.Host = ProductionExactHost
	healthResponse, err := harness.httpClient.Do(healthRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer healthResponse.Body.Close()
	var health HealthResponse
	healthBody, _ := io.ReadAll(healthResponse.Body)
	decodeStrictTestJSON(t, healthBody, &health)
	if healthResponse.StatusCode != http.StatusOK || health.Status != "ok" ||
		string(healthBody) != `{"status":"ok"}` || bytes.Contains(healthBody, []byte("gateway")) ||
		bytes.Contains(healthBody, []byte("received")) || bytes.Contains(healthBody, []byte(testGatewayID)) {
		t.Fatalf("health = %#v status=%d", health, healthResponse.StatusCode)
	}
}

func TestGatewayHandshakeRequiresExactHostAndSingleExactSubprotocol(t *testing.T) {
	harness := newServerTestHarness(t)

	for _, test := range []struct {
		name        string
		host        string
		subprotocol string
	}{
		{name: "wrong host", host: "wrong.example.com", subprotocol: publicedge.GatewayWebSocketSubprotocol},
		{name: "host with port", host: ProductionExactHost + ":443", subprotocol: publicedge.GatewayWebSocketSubprotocol},
		{name: "wrong subprotocol", host: ProductionExactHost, subprotocol: "djonehub.gateway.v2"},
		{name: "empty subprotocol", host: ProductionExactHost, subprotocol: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, err := harness.dialGateway(t, test.host, test.subprotocol, nil)
			if err == nil {
				_ = connection.Close()
				t.Fatal("invalid WebSocket handshake succeeded")
			}
		})
	}

	connection, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol+", extra", nil,
	)
	if err == nil {
		_ = connection.Close()
		t.Fatal("multiple subprotocols succeeded")
	}
}

func TestGatewayHandshakeRequiresSingleExactOriginAndRejectsIdentityHeadersBeforeSlot(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.server.config.maxConnections = 1
	if exactGatewayOrigin(http.Header{}, ProductionExactHost) ||
		exactGatewayOrigin(http.Header{"Origin": []string{
			"https://" + ProductionExactHost,
			"https://" + ProductionExactHost,
		}}, ProductionExactHost) ||
		exactGatewayOrigin(http.Header{
			"Origin": []string{"https://" + ProductionExactHost},
			"origin": []string{"https://" + ProductionExactHost},
		}, ProductionExactHost) {
		t.Fatal("missing or duplicate Origin was accepted")
	}

	for _, test := range []struct {
		name    string
		origin  string
		headers http.Header
	}{
		{name: "wrong origin", origin: "https://attacker.invalid"},
		{name: "cookie", origin: "https://" + ProductionExactHost, headers: http.Header{
			"Cookie": []string{"CF_Authorization=forged"},
		}},
		{name: "tailscale login", origin: "https://" + ProductionExactHost, headers: http.Header{
			"Tailscale-User-Login": []string{"owner@example.invalid"},
		}},
		{name: "tailscale capabilities", origin: "https://" + ProductionExactHost, headers: http.Header{
			"Tailscale-App-Capabilities": []string{`{"identity":"forged"}`},
		}},
		{name: "legacy CF user", origin: "https://" + ProductionExactHost, headers: http.Header{
			"Cf-Access-Authenticated-User-Email": []string{"owner@example.invalid"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection, err := harness.dialGatewayWithOrigin(
				t, ProductionExactHost, test.origin,
				publicedge.GatewayWebSocketSubprotocol, test.headers,
			)
			if err == nil {
				_ = connection.Close()
				t.Fatal("identity-bearing WebSocket handshake succeeded")
			}
			harness.server.mu.RLock()
			inFlight := harness.server.inFlight
			harness.server.mu.RUnlock()
			if inFlight != 0 {
				t.Fatalf("rejected pre-auth handshake consumed %d slots", inFlight)
			}
		})
	}

	// Cloudflare Tunnel metadata is transport context, not gateway identity.
	// It may be present, but a valid signed protocol handshake is still needed.
	connection, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol,
		http.Header{
			"Forwarded":        []string{"for=203.0.113.9;proto=https"},
			"X-Forwarded-For":  []string{"203.0.113.9"},
			"Cf-Connecting-Ip": []string{"203.0.113.9"},
			"Cf-Ray":           []string{"synthetic-ray"},
		},
	)
	if err != nil {
		t.Fatalf("forwarding metadata broke transport-compatible handshake: %v", err)
	}
	_ = connection.Close()
}

func TestSecondGatewayCannotReplaceAuthenticatedLease(t *testing.T) {
	harness := newServerTestHarness(t)
	first := harness.authenticateGateway(t, "boot-first")
	defer first.close()

	secondConnection, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol, nil,
	)
	if err == nil {
		_ = secondConnection.Close()
		t.Fatal("active gateway allowed another unauthenticated upgrade")
	}

	publishSnapshot(t, harness, first, 1)
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.GatewayOnline && state.Snapshot != nil && state.Snapshot.Revision == 1
	}, "first gateway lease to remain healthy")
}

func TestDifferentGatewayIDsCannotReplaceAnonymousSnapshotOwner(t *testing.T) {
	firstKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	harness := newServerTestHarnessWithKeys(t, map[string]*ecdsa.PrivateKey{
		testGatewayID: firstKey,
		"gateway-2":   secondKey,
	})
	first := harness.authenticateGatewayAs(t, testGatewayID, firstKey, "boot-multi-first")
	publishSnapshot(t, harness, first, 1)
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.GatewayOnline && state.Snapshot != nil && state.Snapshot.Revision == 1
	}, "first gateway snapshot")

	connection, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol, nil,
	)
	if err == nil {
		_ = connection.Close()
		t.Fatal("different gateway ID could enter an already-owned anonymous cache")
	}
	first.close()
	waitForOffline(t, harness)

	second := harness.authenticateGatewayAs(t, "gateway-2", secondKey, "boot-multi-second")
	defer second.close()
	publishSnapshot(t, harness, second, 2)
	waitFor(t, func() bool {
		state, _, body := harness.requestSnapshot(t, true, nil)
		return state.GatewayOnline && state.Snapshot != nil && state.Snapshot.Revision == 2 &&
			!bytes.Contains(body, []byte("gateway-2"))
	}, "second gateway after first released global claim")
}

func TestConcurrentDifferentGatewayClaimsHaveExactlyOneWinner(t *testing.T) {
	harness := newServerTestHarness(t)
	start := make(chan struct{})
	type result struct {
		connectionID string
		claimed      bool
	}
	results := make(chan result, 2)
	for _, connectionID := range []string{"gateway-one-connection", "gateway-two-connection"} {
		go func() {
			<-start
			results <- result{connectionID: connectionID, claimed: harness.server.claimActive(connectionID)}
		}()
	}
	close(start)
	first, second := <-results, <-results
	winners := 0
	winner, loser := "", ""
	for _, item := range []result{first, second} {
		if item.claimed {
			winners++
			winner = item.connectionID
		} else {
			loser = item.connectionID
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent global claims produced %d winners: %#v %#v", winners, first, second)
	}
	harness.server.markOffline(loser)
	harness.server.mu.RLock()
	activeAfterLoser := harness.server.activeConnectionID
	harness.server.mu.RUnlock()
	if activeAfterLoser != winner {
		t.Fatalf("losing connection cleared winner: active=%q winner=%q", activeAfterLoser, winner)
	}
	harness.server.markOffline(winner)
}

func TestGatewayProtocolViolationsFailLeaseClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		frame func(*testing.T, *serverTestHarness, *authenticatedGateway) publicedge.Frame
	}{
		{
			name: "replay",
			frame: func(t *testing.T, harness *serverTestHarness, gateway *authenticatedGateway) publicedge.Frame {
				frame := publishSnapshot(t, harness, gateway, 1)
				waitFor(t, func() bool {
					state, _, _ := harness.requestSnapshot(t, true, nil)
					return state.Snapshot != nil
				}, "first replay target")
				return frame
			},
		},
		{
			name: "out of order",
			frame: func(t *testing.T, harness *serverTestHarness, gateway *authenticatedGateway) publicedge.Frame {
				now := harness.clock.Now()
				if _, err := gateway.session.Snapshot(gateway.connectionID, publicedge.SnapshotPayload{
					ObservedAt: now.Unix(), Revision: 1, EventHighWater: 0,
					Service: publicedge.ServiceReady, Call: publicedge.CallIdle, SMS: publicedge.SMSReady,
				}, now); err != nil {
					t.Fatal(err)
				}
				frame, err := gateway.session.Event(gateway.connectionID, publicedge.EventPayload{
					ObservedAt: now.Unix(), Revision: 2, EventSequence: 1,
					Kind: publicedge.EventStateChanged,
				}, now)
				if err != nil {
					t.Fatal(err)
				}
				return frame
			},
		},
		{
			name: "bad signature",
			frame: func(t *testing.T, harness *serverTestHarness, gateway *authenticatedGateway) publicedge.Frame {
				wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				now := harness.clock.Now()
				frame, err := publicedge.NewFrame(publicedge.FrameStateSnapshot, publicedge.FrameMeta{
					GatewayID: gateway.lease.GatewayID, ExpiresAt: now.Add(time.Minute).Unix(),
					BootID: gateway.lease.BootID, LeaseEpoch: gateway.lease.LeaseEpoch, Sequence: 1,
				}, publicedge.SnapshotPayload{
					ObservedAt: now.Unix(), Revision: 1, EventHighWater: 0,
					Service: publicedge.ServiceReady, Call: publicedge.CallIdle, SMS: publicedge.SMSReady,
				})
				if err != nil {
					t.Fatal(err)
				}
				frame, err = publicedge.SignGatewayFrame(wrongKey, frame, gateway.lease.ChannelBinding)
				if err != nil {
					t.Fatal(err)
				}
				return frame
			},
		},
		{
			name: "expired",
			frame: func(t *testing.T, harness *serverTestHarness, gateway *authenticatedGateway) publicedge.Frame {
				frame := publishSnapshotFrame(t, harness, gateway, 1)
				harness.clock.Advance(publicedge.DefaultMessageTTL + time.Second)
				return frame
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newServerTestHarness(t)
			gateway := harness.authenticateGateway(t, "boot-"+strings.ReplaceAll(test.name, " ", "-"))
			defer gateway.close()
			frame := test.frame(t, harness, gateway)
			sendTestFrame(t, gateway.connection, frame)
			waitForOffline(t, harness)
		})
	}
}

func publishSnapshotFrame(t *testing.T, harness *serverTestHarness, gateway *authenticatedGateway, revision uint64) publicedge.Frame {
	t.Helper()
	now := harness.clock.Now()
	frame, err := gateway.session.Snapshot(gateway.connectionID, publicedge.SnapshotPayload{
		ObservedAt: now.Unix(), Revision: revision, EventHighWater: 0,
		Service: publicedge.ServiceReady, Call: publicedge.CallIdle, SMS: publicedge.SMSReady,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestGatewayIsOnlineOnlyWhileFreshSnapshotsDriveTheReadWindow(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.server.config.readTimeout = 50 * time.Millisecond
	gateway := harness.authenticateGateway(t, "boot-freshness")
	defer gateway.close()

	state, _, _ := harness.requestSnapshot(t, true, nil)
	if state.GatewayOnline || state.Snapshot != nil || state.ReceivedAt != 0 {
		t.Fatalf("authenticated connection without snapshot appeared online: %#v", state)
	}
	publishSnapshot(t, harness, gateway, 1)
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.GatewayOnline && state.Snapshot != nil && state.Snapshot.Revision == 1
	}, "fresh snapshot to mark gateway online")

	// With snapshots as the only application frame, silence cannot preserve an
	// online bit past the bounded read window. The last state remains visibly
	// stale because GatewayOnline is false.
	waitForOffline(t, harness)
	state, _, _ = harness.requestSnapshot(t, true, nil)
	if state.GatewayOnline || state.Snapshot == nil || state.Snapshot.Revision != 1 {
		t.Fatalf("bounded stale snapshot state = %#v", state)
	}
}

func TestGatewayEventCannotKeepOldSnapshotOnline(t *testing.T) {
	harness := newServerTestHarness(t)
	gateway := harness.authenticateGateway(t, "boot-event-freshness")
	defer gateway.close()
	publishSnapshot(t, harness, gateway, 1)
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.GatewayOnline && state.Snapshot != nil
	}, "initial snapshot")

	now := harness.clock.Now()
	event, err := gateway.session.Event(gateway.connectionID, publicedge.EventPayload{
		ObservedAt: now.Unix(), Revision: 1, EventSequence: 1,
		Kind: publicedge.EventHeartbeat,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	sendTestFrame(t, gateway.connection, event)
	waitForOffline(t, harness)
	state, _, _ := harness.requestSnapshot(t, true, nil)
	if state.GatewayOnline || state.Snapshot == nil || state.Snapshot.Revision != 1 {
		t.Fatalf("event refreshed stale public state: %#v", state)
	}
}

func TestDisconnectMarksGatewayOfflineAndRetainsOnlyCoarseStaleSnapshot(t *testing.T) {
	harness := newServerTestHarness(t)
	gateway := harness.authenticateGateway(t, "boot-disconnect")
	publishSnapshot(t, harness, gateway, 7)
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.Snapshot != nil && state.Snapshot.Revision == 7
	}, "snapshot before disconnect")
	_ = gateway.connection.Close()
	gateway.session.Close()
	waitForOffline(t, harness)
	state, _, body := harness.requestSnapshot(t, true, nil)
	if state.Snapshot == nil || state.Snapshot.Revision != 7 || state.GatewayOnline {
		t.Fatalf("offline state = %#v", state)
	}
	for _, forbidden := range []string{
		"gateway-1", "boot-disconnect", "phone_number", "sms_body", "message_text", "sdp", "candidate",
	} {
		if bytes.Contains(bytes.ToLower(body), []byte(forbidden)) {
			t.Fatalf("public snapshot contains forbidden value %q: %s", forbidden, body)
		}
	}
}

func TestSnapshotHTTPRequiresInjectedAuthAndIsNeverCacheable(t *testing.T) {
	harness := newServerTestHarness(t)
	forgedHeaders := http.Header{
		"Cookie":            []string{"CF_Authorization=forged"},
		"Origin":            []string{"https://phone.example.com"},
		"Forwarded":         []string{"for=127.0.0.1;proto=https"},
		"X-Forwarded-For":   []string{"127.0.0.1"},
		"X-Tailscale-User":  []string{"owner@example.invalid"},
		"Cf-Access-User":    []string{"owner@example.invalid"},
		"Cf-Connecting-Ip":  []string{"127.0.0.1"},
		"X-Original-Method": []string{"GET"},
	}
	_, response, body := harness.requestSnapshot(t, false, forgedHeaders)
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("Cache-Control") != "no-store" ||
		string(body) != `{"error":"unauthorized"}` {
		t.Fatalf("unauthorized response status=%d cache=%q body=%s",
			response.StatusCode, response.Header.Get("Cache-Control"), body)
	}
	state, response, _ := harness.requestSnapshot(t, true, forgedHeaders)
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" ||
		state.Version != publicedge.ProtocolVersion || state.GatewayOnline || state.Snapshot != nil {
		t.Fatalf("authorized empty state = %#v status=%d", state, response.StatusCode)
	}

	post, _ := http.NewRequest(http.MethodPost, harness.httpServer.URL+SnapshotAPIPath, strings.NewReader(`{}`))
	post.Host = ProductionExactHost
	post.Header.Set("Authorization", testBearerToken)
	postResponse, err := harness.httpClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusMethodNotAllowed || postResponse.Header.Get("Allow") != http.MethodGet ||
		postResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("POST status=%d allow=%q cache=%q", postResponse.StatusCode,
			postResponse.Header.Get("Allow"), postResponse.Header.Get("Cache-Control"))
	}
}

func TestSnapshotHTTPDistinguishesUnavailableKeysFromUnauthorizedIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		authError  error
		wantStatus int
		wantBody   string
	}{
		{
			name: "JWKS unavailable", authError: fmt.Errorf("wrapped: %w", publicedge.ErrAccessUnavailable),
			wantStatus: http.StatusServiceUnavailable, wantBody: `{"error":"unavailable"}`,
		},
		{
			name: "assertion unauthorized", authError: fmt.Errorf("wrapped: %w", publicedge.ErrAccessUnauthorized),
			wantStatus: http.StatusUnauthorized, wantBody: `{"error":"unauthorized"}`,
		},
		{
			name: "unknown auth failure", authError: errors.New("private provider error"),
			wantStatus: http.StatusUnauthorized, wantBody: `{"error":"unauthorized"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newServerTestHarness(t)
			harness.server.config.httpAuth = HTTPAuthFunc(func(*http.Request) error {
				return test.authError
			})
			_, response, body := harness.requestSnapshot(t, false, nil)
			if response.StatusCode != test.wantStatus || string(body) != test.wantBody ||
				response.Header.Get("Cache-Control") != "no-store" ||
				bytes.Contains(body, []byte("private provider")) {
				t.Fatalf("status=%d cache=%q body=%s", response.StatusCode,
					response.Header.Get("Cache-Control"), body)
			}
		})
	}
}

func TestMalformedOrSecretBearingFrameCannotEnterCache(t *testing.T) {
	harness := newServerTestHarness(t)
	gateway := harness.authenticateGateway(t, "boot-secret")
	defer gateway.close()

	secret := "+15551234567 secret SMS body v=0 SDP candidate"
	malformed := []byte(`{"version":1,"type":"state.snapshot","secret":"` + secret + `"}`)
	if err := gateway.connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(gateway.connection, string(malformed)); err != nil {
		t.Fatal(err)
	}
	waitForOffline(t, harness)
	state, _, body := harness.requestSnapshot(t, true, nil)
	if state.Snapshot != nil || bytes.Contains(body, []byte(secret)) {
		t.Fatalf("malformed secret entered cache: %s", body)
	}

	formatted := fmt.Sprintf("%s %#v %s %#v", Config{
		Registry: harness.registry, ExactHost: ProductionExactHost,
		Clock: harness.clock.Now, HTTPAuth: HTTPAuthFunc(func(*http.Request) error {
			return errors.New(secret)
		}),
	}, Config{
		Registry: harness.registry, ExactHost: ProductionExactHost,
		Clock: harness.clock.Now, HTTPAuth: HTTPAuthFunc(func(*http.Request) error {
			return errors.New(secret)
		}),
	}, harness.server, harness.server)
	if strings.Contains(formatted, secret) || strings.Contains(formatted, testGatewayID) {
		t.Fatalf("ordinary formatting leaked sensitive state: %s", formatted)
	}
}

func TestBinaryAndOversizedWebSocketFramesFailLeaseClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		send func(*testing.T, *websocket.Conn, publicedge.Frame)
	}{
		{
			name: "binary",
			send: func(t *testing.T, connection *websocket.Conn, frame publicedge.Frame) {
				data, err := publicedge.EncodeFrame(frame)
				if err != nil {
					t.Fatal(err)
				}
				if err := websocket.Message.Send(connection, data); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized",
			send: func(_ *testing.T, connection *websocket.Conn, _ publicedge.Frame) {
				data := strings.Repeat("x", publicedge.MaxFrameBytes+1)
				// The server may close as soon as it reads the oversized frame
				// header, so a concurrent broken-pipe result is also success.
				_ = websocket.Message.Send(connection, data)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newServerTestHarness(t)
			gateway := harness.authenticateGateway(t, "boot-frame-"+test.name)
			defer gateway.close()
			frame := publishSnapshotFrame(t, harness, gateway, 1)
			if err := gateway.connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			test.send(t, gateway.connection, frame)
			waitForOffline(t, harness)
			state, _, _ := harness.requestSnapshot(t, true, nil)
			if state.Snapshot != nil {
				t.Fatalf("invalid %s frame entered cache: %#v", test.name, state)
			}
		})
	}
}

func TestHandshakeAndAuthenticatedIdleTimeoutsAreBounded(t *testing.T) {
	t.Run("handshake", func(t *testing.T) {
		harness := newServerTestHarness(t)
		harness.server.config.handshakeTimeout = 40 * time.Millisecond
		connection, err := harness.dialGateway(
			t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		var message string
		if err := websocket.Message.Receive(connection, &message); err == nil {
			t.Fatalf("idle unauthenticated connection remained open: %q", message)
		}
	})

	t.Run("authenticated", func(t *testing.T) {
		harness := newServerTestHarness(t)
		harness.server.config.readTimeout = 40 * time.Millisecond
		gateway := harness.authenticateGateway(t, "boot-idle")
		defer gateway.close()
		waitForOffline(t, harness)
	})
}

func TestWebSocketPingAfterAcknowledgementDeadlineKeepsLeaseHealthy(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.server.config.writeTimeout = 20 * time.Millisecond
	gateway := harness.authenticateGateway(t, "boot-ping")
	defer gateway.close()

	// The challenge/authentication write deadline is now expired. The receive
	// window must have replaced it so x/net/websocket's automatic Pong remains
	// bounded without spuriously killing a healthy connection.
	time.Sleep(30 * time.Millisecond)
	pingCodec := websocket.Codec{Marshal: func(value any) ([]byte, byte, error) {
		data, ok := value.([]byte)
		if !ok {
			return nil, websocket.UnknownFrame, websocket.ErrNotSupported
		}
		return data, websocket.PingFrame, nil
	}}
	if err := pingCodec.Send(gateway.connection, []byte("bounded-ping")); err != nil {
		t.Fatal(err)
	}
	publishSnapshot(t, harness, gateway, 1)
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.GatewayOnline && state.Snapshot != nil && state.Snapshot.Revision == 1
	}, "healthy lease after WebSocket ping")
}

func TestConnectionCapacityIsBoundedBeforeProtocolHandshake(t *testing.T) {
	harness := newServerTestHarness(t)
	harness.server.config.maxConnections = 1
	first, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := harness.dialGateway(
		t, ProductionExactHost, publicedge.GatewayWebSocketSubprotocol, nil,
	)
	if err == nil {
		_ = second.Close()
		t.Fatal("connection bound allowed a second in-flight WebSocket")
	}
}

func TestConcurrentSnapshotReadsAndPublishesAreRaceFree(t *testing.T) {
	harness := newServerTestHarness(t)
	gateway := harness.authenticateGateway(t, "boot-race")
	defer gateway.close()

	stop := make(chan struct{})
	errCh := make(chan error, 16)
	var readers sync.WaitGroup
	for index := 0; index < 12; index++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				request, _ := http.NewRequest(http.MethodGet, harness.httpServer.URL+SnapshotAPIPath, nil)
				request.Host = ProductionExactHost
				request.Header.Set("Authorization", testBearerToken)
				response, err := harness.httpClient.Do(request)
				if err != nil {
					errCh <- err
					return
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if readErr != nil || response.StatusCode != http.StatusOK {
					errCh <- fmt.Errorf("snapshot read status=%d err=%v", response.StatusCode, readErr)
					return
				}
			}
		}()
	}
	for revision := uint64(1); revision <= 80; revision++ {
		publishSnapshot(t, harness, gateway, revision)
	}
	waitFor(t, func() bool {
		state, _, _ := harness.requestSnapshot(t, true, nil)
		return state.Snapshot != nil && state.Snapshot.Revision == 80
	}, "last concurrent snapshot")
	close(stop)
	readers.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestClosePromptlyUnblocksAuthenticatedConnectionAndReleasesLease(t *testing.T) {
	harness := newServerTestHarness(t)
	gateway := harness.authenticateGateway(t, "boot-close")
	defer gateway.close()

	closed := make(chan struct{})
	go func() {
		_ = harness.server.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Server.Close did not promptly unblock WebSocket handler")
	}
	_ = gateway.connection.SetReadDeadline(time.Now().Add(time.Second))
	var message string
	if err := websocket.Message.Receive(gateway.connection, &message); err == nil {
		t.Fatalf("connection remained readable after Close: %q", message)
	}

	probeSession, err := publicedge.NewGatewaySession(publicedge.GatewaySessionConfig{
		ConnectionID: "client-after-close", GatewayID: testGatewayID,
		BootID: "boot-after-close", PrivateKey: harness.gatewayKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer probeSession.Close()
	hello, err := probeSession.Hello(harness.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := harness.registry.Open("registry-after-close", hello, harness.clock.Now())
	if err != nil {
		t.Fatalf("Close did not release registry lease: %v", err)
	}
	lease.Close()
}

func TestCloseInterruptsConnectionInsideWebSocketUpgradeGap(t *testing.T) {
	harness := newServerTestHarness(t)
	serverSide, peerSide := net.Pipe()
	defer peerSide.Close()
	response := &blockingHijackResponse{
		header: make(http.Header), connection: serverSide, hijacked: make(chan struct{}),
	}
	request := httptest.NewRequest(
		http.MethodGet, "https://"+ProductionExactHost+publicedge.GatewayWebSocketPath, nil,
	)
	request.Host = ProductionExactHost
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-Websocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-Websocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Protocol", publicedge.GatewayWebSocketSubprotocol)
	request.Header.Set("Origin", "https://"+ProductionExactHost)

	serveDone := make(chan struct{})
	go func() {
		harness.server.ServeHTTP(response, request)
		close(serveDone)
	}()
	select {
	case <-response.hijacked:
	case <-time.After(time.Second):
		_ = peerSide.Close()
		t.Fatal("WebSocket connection was not hijacked")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = harness.server.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		_ = peerSide.Close()
		<-serveDone
		t.Fatal("Close could not interrupt the pre-Handler upgrade gap")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		_ = peerSide.Close()
		t.Fatal("upgrading handler survived Close")
	}
}

func TestConfigAndRouteBoundsFailClosed(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := publicedge.NewRegistry(publicedge.RegistryConfig{
		GatewayKeys: map[string]*ecdsa.PublicKey{testGatewayID: &key.PublicKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := HTTPAuthFunc(func(*http.Request) error { return nil })
	for _, config := range []Config{
		{HTTPAuth: auth},
		{Registry: registry},
		{Registry: registry, HTTPAuth: auth, ExactHost: " phone.example.com"},
		{Registry: registry, HTTPAuth: auth, ReadTimeout: maxReadTimeout + time.Second},
		{Registry: registry, HTTPAuth: auth, MaxConnections: maxConnections + 1},
	} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid config accepted: %#v err=%v", config, err)
		}
	}

	harness := newServerTestHarness(t)
	nonHijackingRequest := httptest.NewRequest(
		http.MethodGet, "https://"+ProductionExactHost+publicedge.GatewayWebSocketPath, nil,
	)
	nonHijackingRequest.Host = ProductionExactHost
	nonHijackingRequest.Header.Set("Sec-WebSocket-Protocol", publicedge.GatewayWebSocketSubprotocol)
	nonHijackingResponse := httptest.NewRecorder()
	harness.server.ServeHTTP(nonHijackingResponse, nonHijackingRequest)
	if nonHijackingResponse.Code != http.StatusBadRequest {
		t.Fatalf("non-hijacking WebSocket response = %d", nonHijackingResponse.Code)
	}
	forceQueryRequest := httptest.NewRequest(
		http.MethodGet, "https://"+ProductionExactHost+SnapshotAPIPath, nil,
	)
	forceQueryRequest.Host = ProductionExactHost
	forceQueryRequest.URL.ForceQuery = true
	forceQueryRequest.Header.Set("Authorization", testBearerToken)
	forceQueryResponse := httptest.NewRecorder()
	harness.server.ServeHTTP(forceQueryResponse, forceQueryRequest)
	if forceQueryResponse.Code != http.StatusNotFound {
		t.Fatalf("ForceQuery route response = %d", forceQueryResponse.Code)
	}

	for _, requestURL := range []string{
		harness.httpServer.URL + "/api/public/v1/state/../state/snapshot",
		harness.httpServer.URL + SnapshotAPIPath + "?gateway=1",
		harness.httpServer.URL + "/api/public/v1/sms",
		harness.httpServer.URL + "/api/public/v1/mutate",
		harness.httpServer.URL + "/proxy/http://127.0.0.1:7576/",
	} {
		request, _ := http.NewRequest(http.MethodGet, requestURL, nil)
		request.Host = ProductionExactHost
		request.Header.Set("Authorization", testBearerToken)
		response, err := harness.httpClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unexpected route %q returned %d", requestURL, response.StatusCode)
		}
	}
}
