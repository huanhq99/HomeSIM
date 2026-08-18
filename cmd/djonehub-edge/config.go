package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/iniwex5/vohive/internal/publicedge"
	"github.com/iniwex5/vohive/internal/publicedgeserver"
)

const (
	edgeListenAddress            = "0.0.0.0:8080"
	edgePublicKeyMaximumBytes    = 8 << 10
	edgeAllowedEmailsMaximumSize = 8 << 10
	edgeAllowedEmailMaximumCount = 8
)

var (
	errEdgeConfiguration = errors.New("public edge runtime configuration is invalid")
	edgeGatewayIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)
)

type edgeRuntimeConfig struct {
	listenAddress string
	exactHost     string
	registry      *publicedge.Registry
	access        *publicedge.AccessVerifier
}

func (edgeRuntimeConfig) String() string   { return "edgeRuntimeConfig{redacted}" }
func (edgeRuntimeConfig) GoString() string { return "edgeRuntimeConfig{redacted}" }

type environmentLookup func(string) (string, bool)

func loadEdgeRuntimeConfig(lookup environmentLookup, client *http.Client) (edgeRuntimeConfig, error) {
	if lookup == nil {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	required := []string{
		"DJI4G_EDGE_LISTEN_ADDR",
		"DJI4G_EDGE_PUBLIC_HOST",
		"DJI4G_EDGE_GATEWAY_ID",
		"DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE",
		"DJI4G_ACCESS_TEAM_DOMAIN",
		"DJI4G_ACCESS_AUDIENCE",
		"DJI4G_ACCESS_ALLOWED_EMAILS_FILE",
	}
	values := make(map[string]string, len(required))
	for _, name := range required {
		value, present := lookup(name)
		if !present || value == "" || strings.TrimSpace(value) != value {
			return edgeRuntimeConfig{}, errEdgeConfiguration
		}
		values[name] = value
	}
	for _, name := range []string{
		"DJI4G_EDGE_TRUST_FORWARDED_IDENTITY",
		"DJI4G_EDGE_ENROLLMENT_ENABLED",
		"DJI4G_EDGE_MUTATIONS_ENABLED",
		"DJI4G_EDGE_PUSH_ENABLED",
		"DJI4G_EDGE_TURN_ISSUANCE_ENABLED",
	} {
		value, present := lookup(name)
		if !present || value != "false" {
			return edgeRuntimeConfig{}, errEdgeConfiguration
		}
	}
	if values["DJI4G_EDGE_LISTEN_ADDR"] != edgeListenAddress ||
		values["DJI4G_EDGE_PUBLIC_HOST"] != publicedgeserver.ProductionExactHost ||
		!edgeGatewayIDPattern.MatchString(values["DJI4G_EDGE_GATEWAY_ID"]) {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	publicKeyPath := values["DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE"]
	emailsPath := values["DJI4G_ACCESS_ALLOWED_EMAILS_FILE"]
	if !filepath.IsAbs(publicKeyPath) || !filepath.IsAbs(emailsPath) {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	publicKey, err := loadEdgeGatewayPublicKey(publicKeyPath)
	if err != nil {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	emails, err := loadEdgeAllowedEmails(emailsPath)
	if err != nil {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	registry, err := publicedge.NewRegistry(publicedge.RegistryConfig{
		GatewayKeys: map[string]*ecdsa.PublicKey{
			values["DJI4G_EDGE_GATEWAY_ID"]: publicKey,
		},
	})
	if err != nil {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	access, err := publicedge.NewAccessVerifier(publicedge.AccessConfig{
		TeamDomain: values["DJI4G_ACCESS_TEAM_DOMAIN"],
		Audience:   values["DJI4G_ACCESS_AUDIENCE"], AllowedEmails: emails,
		HTTPClient: client,
	})
	if err != nil {
		return edgeRuntimeConfig{}, errEdgeConfiguration
	}
	return edgeRuntimeConfig{
		listenAddress: values["DJI4G_EDGE_LISTEN_ADDR"],
		exactHost:     values["DJI4G_EDGE_PUBLIC_HOST"], registry: registry, access: access,
	}, nil
}

func loadEdgeGatewayPublicKey(path string) (*ecdsa.PublicKey, error) {
	data, err := readPrivateEdgeFile(path, edgePublicKeyMaximumBytes)
	if err != nil {
		return nil, errEdgeConfiguration
	}
	defer zeroEdgeBytes(data)
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 ||
		len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errEdgeConfiguration
	}
	defer zeroEdgeBytes(block.Bytes)
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errEdgeConfiguration
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() || key.X == nil || key.Y == nil ||
		!key.Curve.IsOnCurve(key.X, key.Y) {
		return nil, errEdgeConfiguration
	}
	return key, nil
}

func loadEdgeAllowedEmails(path string) ([]string, error) {
	data, err := readPrivateEdgeFile(path, edgeAllowedEmailsMaximumSize)
	if err != nil {
		return nil, errEdgeConfiguration
	}
	defer zeroEdgeBytes(data)
	if !utf8.Valid(data) || strings.ContainsRune(string(data), '\r') {
		return nil, errEdgeConfiguration
	}
	content := strings.TrimSuffix(string(data), "\n")
	if content == "" || strings.Contains(content, "\n\n") || strings.HasSuffix(content, "\n") {
		return nil, errEdgeConfiguration
	}
	emails := strings.Split(content, "\n")
	if len(emails) == 0 || len(emails) > edgeAllowedEmailMaximumCount {
		return nil, errEdgeConfiguration
	}
	return emails, nil
}

type edgeAccessAuth struct{ verifier *publicedge.AccessVerifier }

func (auth edgeAccessAuth) AuthorizeHTTP(request *http.Request) error {
	if auth.verifier == nil {
		return publicedge.ErrAccessUnauthorized
	}
	_, err := auth.verifier.VerifyRequest(request)
	return err
}

func zeroEdgeBytes(data []byte) {
	for index := range data {
		data[index] = 0
	}
}
