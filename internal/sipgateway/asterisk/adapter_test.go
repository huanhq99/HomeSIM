package asterisk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
	"golang.org/x/net/websocket"
)

type answerBehavior int

const (
	answerApplied answerBehavior = iota
	answerRejected
	answerAborted
	answerBlocked
)

type fakeAsterisk struct {
	t           *testing.T
	username    string
	password    string
	app         string
	server      *httptest.Server
	entityID    string
	version     string
	startupTime string
	policyID    string
	infoRaw     []byte

	mu                sync.Mutex
	eventWS           *websocket.Conn
	mediaWS           *websocket.Conn
	eventReady        chan struct{}
	mediaReady        chan struct{}
	eventReadyOnce    sync.Once
	mediaReadyOnce    sync.Once
	eventWriteMu      sync.Mutex
	mediaWriteMu      sync.Mutex
	channels          map[string]string
	bridges           map[string][]string
	mediaChannelID    string
	mediaConnection   string
	mediaReceived     chan []byte
	answerBehavior    answerBehavior
	answerCalls       atomic.Int64
	answerEntered     chan struct{}
	answerExited      chan struct{}
	answerEnterOnce   sync.Once
	answerExitOnce    sync.Once
	endCalls          atomic.Int64
	createEndpoint    string
	inspectMutate     func(*ariChannel)
	requestLog        []string
	startupChannels   []ariChannel
	barrierHook       func(int, string)
	channelListHook   func()
	barrierNoEvent    bool
	barrierStatus     int
	channelListStatus int
	channelListRaw    []byte
	channelGetCalls   atomic.Int64
	channelGetHook    func(int, string)
	authFailures      atomic.Int64
	eventConnections  atomic.Int64
	barrierCalls      atomic.Int64
	infoCalls         atomic.Int64
	infoHook          func(int)
	restRequestHook   func(int, ariRESTRequest) (*ariRESTResponse, bool)
	restResponseHook  func(int, *ariRESTResponse)
	restRequests      atomic.Int64
	directRESTCalls   atomic.Int64
	closed            chan struct{}
	closeOnce         sync.Once
}

func newFakeAsterisk(t *testing.T) *fakeAsterisk {
	t.Helper()
	fake := &fakeAsterisk{
		t: t, username: "ari-user", password: "ari-secret", app: "djonehub",
		entityID: "00:11:22:33:44:55", version: "22.10.1",
		startupTime: "2026-08-14T01:02:03.000+00:00", policyID: "dj1-incoming-v1",
		eventReady: make(chan struct{}), mediaReady: make(chan struct{}),
		channels: make(map[string]string), bridges: make(map[string][]string),
		mediaConnection: "media-connection-0001", mediaReceived: make(chan []byte, 16),
		answerEntered: make(chan struct{}), answerExited: make(chan struct{}),
		closed: make(chan struct{}),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.close)
	return fake
}

func (f *fakeAsterisk) config() Config {
	return Config{
		GatewayID: "home-asterisk", BaseURL: f.server.URL + "/ari",
		Application: f.app, IncomingContext: "from-volte-gateway", IncomingEndpoint: "volte-gateway",
		Username: f.username, Password: f.password,
		ExpectedVersion: f.version, IncomingPolicyID: f.policyID, ExpectedEntityID: f.entityID,
		RecoverySecret: []byte("0123456789abcdef0123456789abcdef"),
		HTTPTimeout:    time.Second, PrepareTimeout: 2 * time.Second,
		ConfirmTimeout: time.Second, IOTimeout: time.Second,
	}
}

func (f *fakeAsterisk) close() {
	f.closeOnce.Do(func() {
		close(f.closed)
		f.mu.Lock()
		if f.eventWS != nil {
			_ = f.eventWS.Close()
		}
		if f.mediaWS != nil {
			_ = f.mediaWS.Close()
		}
		f.mu.Unlock()
		f.server.Close()
	})
}

func (f *fakeAsterisk) handle(response http.ResponseWriter, request *http.Request) {
	username, password, ok := request.BasicAuth()
	if !ok || username != f.username || password != f.password || request.URL.User != nil ||
		request.URL.Query().Get("api_key") != "" {
		f.authFailures.Add(1)
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if request.URL.Path == "/ari/events" {
		f.handleEvents(response, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/media/") {
		f.handleMedia(response, request)
		return
	}
	f.handleREST(response, request)
}

func (f *fakeAsterisk) handleEvents(response http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("app") != f.app || request.URL.Query().Get("subscribeAll") != "false" {
		http.Error(response, "bad query", http.StatusBadRequest)
		return
	}
	server := websocket.Server{
		Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil },
		Handler: func(connection *websocket.Conn) {
			f.eventConnections.Add(1)
			f.mu.Lock()
			f.eventWS = connection
			f.mu.Unlock()
			f.eventReadyOnce.Do(func() { close(f.eventReady) })
			operationCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var operations sync.WaitGroup
			defer operations.Wait()
			for {
				var message framedMessage
				if err := receiveFrame.Receive(connection, &message); err != nil {
					cancel()
					return
				}
				var restRequest ariRESTRequest
				if message.payloadType != websocket.TextFrame ||
					decodeExactJSON(message.data, &restRequest) != nil ||
					restRequest.Type != "RESTRequest" ||
					!validOpaque(restRequest.TransactionID, 1, 256) ||
					!validOpaque(restRequest.RequestID, 1, 256) ||
					!validRESTURI(restRequest.URI) {
					return
				}
				operations.Add(1)
				go func(request ariRESTRequest) {
					defer operations.Done()
					f.handleWSREST(operationCtx, connection, request)
				}(restRequest)
			}
		},
	}
	server.ServeHTTP(response, request)
}

func (f *fakeAsterisk) handleWSREST(ctx context.Context, connection *websocket.Conn, request ariRESTRequest) {
	index := int(f.restRequests.Add(1))
	f.mu.Lock()
	requestHook := f.restRequestHook
	f.mu.Unlock()
	if requestHook != nil {
		if response, handled := requestHook(index, request); handled {
			f.sendWSRESTResponse(connection, response)
			return
		}
	}
	target := f.server.URL + "/ari/" + request.URI
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, target, nil)
	if err != nil {
		_ = connection.Close()
		return
	}
	httpRequest.SetBasicAuth(f.username, f.password)
	httpRequest.Header.Set("X-Fake-ARI-REST-Over-WebSocket", "1")
	response, err := f.server.Client().Do(httpRequest)
	if err != nil {
		_ = connection.Close()
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, defaultMaxHTTPBodyBytes+1))
	_ = response.Body.Close()
	if readErr != nil || len(body) > defaultMaxHTTPBodyBytes {
		_ = connection.Close()
		return
	}
	f.mu.Lock()
	entityID := f.entityID
	hook := f.restResponseHook
	f.mu.Unlock()
	restResponse := ariRESTResponse{
		Type: "RESTResponse", TransactionID: request.TransactionID,
		RequestID: request.RequestID, StatusCode: response.StatusCode,
		ReasonPhrase: http.StatusText(response.StatusCode), URI: request.URI,
		MessageBody: string(body), Timestamp: "2026-08-14T01:03:03.000+00:00",
		AsteriskID: entityID, Application: f.app,
	}
	if len(body) != 0 {
		restResponse.ContentType = "application/json"
	}
	if hook != nil {
		hook(index, &restResponse)
	}
	f.sendWSRESTResponse(connection, &restResponse)
}

func (f *fakeAsterisk) sendWSRESTResponse(connection *websocket.Conn, response *ariRESTResponse) {
	if response == nil {
		_ = connection.Close()
		return
	}
	payload, err := json.Marshal(response)
	if err != nil {
		_ = connection.Close()
		return
	}
	f.eventWriteMu.Lock()
	err = websocket.Message.Send(connection, string(payload))
	f.eventWriteMu.Unlock()
	if err != nil {
		_ = connection.Close()
	}
}

func (f *fakeAsterisk) handleMedia(response http.ResponseWriter, request *http.Request) {
	connectionID := strings.TrimPrefix(request.URL.Path, "/media/")
	if connectionID != f.mediaConnection {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	server := websocket.Server{
		Handshake: func(config *websocket.Config, _ *http.Request) error {
			if !slices.Contains(config.Protocol, "media") {
				return errors.New("missing media protocol")
			}
			config.Protocol = []string{"media"}
			return nil
		},
		Handler: func(connection *websocket.Conn) {
			f.mu.Lock()
			f.mediaWS = connection
			mediaChannelID := f.mediaChannelID
			f.mu.Unlock()
			start, _ := json.Marshal(mediaControl{
				Event: "MEDIA_START", ConnectionID: f.mediaConnection,
				ChannelID: mediaChannelID, Format: "ulaw",
				OptimalFrameSize: pcmuFrameBytes, Ptime: 20,
			})
			_ = websocket.Message.Send(connection, string(start))
			f.mediaReadyOnce.Do(func() { close(f.mediaReady) })
			for {
				var message framedMessage
				if err := receiveFrame.Receive(connection, &message); err != nil {
					return
				}
				if message.payloadType == websocket.BinaryFrame {
					select {
					case f.mediaReceived <- append([]byte(nil), message.data...):
					default:
					}
				} else if message.payloadType == websocket.TextFrame {
					var command mediaCommand
					if json.Unmarshal(message.data, &command) != nil || command.Command != "GET_STATUS" {
						return
					}
					status, _ := json.Marshal(mediaControl{Event: "STATUS", ChannelID: mediaChannelID})
					f.mediaWriteMu.Lock()
					err := websocket.Message.Send(connection, string(status))
					f.mediaWriteMu.Unlock()
					if err != nil {
						return
					}
				}
			}
		},
	}
	server.ServeHTTP(response, request)
}

func (f *fakeAsterisk) handleREST(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Fake-ARI-REST-Over-WebSocket") != "1" {
		f.directRESTCalls.Add(1)
	}
	path := strings.TrimPrefix(request.URL.Path, "/ari/")
	f.mu.Lock()
	f.requestLog = append(f.requestLog, request.Method+" "+path)
	f.mu.Unlock()
	switch {
	case request.Method == http.MethodGet && path == "asterisk/info":
		if request.URL.Query().Get("only") != "system,status" {
			http.Error(response, "bad query", http.StatusBadRequest)
			return
		}
		index := int(f.infoCalls.Add(1))
		f.mu.Lock()
		hook := f.infoHook
		f.mu.Unlock()
		if hook != nil {
			hook(index)
		}
		f.mu.Lock()
		entityID, version, startupTime := f.entityID, f.version, f.startupTime
		infoRaw := append([]byte(nil), f.infoRaw...)
		f.mu.Unlock()
		if infoRaw != nil {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(infoRaw)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{
			"system": map[string]string{"entity_id": entityID, "version": version},
			"status": map[string]string{
				"startup_time": startupTime, "last_reload_time": startupTime,
			},
		})
	case request.Method == http.MethodPost && strings.HasPrefix(path, "events/user/"):
		marker := strings.TrimPrefix(path, "events/user/")
		if !validOpaque(marker, 1, 128) || request.URL.Query().Get("application") != f.app {
			http.Error(response, "bad barrier", http.StatusBadRequest)
			return
		}
		index := int(f.barrierCalls.Add(1))
		f.mu.Lock()
		hook, noEvent, status := f.barrierHook, f.barrierNoEvent, f.barrierStatus
		f.mu.Unlock()
		if hook != nil {
			hook(index, marker)
		}
		if status != 0 && status != http.StatusNoContent {
			http.Error(response, "barrier failure", status)
			return
		}
		response.WriteHeader(http.StatusNoContent)
		if !noEvent {
			_ = f.sendEvent(ariEvent{
				Type: "ChannelUserevent", Application: f.app, EventName: marker,
			})
		}
	case request.Method == http.MethodGet && path == "channels":
		f.mu.Lock()
		channels := append([]ariChannel(nil), f.startupChannels...)
		for channelID, state := range f.channels {
			channel := ariChannel{ID: channelID, Name: "WebSocket/INCOMING-00000001", State: state}
			if !strings.HasPrefix(channelID, "dj1m_") {
				channel = f.incomingChannel(channelID, state)
			}
			channels = append(channels, channel)
		}
		hook, status := f.channelListHook, f.channelListStatus
		raw := append([]byte(nil), f.channelListRaw...)
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
		if status != 0 && status != http.StatusOK {
			http.Error(response, "channel list failure", status)
			return
		}
		if raw != nil {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(raw)
			return
		}
		writeJSON(response, http.StatusOK, channels)
	case request.Method == http.MethodPost && path == "channels/create":
		channelID := request.URL.Query().Get("channelId")
		f.mu.Lock()
		f.mediaChannelID = channelID
		f.channels[channelID] = "Down"
		f.createEndpoint = request.URL.Query().Get("endpoint")
		f.mu.Unlock()
		writeJSON(response, http.StatusOK, ariChannel{ID: channelID, Name: "WebSocket/INCOMING-00000001", State: "Down"})
	case request.Method == http.MethodPost && strings.HasPrefix(path, "channels/") && strings.HasSuffix(path, "/dial"):
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "channels/"), "/dial")
		writeJSON(response, http.StatusNoContent, nil)
		_ = f.sendEvent(ariEvent{
			Type: "Dial", DialStatus: "",
			Peer: &ariChannel{ID: channelID, Name: "WebSocket/INCOMING-00000001", ChannelVars: map[string]string{
				"MEDIA_WEBSOCKET_CONNECTION_ID": f.mediaConnection,
			}},
		})
	case request.Method == http.MethodPost && strings.HasSuffix(path, "/answer"):
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "channels/"), "/answer")
		f.answerCalls.Add(1)
		switch f.answerBehavior {
		case answerRejected:
			http.Error(response, "conflict", http.StatusConflict)
		case answerAborted:
			hijacker, ok := response.(http.Hijacker)
			if !ok {
				panic("missing hijacker")
			}
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
		case answerBlocked:
			f.answerEnterOnce.Do(func() { close(f.answerEntered) })
			<-request.Context().Done()
			f.answerExitOnce.Do(func() { close(f.answerExited) })
		default:
			f.mu.Lock()
			f.channels[channelID] = "Up"
			f.mu.Unlock()
			response.WriteHeader(http.StatusNoContent)
			active := f.incomingChannel(channelID, "Up")
			_ = f.sendEvent(ariEvent{Type: "ChannelStateChange", Channel: &active})
		}
	case request.Method == http.MethodDelete && strings.HasPrefix(path, "channels/"):
		channelID := strings.TrimPrefix(path, "channels/")
		f.mu.Lock()
		_, exists := f.channels[channelID]
		delete(f.channels, channelID)
		f.mu.Unlock()
		if !exists {
			http.Error(response, "not found", http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(channelID, "dj1m_") {
			f.endCalls.Add(1)
		}
		response.WriteHeader(http.StatusNoContent)
		destroyed := ariChannel{ID: channelID}
		if !strings.HasPrefix(channelID, "dj1m_") {
			destroyed = f.incomingChannel(channelID, "Down")
		}
		_ = f.sendEvent(ariEvent{Type: "ChannelDestroyed", Channel: &destroyed})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "channels/"):
		channelID := strings.TrimPrefix(path, "channels/")
		index := int(f.channelGetCalls.Add(1))
		f.mu.Lock()
		hook := f.channelGetHook
		f.mu.Unlock()
		if hook != nil {
			hook(index, channelID)
		}
		f.mu.Lock()
		state, exists := f.channels[channelID]
		f.mu.Unlock()
		if !exists {
			http.Error(response, "not found", http.StatusNotFound)
			return
		}
		channel := ariChannel{ID: channelID, State: state}
		if !strings.HasPrefix(channelID, "dj1m_") {
			channel = f.incomingChannel(channelID, state)
		}
		f.mu.Lock()
		mutate := f.inspectMutate
		f.mu.Unlock()
		if mutate != nil && !strings.HasPrefix(channelID, "dj1m_") {
			mutate(&channel)
		}
		writeJSON(response, http.StatusOK, channel)
	case request.Method == http.MethodPost && strings.HasPrefix(path, "bridges/") && strings.HasSuffix(path, "/addChannel"):
		bridgeID := strings.TrimSuffix(strings.TrimPrefix(path, "bridges/"), "/addChannel")
		f.mu.Lock()
		f.bridges[bridgeID] = append(f.bridges[bridgeID], request.URL.Query().Get("channel"))
		f.mu.Unlock()
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodPost && strings.HasPrefix(path, "bridges/"):
		bridgeID := strings.TrimPrefix(path, "bridges/")
		f.mu.Lock()
		f.bridges[bridgeID] = nil
		f.mu.Unlock()
		writeJSON(response, http.StatusOK, ariBridge{ID: bridgeID})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "bridges/"):
		bridgeID := strings.TrimPrefix(path, "bridges/")
		f.mu.Lock()
		channels, exists := f.bridges[bridgeID]
		f.mu.Unlock()
		if !exists {
			http.Error(response, "not found", http.StatusNotFound)
			return
		}
		writeJSON(response, http.StatusOK, ariBridge{ID: bridgeID, Channels: append([]string(nil), channels...)})
	case request.Method == http.MethodDelete && strings.HasPrefix(path, "bridges/"):
		bridgeID := strings.TrimPrefix(path, "bridges/")
		f.mu.Lock()
		delete(f.bridges, bridgeID)
		f.mu.Unlock()
		response.WriteHeader(http.StatusNoContent)
	default:
		http.Error(response, "not found", http.StatusNotFound)
	}
}

func (f *fakeAsterisk) sendEvent(event ariEvent) error {
	select {
	case <-f.eventReady:
	case <-time.After(time.Second):
		return errors.New("event socket not ready")
	}
	f.mu.Lock()
	connection := f.eventWS
	entityID := f.entityID
	f.mu.Unlock()
	if event.Application == "" {
		event.Application = f.app
	}
	if event.AsteriskID == "" {
		event.AsteriskID = entityID
	}
	if event.Timestamp == "" {
		event.Timestamp = "2026-08-14T01:03:03.000+00:00"
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f.eventWriteMu.Lock()
	defer f.eventWriteMu.Unlock()
	return websocket.Message.Send(connection, string(data))
}

func (f *fakeAsterisk) sendRawEvent(data string) error {
	select {
	case <-f.eventReady:
	case <-time.After(time.Second):
		return errors.New("event socket not ready")
	}
	f.mu.Lock()
	connection := f.eventWS
	f.mu.Unlock()
	f.eventWriteMu.Lock()
	defer f.eventWriteMu.Unlock()
	return websocket.Message.Send(connection, data)
}

func (f *fakeAsterisk) incoming(handle string) {
	f.mu.Lock()
	f.channels[handle] = "Ring"
	f.mu.Unlock()
	if err := f.sendEvent(ariEvent{
		Type: "StasisStart", Args: []string{"incoming"},
		Channel: pointerARIChannel(f.incomingChannel(handle, "Ring")),
	}); err != nil {
		f.t.Fatalf("send incoming: %v", err)
	}
}

func (f *fakeAsterisk) incomingChannel(handle, state string) ariChannel {
	channel := ariChannel{
		ID: handle, Name: "PJSIP/volte-gateway-00000001", State: state,
		CreationTime: "2026-08-14T01:03:04.000+00:00",
		ProtocolID:   "sip-call-id-0001@pbx.invalid",
	}
	channel.Dialplan.Context = "from-volte-gateway"
	channel.Dialplan.Exten = "10086"
	channel.Dialplan.Priority = 4
	channel.Dialplan.AppName = "Stasis"
	channel.Dialplan.AppData = f.app + ",incoming"
	channel.ChannelVars = map[string]string{incomingPolicyVariable: f.policyID}
	return channel
}

func pointerARIChannel(channel ariChannel) *ariChannel { return &channel }

func (f *fakeAsterisk) sendMediaControl(control mediaControl) error {
	select {
	case <-f.mediaReady:
	case <-time.After(time.Second):
		return errors.New("media socket not ready")
	}
	f.mu.Lock()
	connection := f.mediaWS
	f.mu.Unlock()
	data, err := json.Marshal(control)
	if err != nil {
		return err
	}
	f.mediaWriteMu.Lock()
	defer f.mediaWriteMu.Unlock()
	return websocket.Message.Send(connection, string(data))
}

func (f *fakeAsterisk) sendMedia(payload []byte) error {
	select {
	case <-f.mediaReady:
	case <-time.After(time.Second):
		return errors.New("media socket not ready")
	}
	f.mu.Lock()
	connection := f.mediaWS
	f.mu.Unlock()
	f.mediaWriteMu.Lock()
	defer f.mediaWriteMu.Unlock()
	return websocket.Message.Send(connection, append([]byte(nil), payload...))
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(response).Encode(value)
	}
}

func newObservedIncoming(t *testing.T, fake *fakeAsterisk) (*Adapter, sipgateway.CallSnapshot) {
	t.Helper()
	adapter, err := New(context.Background(), fake.config())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	restart, err := adapter.Observe(context.Background())
	if err != nil || restart.Kind != sipgateway.EventGatewayRestart || restart.BootEpoch == "" {
		t.Fatalf("restart=%+v err=%v", restart, err)
	}
	fake.incoming("incoming-channel-0001")
	incoming, err := adapter.Observe(context.Background())
	if err != nil || incoming.Call == nil || incoming.Call.State != sipgateway.ProviderCallIncoming {
		t.Fatalf("incoming=%+v err=%v", incoming, err)
	}
	return adapter, *incoming.Call
}

func prepareMedia(t *testing.T, adapter *Adapter, incoming sipgateway.CallSnapshot) sipgateway.MediaSession {
	t.Helper()
	media, err := adapter.PrepareMedia(context.Background(), sipgateway.PrepareMediaRequest{
		Call: incoming.Ref, Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
	})
	if err != nil {
		t.Fatalf("PrepareMedia: %v", err)
	}
	if snapshot := media.Snapshot(); !snapshot.Prepared || snapshot.Closed || snapshot.Codec != sipgateway.CodecPCMU {
		t.Fatalf("media snapshot=%+v", snapshot)
	}
	return media
}

func TestConfigRequiresTLSOrLoopbackAndRedactsCredentials(t *testing.T) {
	_, err := New(context.Background(), Config{
		GatewayID: "gateway", BaseURL: "http://pbx.example/ari", Application: "app",
		IncomingContext: "from-gateway", IncomingEndpoint: "gateway-endpoint",
		Username: "user", Password: "top-secret",
	})
	if !errors.Is(err, ErrInsecureEndpoint) {
		t.Fatalf("insecure endpoint error=%v", err)
	}
	_, err = New(context.Background(), Config{
		GatewayID: "gateway", BaseURL: "https://user:top-secret@pbx.example/ari", Application: "app",
		IncomingContext: "from-gateway", IncomingEndpoint: "gateway-endpoint",
		Username: "user", Password: "top-secret",
	})
	if !errors.Is(err, ErrConfiguration) {
		t.Fatalf("URL userinfo error=%v", err)
	}
	cfg := Config{Username: "user", Password: "top-secret", BaseURL: "https://pbx.example/ari"}
	if strings.Contains(cfg.String(), "top-secret") || strings.Contains(cfg.GoString(), "top-secret") {
		t.Fatal("Config formatting leaked a credential")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(encoded), "top-secret") {
		t.Fatalf("Config JSON leaked a credential: %s err=%v", encoded, err)
	}
}

func TestConfigRequiresCallerProvidedVersionAndIncomingPolicy(t *testing.T) {
	fake := newFakeAsterisk(t)
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing version", mutate: func(cfg *Config) { cfg.ExpectedVersion = "" }},
		{name: "missing policy", mutate: func(cfg *Config) { cfg.IncomingPolicyID = "" }},
		{name: "unsafe policy", mutate: func(cfg *Config) { cfg.IncomingPolicyID = "bad policy" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := fake.config()
			test.mutate(&cfg)
			if err := ValidateConfig(cfg); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("ValidateConfig error=%v", err)
			}
		})
	}
}

func TestHTTPClientBoundsBodiesAndNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "user" || password != "secret" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/ari/redirect":
			response.Header().Set("Location", "/ari/target")
			response.WriteHeader(http.StatusTemporaryRedirect)
		case "/ari/target":
			redirected.Add(1)
			response.WriteHeader(http.StatusNoContent)
		case "/ari/large":
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte(strings.Repeat("x", 33)))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	normalized, err := normalizeConfig(Config{
		GatewayID: "gateway", BaseURL: server.URL + "/ari", Application: "app",
		IncomingContext: "from-gateway", IncomingEndpoint: "gateway-endpoint",
		ExpectedVersion: "22.10.1", IncomingPolicyID: "incoming-v1",
		Username: "user", Password: "secret", MaxHTTPBodyBytes: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := newARIHTTPClient(normalized)
	defer client.close()
	response, err := client.do(context.Background(), http.MethodPost, []string{"redirect"}, nil)
	if err != nil || response.status != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("redirect response=%+v followed=%d err=%v", response, redirected.Load(), err)
	}
	if _, err := client.do(context.Background(), http.MethodGet, []string{"large"}, nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("oversized response error=%v", err)
	}
}

func TestIncomingPrepareAnswerMediaAndEnd(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	media := prepareMedia(t, adapter, incoming)
	if got := fake.answerCalls.Load(); got != 0 {
		t.Fatalf("PrepareMedia answered incoming call: calls=%d", got)
	}
	fake.mu.Lock()
	createdEndpoint := fake.createEndpoint
	fake.mu.Unlock()
	if createdEndpoint != mediaEndpoint {
		t.Fatalf("created endpoint=%q", createdEndpoint)
	}

	answerRequest := sipgateway.AnswerIncomingRequest{
		Call: incoming.Ref, CommandID: "answer-command-0001", MediaLeaseID: media.ID(),
	}
	answered, err := adapter.AnswerIncoming(context.Background(), answerRequest)
	if err != nil || answered.Outcome != sipgateway.CommandApplied || answered.Current == nil ||
		answered.Current.State != sipgateway.ProviderCallActive || answered.Current.Ref.Revision <= incoming.Ref.Revision {
		t.Fatalf("answered=%+v err=%v", answered, err)
	}
	replayed, err := adapter.AnswerIncoming(context.Background(), answerRequest)
	if err != nil || replayed.Outcome != sipgateway.CommandApplied || fake.answerCalls.Load() != 1 {
		t.Fatalf("answer replay=%+v calls=%d err=%v", replayed, fake.answerCalls.Load(), err)
	}
	conflict := answerRequest
	conflict.MediaLeaseID = "another-private-lease"
	if _, err := adapter.AnswerIncoming(context.Background(), conflict); !errors.Is(err, sipgateway.ErrCommandConflict) {
		t.Fatalf("answer conflict error=%v", err)
	}

	if _, err := media.Activate(41); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	gatewayPayload := make([]byte, pcmuFrameBytes)
	for index := range gatewayPayload {
		gatewayPayload[index] = byte(index)
	}
	if err := fake.sendMedia(gatewayPayload); err != nil {
		t.Fatal(err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
	defer readCancel()
	frame, err := media.ReadGatewayFrame(readCtx)
	if err != nil || frame.Sequence == 0 || !slices.Equal(frame.Payload, gatewayPayload) {
		t.Fatalf("gateway frame=%+v err=%v", frame, err)
	}
	clientPayload := make([]byte, pcmuFrameBytes)
	clientPayload[0] = 0x7f
	if err := media.WriteGatewayFrame(context.Background(), sipgateway.MediaFrame{Sequence: 1, Payload: clientPayload}); err != nil {
		t.Fatalf("WriteGatewayFrame: %v", err)
	}
	select {
	case received := <-fake.mediaReceived:
		if !slices.Equal(received, clientPayload) {
			t.Fatalf("media payload mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("Asterisk did not receive client media")
	}
	if snapshot := media.Snapshot(); !snapshot.BidirectionalFresh(time.Now(), time.Second) {
		t.Fatalf("media is not fresh: %+v", snapshot)
	}

	active := *answered.Current
	ended, err := adapter.EndActive(context.Background(), sipgateway.EndActiveRequest{
		Call: active.Ref, CommandID: "end-command-0001",
	})
	if err != nil || ended.Outcome != sipgateway.CommandApplied || ended.Current == nil ||
		ended.Current.State != sipgateway.ProviderCallEnded || fake.endCalls.Load() != 1 {
		t.Fatalf("ended=%+v calls=%d err=%v", ended, fake.endCalls.Load(), err)
	}
	if fake.authFailures.Load() != 0 {
		t.Fatalf("authentication failures=%d", fake.authFailures.Load())
	}
	if fake.directRESTCalls.Load() != 1 || fake.restRequests.Load() == 0 {
		t.Fatalf("live REST transport direct=%d websocket=%d", fake.directRESTCalls.Load(), fake.restRequests.Load())
	}
}

func TestIncomingRequiresExactDialplanEndpointArgumentAndState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ariEvent)
	}{
		{name: "argument shape", mutate: func(event *ariEvent) { event.Args = []string{"other", "incoming"} }},
		{name: "dialplan context", mutate: func(event *ariEvent) { event.Channel.Dialplan.Context = "wrong-context" }},
		{name: "PJSIP endpoint", mutate: func(event *ariEvent) { event.Channel.Name = "PJSIP/volte-gateway-evil-00000001" }},
		{name: "dialplan app data", mutate: func(event *ariEvent) { event.Channel.Dialplan.AppData = "djonehub,wrong" }},
		{name: "missing policy marker", mutate: func(event *ariEvent) { event.Channel.ChannelVars = nil }},
		{name: "wrong policy marker", mutate: func(event *ariEvent) {
			event.Channel.ChannelVars[incomingPolicyVariable] = "other-policy"
		}},
		{name: "channel state", mutate: func(event *ariEvent) { event.Channel.State = "Down" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			adapter, err := New(context.Background(), fake.config())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = adapter.Close() })
			if _, err := adapter.Observe(context.Background()); err != nil {
				t.Fatal(err)
			}
			channel := fake.incomingChannel("untrusted-incoming", "Ring")
			event := ariEvent{Type: "StasisStart", Args: []string{"incoming"}, Channel: &channel}
			test.mutate(&event)
			if err := fake.sendEvent(event); err != nil {
				t.Fatal(err)
			}
			select {
			case <-adapter.done:
			case <-time.After(time.Second):
				t.Fatal("adapter did not fail closed on an inexact incoming channel")
			}
		})
	}
}

func TestInspectRejectsUnexpectedChannelState(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	fake.mu.Lock()
	fake.channels[incoming.Ref.ProviderHandle] = "Down"
	fake.mu.Unlock()
	if _, err := adapter.Inspect(context.Background(), incoming.Ref); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Inspect unexpected state error=%v", err)
	}
}

func TestKnownCallEventRejectsUnexpectedChannelState(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	unexpected := fake.incomingChannel(incoming.Ref.ProviderHandle, "Busy")
	if err := fake.sendEvent(ariEvent{Type: "ChannelStateChange", Channel: &unexpected}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-adapter.done:
	case <-time.After(time.Second):
		t.Fatal("adapter did not fail closed on an unexpected known-call state")
	}
}

func TestKnownActiveCallIgnoresDelayedRingingEvent(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	active := fake.incomingChannel(incoming.Ref.ProviderHandle, "Up")
	if err := adapter.applyARIEvent(ariEvent{
		Type: "ChannelStateChange", Application: fake.app, AsteriskID: fake.entityID,
		Timestamp: "2026-08-14T01:03:05Z", Channel: &active,
	}); err != nil {
		t.Fatal(err)
	}
	rollback := fake.incomingChannel(incoming.Ref.ProviderHandle, "Ring")
	if err := adapter.applyARIEvent(ariEvent{
		Type: "ChannelStateChange", Application: fake.app, AsteriskID: fake.entityID,
		Timestamp: "2026-08-14T01:03:06Z", Channel: &rollback,
	}); err != nil {
		t.Fatalf("delayed ringing event error=%v", err)
	}
	current, err := adapter.currentDialog(incoming.Ref)
	if err != nil || current.State != sipgateway.ProviderCallActive || current.Ref.Revision != incoming.Ref.Revision+1 {
		t.Fatalf("current after delayed ringing=%+v err=%v", current, err)
	}
}

func TestUnrelatedDialEventIsIgnoredBeforeMediaIdentityValidation(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), fake.config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := fake.sendEvent(ariEvent{
		Type: "Dial", Peer: &ariChannel{ID: "unrelated-channel", Name: "PJSIP/unrelated-00000001"},
	}); err != nil {
		t.Fatal(err)
	}
	fake.incoming("incoming-after-unrelated-dial")
	observed, err := adapter.Observe(context.Background())
	if err != nil || observed.Call == nil || observed.Call.State != sipgateway.ProviderCallIncoming {
		t.Fatalf("observation after unrelated Dial=%+v err=%v", observed, err)
	}
}

func TestPrepareMediaDoesNotPublishSessionClosedBeforeRegistration(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	adapter.beforeMediaRegister = func(session *mediaSession) { session.closeLocal() }
	media, err := adapter.PrepareMedia(context.Background(), sipgateway.PrepareMediaRequest{
		Call: incoming.Ref, Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
	})
	if media != nil || !errors.Is(err, sipgateway.ErrMediaNotPrepared) {
		t.Fatalf("PrepareMedia media=%v err=%v", media, err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.media) != 0 || adapter.mediaByCall[incoming.Ref.ProviderHandle] != "" {
		t.Fatalf("closed media session remained registered: media=%d owner=%q",
			len(adapter.media), adapter.mediaByCall[incoming.Ref.ProviderHandle])
	}
}

func TestPrepareMediaRepeatsCleanupWhenSessionClosesBeforeBridgeCreation(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	adapter.afterMediaPrepared = func(session *mediaSession) { _ = session.Close() }
	media, err := adapter.PrepareMedia(context.Background(), sipgateway.PrepareMediaRequest{
		Call: incoming.Ref, Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
	})
	if media != nil || !errors.Is(err, sipgateway.ErrMediaNotPrepared) {
		t.Fatalf("PrepareMedia media=%v err=%v", media, err)
	}
	eventuallyAsterisk(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		_, mediaExists := fake.channels[fake.mediaChannelID]
		return !mediaExists && len(fake.bridges) == 0
	})
	adapter.afterMediaPrepared = nil
	if retry := prepareMedia(t, adapter, incoming); retry == nil {
		t.Fatal("same ringing call could not prepare a fresh media lease")
	}
}

func TestAdapterCloseDeletesOnlyOwnedBridgeAndMediaChannel(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	_ = prepareMedia(t, adapter, incoming)
	fake.mu.Lock()
	mediaChannelID := fake.mediaChannelID
	if len(fake.bridges) != 1 {
		fake.mu.Unlock()
		t.Fatalf("prepared bridge count=%d", len(fake.bridges))
	}
	fake.mu.Unlock()
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, exists := fake.channels[mediaChannelID]; exists {
		t.Fatal("adapter-owned media channel remained after Close")
	}
	if len(fake.bridges) != 0 {
		t.Fatalf("adapter-owned bridge remained after Close: %d", len(fake.bridges))
	}
	if _, exists := fake.channels[incoming.Ref.ProviderHandle]; !exists {
		t.Fatal("Close deleted the incoming PJSIP channel")
	}
}

func TestCloseCancelsInFlightHTTPMutationAndSuppressesBufferedEvents(t *testing.T) {
	fake := newFakeAsterisk(t)
	fake.answerBehavior = answerBlocked
	adapter, incoming := newObservedIncoming(t, fake)
	media := prepareMedia(t, adapter, incoming)
	answerDone := make(chan error, 1)
	go func() {
		_, err := adapter.AnswerIncoming(context.Background(), sipgateway.AnswerIncomingRequest{
			Call: incoming.Ref, CommandID: "blocked-answer-command", MediaLeaseID: media.ID(),
		})
		answerDone <- err
	}()
	select {
	case <-fake.answerEntered:
	case <-time.After(time.Second):
		t.Fatal("answer mutation did not reach the server")
	}
	closed := make(chan error, 1)
	go func() { closed <- adapter.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the in-flight HTTP mutation")
	}
	select {
	case <-answerDone:
	case <-time.After(time.Second):
		t.Fatal("answer mutation remained blocked after Close")
	}
	select {
	case <-fake.answerExited:
	case <-time.After(time.Second):
		t.Fatal("server request context was not canceled")
	}
	if _, err := adapter.Observe(context.Background()); !errors.Is(err, sipgateway.ErrClosed) {
		t.Fatalf("Observe after Close error=%v", err)
	}
}

func TestMediaXOFFBlocksUntilXONAndCloseUnblocks(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	media := prepareMedia(t, adapter, incoming)
	session := media.(*mediaSession)
	fake.mu.Lock()
	channelID := fake.mediaChannelID
	fake.mu.Unlock()
	if err := fake.sendMediaControl(mediaControl{Event: "MEDIA_XOFF", ChannelID: channelID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		session.mu.Lock()
		paused := session.flowPaused
		session.mu.Unlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("MEDIA_XOFF was not applied")
		}
		time.Sleep(time.Millisecond)
	}
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := media.WriteGatewayFrame(writeCtx, sipgateway.MediaFrame{Sequence: 1, Payload: make([]byte, pcmuFrameBytes)})
	cancelWrite()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write during XOFF error=%v", err)
	}
	if err := fake.sendMediaControl(mediaControl{Event: "MEDIA_XON", ChannelID: channelID}); err != nil {
		t.Fatal(err)
	}
	if err := media.WriteGatewayFrame(context.Background(), sipgateway.MediaFrame{Sequence: 2, Payload: make([]byte, pcmuFrameBytes)}); err != nil {
		t.Fatalf("write after XON: %v", err)
	}

	readResult := make(chan error, 1)
	go func() {
		_, err := media.ReadGatewayFrame(context.Background())
		readResult <- err
	}()
	if err := media.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readResult:
		if !errors.Is(err, sipgateway.ErrMediaClosed) {
			t.Fatalf("blocked read error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock media read")
	}
}

func TestEarlyMediaBeyondCapacityIsDroppedUntilActivation(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	media := prepareMedia(t, adapter, incoming)
	for index := 0; index < defaultMediaQueueCapacity*3; index++ {
		payload := make([]byte, pcmuFrameBytes)
		payload[0] = byte(index)
		if err := fake.sendMedia(payload); err != nil {
			t.Fatal(err)
		}
	}
	eventuallyAsterisk(t, func() bool {
		snapshot := media.Snapshot()
		return snapshot.Prepared && !snapshot.Closed
	})
	if _, err := media.Activate(53); err != nil {
		t.Fatalf("Activate after early media: %v", err)
	}
	live := make([]byte, pcmuFrameBytes)
	live[0] = 0xa7
	if err := fake.sendMedia(live); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frame, err := media.ReadGatewayFrame(ctx)
	if err != nil || !slices.Equal(frame.Payload, live) {
		t.Fatalf("post-activation frame=%+v err=%v", frame, err)
	}
	if snapshot := media.Snapshot(); snapshot.GatewayToClientFrames != 1 ||
		snapshot.LastGatewayToClientEpoch != 53 {
		t.Fatalf("early media affected activation freshness: %+v", snapshot)
	}
}

func TestUnknownAndRejectedAnswerAreNeverRetried(t *testing.T) {
	for _, test := range []struct {
		name      string
		behavior  answerBehavior
		want      sipgateway.CommandOutcome
		wantState sipgateway.ProviderCallState
	}{
		{name: "aborted response is unknown", behavior: answerAborted, want: sipgateway.CommandUnknown},
		{name: "explicit unchanged conflict is rejected", behavior: answerRejected, want: sipgateway.CommandRejected, wantState: sipgateway.ProviderCallIncoming},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			fake.answerBehavior = test.behavior
			adapter, incoming := newObservedIncoming(t, fake)
			media := prepareMedia(t, adapter, incoming)
			request := sipgateway.AnswerIncomingRequest{
				Call: incoming.Ref, CommandID: "single-answer-command", MediaLeaseID: media.ID(),
			}
			result, err := adapter.AnswerIncoming(context.Background(), request)
			if err != nil || result.Outcome != test.want {
				t.Fatalf("first result=%+v err=%v", result, err)
			}
			if result.Current != nil && result.Current.State != test.wantState {
				t.Fatalf("current=%+v", result.Current)
			}
			replay, err := adapter.AnswerIncoming(context.Background(), request)
			if err != nil || replay.Outcome != test.want || fake.answerCalls.Load() != 1 {
				t.Fatalf("replay=%+v calls=%d err=%v", replay, fake.answerCalls.Load(), err)
			}
		})
	}
}

func TestObservationQueueIsBoundedAndCloseUnblocksObserve(t *testing.T) {
	fake := newFakeAsterisk(t)
	cfg := fake.config()
	cfg.EventCapacity = 1
	adapter, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake.incoming("queue-overflow-channel")
	select {
	case <-adapter.done:
	case <-time.After(time.Second):
		t.Fatal("adapter did not fail closed on observation overflow")
	}
	_ = adapter.Close()

	fake2 := newFakeAsterisk(t)
	adapter2, err := New(context.Background(), fake2.config())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter2.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	observeResult := make(chan error, 1)
	go func() {
		_, err := adapter2.Observe(context.Background())
		observeResult <- err
	}()
	if err := adapter2.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-observeResult:
		if !errors.Is(err, sipgateway.ErrClosed) {
			t.Fatalf("Observe close error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Observe")
	}
}

func TestAdapterAndMediaImplementContracts(t *testing.T) {
	// Keep an explicit compile-time assertion that Adapter implements the
	// entire provider contract while its concrete media stays private.
	var _ sipgateway.Adapter = (*Adapter)(nil)
	var _ sipgateway.RecoveryAdapter = (*Adapter)(nil)
	var _ sipgateway.RecoveryInspector = (*RecoveryInspector)(nil)
	var _ sipgateway.MediaSession = (*mediaSession)(nil)
}

func TestNewReadsAndOptionallyPinsExactPBXIdentityBeforeEvents(t *testing.T) {
	fake := newFakeAsterisk(t)
	cfg := fake.config()
	cfg.ExpectedStartupTime = fake.startupTime
	adapter, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	firstRequest := fake.requestLog[0]
	fake.mu.Unlock()
	if firstRequest != "GET asterisk/info" || fake.eventConnections.Load() != 1 {
		t.Fatalf("first request=%q event connections=%d", firstRequest, fake.eventConnections.Load())
	}

	bad := fake.config()
	bad.ExpectedEntityID = "wrong-pbx"
	if adapter, err := New(context.Background(), bad); adapter != nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("PBX pin mismatch adapter=%v err=%v", adapter, err)
	}
	if fake.eventConnections.Load() != 1 {
		t.Fatalf("PBX mismatch opened an event WebSocket: %d", fake.eventConnections.Load())
	}

	badVersion := fake.config()
	badVersion.ExpectedVersion = "22.10.0"
	if adapter, err := New(context.Background(), badVersion); adapter != nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("PBX version mismatch adapter=%v err=%v", adapter, err)
	}
	if fake.eventConnections.Load() != 1 {
		t.Fatalf("PBX version mismatch opened an event WebSocket: %d", fake.eventConnections.Load())
	}

	invalid := fake.config()
	invalid.ExpectedEntityID = ""
	invalid.ExpectedStartupTime = fake.startupTime
	if err := ValidateConfig(invalid); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("startup-only pin error=%v", err)
	}
}

func TestNewRejectsDuplicateTrailingAndUnknownPBXInfoBeforeEvents(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "duplicate",
			body: `{"system":{"entity_id":"00:11:22:33:44:55","version":"22.8"},` +
				`"status":{"startup_time":"2026-08-14T01:02:03Z","last_reload_time":"2026-08-14T01:02:03Z"},` +
				`"system":{"entity_id":"00:11:22:33:44:55","version":"22.8"}}`,
		},
		{
			name: "trailing",
			body: `{"system":{"entity_id":"00:11:22:33:44:55","version":"22.8"},` +
				`"status":{"startup_time":"2026-08-14T01:02:03Z","last_reload_time":"2026-08-14T01:02:03Z"}} {}`,
		},
		{
			name: "unknown",
			body: `{"system":{"entity_id":"00:11:22:33:44:55","version":"22.8","extra":true},` +
				`"status":{"startup_time":"2026-08-14T01:02:03Z","last_reload_time":"2026-08-14T01:02:03Z"}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			fake.mu.Lock()
			fake.infoRaw = []byte(test.body)
			fake.mu.Unlock()
			if adapter, err := New(context.Background(), fake.config()); adapter != nil || !errors.Is(err, ErrProtocol) {
				t.Fatalf("adapter=%v err=%v", adapter, err)
			}
			if fake.eventConnections.Load() != 0 {
				t.Fatalf("invalid info opened an event WebSocket: %d", fake.eventConnections.Load())
			}
		})
	}
}

func TestRecoveryTokenCrossesProcessAndHTTPOnlyInspectorUsesExactGET(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	token, err := adapter.RecoveryToken(incoming.Ref)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := token.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || len(encoded) > sipgateway.MaxRecoveryTokenBytes ||
		strings.Contains(string(encoded), "sip-call-id-0001@pbx.invalid") {
		t.Fatalf("unsafe recovery token=%q", encoded)
	}
	publicJSON, err := json.Marshal(token)
	if err != nil || strings.Contains(string(publicJSON), incoming.Ref.ProviderHandle) ||
		strings.Contains(string(publicJSON), "channel_id") {
		t.Fatalf("public token JSON=%s err=%v", publicJSON, err)
	}
	var restored sipgateway.RecoveryToken
	if err := restored.UnmarshalBinary(encoded); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	connections := fake.eventConnections.Load()
	inspector, err := NewRecoveryInspector(context.Background(), fake.config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspector.Close() })
	if fake.eventConnections.Load() != connections {
		t.Fatal("HTTP-only recovery inspector opened an event WebSocket")
	}
	fake.mu.Lock()
	requestStart := len(fake.requestLog)
	fake.mu.Unlock()
	result, err := inspector.InspectRecovery(context.Background(), restored)
	if err != nil || result.State != sipgateway.ProviderCallIncoming {
		t.Fatalf("recovery result=%+v err=%v", result, err)
	}
	fake.mu.Lock()
	requests := append([]string(nil), fake.requestLog[requestStart:]...)
	fake.mu.Unlock()
	wantRequests := []string{
		"GET asterisk/info",
		"GET channels/" + incoming.Ref.ProviderHandle,
		"GET asterisk/info",
	}
	if !slices.Equal(requests, wantRequests) {
		t.Fatalf("recovery requests=%v", requests)
	}
	if fake.answerCalls.Load() != 0 || fake.endCalls.Load() != 0 {
		t.Fatalf("recovery mutated provider: answer=%d end=%d", fake.answerCalls.Load(), fake.endCalls.Load())
	}

	fake.mu.Lock()
	delete(fake.channels, incoming.Ref.ProviderHandle)
	fake.mu.Unlock()
	ended, err := inspector.InspectRecovery(context.Background(), restored)
	if err != nil || ended.State != sipgateway.ProviderCallEnded {
		t.Fatalf("404 recovery=%+v err=%v", ended, err)
	}
}

func TestInspectRecoveryRejectsPBXDriftAroundChannelGET(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeAsterisk, string)
	}{
		{
			name: "between preflight info and channel GET",
			configure: func(fake *fakeAsterisk, handle string) {
				fake.mu.Lock()
				fake.channelGetHook = func(_ int, channelID string) {
					fake.mu.Lock()
					fake.startupTime = "2026-08-14T03:04:05Z"
					delete(fake.channels, channelID)
					fake.mu.Unlock()
				}
				fake.mu.Unlock()
			},
		},
		{
			name: "between channel GET and final info",
			configure: func(fake *fakeAsterisk, handle string) {
				fake.mu.Lock()
				delete(fake.channels, handle)
				postInfoCall := int(fake.infoCalls.Load()) + 2
				fake.infoHook = func(index int) {
					if index != postInfoCall {
						return
					}
					fake.mu.Lock()
					fake.startupTime = "2026-08-14T03:04:05Z"
					fake.mu.Unlock()
				}
				fake.mu.Unlock()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAsterisk(t)
			adapter, incoming := newObservedIncoming(t, fake)
			token, err := adapter.RecoveryToken(incoming.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if err := adapter.Close(); err != nil {
				t.Fatal(err)
			}
			inspector, err := NewRecoveryInspector(context.Background(), fake.config())
			if err != nil {
				t.Fatal(err)
			}
			defer inspector.Close()
			fake.mu.Lock()
			requestStart := len(fake.requestLog)
			fake.mu.Unlock()
			test.configure(fake, incoming.Ref.ProviderHandle)

			if result, err := inspector.InspectRecovery(context.Background(), token); !errors.Is(err, sipgateway.ErrReconcileRequired) || result.State != "" {
				t.Fatalf("drift recovery=%+v err=%v", result, err)
			}
			fake.mu.Lock()
			requests := append([]string(nil), fake.requestLog[requestStart:]...)
			fake.mu.Unlock()
			wantRequests := []string{
				"GET asterisk/info",
				"GET channels/" + incoming.Ref.ProviderHandle,
				"GET asterisk/info",
			}
			if !slices.Equal(requests, wantRequests) {
				t.Fatalf("drift recovery requests=%v", requests)
			}
		})
	}
}

func TestNewRecoveryInspectorMapsPBXIdentityDriftToReconcileRequired(t *testing.T) {
	fake := newFakeAsterisk(t)
	cfg := fake.config()
	fake.mu.Lock()
	fake.version = "22.10.2"
	fake.mu.Unlock()
	inspector, err := NewRecoveryInspector(context.Background(), cfg)
	if inspector != nil || !errors.Is(err, sipgateway.ErrReconcileRequired) || !errors.Is(err, ErrProtocol) {
		t.Fatalf("inspector=%v err=%v", inspector, err)
	}
}

func TestInspectRecoveryRejectsEveryImmutableIdentityMismatchWithoutAdoption(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	token, err := adapter.RecoveryToken(incoming.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ariChannel)
	}{
		{name: "id", mutate: func(channel *ariChannel) { channel.ID = "different-id" }},
		{name: "name", mutate: func(channel *ariChannel) { channel.Name = "PJSIP/volte-gateway-00000002" }},
		{name: "context", mutate: func(channel *ariChannel) { channel.Dialplan.Context = "other-context" }},
		{name: "extension", mutate: func(channel *ariChannel) { channel.Dialplan.Exten = "10010" }},
		{name: "priority", mutate: func(channel *ariChannel) { channel.Dialplan.Priority++ }},
		{name: "application", mutate: func(channel *ariChannel) { channel.Dialplan.AppName = "Other" }},
		{name: "application data", mutate: func(channel *ariChannel) { channel.Dialplan.AppData = "djonehub,other" }},
		{name: "creation time", mutate: func(channel *ariChannel) { channel.CreationTime = "2026-08-14T02:03:04Z" }},
		{name: "protocol id", mutate: func(channel *ariChannel) { channel.ProtocolID = "other-call-id@pbx.invalid" }},
		{name: "policy marker", mutate: func(channel *ariChannel) {
			channel.ChannelVars[incomingPolicyVariable] = "other-policy"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake.mu.Lock()
			fake.inspectMutate = test.mutate
			fake.mu.Unlock()
			if _, err := adapter.InspectRecovery(context.Background(), token); !errors.Is(err, sipgateway.ErrReconcileRequired) {
				t.Fatalf("mismatch error=%v", err)
			}
			adapter.mu.Lock()
			callCount := len(adapter.calls)
			adapter.mu.Unlock()
			if callCount != 1 {
				t.Fatalf("recovery changed calls map: %d", callCount)
			}
		})
	}
}

func TestInspectRecoveryFailsClosedOnTokenOrPBXIncarnationMismatch(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	token, err := adapter.RecoveryToken(incoming.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.startupTime = "2026-08-14T03:04:05Z"
	fake.mu.Unlock()
	inspector, err := NewRecoveryInspector(context.Background(), fake.config())
	if err != nil {
		t.Fatal(err)
	}
	defer inspector.Close()
	fake.mu.Lock()
	requestStart := len(fake.requestLog)
	fake.mu.Unlock()
	if _, err := inspector.InspectRecovery(context.Background(), token); !errors.Is(err, sipgateway.ErrReconcileRequired) {
		t.Fatalf("incarnation mismatch error=%v", err)
	}
	fake.mu.Lock()
	requests := append([]string(nil), fake.requestLog[requestStart:]...)
	fake.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("incarnation mismatch queried a channel: %v", requests)
	}

	var malformed sipgateway.RecoveryToken
	if err := malformed.UnmarshalBinary([]byte(`{"version":1,"identity":{},"extra":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := inspector.InspectRecovery(context.Background(), malformed); !errors.Is(err, sipgateway.ErrReconcileRequired) {
		t.Fatalf("malformed token error=%v", err)
	}
}

func TestInspectRecoveryRejectsChangedPolicyBeforeChannelGET(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, incoming := newObservedIncoming(t, fake)
	token, err := adapter.RecoveryToken(incoming.Ref)
	if err != nil {
		t.Fatal(err)
	}

	cfg := fake.config()
	cfg.IncomingPolicyID = "dj1-incoming-v2"
	inspector, err := NewRecoveryInspector(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer inspector.Close()
	fake.mu.Lock()
	requestStart := len(fake.requestLog)
	fake.mu.Unlock()
	if _, err := inspector.InspectRecovery(context.Background(), token); !errors.Is(err, sipgateway.ErrReconcileRequired) {
		t.Fatalf("policy mismatch error=%v", err)
	}
	fake.mu.Lock()
	requests := append([]string(nil), fake.requestLog[requestStart:]...)
	fake.mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("policy mismatch queried a channel: %v", requests)
	}
}

func TestRecoveryWithoutProtocolDigestSecretRemainsReadOnlyAndStrict(t *testing.T) {
	fake := newFakeAsterisk(t)
	cfg := fake.config()
	cfg.RecoverySecret = nil
	adapter, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.incoming("incoming-without-protocol-digest")
	event, err := adapter.Observe(context.Background())
	if err != nil || event.Call == nil {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	token, err := adapter.RecoveryToken(event.Call.Ref)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.inspectMutate = func(channel *ariChannel) { channel.ProtocolID = "" }
	fake.mu.Unlock()
	result, err := adapter.InspectRecovery(context.Background(), token)
	if err != nil || result.State != sipgateway.ProviderCallIncoming {
		t.Fatalf("recovery without protocol digest=%+v err=%v", result, err)
	}
}

func TestRecoveryTokenFailsClosedWhenConfiguredProtocolIdentityIsMissing(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), fake.config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	if _, err := adapter.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	channel := fake.incomingChannel("incoming-missing-protocol-id", "Ring")
	channel.ProtocolID = ""
	if err := fake.sendEvent(ariEvent{
		Type: "StasisStart", Args: []string{"incoming"}, Channel: &channel,
	}); err != nil {
		t.Fatal(err)
	}
	event, err := adapter.Observe(context.Background())
	if err != nil || event.Call == nil {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	if _, err := adapter.RecoveryToken(event.Call.Ref); !errors.Is(err, sipgateway.ErrReconcileRequired) {
		t.Fatalf("missing protocol recovery token error=%v", err)
	}
}

func TestRecoverySecretCopiesAreZeroedOnClose(t *testing.T) {
	fake := newFakeAsterisk(t)
	adapter, err := New(context.Background(), fake.config())
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	for index, value := range adapter.cfg.recoverySecret {
		if value != 0 {
			t.Fatalf("adapter recovery secret byte %d was not zeroed", index)
		}
	}

	inspector, err := NewRecoveryInspector(context.Background(), fake.config())
	if err != nil {
		t.Fatal(err)
	}
	if err := inspector.Close(); err != nil {
		t.Fatal(err)
	}
	for index, value := range inspector.cfg.recoverySecret {
		if value != 0 {
			t.Fatalf("inspector recovery secret byte %d was not zeroed", index)
		}
	}
}

func eventuallyAsterisk(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
