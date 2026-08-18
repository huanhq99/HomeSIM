package remotevoice_test

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
	"github.com/iniwex5/vohive/internal/turnauth"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

func TestLivePublicTURNTCPConnectsTwoRelayOnlyPionPeers(t *testing.T) {
	if os.Getenv("MACCELLULAR_REMOTEVOICE_TURN_E2E") != "1" {
		t.Skip("set MACCELLULAR_REMOTEVOICE_TURN_E2E=1 for the explicit public TURN test")
	}
	secretPath := os.Getenv("MACCELLULAR_REMOTEVOICE_TURN_SECRET_FILE")
	connectAddress := os.Getenv("MACCELLULAR_REMOTEVOICE_TURN_CONNECT_ADDRESS")
	if connectAddress == "" {
		t.Fatal("set MACCELLULAR_REMOTEVOICE_TURN_CONNECT_ADDRESS to the deployed TURN IPv4")
	}
	info, err := os.Lstat(secretPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatal("live TURN secret must be one regular mode-0600 file")
	}
	secret, err := os.ReadFile(secretPath)
	if err != nil || len(secret) < 32 || len(secret) > 4096 {
		t.Fatal("live TURN secret is unavailable")
	}
	defer func() {
		for index := range secret {
			secret[index] = 0
		}
	}()

	issuer, err := turnauth.New(turnauth.Config{
		Host: "turn.example.com", UDPPort: 3478, TLSPort: 443,
		TTL: 2 * time.Minute, Secret: secret,
	})
	if err != nil {
		t.Fatal("create live TURN issuer")
	}
	defer issuer.Close()
	credentials, err := issuer.Issue()
	if err != nil {
		t.Fatal("issue live TURN credential")
	}
	relay, err := credentials.ServerRelayWithResolvedIPv4(connectAddress)
	if err != nil {
		t.Fatal("build live TURN relay bundle")
	}

	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypePCMU, ClockRate: remotevoice.SampleRate, Channels: 1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		t.Fatal(err)
	}
	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.Writer = io.Discard
	loggerFactory.DefaultLogLevel = logging.LogLevelDisabled
	var settings webrtc.SettingEngine
	settings.LoggerFactory = loggerFactory
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4})
	clientAPI := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine), webrtc.WithInterceptorRegistry(interceptors),
		webrtc.WithSettingEngine(settings),
	)
	configuration := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{
			URLs: append([]string(nil), relay.URLs...), Username: relay.Username,
			Credential: relay.Password, CredentialType: webrtc.ICECredentialTypePassword,
		}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}
	client, err := clientAPI.NewPeerConnection(configuration)
	if err != nil {
		t.Fatal("create relay-only browser peer")
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatal("add browser audio transceiver")
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal("create browser offer")
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal("set browser offer")
	}
	select {
	case <-gathered:
	case <-time.After(20 * time.Second):
		t.Fatal("relay-only browser ICE gathering timed out")
	}

	port, err := remotevoice.NewFramePort(64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, answer, err := remotevoice.Answer(ctx, []byte(client.LocalDescription().SDP), remotevoice.Config{
		Token: "live-turn-session", Generation: 1, UDPMin: 55000, UDPMax: 55063,
		AnswerTimeout: 20 * time.Second,
		TURNRelay: &remotevoice.TURNRelayConfig{
			URLs: append([]string(nil), relay.URLs...), Username: relay.Username, Password: relay.Password,
			CredentialType: remotevoice.TURNCredentialTypePassword,
		},
	}, port)
	if err != nil {
		t.Fatal("gateway relay-only answer failed")
	}
	defer session.Close()
	connected := make(chan struct{}, 1)
	client.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatal("set gateway relay-only answer")
	}
	select {
	case <-connected:
	case <-time.After(20 * time.Second):
		t.Fatal("two relay-only Pion peers did not connect")
	}
}
