package remotevoice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func loopbackInterface(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			return iface.Name
		}
	}
	t.Fatal("no up loopback interface")
	return ""
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Token:                 "test-session-token",
		Generation:            7,
		AllowedInterfaces:     []string{loopbackInterface(t)},
		AllowedLocalCIDRs:     []string{"127.0.0.0/8", "::1/128"},
		AllowedRemoteCIDRs:    []string{"127.0.0.1/32", "::1/128"},
		UDPMin:                41000,
		UDPMax:                41999,
		AnswerTimeout:         3 * time.Second,
		allowLoopbackForTests: true,
	}
}

func pcmuAPI(t *testing.T) *webrtc.API {
	t.Helper()
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: SampleRate, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptors); err != nil {
		t.Fatal(err)
	}
	var settings webrtc.SettingEngine
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetIncludeLoopbackCandidate(true)
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settings),
		webrtc.WithInterceptorRegistry(interceptors),
	)
}

func TestPionPeerTransfersPCMUFrames(t *testing.T) {
	client, err := pcmuAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	downlinkTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: SampleRate, Channels: 1},
		"client-audio", "client",
	)
	if err != nil {
		t.Fatal(err)
	}
	clientSender, err := client.AddTrack(downlinkTrack)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			if _, _, readErr := clientSender.ReadRTCP(); readErr != nil {
				return
			}
		}
	}()

	type receivedPacket struct {
		payloadType uint8
		length      int
		timestamp   uint32
	}
	received := make(chan receivedPacket, 128)
	client.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, readErr := track.ReadRTP()
				if readErr != nil {
					return
				}
				received <- receivedPacket{uint8(packet.PayloadType), len(packet.Payload), packet.Timestamp}
			}
		}()
	})

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gatherClient := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gatherClient:
	case <-time.After(3 * time.Second):
		t.Fatal("client ICE gathering timed out")
	}

	port, err := NewFramePort(256)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := port.PushUplink(filledFrame(int16(200 + i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, answer, err := Answer(ctx, []byte(client.LocalDescription().SDP), testConfig(t), port)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close() // fallback cleanup for failures before the explicit close check
	if session.Token() != "test-session-token" || session.Generation() != 7 {
		t.Fatalf("session identity = %q/%d", session.Token(), session.Generation())
	}
	connected := make(chan struct{})
	var connectedOnce sync.Once
	client.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(connected) })
		}
	})
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("peers did not connect")
	}

	var previousTimestamp uint32
	for i := 0; i < 100; i++ {
		select {
		case packet := <-received:
			if packet.payloadType != 0 || packet.length != FrameSamples {
				t.Fatalf("packet %d: PT=%d length=%d, want PT=0 length=%d", i, packet.payloadType, packet.length, FrameSamples)
			}
			if i > 0 && packet.timestamp-previousTimestamp != FrameSamples {
				t.Fatalf("packet %d timestamp delta=%d, want %d", i, packet.timestamp-previousTimestamp, FrameSamples)
			}
			previousTimestamp = packet.timestamp
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out after %d uplink packets", i)
		}
	}

	for i := 0; i < 100; i++ {
		data := EncodePCMU(filledFrame(int16(1000 + i)))
		if err := downlinkTrack.WriteSample(media.Sample{Data: data, Duration: FrameMillis * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if frame, ok := port.TakeDownlink(); ok {
				if len(frame) != FrameSamples {
					t.Fatalf("downlink frame %d has %d samples", i, len(frame))
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out after %d downlink frames", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var networkActivity NetworkActivitySnapshot
	for {
		networkActivity = session.NetworkActivitySnapshot()
		if networkActivity.ReceiverReports > 0 && networkActivity.SelectedPair.PolicyValid() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for selected-pair and exact-SSRC reception proof: %+v", networkActivity)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !networkActivity.Connected || !networkActivity.LocalTrackStarted || networkActivity.UplinkPackets == 0 ||
		networkActivity.RealUplinkPackets == 0 || networkActivity.RealUplinkPacketsAtLastReceiverReport == 0 ||
		!networkActivity.RemoteTrackReady || networkActivity.RemoteRTPPackets == 0 || networkActivity.ReceiverReports == 0 ||
		networkActivity.LastUplinkAt.IsZero() || networkActivity.LastRemoteRTPAt.IsZero() {
		t.Fatalf("network activity = %+v, want connected two-way RTP", networkActivity)
	}
	if networkActivity.RealUplinkPackets != 100 || networkActivity.UplinkPackets < networkActivity.RealUplinkPackets {
		t.Fatalf("real/synthetic uplink accounting = %+v, want exactly 100 queued real frames", networkActivity)
	}
	if pair := networkActivity.SelectedPair; !pair.Established || pair.Protocol != SelectedPairProtocolUDP ||
		pair.LocalCandidateType != SelectedCandidateTypeHost ||
		(pair.RemoteCandidateType != SelectedCandidateTypeHost && pair.RemoteCandidateType != SelectedCandidateTypePeerReflexive) ||
		!pair.LocalAddressAllowed || !pair.RemoteAddressAllowed {
		t.Fatalf("selected pair = %+v, want allowed UDP host-to-host/prflx RTP path", pair)
	}
	encodedPair, err := json.Marshal(networkActivity.SelectedPair)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedPair), "127.0.0.1") || strings.Contains(string(encodedPair), "41000") {
		t.Fatalf("selected-pair observation leaked an endpoint: %s", encodedPair)
	}
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	select {
	case closeErr := <-closed:
		if closeErr != nil {
			t.Fatalf("close live session: %v", closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live session Close did not stop its media workers")
	}
}

func TestPionRemoteIPFilterBlocksNonAllowlistedExactHost(t *testing.T) {
	client, err := pcmuAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(3 * time.Second):
		t.Fatal("client ICE gathering timed out")
	}
	offerSDP := client.LocalDescription().SDP
	if !strings.Contains(offerSDP, " 127.0.0.1 ") {
		t.Fatalf("test offer lacks expected loopback candidate: %s", offerSDP)
	}
	cfg := testConfig(t)
	cfg.AllowedRemoteCIDRs = []string{"127.0.0.2/32"}
	port, err := NewFramePort(4)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	session, answer, err := Answer(ctx, []byte(offerSDP), cfg, port)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if session.Connected() {
			t.Fatal("server connected to a remote candidate outside the exact-host allowlist")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if activity := session.NetworkActivitySnapshot(); activity.SelectedPair.PolicyValid() || activity.SelectedPair.Established {
		t.Fatalf("blocked remote candidate produced selected-pair proof: %+v", activity.SelectedPair)
	}
}

func TestObserveSelectedPairFailsClosedAndRedactsEndpoints(t *testing.T) {
	t.Parallel()
	policy, err := newSelectedPairPolicy(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	newPair := func(localAddress, remoteAddress string, protocol webrtc.ICEProtocol, remoteType webrtc.ICECandidateType) *webrtc.ICECandidatePair {
		return webrtc.NewICECandidatePair(
			&webrtc.ICECandidate{
				Address: localAddress, Protocol: protocol, Port: 41001,
				Typ: webrtc.ICECandidateTypeHost, Component: 1,
			},
			&webrtc.ICECandidate{
				Address: remoteAddress, Protocol: protocol, Port: 51001,
				Typ: remoteType, Component: 1,
			},
		)
	}
	host := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeHost)
	host.Local.Foundation = "secret-candidate-foundation"
	host.Remote.RelatedAddress = "secret-related-address"
	host.Remote.SDPMid = "secret-sdp-mid"
	prflx := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypePrflx)
	wrongLocal := newPair("100.100.10.2", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeHost)
	wrongRemote := newPair("127.0.0.1", "127.0.0.2", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeHost)
	tcp := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolTCP, webrtc.ICECandidateTypeHost)
	srflx := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeSrflx)
	relay := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeRelay)
	wrongLocalType := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeHost)
	wrongLocalType.Local.Typ = webrtc.ICECandidateTypePrflx
	mixedProtocol := newPair("127.0.0.1", "127.0.0.1", webrtc.ICEProtocolUDP, webrtc.ICECandidateTypeHost)
	mixedProtocol.Remote.Protocol = webrtc.ICEProtocolTCP

	for _, test := range []struct {
		name string
		pair *webrtc.ICECandidatePair
		want bool
	}{
		{name: "host", pair: host, want: true},
		{name: "peer-reflexive", pair: prflx, want: true},
		{name: "missing", pair: nil, want: false},
		{name: "missing-local", pair: &webrtc.ICECandidatePair{Remote: host.Remote}, want: false},
		{name: "wrong-local-address", pair: wrongLocal, want: false},
		{name: "wrong-remote-address", pair: wrongRemote, want: false},
		{name: "tcp", pair: tcp, want: false},
		{name: "server-reflexive", pair: srflx, want: false},
		{name: "relay", pair: relay, want: false},
		{name: "peer-reflexive-local", pair: wrongLocalType, want: false},
		{name: "mixed-protocol", pair: mixedProtocol, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := observeSelectedPair(test.pair, policy)
			if observed.PolicyValid() != test.want {
				t.Fatalf("PolicyValid = %v, want %v: %+v", observed.PolicyValid(), test.want, observed)
			}
			encoded, marshalErr := json.Marshal(observed)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, endpoint := range []string{
				"127.0.0.1", "127.0.0.2", "100.100.10.2", "41001", "51001",
				"secret-candidate-foundation", "secret-related-address", "secret-sdp-mid",
			} {
				if strings.Contains(string(encoded), endpoint) {
					t.Fatalf("observation leaked endpoint %q: %s", endpoint, encoded)
				}
			}
		})
	}
}

func TestAnswerNeverEmitsRawPionCandidateLogs(t *testing.T) {
	// This test intentionally exercises process-global environment and output,
	// so it must remain a non-parallel top-level test.
	t.Setenv("PION_LOG_WARN", "all")
	client, err := pcmuAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "secret-candidate-marker-203-0-113-77"
	offer.SDP += "a=candidate:" + marker + "\r\n"
	port, err := NewFramePort(1)
	if err != nil {
		t.Fatal(err)
	}
	var answerErr error
	output := captureProcessOutput(t, func() {
		var session *Session
		session, _, answerErr = Answer(context.Background(), []byte(offer.SDP), testConfig(t), port)
		if session != nil {
			_ = session.Close()
		}
	})
	if answerErr == nil {
		t.Fatal("malformed candidate unexpectedly accepted")
	}
	if strings.Contains(answerErr.Error(), marker) {
		t.Fatalf("returned SDP error leaked raw remote candidate: %q", answerErr)
	}
	if strings.Contains(output, marker) {
		t.Fatalf("Pion emitted raw remote candidate to process output: %q", output)
	}
}

func captureProcessOutput(t *testing.T, fn func()) (captured string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	result := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		result <- string(data)
	}()
	os.Stdout, os.Stderr = writer, writer
	defer func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
		_ = writer.Close()
		captured = <-result
		_ = reader.Close()
	}()
	fn()
	return ""
}

func TestAnswerRejectsBadInputs(t *testing.T) {
	t.Parallel()
	port, err := NewFramePort(1)
	if err != nil {
		t.Fatal(err)
	}
	valid := testConfig(t)
	cases := []struct {
		name  string
		offer []byte
		cfg   Config
	}{
		{"oversized", make([]byte, MaxOfferBytes+1), valid},
		{"not SDP", []byte("not an SDP offer"), valid},
		{"two audio sections", []byte("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), valid},
		{"audio and video sections", []byte("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"), valid},
		{"audio and application sections", []byte("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n"), valid},
		{"video only", []byte("v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"), valid},
		{"no token", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.Token = ""; return c }()},
		{"no interfaces", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.AllowedInterfaces = nil; return c }()},
		{"no local CIDR", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.AllowedLocalCIDRs = nil; return c }()},
		{"no remote CIDR", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.AllowedRemoteCIDRs = nil; return c }()},
		{"bad UDP range", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.UDPMin, c.UDPMax = 42000, 41000; return c }()},
		{"production loopback denied", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.allowLoopbackForTests = false; return c }()},
		{"wide IPv4 denied", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.AllowedRemoteCIDRs = []string{"0.0.0.0/0"}; return c }()},
		{"private LAN denied", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config { c := valid; c.AllowedRemoteCIDRs = []string{"192.168.0.0/16"}; return c }()},
		{"wide Tailscale superset denied", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config {
			c := valid
			c.allowLoopbackForTests = false
			c.AllowedLocalCIDRs = []string{"100.0.0.0/8"}
			c.AllowedRemoteCIDRs = []string{"100.0.0.0/8"}
			return c
		}()},
		{"wide IPv6 denied", []byte("m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"), func() Config {
			c := valid
			c.allowLoopbackForTests = false
			c.AllowedLocalCIDRs = []string{"::/0"}
			c.AllowedRemoteCIDRs = []string{"::/0"}
			return c
		}()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			session, _, answerErr := Answer(context.Background(), testCase.offer, testCase.cfg, port)
			if answerErr == nil {
				if session != nil {
					_ = session.Close()
				}
				t.Fatal("Answer unexpectedly accepted bad input")
			}
		})
	}
}

func TestCountMediaSections(t *testing.T) {
	t.Parallel()
	total, audio := countMediaSections("v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n")
	if total != 2 || audio != 1 {
		t.Fatalf("media sections = total %d audio %d, want 2/1", total, audio)
	}
}

func TestReceiverReportMustAcknowledgeExactSenderSSRC(t *testing.T) {
	t.Parallel()
	packets := []rtcp.Packet{
		&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 41, LastSequenceNumber: 9}}},
	}
	if !receiverReportAcknowledgesSSRC(packets, 41) {
		t.Fatal("matched ReceiverReport was not accepted")
	}
	if receiverReportAcknowledgesSSRC(packets, 42) || receiverReportAcknowledgesSSRC(packets, 0) {
		t.Fatal("unmatched or zero sender SSRC was accepted")
	}
	if receiverReportAcknowledgesSSRC([]rtcp.Packet{
		&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: 41}}},
		&rtcp.ReceiverReport{},
		&rtcp.SenderReport{SSRC: 41},
	}, 41) {
		t.Fatal("empty reception block, RR, or SenderReport was accepted as receiver proof")
	}
	if !receiverReportAcknowledgesSSRC([]rtcp.Packet{
		&rtcp.SenderReport{SSRC: 99, Reports: []rtcp.ReceptionReport{{SSRC: 41, LastSequenceNumber: 10}}},
	}, 41) {
		t.Fatal("matching reception block carried by SenderReport was rejected")
	}
}

func TestPCMUFrameAssemblerDropsPartialFrameAcrossActivationEpoch(t *testing.T) {
	t.Parallel()
	assembler := pcmuFrameAssembler{}
	firstHalf := make([]int16, FrameSamples/2)
	for index := range firstHalf {
		firstHalf[index] = 11
	}
	if frames := assembler.append(0, firstHalf); len(frames) != 0 {
		t.Fatalf("partial pre-activation payload produced %d frames", len(frames))
	}
	secondHalf := make([]int16, FrameSamples/2)
	for index := range secondHalf {
		secondHalf[index] = 22
	}
	if frames := assembler.append(1, secondHalf); len(frames) != 0 {
		t.Fatalf("mixed pre/post-activation payload produced %d frames", len(frames))
	}
	frames := assembler.append(1, secondHalf)
	if len(frames) != 1 {
		t.Fatalf("two post-activation halves produced %d frames, want one", len(frames))
	}
	for _, sample := range frames[0] {
		if sample != 22 {
			t.Fatalf("post-activation frame retained pre-activation sample %d", sample)
		}
	}
}

func TestSessionRealUplinkRequiresLaterSameEpochReceiverReport(t *testing.T) {
	t.Parallel()
	session := &Session{mediaEpoch: 1}
	session.markReceiverReport(1, time.Now())
	session.markUplinkPacket(1, true)
	activity := session.NetworkActivitySnapshot()
	if activity.RealUplinkPackets != 1 || activity.RealUplinkPacketsAtLastReceiverReport != 0 {
		t.Fatalf("pre-existing RR acknowledged later real uplink: %+v", activity)
	}
	session.markReceiverReport(1, activity.LastRealUplinkAt.Add(-time.Nanosecond))
	if activity := session.NetworkActivitySnapshot(); activity.RealUplinkPacketsAtLastReceiverReport != 0 {
		t.Fatalf("queued older RR acknowledged real uplink: %+v", activity)
	}
	session.markReceiverReport(0, activity.LastRealUplinkAt.Add(time.Second))
	if activity := session.NetworkActivitySnapshot(); activity.RealUplinkPacketsAtLastReceiverReport != 0 {
		t.Fatalf("wrong-epoch RR acknowledged real uplink: %+v", activity)
	}
	session.markReceiverReport(1, activity.LastRealUplinkAt.Add(time.Nanosecond))
	activity = session.NetworkActivitySnapshot()
	if activity.RealUplinkPacketsAtLastReceiverReport != 1 {
		t.Fatalf("later same-epoch RR did not acknowledge real uplink: %+v", activity)
	}
	// Continuous frames establish the next proof but do not erase the already
	// acknowledged lower bound between periodic reports.
	session.markUplinkPacket(1, true)
	session.markUplinkPacket(1, true)
	if activity := session.NetworkActivitySnapshot(); activity.RealUplinkPacketsAtLastReceiverReport != 1 {
		t.Fatalf("continuous real uplink made causal proof flicker: %+v", activity)
	}
}

func TestValidateConfigAcceptsOnlyTailscaleSubnetsInProduction(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.allowLoopbackForTests = false
	cfg.AllowedInterfaces = []string{"tailscale0"}
	cfg.AllowedLocalCIDRs = []string{"100.100.10.2/32", "fd7a:115c:a1e0:ab12::/64"}
	cfg.AllowedRemoteCIDRs = []string{"100.100.20.3/32", "fd7a:115c:a1e0:ab12::4/128"}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("exact/subnet Tailscale allowlist rejected: %v", err)
	}
}

func TestValidateConfigRejectsRemoteTailscaleRanges(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.allowLoopbackForTests = false
	cfg.AllowedInterfaces = []string{"tailscale0"}
	cfg.AllowedLocalCIDRs = []string{"100.64.0.0/10"}
	for _, remote := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48", "100.100.20.0/24"} {
		candidate := cfg
		candidate.AllowedRemoteCIDRs = []string{remote}
		if err := validateConfig(candidate); err == nil {
			t.Fatalf("remote range %q unexpectedly accepted", remote)
		}
	}
}

func TestTURNRelayConfigValidationAndPeerConfiguration(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Token: "relay-session-token", Generation: 19,
		UDPMin: 42000, UDPMax: 42999, AnswerTimeout: 3 * time.Second,
		TURNRelay: &TURNRelayConfig{
			URLs: []string{
				"turn:turn.example.test:3478?transport=udp",
				"turns:turn.example.test:443?transport=tcp",
			},
			Username: "relay-user", Password: "relay-password",
			CredentialType: TURNCredentialTypePassword,
		},
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid TURN relay config rejected: %v", err)
	}
	peerConfig := peerConnectionConfiguration(cfg)
	if peerConfig.ICETransportPolicy != webrtc.ICETransportPolicyRelay || len(peerConfig.ICEServers) != 1 {
		t.Fatal("peer config does not enforce relay-only ICE")
	}
	server := peerConfig.ICEServers[0]
	if server.Username != cfg.TURNRelay.Username || server.Credential != cfg.TURNRelay.Password ||
		server.CredentialType != webrtc.ICECredentialTypePassword || len(server.URLs) != 2 {
		t.Fatal("peer config did not preserve the complete TURN credential bundle")
	}
	server.URLs[0] = "turn:mutated.invalid:3478?transport=udp"
	if cfg.TURNRelay.URLs[0] == server.URLs[0] {
		t.Fatal("peer configuration aliases caller-owned TURN URLs")
	}

	resolved := cfg
	resolvedRelay := *cfg.TURNRelay
	resolvedRelay.URLs = []string{
		"turn:8.8.8.8:3478?transport=udp",
		"turn:8.8.8.8:3478?transport=tcp",
		"turns:turn.example.test:443?transport=tcp",
	}
	resolved.TURNRelay = &resolvedRelay
	if err := validateConfig(resolved); err != nil {
		t.Fatalf("valid resolved TURN relay config rejected: %v", err)
	}
	if got := peerConnectionConfiguration(resolved).ICEServers[0].URLs; !slices.Equal(got, resolvedRelay.URLs) {
		t.Fatalf("resolved TURN URLs=%v want=%v", got, resolvedRelay.URLs)
	}
	for _, test := range []struct {
		name   string
		mutate func([]string)
	}{
		{name: "synthetic IPv4", mutate: func(urls []string) {
			urls[0] = "turn:198.19.153.67:3478?transport=udp"
			urls[1] = "turn:198.19.153.67:3478?transport=tcp"
		}},
		{name: "private IPv4", mutate: func(urls []string) {
			urls[0] = "turn:10.0.0.1:3478?transport=udp"
			urls[1] = "turn:10.0.0.1:3478?transport=tcp"
		}},
		{name: "TCP host mismatch", mutate: func(urls []string) { urls[1] = "turn:8.8.4.4:3478?transport=tcp" }},
		{name: "TCP port mismatch", mutate: func(urls []string) { urls[1] = "turn:8.8.8.8:3479?transport=tcp" }},
		{name: "second URL not TCP", mutate: func(urls []string) { urls[1] = "turn:8.8.8.8:3478?transport=udp" }},
	} {
		t.Run("resolved "+test.name, func(t *testing.T) {
			candidate := resolved
			relay := *resolved.TURNRelay
			relay.URLs = append([]string(nil), resolvedRelay.URLs...)
			test.mutate(relay.URLs)
			candidate.TURNRelay = &relay
			if err := validateConfig(candidate); err == nil {
				t.Fatal("invalid resolved TURN relay config accepted")
			}
		})
	}

	invalid := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing URLs", mutate: func(c *Config) { c.TURNRelay.URLs = nil }},
		{name: "missing TLS fallback", mutate: func(c *Config) { c.TURNRelay.URLs = c.TURNRelay.URLs[:1] }},
		{name: "missing username", mutate: func(c *Config) { c.TURNRelay.Username = "" }},
		{name: "missing password", mutate: func(c *Config) { c.TURNRelay.Password = "" }},
		{name: "unsupported credential type", mutate: func(c *Config) { c.TURNRelay.CredentialType = "oauth" }},
		{name: "reversed endpoints", mutate: func(c *Config) { c.TURNRelay.URLs[0], c.TURNRelay.URLs[1] = c.TURNRelay.URLs[1], c.TURNRelay.URLs[0] }},
		{name: "STUN URL", mutate: func(c *Config) { c.TURNRelay.URLs[0] = "stun:secret.invalid:3478" }},
		{name: "plaintext TCP fallback", mutate: func(c *Config) { c.TURNRelay.URLs[1] = "turn:turn.example.test:443?transport=tcp" }},
		{name: "TLS over UDP", mutate: func(c *Config) { c.TURNRelay.URLs[1] = "turns:turn.example.test:443?transport=udp" }},
		{name: "fallback host differs", mutate: func(c *Config) { c.TURNRelay.URLs[1] = "turns:secret.invalid:443?transport=tcp" }},
		{name: "missing transport", mutate: func(c *Config) { c.TURNRelay.URLs[1] = "turns:turn.example.test:443" }},
		{name: "extra query", mutate: func(c *Config) { c.TURNRelay.URLs[0] += "&marker=secret.invalid" }},
		{name: "embedded credentials", mutate: func(c *Config) {
			c.TURNRelay.URLs[0] = "turn:marker-user:marker-password@secret.invalid:3478?transport=udp"
		}},
		{name: "bad port", mutate: func(c *Config) { c.TURNRelay.URLs[0] = "turn:secret.invalid:70000?transport=udp" }},
		{name: "legacy CIDR ambiguity", mutate: func(c *Config) { c.AllowedRemoteCIDRs = []string{"100.100.20.3/32"} }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			candidate := cfg
			relay := *cfg.TURNRelay
			relay.URLs = append([]string(nil), cfg.TURNRelay.URLs...)
			candidate.TURNRelay = &relay
			test.mutate(&candidate)
			err := validateConfig(candidate)
			if err == nil {
				t.Fatal("invalid TURN relay config accepted")
			}
			for _, secret := range []string{"relay-user", "relay-password", "marker-user", "marker-password", "secret.invalid"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("validation error leaked caller configuration: %v", err)
				}
			}
		})
	}
}

func TestTURNRelayEnablesIPv4UDPAndTCPWithoutChangingLegacyNetworkTypes(t *testing.T) {
	t.Parallel()
	wantLegacy := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
	wantRelay := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4}
	if got := networkTypesForConfig(Config{}); !slices.Equal(got, wantLegacy) {
		t.Fatalf("legacy network types=%v want=%v", got, wantLegacy)
	}
	if got := networkTypesForConfig(Config{TURNRelay: &TURNRelayConfig{}}); !slices.Equal(got, wantRelay) {
		t.Fatalf("TURN relay network types=%v want=%v", got, wantRelay)
	}
}

func TestTURNRelaySelectedPairRequiresUDPRelayToRelay(t *testing.T) {
	t.Parallel()
	cfg := Config{TURNRelay: &TURNRelayConfig{}}
	policy, err := newSelectedPairPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	newPair := func(localType, remoteType webrtc.ICECandidateType, localProtocol, remoteProtocol webrtc.ICEProtocol) *webrtc.ICECandidatePair {
		local := &webrtc.ICECandidate{Address: "192.0.2.10", Protocol: localProtocol, Port: 41001, Typ: localType, Component: 1}
		remote := &webrtc.ICECandidate{Address: "198.51.100.20", Protocol: remoteProtocol, Port: 51001, Typ: remoteType, Component: 1}
		return webrtc.NewICECandidatePair(local, remote)
	}
	for _, test := range []struct {
		name string
		pair *webrtc.ICECandidatePair
		want bool
	}{
		{name: "UDP relay relay", pair: newPair(webrtc.ICECandidateTypeRelay, webrtc.ICECandidateTypeRelay, webrtc.ICEProtocolUDP, webrtc.ICEProtocolUDP), want: true},
		{name: "UDP relay host", pair: newPair(webrtc.ICECandidateTypeRelay, webrtc.ICECandidateTypeHost, webrtc.ICEProtocolUDP, webrtc.ICEProtocolUDP)},
		{name: "UDP host relay", pair: newPair(webrtc.ICECandidateTypeHost, webrtc.ICECandidateTypeRelay, webrtc.ICEProtocolUDP, webrtc.ICEProtocolUDP)},
		{name: "TCP relay relay", pair: newPair(webrtc.ICECandidateTypeRelay, webrtc.ICECandidateTypeRelay, webrtc.ICEProtocolTCP, webrtc.ICEProtocolTCP)},
		{name: "mixed protocol", pair: newPair(webrtc.ICECandidateTypeRelay, webrtc.ICECandidateTypeRelay, webrtc.ICEProtocolUDP, webrtc.ICEProtocolTCP)},
		{name: "missing", pair: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := observeSelectedPair(test.pair, policy)
			if snapshot.Profile != SelectedPairProfileTURNRelay && test.pair != nil {
				t.Fatalf("profile=%q, want TURN relay", snapshot.Profile)
			}
			if snapshot.PolicyValid() != test.want {
				t.Fatalf("PolicyValid=%v want=%v: %+v", snapshot.PolicyValid(), test.want, snapshot)
			}
		})
	}
	resolvedPolicy, err := newSelectedPairPolicy(Config{TURNRelay: &TURNRelayConfig{URLs: []string{
		"turn:8.8.8.8:3478?transport=udp",
		"turn:8.8.8.8:3478?transport=tcp",
		"turns:turn.example.test:443?transport=tcp",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	prflxFromTURN := webrtc.NewICECandidatePair(
		&webrtc.ICECandidate{Address: "8.8.8.8", Protocol: webrtc.ICEProtocolUDP, Port: 49160, Typ: webrtc.ICECandidateTypeRelay, Component: 1},
		&webrtc.ICECandidate{Address: "8.8.8.8", Protocol: webrtc.ICEProtocolUDP, Port: 49161, Typ: webrtc.ICECandidateTypePrflx, Component: 1},
	)
	if snapshot := observeSelectedPair(prflxFromTURN, resolvedPolicy); !snapshot.PolicyValid() || !snapshot.RemoteAddressAllowed {
		t.Fatalf("TURN-IP-bound peer-reflexive pair rejected: %+v", snapshot)
	}
	directPrflx := webrtc.NewICECandidatePair(
		&webrtc.ICECandidate{Address: "8.8.8.8", Protocol: webrtc.ICEProtocolUDP, Port: 49160, Typ: webrtc.ICECandidateTypeRelay, Component: 1},
		&webrtc.ICECandidate{Address: "8.8.4.4", Protocol: webrtc.ICEProtocolUDP, Port: 49161, Typ: webrtc.ICECandidateTypePrflx, Component: 1},
	)
	if snapshot := observeSelectedPair(directPrflx, resolvedPolicy); snapshot.PolicyValid() {
		t.Fatalf("non-TURN peer-reflexive address passed relay-only policy: %+v", snapshot)
	}
	// The zero profile remains the pre-existing Tailscale host/prflx policy.
	legacy := SelectedPairSnapshot{
		Established: true, Protocol: SelectedPairProtocolUDP,
		LocalCandidateType: SelectedCandidateTypeHost, RemoteCandidateType: SelectedCandidateTypePeerReflexive,
		LocalAddressAllowed: true, RemoteAddressAllowed: true,
	}
	if !legacy.PolicyValid() {
		t.Fatal("legacy zero-profile selected pair no longer validates")
	}
}

func TestTURNRelayConfigurationFormattingIsRedacted(t *testing.T) {
	t.Parallel()
	relay := TURNRelayConfig{
		URLs: []string{
			"turn:sensitive-relay.example:3478?transport=udp",
			"turns:sensitive-relay.example:443?transport=tcp",
		},
		Username: "sensitive-user", Password: "sensitive-password",
		CredentialType: TURNCredentialTypePassword,
	}
	cfg := Config{Token: "sensitive-token", Generation: 1, TURNRelay: &relay}
	formatted := fmt.Sprintf("%v %+v %#v / %v %+v %#v", cfg, cfg, cfg, relay, relay, relay)
	for _, secret := range []string{"sensitive-relay", "sensitive-user", "sensitive-password", "sensitive-token"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("default formatting leaked %q: %s", secret, formatted)
		}
	}
	for _, value := range []any{cfg, relay} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "sensitive") {
			t.Fatalf("JSON leaked TURN configuration: %s", encoded)
		}
	}
	if encoded, err := json.Marshal(relay); err != nil || string(encoded) != `{"redacted":true}` {
		t.Fatalf("TURN relay JSON=%s err=%v", encoded, err)
	}
}

func TestAnswerRejectsOfferWithoutPCMU(t *testing.T) {
	t.Parallel()
	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Remove every PCMU payload advertisement while leaving an otherwise valid
	// offer. Pion's default audio set still supplies other codecs.
	lines := strings.Split(offer.SDP, "\r\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.Contains(line, "PCMU/8000") || strings.HasPrefix(line, "a=rtcp-fb:0") {
			continue
		}
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			withoutZero := fields[:0]
			for _, field := range fields {
				if field != "0" {
					withoutZero = append(withoutZero, field)
				}
			}
			line = strings.Join(withoutZero, " ")
		}
		filtered = append(filtered, line)
	}
	port, _ := NewFramePort(1)
	session, _, answerErr := Answer(context.Background(), []byte(strings.Join(filtered, "\r\n")), testConfig(t), port)
	if answerErr == nil {
		_ = session.Close()
		t.Fatal("Answer accepted an offer without PCMU")
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	// Covered by the end-to-end test for a live session. This test protects the
	// API contract independently with a minimal closeable peer.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		token: "token", generation: 1, port: nil, pc: pc, cancel: cancel,
		terminalDone: make(chan struct{}),
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
	}()
	if err := s.Close(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after local Close = %v, want nil", err)
	}
}

func ExampleConfig() {
	fmt.Println("remotevoice Config supports restricted Tailscale and independent TURN relay-only profiles")
	// Output: remotevoice Config supports restricted Tailscale and independent TURN relay-only profiles
}
