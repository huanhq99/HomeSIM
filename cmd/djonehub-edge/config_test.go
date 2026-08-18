package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewEdgeAccessHTTPClientIsDirectAndBounded(t *testing.T) {
	client := newEdgeAccessHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.DialContext == nil ||
		client.Timeout != 5*time.Second || transport.TLSClientConfig == nil ||
		transport.TLSClientConfig.MinVersion != tls.VersionTLS13 ||
		transport.ResponseHeaderTimeout != 5*time.Second || transport.TLSHandshakeTimeout != 5*time.Second {
		t.Fatalf("unexpected access HTTP policy: client=%v transport=%v", client, transport)
	}
}

func TestLoadEdgeRuntimeConfigExactDefaultOffSurface(t *testing.T) {
	environment := validEdgeTestEnvironment(t)
	config, err := loadEdgeRuntimeConfig(mapEdgeEnvironment(environment), nil)
	if err != nil || config.registry == nil || config.access == nil ||
		config.listenAddress != edgeListenAddress || config.exactHost != "phone.example.com" {
		t.Fatalf("config=%v err=%v", config, err)
	}
	formatted := fmt.Sprintf("%+v %#v", config, config)
	if strings.Contains(formatted, "allowed@example.invalid") || strings.Contains(formatted, "/private") {
		t.Fatal("runtime config formatting leaked private configuration")
	}

	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing", mutate: func(values map[string]string) { delete(values, "DJI4G_ACCESS_AUDIENCE") }},
		{name: "whitespace", mutate: func(values map[string]string) { values["DJI4G_ACCESS_AUDIENCE"] += " " }},
		{name: "listen", mutate: func(values map[string]string) { values["DJI4G_EDGE_LISTEN_ADDR"] = "0.0.0.0:80" }},
		{name: "host", mutate: func(values map[string]string) { values["DJI4G_EDGE_PUBLIC_HOST"] = "other.example.com" }},
		{name: "gateway", mutate: func(values map[string]string) { values["DJI4G_EDGE_GATEWAY_ID"] = "Home Gateway" }},
		{name: "relative_key", mutate: func(values map[string]string) { values["DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE"] = "gateway.pem" }},
		{name: "forwarded_identity", mutate: func(values map[string]string) { values["DJI4G_EDGE_TRUST_FORWARDED_IDENTITY"] = "true" }},
		{name: "enrollment", mutate: func(values map[string]string) { values["DJI4G_EDGE_ENROLLMENT_ENABLED"] = "true" }},
		{name: "mutations", mutate: func(values map[string]string) { values["DJI4G_EDGE_MUTATIONS_ENABLED"] = "true" }},
		{name: "push", mutate: func(values map[string]string) { values["DJI4G_EDGE_PUSH_ENABLED"] = "true" }},
		{name: "turn", mutate: func(values map[string]string) { values["DJI4G_EDGE_TURN_ISSUANCE_ENABLED"] = "true" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneEdgeEnvironment(environment)
			test.mutate(candidate)
			if _, err := loadEdgeRuntimeConfig(mapEdgeEnvironment(candidate), nil); !errors.Is(err, errEdgeConfiguration) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadEdgeGatewayPublicKeyRejectsAmbiguity(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	valid := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "gateway.pem")
	writeEdgeTestPrivateFile(t, path, valid)
	loaded, err := loadEdgeGatewayPublicKey(path)
	if err != nil || loaded.X.Cmp(key.X) != 0 || loaded.Y.Cmp(key.Y) != 0 {
		t.Fatalf("load err=%v", err)
	}

	for _, candidate := range []struct {
		name string
		data []byte
	}{
		{name: "wrong_label", data: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
		{name: "trailing", data: append(append([]byte(nil), valid...), []byte("unexpected")...)},
		{name: "private", data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			writeEdgeTestPrivateFile(t, path, candidate.data)
			if _, err := loadEdgeGatewayPublicKey(path); !errors.Is(err, errEdgeConfiguration) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadEdgeAllowedEmailsIsBoundedAndExact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "emails")
	writeEdgeTestPrivateFile(t, path, []byte("allowed@example.invalid\nsecond@example.invalid\n"))
	emails, err := loadEdgeAllowedEmails(path)
	if err != nil || len(emails) != 2 || emails[0] != "allowed@example.invalid" {
		t.Fatalf("emails=%v err=%v", emails, err)
	}
	for _, invalid := range []string{
		"", "allowed@example.invalid\n\nsecond@example.invalid", "allowed@example.invalid\r\n",
		"one@example.invalid\ntwo@example.invalid\nthree@example.invalid\nfour@example.invalid\nfive@example.invalid\nsix@example.invalid\nseven@example.invalid\neight@example.invalid\nnine@example.invalid",
	} {
		writeEdgeTestPrivateFile(t, path, []byte(invalid))
		if _, err := loadEdgeAllowedEmails(path); !errors.Is(err, errEdgeConfiguration) {
			t.Fatalf("invalid=%q err=%v", invalid, err)
		}
	}
}

func validEdgeTestEnvironment(t *testing.T) map[string]string {
	t.Helper()
	directory := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "gateway-public.pem")
	emailsPath := filepath.Join(directory, "allowed-emails")
	writeEdgeTestPrivateFile(t, keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	writeEdgeTestPrivateFile(t, emailsPath, []byte("allowed@example.invalid\n"))
	return map[string]string{
		"DJI4G_EDGE_LISTEN_ADDR":              edgeListenAddress,
		"DJI4G_EDGE_PUBLIC_HOST":              "phone.example.com",
		"DJI4G_EDGE_GATEWAY_ID":               "home-gateway-1",
		"DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE":  keyPath,
		"DJI4G_ACCESS_TEAM_DOMAIN":            "synthetic.cloudflareaccess.com",
		"DJI4G_ACCESS_AUDIENCE":               "synthetic_audience_123",
		"DJI4G_ACCESS_ALLOWED_EMAILS_FILE":    emailsPath,
		"DJI4G_EDGE_TRUST_FORWARDED_IDENTITY": "false",
		"DJI4G_EDGE_ENROLLMENT_ENABLED":       "false",
		"DJI4G_EDGE_MUTATIONS_ENABLED":        "false",
		"DJI4G_EDGE_PUSH_ENABLED":             "false",
		"DJI4G_EDGE_TURN_ISSUANCE_ENABLED":    "false",
	}
}

func mapEdgeEnvironment(values map[string]string) environmentLookup {
	return func(name string) (string, bool) {
		value, present := values[name]
		return value, present
	}
}

func cloneEdgeEnvironment(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func writeEdgeTestPrivateFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
