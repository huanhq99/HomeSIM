package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestPortScore(t *testing.T) {
	tests := []struct {
		name string
		port string
		want int
	}{
		{name: "named Quectel port", port: "/dev/cu.Quectel-AT", want: 100},
		{name: "usb modem", port: "/dev/cu.usbmodem2101", want: 80},
		{name: "usb serial", port: "/dev/cu.usbserial-1420", want: 60},
		{name: "windows COM", port: "COM12", want: 50},
		{name: "bluetooth", port: "/dev/cu.Bluetooth-Incoming-Port", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := portScore(tt.port); got != tt.want {
				t.Fatalf("portScore(%q) = %d, want %d", tt.port, got, tt.want)
			}
		})
	}
}

func TestFilterCandidateATPorts(t *testing.T) {
	windows := filterCandidateATPorts([]string{"COM3", "COM12", "LPT1", ""}, "windows")
	if len(windows) != 2 || windows[0] != "COM3" || windows[1] != "COM12" {
		t.Fatalf("Windows ports = %v, want [COM3 COM12]", windows)
	}

	darwin := filterCandidateATPorts([]string{
		"/dev/cu.Bluetooth-Incoming-Port",
		"/dev/cu.usbmodem2101",
		"/dev/cu.Quectel-AT",
	}, "darwin")
	if len(darwin) != 2 || darwin[0] != "/dev/cu.usbmodem2101" || darwin[1] != "/dev/cu.Quectel-AT" {
		t.Fatalf("Darwin ports = %v", darwin)
	}
}

func TestPlanModemRuntimeStartupDefaultPreservesBackgroundWork(t *testing.T) {
	plan := planModemRuntimeStartup(false, false)
	if !plan.SMS || !plan.CallPoll || !plan.GPSPoll || !plan.GPSReadback {
		t.Fatalf("default startup plan = %+v, want all existing background work enabled", plan)
	}
}

func TestPlanModemRuntimeStartupSMSOnlyKeepsOnlySMS(t *testing.T) {
	plan := planModemRuntimeStartup(true, false)
	if !plan.SMS {
		t.Fatalf("SMS-only startup plan = %+v, want SMS work enabled", plan)
	}
	if plan.CallPoll || plan.GPSPoll || plan.GPSReadback {
		t.Fatalf("SMS-only startup plan = %+v, want call/GPS background work disabled", plan)
	}
}

func TestPlanModemRuntimeStartupPhoneRelayKeepsSMSAndCallsOnly(t *testing.T) {
	plan := planModemRuntimeStartup(false, true)
	if !plan.SMS || !plan.CallPoll {
		t.Fatalf("phone-relay startup plan = %+v, want SMS and call polling enabled", plan)
	}
	if plan.GPSPoll || plan.GPSReadback {
		t.Fatalf("phone-relay startup plan = %+v, want unrelated GPS work disabled", plan)
	}
}

func TestShouldProbeSerialAT(t *testing.T) {
	tests := []struct {
		name      string
		port      string
		usbATOnly bool
		wantProbe bool
		wantErr   error
	}{
		{name: "default auto discovery", wantProbe: true},
		{name: "explicit serial port", port: "/dev/cu.Quectel-AT"},
		{name: "USB AT only", usbATOnly: true},
		{name: "conflicting explicit port", port: "/dev/cu.Quectel-AT", usbATOnly: true, wantErr: errUSBATOnlyWithSerialPort},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe, err := shouldProbeSerialAT(test.port, test.usbATOnly)
			if err != test.wantErr {
				t.Fatalf("shouldProbeSerialAT(%q, %t) error = %v, want %v", test.port, test.usbATOnly, err, test.wantErr)
			}
			if probe != test.wantProbe {
				t.Fatalf("shouldProbeSerialAT(%q, %t) = %t, want %t", test.port, test.usbATOnly, probe, test.wantProbe)
			}
		})
	}
}

func TestValidateModemRuntimeMode(t *testing.T) {
	if err := validateModemRuntimeMode(true, false, &publicWebStartupConfig{DirectVoice: true}); err == nil {
		t.Fatal("sms-only runtime accepted public direct voice")
	}
	if err := validateModemRuntimeMode(true, true, nil); err == nil {
		t.Fatal("conflicting modem runtime modes were accepted")
	}
	for _, test := range []struct {
		smsOnly    bool
		phoneRelay bool
		config     *publicWebStartupConfig
	}{
		{smsOnly: false, config: &publicWebStartupConfig{DirectVoice: true}},
		{phoneRelay: true, config: &publicWebStartupConfig{DirectVoice: true}},
		{smsOnly: true, config: &publicWebStartupConfig{ExternalVoice: true}},
		{smsOnly: true, config: nil},
	} {
		if err := validateModemRuntimeMode(test.smsOnly, test.phoneRelay, test.config); err != nil {
			t.Fatalf("valid combination rejected: %+v err=%v", test, err)
		}
	}
}

func TestValidateRemoteMediaControlStartup(t *testing.T) {
	direct := &app{publicWeb: &publicWebStartupConfig{DirectVoice: true}}
	if err := validateRemoteMediaControlStartup(direct, nil, nil); err == nil {
		t.Fatal("public direct voice accepted a missing local media control server")
	}
	if err := validateRemoteMediaControlStartup(direct, nil, errors.New("listen failed")); err == nil {
		t.Fatal("public direct voice accepted a failed local media control server")
	}
	if err := validateRemoteMediaControlStartup(direct, &remoteMediaControlServer{}, nil); err != nil {
		t.Fatalf("public direct voice rejected a ready local media control server: %v", err)
	}
	legacy := &app{publicWeb: &publicWebStartupConfig{DirectVoice: false}}
	if err := validateRemoteMediaControlStartup(legacy, nil, errors.New("listen failed")); err != nil {
		t.Fatalf("legacy optional media fallback changed: %v", err)
	}
}

func TestParseUSBNetMode(t *testing.T) {
	for _, tt := range []struct {
		response string
		want     string
	}{
		{response: "AT+QCFG=\"usbnet\"\r\n+QCFG: \"usbnet\",0\r\nOK", want: "0"},
		{response: "+QCFG: \"usbnet\",1\r\nOK", want: "1"},
		{response: "ERROR", want: ""},
	} {
		if got := parseUSBNetMode(tt.response); got != tt.want {
			t.Fatalf("parseUSBNetMode(%q) = %q, want %q", tt.response, got, tt.want)
		}
	}
}

func TestParseMacNetworkServices(t *testing.T) {
	input := `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) Baiwang 2
(Hardware Port: Baiwang, Device: en8)

(*) Baiwang
(Hardware Port: Baiwang, Device: en10)
`
	services := parseMacNetworkServices(input)
	if len(services) != 3 {
		t.Fatalf("service count = %d, want 3", len(services))
	}
	if services[1].Name != "Baiwang 2" || services[1].Device != "en8" ||
		services[1].Disabled || !isDJICellularService(services[1]) {
		t.Fatalf("active cellular service = %+v", services[1])
	}
	if !services[2].Disabled || !isDJICellularService(services[2]) {
		t.Fatalf("disabled cellular service = %+v", services[2])
	}
}

func TestIsDJICellularServiceRelaxed(t *testing.T) {
	tests := []struct {
		name    string
		service macNetworkService
		want    bool
	}{
		{
			name:    "hardware port Baiwang",
			service: macNetworkService{Name: "Baiwang 2", HardwarePort: "Baiwang", Device: "en8"},
			want:    true,
		},
		{
			name:    "service name contains baiwang",
			service: macNetworkService{Name: "Baiwang", HardwarePort: "USB LAN", Device: "en6"},
			want:    true,
		},
		{
			name:    "mixed case",
			service: macNetworkService{Name: "BAIWANG", HardwarePort: "baiwang", Device: "en10"},
			want:    true,
		},
		{
			name:    "not a cellular service",
			service: macNetworkService{Name: "Wi-Fi", HardwarePort: "Wi-Fi", Device: "en0"},
			want:    false,
		},
		{
			name:    "baiwang without en device",
			service: macNetworkService{Name: "Baiwang", HardwarePort: "Baiwang", Device: "bridge0"},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDJICellularService(tt.service); got != tt.want {
				t.Fatalf("isDJICellularService(%+v) = %v, want %v", tt.service, got, tt.want)
			}
		})
	}
}

func TestIsDJICellularServiceRequiresNameAndEthernetDevice(t *testing.T) {
	if !isDJICellularService(macNetworkService{Name: "Baiwang Concurrent", HardwarePort: "Baiwang", Device: "en9"}) {
		t.Fatal("expected Baiwang en* service candidate")
	}
	for _, service := range []macNetworkService{
		{Name: "Baiwang", HardwarePort: "Baiwang", Device: "utun4"},
		{Name: "Studio Display", HardwarePort: "USB 10/100/1000 LAN", Device: "en7"},
	} {
		if isDJICellularService(service) {
			t.Fatalf("unexpected candidate: %+v", service)
		}
	}
}

func TestParseMacIPv4ServiceInfo(t *testing.T) {
	info := parseMacIPv4ServiceInfo(`DHCP Configuration
IP address: 192.168.225.29
Subnet mask: 255.255.255.0
Router: 192.168.225.1
`)
	if info.Address != "192.168.225.29" || info.Subnet != "255.255.255.0" {
		t.Fatalf("IPv4 service info = %+v", info)
	}
}

func TestInitUSBATESIMManagerAfterDelayedUSBOpen(t *testing.T) {
	instance := &app{}

	manager, switchAllowed := instance.currentESIMManager()
	if manager != nil || switchAllowed {
		t.Fatalf("initial eSIM state = (%v, %v), want unavailable", manager, switchAllowed)
	}

	instance.initUSBATESIMManager()
	manager, switchAllowed = instance.currentESIMManager()
	if manager == nil {
		t.Fatal("USB AT recovery did not initialize the eSIM manager")
	}
	if !switchAllowed {
		t.Fatal("USB AT eSIM manager should allow profile switching")
	}

	instance.initUSBATESIMManager()
	managerAgain, _ := instance.currentESIMManager()
	if managerAgain != manager {
		t.Fatal("repeated USB AT recovery replaced the existing eSIM manager")
	}
}

func TestSecurityHeadersRequireLoopbackHostAndOrigin(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	tests := []struct {
		name   string
		host   string
		origin string
		site   string
		want   int
	}{
		{name: "native IPv4", host: "127.0.0.1:7576", want: http.StatusNoContent},
		{name: "native IPv6", host: "[::1]:7576", want: http.StatusNoContent},
		{name: "same-origin browser", host: "localhost:7576", origin: "http://localhost:7576", site: "same-origin", want: http.StatusNoContent},
		{name: "DNS rebinding host", host: "attacker.example", want: http.StatusForbidden},
		{name: "foreign origin", host: "127.0.0.1:7576", origin: "https://attacker.example", want: http.StatusForbidden},
		{name: "cross-site fetch", host: "127.0.0.1:7576", origin: "http://127.0.0.1:7576", site: "cross-site", want: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7576/api/test", nil)
			req.Host = test.host
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			if test.site != "" {
				req.Header.Set("Sec-Fetch-Site", test.site)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestLoopbackListenAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7576", "[::1]:7576", "localhost:7576"} {
		if !isLoopbackListenAddress(address) {
			t.Fatalf("loopback listen address rejected: %q", address)
		}
	}
	for _, address := range []string{"0.0.0.0:7576", ":7576", "192.168.1.10:7576", "bad"} {
		if isLoopbackListenAddress(address) {
			t.Fatalf("non-loopback listen address accepted: %q", address)
		}
	}
}

func TestSafeDiagnosticATCommand(t *testing.T) {
	for _, command := range []string{
		"AT", "ATI", "AT+CSQ", "AT+CPIN?", "AT+CEREG?", "AT+CGDCONT?",
		"AT+QPCMV?", "AT+QPCMV=?", "AT+CLCC", "AT+CGPADDR=1",
		`AT+QCFG="USBCFG"`, `at+qcfg="ims"`,
	} {
		if !isSafeDiagnosticATCommand(command) {
			t.Fatalf("safe diagnostic command rejected: %q", command)
		}
	}
	for _, command := range []string{
		"ATD10086;", "ATA", "ATH", "AT+CFUN=1,1", `AT+QCFG="ims",1`,
		"AT+CMGD=1", "AT+CSQ\nAT+CFUN=1,1", "ATA?", "ATH?", "ATD10086?",
		"AT+CHUP?", "AT+VTS?", "AT+CLDTMF?", "AT+QPCMV=1,2?", "AT+CFUN=1?",
		"AT+UNKNOWN?",
	} {
		if isSafeDiagnosticATCommand(command) {
			t.Fatalf("mutating AT command accepted: %q", command)
		}
	}
}

func TestUSBATPacketAndIMSStatusParsing(t *testing.T) {
	if !parseUSBATPSAttached("AT+CGATT?\r\n+CGATT: 1\r\nOK") {
		t.Fatal("attached packet state was not parsed")
	}
	if parseUSBATPSAttached("+CGATT: 0\r\nOK") {
		t.Fatal("detached packet state was parsed as attached")
	}
	contexts := "+CGDCONT: 1,\"IPV4V6\",\"CTNET\",\"0.0.0.0\",0,0\r\n" +
		"+CGDCONT: 5,\"IPV4V6\",\"ims\",\"0.0.0.0\",0,0\r\n"
	if got := parseUSBATPrimaryAPN(contexts); got != "CTNET" {
		t.Fatalf("primary APN = %q", got)
	}
	if got := parseUSBATIMSStatus("+QIMS: 1\r\nOK"); got != 1 {
		t.Fatalf("IMS status = %d", got)
	}
}

func TestUSBNetMutationRejectsConcurrentRequest(t *testing.T) {
	instance := newDemoApp()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	instance.moduleMutationLockedHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	firstResponse := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodPost, "/api/network/usbnet", bytes.NewBufferString(`{"mode":1,"confirm":true}`))
	done := make(chan struct{})
	go func() {
		instance.setUSBNetMode(firstResponse, firstRequest)
		close(done)
	}()
	<-entered

	secondResponse := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/api/network/usbnet", bytes.NewBufferString(`{"mode":0,"confirm":true}`))
	instance.setUSBNetMode(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusConflict {
		t.Fatalf("second mutation status = %d, want %d", secondResponse.Code, http.StatusConflict)
	}

	close(release)
	<-done
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first mutation status = %d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
}
