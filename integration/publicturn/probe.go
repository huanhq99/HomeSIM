package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iniwex5/vohive/internal/turnauth"
	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v5"
)

const (
	publicTURNHost      = "turn.example.com"
	publicTURNPort      = 3478
	publicTURNTLSPort   = 443
	publicTURNRealm     = "turn.example.com"
	publicRelayPortMin  = 49160
	publicRelayPortMax  = 49167
	credentialTTL       = 2 * time.Minute
	overallProbeTimeout = 35 * time.Second
	packetTimeout       = 5 * time.Second
	turnRTO             = 150 * time.Millisecond
	stunInitialRTO      = 250 * time.Millisecond
	stunMaxAttempts     = 5
	secretMinBytes      = 43
	secretMaxBytes      = 128
	randomPayloadBytes  = 32
	stunControlTimeout  = 9 * time.Second
)

type failureStage string

const (
	stageSecretInput    failureStage = "secret-input"
	stageCredential     failureStage = "credential-issuance"
	stageDNS            failureStage = "dns-resolution"
	stageUDPSocket      failureStage = "udp-socket"
	stageTCPConnect     failureStage = "tcp-connect"
	stageTLSTCPConnect  failureStage = "tls-tcp-connect"
	stageTLSHandshake   failureStage = "tls-handshake"
	stageTLSCertificate failureStage = "tls-certificate"
	stagePeerMapping    failureStage = "peer-mapping"
	stageTURNClient     failureStage = "turn-client"
	stageTURNAllocate   failureStage = "turn-allocation"
	stageTURNNoResponse failureStage = "turn-allocation-no-response"
	stageClientMapping  failureStage = "client-mapping"
	stageRelayPort      failureStage = "relay-port-policy"
	stagePayloadEntropy failureStage = "payload-entropy"
	stageTURNPermission failureStage = "turn-permission"
	stagePeerToRelay    failureStage = "peer-to-relay"
	stageRelayToPeer    failureStage = "relay-to-peer"
	stageCleanup        failureStage = "cleanup"
)

var validFailureStages = map[failureStage]bool{
	stageSecretInput: true, stageCredential: true, stageDNS: true,
	stageUDPSocket: true, stageTCPConnect: true, stageTLSTCPConnect: true, stageTLSHandshake: true,
	stageTLSCertificate: true, stagePeerMapping: true, stageTURNClient: true,
	stageTURNAllocate: true, stageTURNNoResponse: true, stageClientMapping: true, stageRelayPort: true, stagePayloadEntropy: true,
	stageTURNPermission: true, stagePeerToRelay: true, stageRelayToPeer: true,
	stageCleanup: true,
}

type probeFailure struct {
	stage       failureStage
	udpSent     int
	udpReceived int
}

func (failure *probeFailure) Error() string { return "public TURN probe stage failed" }

func failAt(stage failureStage) error { return &probeFailure{stage: stage} }

func failWithTraffic(stage failureStage, sent, received int) error {
	return &probeFailure{stage: stage, udpSent: sent, udpReceived: received}
}

type probeResult struct{ RelayPort int }

type probeTransport string

const (
	probeTransportUDP probeTransport = "udp"
	probeTransportTCP probeTransport = "tcp"
	probeTransportTLS probeTransport = "tls"
)

type stunControlResult struct {
	Sent     int
	Received int
	Mapped   bool
}

type permissionCreator interface {
	CreatePermission(...net.Addr) error
}

type exchangeOptions struct {
	Deadline          time.Time
	Random            io.Reader
	RequirePublicPeer bool
}

type allocationMappingObserver struct {
	net.PacketConn
	mutex       sync.RWMutex
	mapped      *net.UDPAddr
	udpSent     int
	udpReceived int
}

func (observer *allocationMappingObserver) ReadFrom(buffer []byte) (int, net.Addr, error) {
	count, from, err := observer.PacketConn.ReadFrom(buffer)
	if err == nil {
		observer.mutex.Lock()
		observer.udpReceived++
		observer.mutex.Unlock()
		observer.observe(buffer[:count])
	}
	return count, from, err
}

func (observer *allocationMappingObserver) WriteTo(payload []byte, address net.Addr) (int, error) {
	count, err := observer.PacketConn.WriteTo(payload, address)
	if count > 0 {
		observer.mutex.Lock()
		observer.udpSent++
		observer.mutex.Unlock()
	}
	return count, err
}

func (observer *allocationMappingObserver) observe(raw []byte) {
	if !stun.IsMessage(raw) {
		return
	}
	message := &stun.Message{Raw: append([]byte(nil), raw...)}
	if message.Decode() != nil ||
		message.Type != stun.NewType(stun.MethodAllocate, stun.ClassSuccessResponse) {
		return
	}
	var mapped stun.XORMappedAddress
	if mapped.GetFrom(message) != nil || mapped.IP.To4() == nil || mapped.Port < 1 || mapped.Port > 65535 {
		return
	}
	address := &net.UDPAddr{IP: append(net.IP(nil), mapped.IP.To4()...), Port: mapped.Port}
	observer.mutex.Lock()
	observer.mapped = address
	observer.mutex.Unlock()
}

func (observer *allocationMappingObserver) mappedAddress() *net.UDPAddr {
	observer.mutex.RLock()
	defer observer.mutex.RUnlock()
	if observer.mapped == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), observer.mapped.IP...), Port: observer.mapped.Port}
}

func (observer *allocationMappingObserver) trafficCounts() (int, int) {
	observer.mutex.RLock()
	defer observer.mutex.RUnlock()
	return observer.udpSent, observer.udpReceived
}

type probeResources struct {
	relayConn  net.PacketConn
	client     *turn.Client
	clientConn net.PacketConn
	peerConn   net.PacketConn
}

func (resources *probeResources) close() bool {
	ok := true
	if resources.relayConn != nil {
		if err := resources.relayConn.Close(); err != nil {
			ok = false
		}
		resources.relayConn = nil
	}
	if resources.client != nil {
		resources.client.Close()
		resources.client = nil
	}
	if resources.clientConn != nil {
		if err := resources.clientConn.Close(); err != nil {
			ok = false
		}
		resources.clientConn = nil
	}
	if resources.peerConn != nil {
		if err := resources.peerConn.Close(); err != nil {
			ok = false
		}
		resources.peerConn = nil
	}
	return ok
}

func liveProbe(secretFile string, connectAddress string, transport probeTransport) (probeResult, error) {
	var secret []byte
	var err error
	if secretFile == "-" {
		secret, err = readSecretReader(os.Stdin)
	} else {
		secret, err = readSecretFile(secretFile)
	}
	if err != nil {
		return probeResult{}, failAt(stageSecretInput)
	}
	defer clearBytes(secret)

	issuer, err := turnauth.New(turnauth.Config{
		Host: publicTURNHost, UDPPort: publicTURNPort, TLSPort: publicTURNTLSPort,
		TTL: credentialTTL, Secret: secret,
	})
	if err != nil {
		return probeResult{}, failAt(stageCredential)
	}
	defer issuer.Close()

	credentials, err := issuer.Issue()
	if err != nil {
		return probeResult{}, failAt(stageCredential)
	}
	relayCredential, err := credentials.ServerRelay()
	if err != nil || len(relayCredential.URLs) != 2 ||
		relayCredential.URLs[0] != "turn:turn.example.com:3478?transport=udp" ||
		relayCredential.URLs[1] != "turns:turn.example.com:443?transport=tcp" {
		return probeResult{}, failAt(stageCredential)
	}

	ctx, cancel := context.WithTimeout(context.Background(), overallProbeTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	serverAddr, err := resolveTURNIPv4(ctx, net.DefaultResolver, connectAddress)
	if err != nil {
		return probeResult{}, failAt(stageDNS)
	}
	if transport == probeTransportTLS {
		serverAddr.Port = publicTURNTLSPort
	} else if transport != probeTransportUDP && transport != probeTransportTCP {
		return probeResult{}, failAt(stageCredential)
	}

	resources := &probeResources{}
	defer resources.close()
	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return probeResult{}, failAt(stageUDPSocket)
	}
	resources.peerConn = peerConn

	var baseClientConn net.PacketConn
	if transport == probeTransportUDP {
		baseClientConn, err = net.ListenPacket("udp4", "0.0.0.0:0")
		if err != nil {
			return probeResult{}, failAt(stageUDPSocket)
		}
	} else {
		rawConn, dialErr := (&net.Dialer{}).DialContext(ctx, "tcp4", serverAddr.String())
		if dialErr != nil {
			if transport == probeTransportTCP {
				return probeResult{}, failAt(stageTCPConnect)
			}
			return probeResult{}, failAt(stageTLSTCPConnect)
		}
		if transport == probeTransportTCP {
			baseClientConn = turn.NewSTUNConn(rawConn)
		} else {
			tlsConn := tls.Client(rawConn, publicTURNTLSConfig())
			if handshakeErr := tlsConn.HandshakeContext(ctx); handshakeErr != nil {
				_ = rawConn.Close()
				return probeResult{}, failAt(stageTLSHandshake)
			}
			state := tlsConn.ConnectionState()
			if !state.HandshakeComplete || len(state.VerifiedChains) == 0 ||
				len(state.PeerCertificates) == 0 || state.PeerCertificates[0].VerifyHostname(publicTURNHost) != nil {
				_ = tlsConn.Close()
				return probeResult{}, failAt(stageTLSCertificate)
			}
			baseClientConn = turn.NewSTUNConn(tlsConn)
		}
	}
	clientConn := &allocationMappingObserver{PacketConn: baseClientConn}
	resources.clientConn = clientConn
	if err := clientConn.SetDeadline(deadline); err != nil {
		return probeResult{}, failAt(stageUDPSocket)
	}

	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.Writer = io.Discard
	loggerFactory.DefaultLogLevel = logging.LogLevelDisabled
	loggerFactory.ScopeLevels = map[string]logging.LogLevel{}
	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: serverAddr.String(), TURNServerAddr: serverAddr.String(),
		Username: relayCredential.Username, Password: relayCredential.Password,
		Realm: publicTURNRealm, Conn: clientConn, RTO: turnRTO,
		RequestedAddressFamily: turn.RequestedAddressFamilyIPv4,
		LoggerFactory:          loggerFactory,
	})
	if err != nil {
		return probeResult{}, failAt(stageTURNClient)
	}
	resources.client = client
	if err := client.Listen(); err != nil {
		return probeResult{}, failAt(stageTURNClient)
	}

	relayConn, err := client.Allocate()
	if err != nil {
		sent, received := clientConn.trafficCounts()
		if transport == probeTransportUDP && sent > 0 && received == 0 {
			return probeResult{}, failWithTraffic(stageTURNNoResponse, sent, received)
		}
		return probeResult{}, failAt(stageTURNAllocate)
	}
	resources.relayConn = relayConn
	clientMappedAddr := clientConn.mappedAddress()
	if !isPublicIPv4(clientMappedAddr) {
		return probeResult{}, failAt(stageClientMapping)
	}
	relayAddr, err := validatedRelayAddress(relayConn.LocalAddr())
	if err != nil {
		return probeResult{}, failAt(stageRelayPort)
	}

	if err := exchangeBidirectional(client, relayConn, peerConn, relayAddr, clientMappedAddr, exchangeOptions{
		Deadline: deadline, Random: rand.Reader, RequirePublicPeer: true,
	}); err != nil {
		return probeResult{}, err
	}
	if !resources.close() {
		return probeResult{}, failAt(stageCleanup)
	}
	return probeResult{RelayPort: relayAddr.Port}, nil
}

func publicTURNTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: publicTURNHost,
	}
}

func liveSTUNControl(endpoint string) stunControlResult {
	ctx, cancel := context.WithTimeout(context.Background(), stunControlTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	serverAddr, err := resolveSTUNControl(ctx, net.DefaultResolver, endpoint)
	if err != nil {
		return stunControlResult{}
	}
	return runSTUNBindingControl(serverAddr, deadline)
}

func runSTUNBindingControl(serverAddr *net.UDPAddr, deadline time.Time) stunControlResult {
	baseConn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return stunControlResult{}
	}
	conn := &allocationMappingObserver{PacketConn: baseConn}
	defer conn.Close()
	request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.Fingerprint)
	if err != nil {
		return stunControlResult{}
	}
	response, roundTripErr := stunRoundTrip(conn, serverAddr, request, deadline)
	sent, received := conn.trafficCounts()
	result := stunControlResult{Sent: sent, Received: received}
	if roundTripErr != nil || response.Type != stun.BindingSuccess {
		return result
	}
	mapped, err := mappedAddressFromResponse(response)
	if err == nil && isPublicIPv4(mapped) {
		result.Mapped = true
	}
	return result
}

func readSecretFile(path string) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("invalid secret path")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("secret file unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		info.Size() < secretMinBytes || info.Size() > secretMaxBytes {
		return nil, errors.New("invalid secret file")
	}

	secret := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(file, secret); err != nil {
		clearBytes(secret)
		return nil, errors.New("secret file read failed")
	}
	extra := make([]byte, 1)
	if count, readErr := file.Read(extra); count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		clearBytes(secret)
		return nil, errors.New("secret file changed while reading")
	}
	if err := validateSecretBytes(secret); err != nil {
		clearBytes(secret)
		return nil, err
	}
	return secret, nil
}

func readSecretReader(reader io.Reader) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("secret stream unavailable")
	}
	secret, err := io.ReadAll(io.LimitReader(reader, secretMaxBytes+1))
	if err != nil {
		clearBytes(secret)
		return nil, errors.New("secret stream read failed")
	}
	if err := validateSecretBytes(secret); err != nil {
		clearBytes(secret)
		return nil, err
	}
	return secret, nil
}

func validateSecretBytes(secret []byte) error {
	if len(secret) < secretMinBytes || len(secret) > secretMaxBytes {
		return errors.New("invalid secret length")
	}
	for _, character := range secret {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' {
			return errors.New("invalid secret encoding")
		}
	}
	return nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func resolveSTUNControl(ctx context.Context, resolver ipResolver, endpoint string) (*net.UDPAddr, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || portText == "" {
		return nil, errors.New("invalid STUN control endpoint")
	}
	port, err := net.LookupPort("udp", portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid STUN control port")
	}
	if parsed := net.ParseIP(host); parsed != nil {
		address := &net.UDPAddr{IP: parsed, Port: port}
		if !isPublicIPv4(address) || address.IP.String() != host {
			return nil, errors.New("invalid STUN control address")
		}
		return &net.UDPAddr{IP: append(net.IP(nil), parsed.To4()...), Port: port}, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, errors.New("STUN control DNS lookup failed")
	}
	for _, candidate := range addresses {
		address := &net.UDPAddr{IP: candidate.IP, Port: port}
		if isPublicIPv4(address) {
			return &net.UDPAddr{IP: append(net.IP(nil), candidate.IP.To4()...), Port: port}, nil
		}
	}
	return nil, errors.New("STUN control DNS has no public IPv4 address")
}

func resolveTURNIPv4(ctx context.Context, resolver ipResolver, connectAddress string) (*net.UDPAddr, error) {
	if connectAddress != "" {
		address := &net.UDPAddr{IP: net.ParseIP(connectAddress), Port: publicTURNPort}
		if !isPublicIPv4(address) || address.IP.String() != connectAddress {
			return nil, errors.New("invalid TURN connect address")
		}
		return &net.UDPAddr{IP: append(net.IP(nil), address.IP.To4()...), Port: publicTURNPort}, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, publicTURNHost)
	if err != nil {
		return nil, errors.New("TURN DNS lookup failed")
	}
	for _, address := range addresses {
		candidate := &net.UDPAddr{IP: address.IP, Port: publicTURNPort}
		if ipv4 := address.IP.To4(); ipv4 != nil && isPublicIPv4(candidate) {
			return &net.UDPAddr{IP: append(net.IP(nil), ipv4...), Port: publicTURNPort}, nil
		}
	}
	return nil, errors.New("TURN DNS has no IPv4 address")
}

func discoverPeerMapping(
	conn net.PacketConn,
	serverAddr *net.UDPAddr,
	username string,
	password string,
	deadline time.Time,
) (*net.UDPAddr, error) {
	anonymousRequest, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.Fingerprint)
	if err != nil {
		return nil, errors.New("STUN request build failed")
	}
	response, err := stunRoundTrip(conn, serverAddr, anonymousRequest, deadline)
	if err != nil || stun.Fingerprint.Check(response) != nil {
		return nil, errors.New("STUN challenge failed")
	}
	if response.Type == stun.BindingSuccess {
		return mappedAddressFromResponse(response)
	}
	if response.Type != stun.BindingError {
		return nil, errors.New("unexpected STUN challenge response")
	}

	var code stun.ErrorCodeAttribute
	var realm stun.Realm
	var nonce stun.Nonce
	if code.GetFrom(response) != nil || code.Code != stun.CodeUnauthorized ||
		realm.GetFrom(response) != nil || realm.String() != publicTURNRealm ||
		nonce.GetFrom(response) != nil || len(nonce) == 0 {
		return nil, errors.New("invalid STUN authentication challenge")
	}
	realm = append(stun.Realm(nil), realm...)
	nonce = append(stun.Nonce(nil), nonce...)
	integrity := stun.NewLongTermIntegrity(username, realm.String(), password)
	authenticatedRequest, err := stun.Build(
		stun.BindingRequest,
		stun.TransactionID,
		stun.NewUsername(username),
		realm,
		nonce,
		integrity,
		stun.Fingerprint,
	)
	if err != nil {
		return nil, errors.New("authenticated STUN request build failed")
	}
	response, err = stunRoundTrip(conn, serverAddr, authenticatedRequest, deadline)
	if err != nil || response.Type != stun.BindingSuccess ||
		stun.Fingerprint.Check(response) != nil || integrity.Check(response) != nil {
		return nil, errors.New("authenticated STUN binding failed")
	}
	return mappedAddressFromResponse(response)
}

func stunRoundTrip(
	conn net.PacketConn,
	serverAddr *net.UDPAddr,
	request *stun.Message,
	deadline time.Time,
) (*stun.Message, error) {
	buffer := make([]byte, 2048)
	for attempt := 0; attempt < stunMaxAttempts; attempt++ {
		now := time.Now()
		if !deadline.After(now) {
			return nil, errors.New("STUN deadline exceeded")
		}
		attemptTimeout := stunInitialRTO << attempt
		attemptDeadline := now.Add(attemptTimeout)
		if attemptDeadline.After(deadline) {
			attemptDeadline = deadline
		}
		if err := conn.SetWriteDeadline(attemptDeadline); err != nil {
			return nil, errors.New("STUN write deadline failed")
		}
		if written, err := conn.WriteTo(request.Raw, serverAddr); err != nil || written != len(request.Raw) {
			return nil, errors.New("STUN write failed")
		}
		if err := conn.SetReadDeadline(attemptDeadline); err != nil {
			return nil, errors.New("STUN read deadline failed")
		}
		for {
			count, from, err := conn.ReadFrom(buffer)
			if err != nil {
				var networkError net.Error
				if errors.As(err, &networkError) && networkError.Timeout() {
					break
				}
				return nil, errors.New("STUN read failed")
			}
			if !udpAddressEqual(from, serverAddr) || !stun.IsMessage(buffer[:count]) {
				continue
			}
			message := &stun.Message{Raw: append([]byte(nil), buffer[:count]...)}
			if message.Decode() != nil || message.TransactionID != request.TransactionID {
				continue
			}
			return message, nil
		}
	}
	return nil, errors.New("STUN response timed out")
}

func mappedAddressFromResponse(response *stun.Message) (*net.UDPAddr, error) {
	if response == nil || response.Type != stun.BindingSuccess {
		return nil, errors.New("invalid STUN success response")
	}
	var mapped stun.XORMappedAddress
	if mapped.GetFrom(response) != nil || mapped.IP.To4() == nil || mapped.Port < 1 || mapped.Port > 65535 {
		return nil, errors.New("invalid STUN mapped address")
	}
	return &net.UDPAddr{IP: append(net.IP(nil), mapped.IP.To4()...), Port: mapped.Port}, nil
}

func validatedRelayAddress(address net.Addr) (*net.UDPAddr, error) {
	relayAddr, ok := address.(*net.UDPAddr)
	if !ok || relayAddr.IP.To4() == nil ||
		relayAddr.Port < publicRelayPortMin || relayAddr.Port > publicRelayPortMax {
		return nil, errors.New("relay address outside the public policy")
	}
	return &net.UDPAddr{IP: append(net.IP(nil), relayAddr.IP.To4()...), Port: relayAddr.Port}, nil
}

func exchangeBidirectional(
	permission permissionCreator,
	relayConn net.PacketConn,
	peerConn net.PacketConn,
	relayAddr *net.UDPAddr,
	peerMappedAddr *net.UDPAddr,
	options exchangeOptions,
) error {
	peerToRelay, err := makePayload(options.Random, 0x51)
	if err != nil {
		return failAt(stagePayloadEntropy)
	}
	relayToPeer, err := makePayload(options.Random, 0xa7)
	if err != nil || bytes.Equal(peerToRelay, relayToPeer) {
		return failAt(stagePayloadEntropy)
	}
	if err := permission.CreatePermission(peerMappedAddr); err != nil {
		return failAt(stageTURNPermission)
	}

	peerToRelayDeadline := boundedStepDeadline(options.Deadline)
	if peerConn.SetWriteDeadline(peerToRelayDeadline) != nil {
		return failAt(stagePeerToRelay)
	}
	if written, err := peerConn.WriteTo(peerToRelay, relayAddr); err != nil || written != len(peerToRelay) {
		return failAt(stagePeerToRelay)
	}
	if relayConn.SetReadDeadline(peerToRelayDeadline) != nil {
		return failAt(stagePeerToRelay)
	}
	relayBuffer := make([]byte, len(peerToRelay)+1)
	count, observedPeer, err := relayConn.ReadFrom(relayBuffer)
	if err != nil || count != len(peerToRelay) || !bytes.Equal(relayBuffer[:count], peerToRelay) {
		return failAt(stagePeerToRelay)
	}
	observedPeerAddr, ok := observedPeer.(*net.UDPAddr)
	if !ok || observedPeerAddr.IP.To4() == nil || observedPeerAddr.Port < 1 || observedPeerAddr.Port > 65535 ||
		(options.RequirePublicPeer && !isPublicIPv4(observedPeerAddr)) {
		return failAt(stagePeerToRelay)
	}

	relayToPeerDeadline := boundedStepDeadline(options.Deadline)
	if relayConn.SetWriteDeadline(relayToPeerDeadline) != nil {
		return failAt(stageRelayToPeer)
	}
	if written, err := relayConn.WriteTo(relayToPeer, observedPeerAddr); err != nil || written != len(relayToPeer) {
		return failAt(stageRelayToPeer)
	}
	if peerConn.SetReadDeadline(relayToPeerDeadline) != nil {
		return failAt(stageRelayToPeer)
	}
	peerBuffer := make([]byte, len(relayToPeer)+1)
	count, observedRelay, err := peerConn.ReadFrom(peerBuffer)
	if err != nil || count != len(relayToPeer) || !bytes.Equal(peerBuffer[:count], relayToPeer) ||
		!udpAddressEqual(observedRelay, relayAddr) {
		return failAt(stageRelayToPeer)
	}
	return nil
}

func makePayload(random io.Reader, direction byte) ([]byte, error) {
	if random == nil {
		return nil, errors.New("nil payload entropy")
	}
	payload := make([]byte, len("DJI4G-TURN-E2E")+2+randomPayloadBytes)
	copy(payload, "DJI4G-TURN-E2E")
	payload[len("DJI4G-TURN-E2E")] = 0
	payload[len("DJI4G-TURN-E2E")+1] = direction
	if _, err := io.ReadFull(random, payload[len("DJI4G-TURN-E2E")+2:]); err != nil {
		clearBytes(payload)
		return nil, errors.New("payload entropy unavailable")
	}
	return payload, nil
}

func boundedStepDeadline(overall time.Time) time.Time {
	step := time.Now().Add(packetTimeout)
	if overall.IsZero() || step.Before(overall) {
		return step
	}
	return overall
}

func udpAddressEqual(left net.Addr, right *net.UDPAddr) bool {
	leftUDP, ok := left.(*net.UDPAddr)
	return ok && right != nil && leftUDP.Port == right.Port && leftUDP.IP.Equal(right.IP)
}

func isPublicIPv4(address *net.UDPAddr) bool {
	if address == nil || address.Port < 1 || address.Port > 65535 {
		return false
	}
	ip, ok := netip.AddrFromSlice(address.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	benchmark := netip.MustParsePrefix("198.18.0.0/15")
	return !cgnat.Contains(ip) && !benchmark.Contains(ip)
}
