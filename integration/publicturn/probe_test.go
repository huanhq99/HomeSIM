package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

func TestCLIDefaultIsHermeticAndDoesNotCallProbe(t *testing.T) {
	var called atomic.Bool
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runCLI(nil, "", func(string, string, probeTransport) (probeResult, error) {
		called.Store(true)
		return probeResult{}, nil
	}, func(string) stunControlResult {
		called.Store(true)
		return stunControlResult{}
	}, &stdout, &stderr)
	if code != 0 || called.Load() || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), "SKIP: public TURN/STUN network probes") {
		t.Fatalf("code=%d called=%v stdout=%q stderr=%q", code, called.Load(), stdout.String(), stderr.String())
	}
}

func TestCLIRequiresBothExactGateAndSecretFile(t *testing.T) {
	tests := []struct {
		name string
		args []string
		gate string
	}{
		{name: "secret without gate", args: []string{"--secret-file", "/private/secret"}},
		{name: "wrong gate", gate: "true"},
		{name: "gate without secret", gate: "1"},
		{name: "unexpected option", gate: "1", args: []string{"--secret", "/private/secret"}},
		{name: "invalid transport", gate: "1", args: []string{"--secret-file", "/private/secret", "--transport", "ws"}},
		{name: "stdin and file", gate: "1", args: []string{"--secret-stdin", "--secret-file", "/private/secret"}},
		{name: "stdin repeated", gate: "1", args: []string{"--secret-stdin", "--secret-stdin"}},
		{name: "TLS without secret", gate: "1", args: []string{"--stun-control", "8.8.8.8:3478", "--transport", "tls"}},
		{name: "duplicate transport", gate: "1", args: []string{"--secret-file", "/private/secret", "--transport", "udp", "--transport", "tls"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var called atomic.Bool
			code := runCLI(test.args, test.gate, func(string, string, probeTransport) (probeResult, error) {
				called.Store(true)
				return probeResult{}, nil
			}, func(string) stunControlResult {
				called.Store(true)
				return stunControlResult{}
			}, &bytes.Buffer{}, &bytes.Buffer{})
			if code == 0 || called.Load() {
				t.Fatalf("code=%d called=%v", code, called.Load())
			}
		})
	}
}

func TestPublicTURNTLSConfigurationVerifiesExactServerName(t *testing.T) {
	config := publicTURNTLSConfig()
	if config.ServerName != publicTURNHost || config.InsecureSkipVerify || config.MinVersion < tls.VersionTLS12 {
		t.Fatalf("unsafe TLS config: server_name=%q insecure=%v min_version=%x", config.ServerName, config.InsecureSkipVerify, config.MinVersion)
	}
}

func TestCLIOutputCannotLeakUnknownProbeErrorOrSecretPath(t *testing.T) {
	const marker = "DO-NOT-LOG-TURN-SECRET-MARKER"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runCLI(
		[]string{"--secret-file", "/private/" + marker}, "1",
		func(string, string, probeTransport) (probeResult, error) { return probeResult{}, errors.New(marker) },
		nil,
		&stdout, &stderr,
	)
	combined := stdout.String() + stderr.String()
	if code == 0 || strings.Contains(combined, marker) || !strings.Contains(combined, "failed at internal") {
		t.Fatalf("code=%d output=%q", code, combined)
	}
}

func TestCLIPrintsOnlyBoundedTrafficCountsForNoResponse(t *testing.T) {
	var stderr bytes.Buffer
	code := runCLI(
		[]string{"--secret-file", "/private/secret"},
		"1",
		func(string, string, probeTransport) (probeResult, error) {
			return probeResult{}, failWithTraffic(stageTURNNoResponse, 7, 0)
		},
		nil,
		&bytes.Buffer{},
		&stderr,
	)
	if code == 0 || !strings.Contains(stderr.String(), "udp_tx=7 udp_rx=0") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestReadSecretFileRequiresAbsoluteRegular0600Base64URL(t *testing.T) {
	directory := t.TempDir()
	validPath := filepath.Join(directory, "turn-auth-secret")
	validSecret := strings.Repeat("Ab0_", 11)
	if err := os.WriteFile(validPath, []byte(validSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := readSecretFile(validPath)
	if err != nil || string(secret) != validSecret {
		t.Fatalf("read valid secret: len=%d err=%v", len(secret), err)
	}
	clearBytes(secret)
	for _, value := range secret {
		if value != 0 {
			t.Fatal("clearBytes did not wipe the secret buffer")
		}
	}

	tests := []struct {
		name    string
		path    string
		content string
		mode    os.FileMode
	}{
		{name: "relative path", path: "relative-secret", content: validSecret, mode: 0o600},
		{name: "relaxed mode", path: filepath.Join(directory, "relaxed"), content: validSecret, mode: 0o644},
		{name: "newline", path: filepath.Join(directory, "newline"), content: validSecret + "\n", mode: 0o600},
		{name: "invalid encoding", path: filepath.Join(directory, "invalid"), content: strings.Repeat("A", 42) + "+", mode: 0o600},
		{name: "too short", path: filepath.Join(directory, "short"), content: strings.Repeat("A", 42), mode: 0o600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if filepath.IsAbs(test.path) {
				if err := os.WriteFile(test.path, []byte(test.content), test.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(test.path, test.mode); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readSecretFile(test.path); err == nil {
				t.Fatal("invalid secret input was accepted")
			}
		})
	}
}

func TestReadSecretReaderRequiresBoundedBase64URLWithoutPersistence(t *testing.T) {
	validSecret := strings.Repeat("Ab0_", 11)
	secret, err := readSecretReader(strings.NewReader(validSecret))
	if err != nil || string(secret) != validSecret {
		t.Fatalf("read valid stream: len=%d err=%v", len(secret), err)
	}
	clearBytes(secret)

	for _, value := range []string{
		validSecret + "\n",
		strings.Repeat("A", secretMaxBytes+1),
		strings.Repeat("A", secretMinBytes-1),
		strings.Repeat("A", secretMinBytes-1) + "+",
	} {
		if got, err := readSecretReader(strings.NewReader(value)); err == nil || got != nil {
			t.Fatalf("invalid stream accepted: len=%d err=%v", len(got), err)
		}
	}
}

func TestResolveTURNIPv4UsesOnlyTheExactHostAndIPv4(t *testing.T) {
	resolver := &fakeResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("2001:db8::1")},
		{IP: net.ParseIP("8.8.4.4")},
	}}
	address, err := resolveTURNIPv4(context.Background(), resolver, "")
	if err != nil || resolver.host != publicTURNHost || address.String() != "8.8.4.4:3478" {
		t.Fatalf("host=%q address=%v err=%v", resolver.host, address, err)
	}
}

func TestConnectAddressBypassesDNSWithoutChangingAuthenticationNames(t *testing.T) {
	resolver := &fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("198.19.153.67")}}}
	address, err := resolveTURNIPv4(context.Background(), resolver, "8.8.8.8")
	if err != nil || address.String() != "8.8.8.8:3478" || resolver.host != "" {
		t.Fatalf("host=%q address=%v err=%v", resolver.host, address, err)
	}
	if publicTURNHost != "turn.example.com" || publicTURNRealm != "turn.example.com" {
		t.Fatalf("authentication names drifted: host=%q realm=%q", publicTURNHost, publicTURNRealm)
	}
	if _, err := resolveTURNIPv4(context.Background(), resolver, "198.19.153.67"); err == nil {
		t.Fatal("macOS fake-DNS benchmark address was accepted as a connection target")
	}
}

func TestCLIForwardsPinnedConnectAddress(t *testing.T) {
	var gotSecret string
	var gotConnect string
	var gotTransport probeTransport
	code := runCLI(
		[]string{"--connect-address", "8.8.8.8", "--secret-file", "/private/secret", "--transport", "tls"},
		"1",
		func(secretFile, connectAddress string, transport probeTransport) (probeResult, error) {
			gotSecret = secretFile
			gotConnect = connectAddress
			gotTransport = transport
			return probeResult{RelayPort: publicRelayPortMin}, nil
		},
		nil,
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if code != 0 || gotSecret != "/private/secret" || gotConnect != "8.8.8.8" || gotTransport != probeTransportTLS {
		t.Fatalf("code=%d secret=%q connect=%q transport=%q", code, gotSecret, gotConnect, gotTransport)
	}
}

func TestCLIForwardsSecretStdinWithoutTreatingItAsAPath(t *testing.T) {
	var gotSecret string
	var gotConnect string
	code := runCLI(
		[]string{"--secret-stdin", "--connect-address", "8.8.8.8", "--transport", "udp"},
		"1",
		func(secretFile, connectAddress string, transport probeTransport) (probeResult, error) {
			gotSecret = secretFile
			gotConnect = connectAddress
			if transport != probeTransportUDP {
				t.Fatalf("transport=%q", transport)
			}
			return probeResult{RelayPort: publicRelayPortMin}, nil
		},
		nil,
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if code != 0 || gotSecret != "-" || gotConnect != "8.8.8.8" {
		t.Fatalf("code=%d secret=%q connect=%q", code, gotSecret, gotConnect)
	}
}

func TestCLIReportsEverySTUNControlWithoutPrintingMappedAddress(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runCLI(
		[]string{"--stun-control", "8.8.8.8:3478", "--stun-control", "1.1.1.1:3478"},
		"1",
		nil,
		func(endpoint string) stunControlResult {
			if endpoint == "8.8.8.8:3478" {
				return stunControlResult{Sent: 1, Received: 1, Mapped: true}
			}
			return stunControlResult{Sent: 5, Received: 0}
		},
		&stdout,
		&stderr,
	)
	combined := stdout.String() + stderr.String()
	if code == 0 || !strings.Contains(combined, "stun_control=1 udp_tx=1 udp_rx=1 mapped=verified") ||
		!strings.Contains(combined, "stun_control=2 udp_tx=5 udp_rx=0 mapped=not-verified") ||
		strings.Contains(combined, "8.8.8.8") || strings.Contains(combined, "1.1.1.1") {
		t.Fatalf("code=%d output=%q", code, combined)
	}
}

func TestSTUNControlBindingIsHermeticAndReturnsNoMappedAddress(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serverDone := make(chan error, 1)
	go func() {
		request, from, readErr := readSTUNRequest(server)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if request.Type != stun.BindingRequest || stun.Fingerprint.Check(request) != nil {
			serverDone <- errors.New("invalid control Binding request")
			return
		}
		response, buildErr := stun.Build(
			stun.NewTransactionIDSetter(request.TransactionID),
			stun.BindingSuccess,
			&stun.XORMappedAddress{IP: net.ParseIP("8.8.8.8"), Port: 45678},
			stun.Fingerprint,
		)
		if buildErr != nil {
			serverDone <- buildErr
			return
		}
		_, writeErr := server.WriteToUDP(response.Raw, from)
		serverDone <- writeErr
	}()
	result := runSTUNBindingControl(server.LocalAddr().(*net.UDPAddr), time.Now().Add(3*time.Second))
	if !result.Mapped || result.Sent != 1 || result.Received != 1 {
		t.Fatalf("result=%+v", result)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestResolveSTUNControlRejectsFakeDNSAndAcceptsPinnedPublicIPv4(t *testing.T) {
	resolver := &fakeResolver{addresses: []net.IPAddr{{IP: net.ParseIP("198.19.153.67")}}}
	address, err := resolveSTUNControl(context.Background(), resolver, "74.125.250.129:19302")
	if err != nil || address.String() != "74.125.250.129:19302" || resolver.host != "" {
		t.Fatalf("address=%v host=%q err=%v", address, resolver.host, err)
	}
	if _, err := resolveSTUNControl(context.Background(), resolver, "stun.invalid:3478"); err == nil {
		t.Fatal("fake-DNS benchmark address was accepted for a STUN control")
	}
}

type fakeResolver struct {
	host      string
	addresses []net.IPAddr
}

func (resolver *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	resolver.host = host
	return resolver.addresses, nil
}

func TestDiscoverPeerMappingCompletesSecureSTUNHandshake(t *testing.T) {
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	const username = "synthetic-user"
	const password = "synthetic-password"
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSecureSTUNHandshake(server, username, password)
	}()

	mapped, err := discoverPeerMapping(
		peer, server.LocalAddr().(*net.UDPAddr), username, password, time.Now().Add(3*time.Second),
	)
	if err != nil || mapped.String() != "8.8.8.8:45678" {
		t.Fatalf("mapped=%v err=%v", mapped, err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic STUN server did not finish")
	}
}

func TestAllocationObserverExtractsMappedAddressOnlyFromAllocateSuccess(t *testing.T) {
	observer := &allocationMappingObserver{}
	ignored, err := stun.Build(
		stun.TransactionID,
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: net.ParseIP("8.8.8.8"), Port: 40000},
	)
	if err != nil {
		t.Fatal(err)
	}
	observer.observe(ignored.Raw)
	if observer.mappedAddress() != nil {
		t.Fatal("observer accepted a non-Allocate STUN response")
	}
	allocate, err := stun.Build(
		stun.TransactionID,
		stun.NewType(stun.MethodAllocate, stun.ClassSuccessResponse),
		&stun.XORMappedAddress{IP: net.ParseIP("8.8.4.4"), Port: 45678},
	)
	if err != nil {
		t.Fatal(err)
	}
	observer.observe(allocate.Raw)
	mapped := observer.mappedAddress()
	if mapped == nil || mapped.String() != "8.8.4.4:45678" {
		t.Fatalf("mapped=%v", mapped)
	}
}

func serveSecureSTUNHandshake(server *net.UDPConn, username, password string) error {
	request, from, err := readSTUNRequest(server)
	if err != nil {
		return err
	}
	if request.Type != stun.BindingRequest || stun.Fingerprint.Check(request) != nil {
		return errors.New("invalid anonymous binding request")
	}
	challenge, err := stun.Build(
		stun.NewTransactionIDSetter(request.TransactionID),
		stun.BindingError,
		stun.CodeUnauthorized,
		stun.NewRealm(publicTURNRealm),
		stun.NewNonce("synthetic-nonce"),
		stun.Fingerprint,
	)
	if err != nil {
		return err
	}
	if _, err := server.WriteToUDP(challenge.Raw, from); err != nil {
		return err
	}

	request, from, err = readSTUNRequest(server)
	if err != nil {
		return err
	}
	var gotUsername stun.Username
	var gotRealm stun.Realm
	var gotNonce stun.Nonce
	integrity := stun.NewLongTermIntegrity(username, publicTURNRealm, password)
	if request.Type != stun.BindingRequest || stun.Fingerprint.Check(request) != nil ||
		gotUsername.GetFrom(request) != nil || gotUsername.String() != username ||
		gotRealm.GetFrom(request) != nil || gotRealm.String() != publicTURNRealm ||
		gotNonce.GetFrom(request) != nil || gotNonce.String() != "synthetic-nonce" ||
		integrity.Check(request) != nil {
		return errors.New("invalid authenticated binding request")
	}
	success, err := stun.Build(
		stun.NewTransactionIDSetter(request.TransactionID),
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: net.ParseIP("8.8.8.8"), Port: 45678},
		integrity,
		stun.Fingerprint,
	)
	if err != nil {
		return err
	}
	_, err = server.WriteToUDP(success.Raw, from)
	return err
}

func readSTUNRequest(server *net.UDPConn) (*stun.Message, *net.UDPAddr, error) {
	if err := server.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return nil, nil, err
	}
	buffer := make([]byte, 2048)
	count, from, err := server.ReadFromUDP(buffer)
	if err != nil {
		return nil, nil, err
	}
	message := &stun.Message{Raw: append([]byte(nil), buffer[:count]...)}
	if err := message.Decode(); err != nil {
		return nil, nil, err
	}
	return message, from, nil
}

func TestBidirectionalExchangeUsesDifferentPayloadsAndExactData(t *testing.T) {
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	permission := &recordingPermission{}
	randomInput := bytes.NewReader(bytes.Repeat([]byte{0x5a}, randomPayloadBytes*2))
	err = exchangeBidirectional(
		permission,
		relay,
		peer,
		relay.LocalAddr().(*net.UDPAddr),
		peer.LocalAddr().(*net.UDPAddr),
		exchangeOptions{Deadline: time.Now().Add(3 * time.Second), Random: randomInput},
	)
	if err != nil || permission.calls != 1 || !udpAddressEqual(permission.address, peer.LocalAddr().(*net.UDPAddr)) {
		t.Fatalf("err=%v permission_calls=%d address=%v", err, permission.calls, permission.address)
	}
}

type recordingPermission struct {
	calls   int
	address net.Addr
}

func (permission *recordingPermission) CreatePermission(addresses ...net.Addr) error {
	permission.calls++
	if len(addresses) != 1 {
		return fmt.Errorf("got %d addresses", len(addresses))
	}
	permission.address = addresses[0]
	return nil
}

func TestRelayPortAndPublicPeerPolicies(t *testing.T) {
	for _, port := range []int{publicRelayPortMin, publicRelayPortMax} {
		if _, err := validatedRelayAddress(&net.UDPAddr{IP: net.ParseIP("203.0.113.20"), Port: port}); err != nil {
			t.Fatalf("valid relay port %d rejected: %v", port, err)
		}
	}
	for _, port := range []int{publicRelayPortMin - 1, publicRelayPortMax + 1} {
		if _, err := validatedRelayAddress(&net.UDPAddr{IP: net.ParseIP("203.0.113.20"), Port: port}); err == nil {
			t.Fatalf("invalid relay port %d accepted", port)
		}
	}
	for _, test := range []struct {
		address string
		want    bool
	}{
		{address: "8.8.8.8", want: true},
		{address: "10.0.0.1"},
		{address: "100.64.0.1"},
		{address: "127.0.0.1"},
		{address: "192.168.1.1"},
		{address: "2001:4860:4860::8888"},
	} {
		if got := isPublicIPv4(&net.UDPAddr{IP: net.ParseIP(test.address), Port: 12345}); got != test.want {
			t.Fatalf("isPublicIPv4(%s)=%v want=%v", test.address, got, test.want)
		}
	}
}
