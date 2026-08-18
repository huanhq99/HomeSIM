package turnauth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestIssuerProducesExactCoturnRESTCredential(t *testing.T) {
	secret := bytes.Repeat([]byte{0x4a}, minimumSecretBytes)
	now := time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC)
	issuer, err := New(Config{
		Host: "TURN.EXAMPLE.COM.", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: secret, Now: func() time.Time { return now },
		Random: bytes.NewReader(bytes.Repeat([]byte{0x2c}, credentialRandomBytes)),
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	defer issuer.Close()
	credentials, err := issuer.Issue()
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	wire, err := credentials.EncodeJSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	relay, err := credentials.ServerRelay()
	if err != nil {
		t.Fatalf("server relay credential: %v", err)
	}
	wantServerURLs := []string{
		"turn:turn.example.com:3478?transport=udp",
		"turns:turn.example.com:443?transport=tcp",
	}
	if fmt.Sprint(relay.URLs) != fmt.Sprint(wantServerURLs) ||
		relay.Username == "" || relay.Password == "" || !relay.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("unexpected server relay credential: %v", relay)
	}
	formattedRelay := fmt.Sprintf("%v %+v %#v", relay, relay, relay)
	for _, sensitive := range append(append([]string(nil), relay.URLs...), relay.Username, relay.Password) {
		if strings.Contains(formattedRelay, sensitive) {
			t.Fatalf("server relay formatting leaked material: %s", formattedRelay)
		}
	}
	relayJSON, err := json.Marshal(relay)
	if err != nil || string(relayJSON) != `{"redacted":true}` {
		t.Fatalf("server relay JSON=%s err=%v", relayJSON, err)
	}
	relay.URLs[0] = "turn:mutated.invalid:3478?transport=udp"
	secondRelay, err := credentials.ServerRelay()
	if err != nil || secondRelay.URLs[0] != wantServerURLs[0] {
		t.Fatal("server relay URLs alias credential-owned state")
	}
	var decoded wireCredentials
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantURLs := []string{
		"turn:turn.example.com:3478?transport=udp",
		"turn:turn.example.com:3478?transport=tcp",
		"turns:turn.example.com:443?transport=tcp",
	}
	if fmt.Sprint(decoded.URLs) != fmt.Sprint(wantURLs) {
		t.Fatalf("URLs = %v, want %v", decoded.URLs, wantURLs)
	}
	if decoded.ExpiresAt != now.Add(5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("expires_at = %q", decoded.ExpiresAt)
	}
	mac := hmac.New(sha1.New, secret)
	_, _ = mac.Write([]byte(decoded.Username))
	wantCredential := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if decoded.Credential != wantCredential {
		t.Fatal("credential does not match coturn REST HMAC")
	}
	if !strings.HasPrefix(decoded.Username, fmt.Sprintf("%d:", now.Add(5*time.Minute).Unix())) {
		t.Fatalf("username = %q", decoded.Username)
	}

	resolvedRelay, err := credentials.ServerRelayWithResolvedIPv4("8.8.8.8")
	if err != nil {
		t.Fatalf("resolved server relay credential: %v", err)
	}
	wantResolvedURLs := []string{
		"turn:8.8.8.8:3478?transport=udp",
		"turn:8.8.8.8:3478?transport=tcp",
		"turns:turn.example.com:443?transport=tcp",
	}
	if fmt.Sprint(resolvedRelay.URLs) != fmt.Sprint(wantResolvedURLs) ||
		resolvedRelay.Username != secondRelay.Username || resolvedRelay.Password != secondRelay.Password {
		t.Fatalf("unexpected resolved server relay credential: %v", resolvedRelay)
	}
	resolvedWire, err := credentials.EncodeJSONWithResolvedIPv4("8.8.8.8")
	if err != nil {
		t.Fatalf("encode resolved browser credential: %v", err)
	}
	var resolvedDecoded wireCredentials
	if err := json.Unmarshal(resolvedWire, &resolvedDecoded); err != nil ||
		fmt.Sprint(resolvedDecoded.URLs) != fmt.Sprint(wantResolvedURLs) ||
		resolvedDecoded.Username != decoded.Username || resolvedDecoded.Credential != decoded.Credential {
		t.Fatalf("unexpected resolved browser credential: %+v err=%v", resolvedDecoded, err)
	}
}

func TestServerRelayWithResolvedIPv4RejectsNonPublicAddresses(t *testing.T) {
	credentials := Credentials{
		urls: []string{
			"turn:turn.example.test:3478?transport=udp",
			"turn:turn.example.test:3478?transport=tcp",
			"turns:turn.example.test:443?transport=tcp",
		},
		username: "private-user", credential: "private-password", expiresAt: time.Unix(1, 0),
	}
	for _, address := range []string{
		"", "not-an-ip", "127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1",
		"198.19.153.67", "198.51.100.1", "203.0.113.1", "224.0.0.1", "2001:4860:4860::8888",
	} {
		if _, err := credentials.ServerRelayWithResolvedIPv4(address); err == nil {
			t.Fatalf("non-public IPv4 %q accepted", address)
		} else if (address != "" && strings.Contains(err.Error(), address)) || strings.Contains(err.Error(), "private-") {
			t.Fatalf("validation error leaked caller material: %v", err)
		}
		if _, err := credentials.EncodeJSONWithResolvedIPv4(address); err == nil {
			t.Fatalf("browser credential accepted non-public IPv4 %q", address)
		}
	}
}

func TestServerRelayRejectsMalformedInternalCredentialBundles(t *testing.T) {
	valid := Credentials{
		urls: []string{
			"turn:turn.example.test:3478?transport=udp",
			"turn:turn.example.test:3478?transport=tcp",
			"turns:turn.example.test:443?transport=tcp",
		},
		username: "private-user", credential: "private-password", expiresAt: time.Unix(1, 0),
	}
	for _, test := range []struct {
		name   string
		mutate func(*Credentials)
	}{
		{name: "missing browser TCP URL", mutate: func(c *Credentials) { c.urls = c.urls[:2] }},
		{name: "server UDP replaced by TCP", mutate: func(c *Credentials) { c.urls[0] = c.urls[1] }},
		{name: "browser TCP host differs", mutate: func(c *Credentials) { c.urls[1] = "turn:other.example.test:3478?transport=tcp" }},
		{name: "TLS fallback is plaintext", mutate: func(c *Credentials) { c.urls[2] = "turn:turn.example.test:443?transport=tcp" }},
		{name: "TLS fallback host differs", mutate: func(c *Credentials) { c.urls[2] = "turns:other.example.test:443?transport=tcp" }},
		{name: "embedded userinfo", mutate: func(c *Credentials) { c.urls[0] = "turn:user@turn.example.test:3478?transport=udp" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.urls = append([]string(nil), valid.urls...)
			test.mutate(&candidate)
			if _, err := candidate.ServerRelay(); err == nil {
				t.Fatal("malformed server relay bundle accepted")
			} else {
				for _, sensitive := range []string{"private-user", "private-password", "other.example.test"} {
					if strings.Contains(err.Error(), sensitive) {
						t.Fatalf("validation error leaked credential material: %v", err)
					}
				}
			}
		})
	}
}

func TestCredentialsAreRedactedByOrdinaryFormatting(t *testing.T) {
	credentials := Credentials{
		urls:     []string{"turn:synthetic.example:3478?transport=udp", "x", "y"},
		username: "private-user", credential: "private-password", expiresAt: time.Unix(1, 0),
	}
	for _, rendered := range []string{
		fmt.Sprint(credentials), fmt.Sprintf("%+v", credentials), fmt.Sprintf("%#v", credentials),
		string(mustJSON(t, credentials)),
	} {
		if strings.Contains(rendered, "private-user") || strings.Contains(rendered, "private-password") {
			t.Fatalf("ordinary formatting leaked credentials: %s", rendered)
		}
	}
}

func TestConfigIsRedactedByOrdinaryFormatting(t *testing.T) {
	secret := []byte("private-turn-shared-secret-marker")
	config := Config{Host: "turn.synthetic.example", UDPPort: 3478, TLSPort: 443, TTL: 5 * time.Minute, Secret: secret}
	for _, rendered := range []string{
		fmt.Sprint(config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config),
		string(mustJSON(t, config)),
	} {
		if strings.Contains(rendered, string(secret)) {
			t.Fatalf("ordinary formatting leaked TURN configuration: %s", rendered)
		}
	}
}

func TestIssuerOwnsAndClearsSecret(t *testing.T) {
	secret := bytes.Repeat([]byte{0x55}, minimumSecretBytes)
	issuer, err := New(Config{
		Host: "turn.synthetic.example", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: secret,
		Random: bytes.NewReader(bytes.Repeat([]byte{1}, credentialRandomBytes)),
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	secret[0] = 0
	if issuer.secret[0] != 0x55 {
		t.Fatal("issuer did not own a secret copy")
	}
	if err := issuer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := issuer.Issue(); !errors.Is(err, ErrClosed) {
		t.Fatalf("issue after close = %v", err)
	}
	if err := issuer.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	valid := Config{
		Host: "turn.synthetic.example", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: bytes.Repeat([]byte{1}, minimumSecretBytes),
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"single label host", func(config *Config) { config.Host = "localhost" }},
		{"IP host", func(config *Config) { config.Host = "203.0.113.10" }},
		{"wildcard host", func(config *Config) { config.Host = "*.example.com" }},
		{"userinfo host", func(config *Config) { config.Host = "user@example.com" }},
		{"missing UDP port", func(config *Config) { config.UDPPort = 0 }},
		{"same ports", func(config *Config) { config.TLSPort = config.UDPPort }},
		{"short TTL", func(config *Config) { config.TTL = minimumTTL - time.Second }},
		{"long TTL", func(config *Config) { config.TTL = maximumTTL + time.Second }},
		{"short secret", func(config *Config) { config.Secret = bytes.Repeat([]byte{1}, minimumSecretBytes-1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() = %v", err)
			}
		})
	}
}

func TestIssueFailsClosedOnRandomFailure(t *testing.T) {
	issuer, err := New(Config{
		Host: "turn.synthetic.example", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: bytes.Repeat([]byte{1}, minimumSecretBytes),
		Random: errorReader{},
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	defer issuer.Close()
	if _, err := issuer.Issue(); err == nil {
		t.Fatal("Issue unexpectedly succeeded")
	} else if strings.Contains(err.Error(), "synthetic random failure") {
		t.Fatalf("Issue leaked injected entropy error: %v", err)
	}
}

func TestConcurrentIssueProducesDistinctOpaqueUsers(t *testing.T) {
	issuer, err := New(Config{
		Host: "turn.synthetic.example", UDPPort: 3478, TLSPort: 443,
		TTL: 5 * time.Minute, Secret: bytes.Repeat([]byte{1}, minimumSecretBytes),
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	defer issuer.Close()
	const count = 32
	usernames := make(chan string, count)
	errorsSeen := make(chan error, count)
	for index := 0; index < count; index++ {
		go func() {
			credentials, issueErr := issuer.Issue()
			if issueErr != nil {
				errorsSeen <- issueErr
				return
			}
			wire, encodeErr := credentials.EncodeJSON()
			if encodeErr != nil {
				errorsSeen <- encodeErr
				return
			}
			var decoded wireCredentials
			if decodeErr := json.Unmarshal(wire, &decoded); decodeErr != nil {
				errorsSeen <- decodeErr
				return
			}
			usernames <- decoded.Username
		}()
	}
	seen := make(map[string]struct{}, count)
	for index := 0; index < count; index++ {
		select {
		case issueErr := <-errorsSeen:
			t.Fatalf("concurrent Issue: %v", issueErr)
		case username := <-usernames:
			if _, exists := seen[username]; exists {
				t.Fatalf("duplicate opaque username %q", username)
			}
			seen[username] = struct{}{}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Issue timed out")
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("synthetic random failure") }

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
