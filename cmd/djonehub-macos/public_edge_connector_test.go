package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/publicedge"
	"golang.org/x/net/websocket"
)

func TestParsePublicEdgeStartupConfigDefaultOffAndExact(t *testing.T) {
	if config, err := parsePublicEdgeStartupConfig(publicEdgeFlagConfig{}); err != nil || config != nil {
		t.Fatalf("default config=%v err=%v", config, err)
	}
	valid := publicEdgeFlagConfig{
		URL: publicEdgeEndpoint(), GatewayID: "home-gateway-1",
		PrivateKeyFile:         "/private/gateway.pem",
		AccessClientIDFile:     "/private/access-id",
		AccessClientSecretFile: "/private/access-secret",
	}
	config, err := parsePublicEdgeStartupConfig(valid)
	if err != nil || config == nil || config.endpoint.String() != publicEdgeEndpoint() {
		t.Fatalf("valid config=%v err=%v", config, err)
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", config, config), "private/gateway") {
		t.Fatal("startup config formatting leaked a private path")
	}

	tests := []struct {
		name   string
		mutate func(*publicEdgeFlagConfig)
	}{
		{name: "partial", mutate: func(value *publicEdgeFlagConfig) { value.AccessClientSecretFile = "" }},
		{name: "http", mutate: func(value *publicEdgeFlagConfig) {
			value.URL = "https://phone.example.com" + publicedge.GatewayWebSocketPath
		}},
		{name: "wrong_host", mutate: func(value *publicEdgeFlagConfig) {
			value.URL = "wss://other.example.com" + publicedge.GatewayWebSocketPath
		}},
		{name: "port", mutate: func(value *publicEdgeFlagConfig) {
			value.URL = "wss://phone.example.com:443" + publicedge.GatewayWebSocketPath
		}},
		{name: "query", mutate: func(value *publicEdgeFlagConfig) { value.URL += "?token=secret" }},
		{name: "fragment", mutate: func(value *publicEdgeFlagConfig) { value.URL += "#secret" }},
		{name: "userinfo", mutate: func(value *publicEdgeFlagConfig) {
			value.URL = "wss://user@phone.example.com" + publicedge.GatewayWebSocketPath
		}},
		{name: "wrong_path", mutate: func(value *publicEdgeFlagConfig) { value.URL = "wss://phone.example.com/api/remote/v1/" }},
		{name: "bad_gateway", mutate: func(value *publicEdgeFlagConfig) { value.GatewayID = "Home Gateway" }},
		{name: "relative_key", mutate: func(value *publicEdgeFlagConfig) { value.PrivateKeyFile = "gateway.pem" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if config, err := parsePublicEdgeStartupConfig(candidate); !errors.Is(err, errPublicEdgeConfiguration) || config != nil {
				t.Fatalf("config=%v err=%v", config, err)
			}
		})
	}
}

func TestLoadPublicEdgePrivateKeyRequiresPrivateP256PKCS8(t *testing.T) {
	key := publicEdgeTestKey(t)
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gateway.pem")
	writePublicEdgeTestSecret(t, path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	loaded, err := loadPublicEdgePrivateKey(path)
	if err != nil || loaded.D.Cmp(key.D) != 0 {
		t.Fatalf("load err=%v", err)
	}
	loaded.D.SetInt64(0)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "wrong_label", data: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded})},
		{name: "trailing_block", data: append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), []byte("unexpected")...)},
		{name: "public_key", data: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "candidate.pem")
			writePublicEdgeTestSecret(t, candidate, test.data)
			if _, err := loadPublicEdgePrivateKey(candidate); !errors.Is(err, errPublicEdgeConfiguration) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384DER, err := x509.MarshalPKCS8PrivateKey(p384)
	if err != nil {
		t.Fatal(err)
	}
	p384Path := filepath.Join(t.TempDir(), "p384.pem")
	writePublicEdgeTestSecret(t, p384Path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p384DER}))
	if _, err := loadPublicEdgePrivateKey(p384Path); !errors.Is(err, errPublicEdgeConfiguration) {
		t.Fatalf("P-384 err=%v", err)
	}
}

func TestLoadPublicEdgeAccessValueIsSingleSafeLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access")
	writePublicEdgeTestSecret(t, path, []byte("synthetic-access-token_123\n"))
	value, err := loadPublicEdgeAccessValue(path)
	if err != nil || string(value) != "synthetic-access-token_123" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	zeroPublicEdgeBytes(value)
	for _, bad := range []string{"short", "synthetic token", "synthetic\nsecond", "synthetic:colon"} {
		writePublicEdgeTestSecret(t, path, []byte(bad))
		if _, err := loadPublicEdgeAccessValue(path); !errors.Is(err, errPublicEdgeConfiguration) {
			t.Fatalf("bad=%q err=%v", bad, err)
		}
	}
}

func TestPublicEdgeConnectorConfigJSONIsRedacted(t *testing.T) {
	key := publicEdgeTestKey(t)
	config := publicEdgeConnectorConfig{
		Endpoint: publicEdgeTestEndpoint(t), GatewayID: "home-gateway-1",
		PrivateKey: key, AccessClientID: []byte("sensitive-client-id"),
		AccessClientSecret: []byte("sensitive-client-secret"),
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, secret := range []string{
		"sensitive-client-id", "sensitive-client-secret", key.D.String(), publicEdgeHost,
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("JSON leaked private connector config: %s", text)
		}
	}
	if text != `{"configuration":"redacted"}` {
		t.Fatalf("unexpected redacted JSON: %s", text)
	}
}

func TestDialPublicEdgeWebSocketBoundsWholeUpgradeAndRequiresSelectedProtocol(t *testing.T) {
	endpoint := publicEdgeTestEndpoint(t)
	credentials := []byte("synthetic-access-value")

	t.Run("upgrade timeout", func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			_, _ = http.ReadRequest(bufio.NewReader(server))
			var oneByte [1]byte
			_, _ = server.Read(oneByte[:])
		}()
		started := time.Now()
		connection, err := dialPublicEdgeWebSocketWithTransport(
			context.Background(), endpoint, credentials, credentials, 40*time.Millisecond,
			func(context.Context, *url.URL) (net.Conn, error) { return client, nil },
		)
		if connection != nil {
			_ = connection.Close()
			t.Fatal("unexpected connection")
		}
		if !errors.Is(err, errPublicEdgeTransport) || time.Since(started) > 500*time.Millisecond {
			t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
		}
		_ = server.Close()
		<-serverDone
	})

	for _, test := range []struct {
		name      string
		protocols []string
		wantErr   error
	}{
		{name: "selected", protocols: []string{publicedge.GatewayWebSocketSubprotocol}},
		{name: "omitted", wantErr: errPublicEdgeProtocol},
		{name: "duplicate", protocols: []string{publicedge.GatewayWebSocketSubprotocol, publicedge.GatewayWebSocketSubprotocol}, wantErr: errPublicEdgeProtocol},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			serverDone := make(chan error, 1)
			go func() {
				defer server.Close()
				serverErr := writePublicEdgeUpgradeResponse(server, test.protocols)
				if serverErr == nil {
					var oneByte [1]byte
					_, _ = server.Read(oneByte[:])
				}
				serverDone <- serverErr
			}()
			connection, err := dialPublicEdgeWebSocketWithTransport(
				context.Background(), endpoint, credentials, credentials, time.Second,
				func(context.Context, *url.URL) (net.Conn, error) { return client, nil },
			)
			if test.wantErr == nil {
				if err != nil || connection == nil {
					t.Fatalf("connection=%v err=%v", connection, err)
				}
				if connection.Config().Header.Get("CF-Access-Client-Id") != "" ||
					connection.Config().Header.Get("CF-Access-Client-Secret") != "" {
					t.Fatal("access credentials remained attached after the handshake")
				}
				_ = connection.Close()
			} else {
				if connection != nil {
					_ = connection.Close()
				}
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("err=%v want=%v", err, test.wantErr)
				}
			}
			if serverErr := <-serverDone; serverErr != nil {
				t.Fatal(serverErr)
			}
		})
	}

	t.Run("oversized unfinished response header", func(t *testing.T) {
		client, server := net.Pipe()
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			defer server.Close()
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return
			}
			nonce := request.Header.Get("Sec-WebSocket-Key")
			digest := sha1.Sum([]byte(nonce + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			prefix := fmt.Sprintf(
				"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: %s\r\nX-Oversized: ",
				base64.StdEncoding.EncodeToString(digest[:]), publicedge.GatewayWebSocketSubprotocol,
			)
			_, _ = server.Write([]byte(prefix + strings.Repeat("a", publicEdgeHandshakeHeaderMaximum+1024)))
		}()
		started := time.Now()
		connection, err := dialPublicEdgeWebSocketWithTransport(
			context.Background(), endpoint, credentials, credentials, time.Second,
			func(context.Context, *url.URL) (net.Conn, error) { return client, nil },
		)
		if connection != nil {
			_ = connection.Close()
			t.Fatal("unexpected connection")
		}
		if !errors.Is(err, errPublicEdgeProtocol) || time.Since(started) > 500*time.Millisecond {
			t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
		}
		<-serverDone
	})
}

func TestPublicEdgeConnectorConcurrentCloseWaitsForCleanup(t *testing.T) {
	key := publicEdgeTestKey(t)
	dialStarted := make(chan struct{})
	dialCanceled := make(chan struct{})
	releaseDial := make(chan struct{})
	connector, err := newPublicEdgeConnector(publicEdgeConnectorConfig{
		Endpoint: publicEdgeTestEndpoint(t), GatewayID: "home-gateway-1", PrivateKey: key,
		AccessClientID: []byte("synthetic-client-id"), AccessClientSecret: []byte("synthetic-client-secret"),
		Source: func() publicEdgeCoarseState {
			return publicEdgeCoarseState{Service: publicedge.ServiceReady, Call: publicedge.CallIdle, SMS: publicedge.SMSReady}
		},
		ReconnectMinimum: time.Second, ReconnectMaximum: time.Second,
		Dial: func(ctx context.Context, _ *url.URL, _, _ []byte) (*websocket.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			close(dialCanceled)
			<-releaseDial
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-dialStarted
	firstDone := make(chan struct{})
	go func() {
		_ = connector.Close()
		close(firstDone)
	}()
	<-dialCanceled
	secondDone := make(chan struct{})
	go func() {
		_ = connector.Close()
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("concurrent Close returned before connector cleanup")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseDial)
	for name, done := range map[string]<-chan struct{}{"first": firstDone, "second": secondDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s Close did not finish", name)
		}
	}
}

func TestReceivePublicEdgeFrameRejectsBinary(t *testing.T) {
	server := httptest.NewServer(websocket.Server{Handler: func(connection *websocket.Conn) {
		_ = websocket.Message.Send(connection, []byte(`{"version":1}`))
	}})
	defer server.Close()
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(server.URL, "http"), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := config.DialContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := receivePublicEdgeFrame(connection); !errors.Is(err, errPublicEdgeProtocol) {
		t.Fatalf("err=%v", err)
	}
}

func TestHardClosePublicEdgeWebSocketInterruptsBlockedAutomaticPong(t *testing.T) {
	endpoint := publicEdgeTestEndpoint(t)
	credentials := []byte("synthetic-access-value")
	client, server := net.Pipe()
	pingWritten := make(chan struct{})
	releaseServer := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer server.Close()
		if writePublicEdgeUpgradeResponse(server, []string{publicedge.GatewayWebSocketSubprotocol}) != nil {
			return
		}
		_, _ = server.Write([]byte{0x89, 0x00})
		close(pingWritten)
		<-releaseServer
	}()
	connection, err := dialPublicEdgeWebSocketWithTransport(
		context.Background(), endpoint, credentials, credentials, time.Second,
		func(context.Context, *url.URL) (net.Conn, error) { return client, nil },
	)
	if err != nil {
		close(releaseServer)
		<-serverDone
		t.Fatal(err)
	}
	readerDone := make(chan struct{})
	go func() {
		_, _ = receivePublicEdgeFrame(connection)
		close(readerDone)
	}()
	select {
	case <-pingWritten:
	case <-time.After(time.Second):
		t.Fatal("client did not consume the Ping")
	}
	time.Sleep(10 * time.Millisecond)
	closeDone := make(chan struct{})
	go func() {
		hardClosePublicEdgeWebSocket(connection)
		close(closeDone)
	}()
	for name, done := range map[string]<-chan struct{}{"close": closeDone, "reader": readerDone} {
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("%s remained blocked behind automatic Pong", name)
		}
	}
	close(releaseServer)
	<-serverDone
}

func writePublicEdgeUpgradeResponse(connection net.Conn, protocols []string) error {
	request, err := http.ReadRequest(bufio.NewReader(connection))
	if err != nil {
		return err
	}
	nonce := request.Header.Get("Sec-WebSocket-Key")
	if nonce == "" {
		return errors.New("missing websocket nonce")
	}
	digest := sha1.Sum([]byte(nonce + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	writer := bufio.NewWriter(connection)
	if _, err := fmt.Fprintf(
		writer,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n",
		base64.StdEncoding.EncodeToString(digest[:]),
	); err != nil {
		return err
	}
	for _, protocol := range protocols {
		if _, err := fmt.Fprintf(writer, "Sec-WebSocket-Protocol: %s\r\n", protocol); err != nil {
			return err
		}
	}
	if _, err := writer.WriteString("\r\n"); err != nil {
		return err
	}
	return writer.Flush()
}

func publicEdgeTestEndpoint(t *testing.T) *url.URL {
	t.Helper()
	endpoint, err := url.Parse(publicEdgeEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func TestPublicEdgeConnectorPublishesOnlySignedCoarseState(t *testing.T) {
	key := publicEdgeTestKey(t)
	registry, err := publicedge.NewRegistry(publicedge.RegistryConfig{
		GatewayKeys: map[string]*ecdsa.PublicKey{"home-gateway-1": &key.PublicKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stateMu sync.Mutex
	state := publicEdgeCoarseState{
		Service: publicedge.ServiceReady, Call: publicedge.CallIdle, SMS: publicedge.SMSReady,
	}
	source := func() publicEdgeCoarseState {
		stateMu.Lock()
		defer stateMu.Unlock()
		return state
	}

	serverDone := make(chan error, 1)
	initialSnapshot := make(chan publicedge.SnapshotPayload, 1)
	changedSnapshot := make(chan publicedge.SnapshotPayload, 1)
	server := httptest.NewServer(websocket.Server{
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.URL.Path != publicedge.GatewayWebSocketPath ||
				request.Header.Get("CF-Access-Client-Id") != "synthetic-client-id" ||
				request.Header.Get("CF-Access-Client-Secret") != "synthetic-client-secret" ||
				len(request.Header.Values("CF-Access-Client-Id")) != 1 ||
				len(request.Header.Values("CF-Access-Client-Secret")) != 1 ||
				len(config.Protocol) != 1 || config.Protocol[0] != publicedge.GatewayWebSocketSubprotocol {
				return errors.New("unexpected gateway handshake")
			}
			config.Protocol = []string{publicedge.GatewayWebSocketSubprotocol}
			return nil
		},
		Handler: func(connection *websocket.Conn) {
			serverDone <- runPublicEdgeTestServer(connection, registry, initialSnapshot, changedSnapshot)
		},
	})
	defer server.Close()
	localURL, err := url.Parse("ws" + strings.TrimPrefix(server.URL, "http") + publicedge.GatewayWebSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(publicEdgeEndpoint())
	dial := func(ctx context.Context, _ *url.URL, clientID, clientSecret []byte) (*websocket.Conn, error) {
		config, err := websocket.NewConfig(localURL.String(), server.URL)
		if err != nil {
			return nil, err
		}
		config.Protocol = []string{publicedge.GatewayWebSocketSubprotocol}
		config.Header = make(http.Header)
		config.Header.Set("CF-Access-Client-Id", string(clientID))
		config.Header.Set("CF-Access-Client-Secret", string(clientSecret))
		return config.DialContext(ctx)
	}
	connector, err := newPublicEdgeConnector(publicEdgeConnectorConfig{
		Endpoint: endpoint, GatewayID: "home-gateway-1", PrivateKey: key,
		AccessClientID: []byte("synthetic-client-id"), AccessClientSecret: []byte("synthetic-client-secret"),
		Source: source, HeartbeatInterval: 20 * time.Millisecond,
		ReconnectMinimum: time.Second, ReconnectMaximum: time.Second, Dial: dial,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := connector.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	select {
	case snapshot := <-initialSnapshot:
		if snapshot.Revision != 1 || snapshot.Service != publicedge.ServiceReady ||
			snapshot.Call != publicedge.CallIdle || snapshot.SMS != publicedge.SMSReady {
			t.Fatalf("initial=%+v", snapshot)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for initial snapshot")
	}
	stateMu.Lock()
	state.Service = publicedge.ServiceDegraded
	state.SMS = publicedge.SMSDegraded
	stateMu.Unlock()
	select {
	case snapshot := <-changedSnapshot:
		if snapshot.Revision != 2 || snapshot.Service != publicedge.ServiceDegraded ||
			snapshot.SMS != publicedge.SMSDegraded {
			t.Fatalf("changed=%+v", snapshot)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for changed snapshot")
	}
	cancel()
	_ = connector.Close()
	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, errPublicEdgeTransport) {
			t.Fatalf("server err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}

func runPublicEdgeTestServer(
	connection *websocket.Conn,
	registry *publicedge.Registry,
	initialSnapshot chan<- publicedge.SnapshotPayload,
	changedSnapshot chan<- publicedge.SnapshotPayload,
) error {
	hello, err := receivePublicEdgeFrame(connection)
	if err != nil {
		return err
	}
	now := time.Now()
	session, challenge, err := registry.Open("server-connection-1", hello, now)
	if err != nil {
		return err
	}
	defer session.Close()
	if err := sendPublicEdgeFrame(connection, challenge, now.Add(time.Second)); err != nil {
		return err
	}
	authenticate, err := receivePublicEdgeFrame(connection)
	if err != nil {
		return err
	}
	acknowledgement, err := session.Authenticate(authenticate, time.Now())
	if err != nil {
		return err
	}
	if err := sendPublicEdgeFrame(connection, acknowledgement, time.Now().Add(time.Second)); err != nil {
		return err
	}
	for {
		frame, err := receivePublicEdgeFrame(connection)
		if err != nil {
			return err
		}
		if err := session.AcceptGateway(frame, time.Now()); err != nil {
			return err
		}
		switch frame.Type {
		case publicedge.FrameStateSnapshot:
			var payload publicedge.SnapshotPayload
			if err := decodePublicEdgeTestPayload(frame, &payload); err != nil {
				return err
			}
			if payload.Revision == 1 {
				select {
				case initialSnapshot <- payload:
				default:
				}
			} else if payload.Revision == 2 {
				select {
				case changedSnapshot <- payload:
				default:
				}
			}
		default:
			return errors.New("public edge connector emitted a non-snapshot frame")
		}
	}
}

func decodePublicEdgeTestPayload(frame publicedge.Frame, target any) error {
	return jsonUnmarshalStrict(frame.Payload, target)
}

func jsonUnmarshalStrict(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func publicEdgeTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func writePublicEdgeTestSecret(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
