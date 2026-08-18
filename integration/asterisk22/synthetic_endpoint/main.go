package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
)

const (
	localIP      = "172.31.255.251"
	sipPort      = 35060
	rtpPort      = 27000
	controlPort  = 18080
	asteriskSIP  = "172.31.255.250:5060"
	gatewayPCM   = int16(1500)
	clientPCM    = int16(-1500)
	frameSamples = 160
)

type snapshot struct {
	Invited          bool   `json:"invited"`
	Answered         bool   `json:"answered"`
	Bye              bool   `json:"bye"`
	GotClientPattern bool   `json:"got_client_pattern"`
	Error            string `json:"error,omitempty"`
}

type endpoint struct {
	sip       *net.UDPConn
	rtp       *net.UDPConn
	sipTarget *net.UDPAddr

	mu        sync.Mutex
	state     snapshot
	toHeader  string
	remoteRTP *net.UDPAddr

	gatewayPayload []byte
	clientPayload  []byte
	inviteOnce     sync.Once
	rtpOnce        sync.Once
}

type sipMessage struct {
	start  string
	header map[string]string
	body   string
}

func main() {
	ep, err := newEndpoint()
	if err != nil {
		panic("synthetic endpoint startup failed")
	}
	defer ep.Close()
	go ep.readSIP()
	go ep.readRTP()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(ep.Snapshot())
	})
	mux.HandleFunc("POST /invite", func(w http.ResponseWriter, _ *http.Request) {
		started := false
		ep.inviteOnce.Do(func() {
			started = true
			ep.setInvited()
			if err := ep.Invite(); err != nil {
				ep.setError("sip_invite_failed")
			}
		})
		if !started {
			http.Error(w, "invite already consumed", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	server := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", controlPort),
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic("synthetic endpoint control server failed")
	}
}

func newEndpoint() (*endpoint, error) {
	sip, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: sipPort})
	if err != nil {
		return nil, err
	}
	rtp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: rtpPort})
	if err != nil {
		_ = sip.Close()
		return nil, err
	}
	target, err := net.ResolveUDPAddr("udp4", asteriskSIP)
	if err != nil {
		_ = sip.Close()
		_ = rtp.Close()
		return nil, err
	}
	return &endpoint{
		sip: sip, rtp: rtp, sipTarget: target,
		gatewayPayload: constantPCMU(gatewayPCM), clientPayload: constantPCMU(clientPCM),
	}, nil
}

func (e *endpoint) Invite() error {
	sdp := fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 %s\r\ns=Public Voice E2E\r\n"+
		"c=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\n"+
		"a=rtpmap:0 PCMU/8000\r\na=sendrecv\r\na=ptime:20\r\n", localIP, localIP, rtpPort)
	request := fmt.Sprintf("INVITE sip:s@%s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s:%d;branch=z9hG4bK-synthetic-e2e-invite;rport\r\n"+
		"Max-Forwards: 70\r\nFrom: <sip:synthetic@%s>;tag=synthetic-e2e-from\r\n"+
		"To: <sip:s@%s>\r\nCall-ID: synthetic-public-voice-e2e@container\r\nCSeq: 1 INVITE\r\n"+
		"Contact: <sip:synthetic@%s:%d>\r\nSupported: replaces, timer\r\n"+
		"Content-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s",
		asteriskSIP, localIP, sipPort, localIP, asteriskSIP, localIP, sipPort, len(sdp), sdp)
	_, err := e.sip.WriteToUDP([]byte(request), e.sipTarget)
	return err
}

func (e *endpoint) readSIP() {
	buffer := make([]byte, 64<<10)
	for {
		count, peer, err := e.sip.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		message, err := parseSIP(buffer[:count])
		if err != nil {
			continue
		}
		if strings.HasPrefix(message.start, "SIP/2.0 200") &&
			strings.HasSuffix(strings.ToUpper(message.header["cseq"]), " INVITE") {
			remote, err := parseSDPRTP(message.body)
			if err != nil {
				e.setError("invalid_answer_sdp")
				continue
			}
			e.mu.Lock()
			e.toHeader = message.header["to"]
			e.remoteRTP = remote
			e.mu.Unlock()
			if err := e.sendACK(); err != nil {
				e.setError("sip_ack_failed")
				continue
			}
			e.mu.Lock()
			e.state.Answered = true
			e.mu.Unlock()
			e.rtpOnce.Do(func() { go e.sendRTP() })
			continue
		}
		if strings.HasPrefix(strings.ToUpper(message.start), "BYE ") {
			if err := e.sendResponse(peer, message); err != nil {
				e.setError("sip_bye_response_failed")
				continue
			}
			e.mu.Lock()
			e.state.Bye = true
			e.mu.Unlock()
		}
	}
}

func (e *endpoint) sendACK() error {
	e.mu.Lock()
	to := e.toHeader
	e.mu.Unlock()
	request := fmt.Sprintf("ACK sip:s@%s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s:%d;branch=z9hG4bK-synthetic-e2e-ack;rport\r\n"+
		"Max-Forwards: 70\r\nFrom: <sip:synthetic@%s>;tag=synthetic-e2e-from\r\n"+
		"To: %s\r\nCall-ID: synthetic-public-voice-e2e@container\r\n"+
		"CSeq: 1 ACK\r\nContent-Length: 0\r\n\r\n", asteriskSIP, localIP, sipPort, localIP, to)
	_, err := e.sip.WriteToUDP([]byte(request), e.sipTarget)
	return err
}

func (e *endpoint) sendResponse(peer *net.UDPAddr, request sipMessage) error {
	response := fmt.Sprintf("SIP/2.0 200 OK\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\n"+
		"Call-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n",
		request.header["via"], request.header["from"], request.header["to"],
		request.header["call-id"], request.header["cseq"])
	_, err := e.sip.WriteToUDP([]byte(response), peer)
	return err
}

func (e *endpoint) sendRTP() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	sequence := uint16(1000)
	timestamp := uint32(160000)
	for range ticker.C {
		e.mu.Lock()
		remote := e.remoteRTP
		e.mu.Unlock()
		if remote == nil {
			continue
		}
		packet := buildRTP(sequence, timestamp, 0x4556414e, e.gatewayPayload)
		_, _ = e.rtp.WriteToUDP(packet, remote)
		sequence++
		timestamp += frameSamples
	}
}

func (e *endpoint) readRTP() {
	buffer := make([]byte, 2048)
	for {
		count, _, err := e.rtp.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		payload, ok := parseRTP(buffer[:count])
		if ok && bytes.Equal(payload, e.clientPayload) {
			e.mu.Lock()
			e.state.GotClientPattern = true
			e.mu.Unlock()
		}
	}
}

func (e *endpoint) Snapshot() snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

func (e *endpoint) setInvited() { e.mu.Lock(); e.state.Invited = true; e.mu.Unlock() }
func (e *endpoint) setError(value string) {
	e.mu.Lock()
	if e.state.Error == "" {
		e.state.Error = value
	}
	e.mu.Unlock()
}
func (e *endpoint) Close() { _ = e.sip.Close(); _ = e.rtp.Close() }

func parseSIP(data []byte) (sipMessage, error) {
	raw := string(data)
	separator := "\r\n\r\n"
	index := strings.Index(raw, separator)
	if index < 0 {
		return sipMessage{}, errors.New("missing SIP terminator")
	}
	lines := strings.Split(strings.ReplaceAll(raw[:index], "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return sipMessage{}, errors.New("missing SIP headers")
	}
	message := sipMessage{start: lines[0], header: make(map[string]string), body: raw[index+len(separator):]}
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return sipMessage{}, errors.New("invalid SIP header")
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if _, exists := message.header[name]; !exists {
			message.header[name] = strings.TrimSpace(value)
		}
	}
	return message, nil
}

func parseSDPRTP(sdp string) (*net.UDPAddr, error) {
	host := ""
	port := 0
	pcmu := false
	for _, raw := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "c=IN IP4 ") {
			host = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		}
		if strings.HasPrefix(line, "m=audio ") {
			fields := strings.Fields(line)
			if len(fields) >= 4 && fields[2] == "RTP/AVP" {
				port, _ = strconv.Atoi(fields[1])
				for _, payload := range fields[3:] {
					pcmu = pcmu || payload == "0"
				}
			}
		}
	}
	ip := net.ParseIP(host)
	if ip == nil || port < 1 || port > 65535 || !pcmu {
		return nil, errors.New("invalid PCMU SDP")
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}

func buildRTP(sequence uint16, timestamp, ssrc uint32, payload []byte) []byte {
	packet := make([]byte, 12+len(payload))
	packet[0] = 0x80
	binary.BigEndian.PutUint16(packet[2:4], sequence)
	binary.BigEndian.PutUint32(packet[4:8], timestamp)
	binary.BigEndian.PutUint32(packet[8:12], ssrc)
	copy(packet[12:], payload)
	return packet
}

func parseRTP(packet []byte) ([]byte, bool) {
	if len(packet) != 12+frameSamples || packet[0] != 0x80 || packet[1]&0x7f != 0 {
		return nil, false
	}
	return append([]byte(nil), packet[12:]...), true
}

func constantPCMU(sample int16) []byte {
	pcm := make([]int16, frameSamples)
	for index := range pcm {
		pcm[index] = sample
	}
	return remotevoice.EncodePCMU(pcm)
}
