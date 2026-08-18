package publicedge

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	AccessAssertionHeader = "Cf-Access-Jwt-Assertion"

	DefaultAccessKeyCacheTTL = time.Hour
	accessHTTPTimeout        = 5 * time.Second
	maxAccessTokenBytes      = 32 << 10
	maxAccessHeaderBytes     = 4 << 10
	maxAccessClaimsBytes     = 16 << 10
	maxAccessSignatureBytes  = 1 << 10
	maxAccessJWKSBytes       = 256 << 10
	maxAccessJWKCount        = 16
	maxAccessNegativeKIDs    = 64
)

var (
	ErrAccessConfiguration = errors.New("cloudflare access verifier configuration is invalid")
	ErrAccessUnauthorized  = errors.New("cloudflare access assertion is unauthorized")
	ErrAccessUnavailable   = errors.New("cloudflare access verification keys are unavailable")
)

type accessFailure struct {
	code string
}

func (*accessFailure) Error() string { return ErrAccessUnauthorized.Error() }
func (*accessFailure) Unwrap() error { return ErrAccessUnauthorized }

func rejectAccess(code string) error {
	return &accessFailure{code: code}
}

// AccessFailureCode returns one bounded, attacker-independent diagnostic code.
// It deliberately exposes no assertion, claim, key ID, email, or other input.
func AccessFailureCode(err error) string {
	if errors.Is(err, ErrAccessUnavailable) {
		return "keys_unavailable"
	}
	var failure *accessFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	if errors.Is(err, ErrAccessUnauthorized) {
		return "unauthorized"
	}
	return "verification_error"
}

var (
	accessTeamDomainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$`)
	accessOpaquePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	accessAudiencePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// AccessConfig contains the exact Cloudflare Access trust boundary. TeamDomain
// is a host name (not a URL), Audience is one application AUD tag, and every
// accepted identity must exactly match one AllowedEmails entry.
//
// Its ordinary JSON and formatting forms are deliberately redacted.
type AccessConfig struct {
	TeamDomain    string
	Audience      string
	AllowedEmails []string
	HTTPClient    *http.Client
	Clock         func() time.Time
	CacheTTL      time.Duration
}

// AccessIdentity is the small, verified identity surface returned to the HTTP
// edge. Header-derived identity values are never used as a fallback.
//
// Its ordinary JSON and formatting forms are deliberately redacted.
type AccessIdentity struct {
	Email   string
	Subject string
}

// AccessVerifier validates Cloudflare Access application JWTs using the
// account's rotating RSA JWK set. It is safe for concurrent use.
type AccessVerifier struct {
	teamDomain string
	issuer     string
	audience   string
	allowed    map[string]struct{}
	client     *http.Client
	clock      func() time.Time
	cacheTTL   time.Duration
	jwksURL    string

	mu              sync.Mutex
	keys            map[string]*rsa.PublicKey
	expiresAt       time.Time
	negativeKIDs    map[string]time.Time
	lastMissRefresh time.Time
	refreshFailedTo time.Time
	refreshing      *accessKeyRefresh
}

type accessKeyRefresh struct {
	done chan struct{}
	err  error
}

type accessJWTHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type accessJWTClaims struct {
	Audience      []string `json:"aud"`
	Email         string   `json:"email"`
	ExpiresAt     int64    `json:"exp"`
	IssuedAt      int64    `json:"iat"`
	NotBefore     int64    `json:"nbf"`
	Issuer        string   `json:"iss"`
	Type          string   `json:"type"`
	IdentityNonce string   `json:"identity_nonce"`
	Subject       string   `json:"sub"`
	Country       string   `json:"country,omitempty"`
}

type accessJWKSetWire struct {
	Keys        []json.RawMessage `json:"keys"`
	PublicCert  json.RawMessage   `json:"public_cert,omitempty"`
	PublicCerts json.RawMessage   `json:"public_certs,omitempty"`
}

type accessJWKWire struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	Exponent  string `json:"e"`
	Modulus   string `json:"n"`
}

type accessPEMCertWire struct {
	KeyID       string `json:"kid"`
	Certificate string `json:"cert"`
}

var (
	accessJWTHeaderKeys = exactKeySet("alg", "kid", "typ")
	accessJWTClaimKeys  = exactKeySet(
		"aud", "email", "exp", "iat", "nbf", "iss", "type", "identity_nonce", "sub", "country",
	)
	accessJWKSetKeys  = exactKeySet("keys", "public_cert", "public_certs")
	accessJWKKeys     = exactKeySet("kid", "kty", "alg", "use", "e", "n")
	accessPEMCertKeys = exactKeySet("kid", "cert")
)

// NewAccessVerifier builds a disabled-by-construction verifier: incomplete or
// ambiguous trust configuration is rejected instead of being normalized.
func NewAccessVerifier(config AccessConfig) (*AccessVerifier, error) {
	if !accessTeamDomainPattern.MatchString(config.TeamDomain) ||
		!validAccessAudience(config.Audience) || len(config.AllowedEmails) == 0 {
		return nil, ErrAccessConfiguration
	}
	cacheTTL := config.CacheTTL
	if cacheTTL == 0 {
		cacheTTL = DefaultAccessKeyCacheTTL
	}
	if cacheTTL < time.Second || cacheTTL > 24*time.Hour {
		return nil, ErrAccessConfiguration
	}
	allowed := make(map[string]struct{}, len(config.AllowedEmails))
	for _, email := range config.AllowedEmails {
		if !validAccessEmail(email) {
			return nil, ErrAccessConfiguration
		}
		if _, duplicate := allowed[email]; duplicate {
			return nil, ErrAccessConfiguration
		}
		allowed[email] = struct{}{}
	}

	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	sourceClient := config.HTTPClient
	if sourceClient == nil {
		sourceClient = http.DefaultClient
	}
	client := *sourceClient
	if client.Timeout <= 0 || client.Timeout > accessHTTPTimeout {
		client.Timeout = accessHTTPTimeout
	}
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	issuer := "https://" + config.TeamDomain
	return &AccessVerifier{
		teamDomain:   config.TeamDomain,
		issuer:       issuer,
		audience:     config.Audience,
		allowed:      allowed,
		client:       &client,
		clock:        clock,
		cacheTTL:     cacheTTL,
		jwksURL:      issuer + "/cdn-cgi/access/certs",
		keys:         make(map[string]*rsa.PublicKey),
		negativeKIDs: make(map[string]time.Time),
	}, nil
}

// VerifyRequest accepts exactly one Cf-Access-Jwt-Assertion field value. It
// intentionally ignores authenticated-user, forwarding, and Tailscale headers:
// none of them can establish an identity.
func (verifier *AccessVerifier) VerifyRequest(request *http.Request) (AccessIdentity, error) {
	if verifier == nil || request == nil {
		return AccessIdentity{}, rejectAccess("request")
	}
	assertion, ok := exactAccessAssertion(request.Header)
	if !ok {
		return AccessIdentity{}, rejectAccess("assertion")
	}
	return verifier.verify(request.Context(), assertion)
}

func (verifier *AccessVerifier) verify(ctx context.Context, assertion string) (AccessIdentity, error) {
	if len(assertion) == 0 || len(assertion) > maxAccessTokenBytes {
		return AccessIdentity{}, rejectAccess("token_size")
	}
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return AccessIdentity{}, rejectAccess("token_format")
	}
	headerBytes, ok := decodeCanonicalAccessSegment(parts[0], maxAccessHeaderBytes)
	if !ok || validateNoDuplicateJSONKeys(headerBytes, 4) != nil ||
		validateExactObjectKeys(headerBytes, accessJWTHeaderKeys) != nil {
		return AccessIdentity{}, rejectAccess("header_schema")
	}
	var header accessJWTHeader
	if decodeStrictJSON(headerBytes, &header) != nil {
		return AccessIdentity{}, rejectAccess("header_schema")
	}
	if header.Algorithm != "RS256" {
		return AccessIdentity{}, rejectAccess("header_algorithm")
	}
	// Cloudflare's documented application-token form uses typ=JWT, while its
	// production edge can omit this optional JOSE hint. Authentication remains
	// bound to RS256, the exact signing key, issuer, audience and identity
	// claims; a present non-JWT type is still rejected.
	if header.Type != "" && header.Type != "JWT" {
		return AccessIdentity{}, rejectAccess("header_type")
	}
	if !validAccessKeyID(header.KeyID) {
		return AccessIdentity{}, rejectAccess("header_key_id")
	}
	signature, ok := decodeCanonicalAccessSegment(parts[2], maxAccessSignatureBytes)
	if !ok || len(signature) == 0 {
		return AccessIdentity{}, rejectAccess("signature_format")
	}

	key, err := verifier.keyFor(ctx, header.KeyID)
	if err != nil {
		if errors.Is(err, ErrAccessUnauthorized) {
			return AccessIdentity{}, rejectAccess("key_id")
		}
		return AccessIdentity{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(signature) != key.Size() || rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) != nil {
		return AccessIdentity{}, rejectAccess("signature")
	}

	claimsBytes, ok := decodeCanonicalAccessSegment(parts[1], maxAccessClaimsBytes)
	if !ok || validateNoDuplicateJSONKeys(claimsBytes, 4) != nil {
		return AccessIdentity{}, rejectAccess("claims_schema")
	}
	claims, ok := decodeAccessJWTClaims(claimsBytes)
	if !ok {
		return AccessIdentity{}, rejectAccess("claims_schema")
	}
	if !verifier.validClaims(claims) {
		return AccessIdentity{}, rejectAccess("claims_values")
	}
	return AccessIdentity{Email: claims.Email, Subject: claims.Subject}, nil
}

// decodeAccessJWTClaims accepts bounded, signed extension claims such as
// Cloudflare Access's documented custom claim while decoding only the identity
// fields used for authorization. A case variant of a recognized field remains
// invalid so encoding/json cannot reinterpret it as that field.
func decodeAccessJWTClaims(data []byte) (accessJWTClaims, bool) {
	if !utf8.Valid(data) {
		return accessJWTClaims{}, false
	}
	var object map[string]json.RawMessage
	if decodeStrictJSON(data, &object) != nil || object == nil || len(object) > 64 {
		return accessJWTClaims{}, false
	}
	recognized := make(map[string]json.RawMessage, len(accessJWTClaimKeys))
	for key, value := range object {
		if _, ok := accessJWTClaimKeys[key]; ok {
			recognized[key] = value
			continue
		}
		for canonical := range accessJWTClaimKeys {
			if strings.EqualFold(key, canonical) {
				return accessJWTClaims{}, false
			}
		}
	}
	knownBytes, err := json.Marshal(recognized)
	if err != nil {
		return accessJWTClaims{}, false
	}
	var claims accessJWTClaims
	if decodeStrictJSON(knownBytes, &claims) != nil {
		return accessJWTClaims{}, false
	}
	return claims, true
}

func (verifier *AccessVerifier) validClaims(claims accessJWTClaims) bool {
	now := verifier.clock().Unix()
	if len(claims.Audience) != 1 || claims.Audience[0] != verifier.audience ||
		claims.Issuer != verifier.issuer || claims.Type != "app" ||
		claims.ExpiresAt <= now || claims.NotBefore <= 0 || claims.NotBefore > now ||
		claims.IssuedAt <= 0 || claims.IssuedAt > now ||
		claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt <= claims.NotBefore ||
		!validAccessEmail(claims.Email) || !validAccessOpaque(claims.Subject, 256) ||
		!validAccessOpaque(claims.IdentityNonce, 512) || !validAccessCountry(claims.Country) {
		return false
	}
	_, allowed := verifier.allowed[claims.Email]
	return allowed
}

func exactAccessAssertion(headers http.Header) (string, bool) {
	var assertion string
	count := 0
	for name, values := range headers {
		if !strings.EqualFold(name, AccessAssertionHeader) {
			continue
		}
		count += len(values)
		if len(values) == 1 {
			assertion = values[0]
		}
	}
	return assertion, count == 1 && assertion != "" && strings.TrimSpace(assertion) == assertion
}

func decodeCanonicalAccessSegment(encoded string, limit int) ([]byte, bool) {
	if encoded == "" || len(encoded) > base64.RawURLEncoding.EncodedLen(limit) {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > limit ||
		base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, false
	}
	return decoded, true
}

func (verifier *AccessVerifier) keyFor(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	for {
		now := verifier.clock()
		verifier.mu.Lock()
		fresh := now.Before(verifier.expiresAt)
		if fresh {
			if key := verifier.keys[keyID]; key != nil {
				verifier.mu.Unlock()
				return key, nil
			}
			if until, rejected := verifier.negativeKIDs[keyID]; rejected && now.Before(until) {
				verifier.mu.Unlock()
				return nil, ErrAccessUnauthorized
			}
			cooldown := accessUnknownKIDCooldown(verifier.cacheTTL)
			if !verifier.lastMissRefresh.IsZero() && now.Before(verifier.lastMissRefresh.Add(cooldown)) {
				verifier.mu.Unlock()
				return nil, ErrAccessUnauthorized
			}
		}
		// A fast upstream 5xx or malformed JWK set must not turn every new
		// request wave into another upstream fetch. Known keys in a still-fresh
		// cache were returned above; every lookup that actually needs a refresh
		// fails closed during this short, bounded outage cooldown.
		if now.Before(verifier.refreshFailedTo) {
			verifier.mu.Unlock()
			return nil, ErrAccessUnavailable
		}
		if active := verifier.refreshing; active != nil {
			verifier.mu.Unlock()
			select {
			case <-active.done:
				if active.err != nil {
					return nil, ErrAccessUnavailable
				}
				continue
			case <-ctx.Done():
				return nil, ErrAccessUnavailable
			}
		}

		active := &accessKeyRefresh{done: make(chan struct{})}
		verifier.refreshing = active
		verifier.mu.Unlock()

		keys, loadErr := verifier.fetchKeys()
		completedAt := verifier.clock()
		verifier.mu.Lock()
		if loadErr == nil {
			verifier.keys = keys
			verifier.expiresAt = completedAt.Add(verifier.cacheTTL)
			verifier.refreshFailedTo = time.Time{}
			for rejectedID, until := range verifier.negativeKIDs {
				if !completedAt.Before(until) || keys[rejectedID] != nil {
					delete(verifier.negativeKIDs, rejectedID)
				}
			}
			if keys[keyID] == nil {
				if len(verifier.negativeKIDs) >= maxAccessNegativeKIDs {
					verifier.negativeKIDs = make(map[string]time.Time)
				}
				verifier.negativeKIDs[keyID] = completedAt.Add(accessUnknownKIDCooldown(verifier.cacheTTL))
				verifier.lastMissRefresh = completedAt
			}
		} else {
			verifier.refreshFailedTo = completedAt.Add(accessRefreshFailureCooldown(verifier.cacheTTL))
		}
		active.err = loadErr
		verifier.refreshing = nil
		close(active.done)
		key := verifier.keys[keyID]
		verifier.mu.Unlock()

		if loadErr != nil {
			return nil, ErrAccessUnavailable
		}
		if key == nil {
			return nil, ErrAccessUnauthorized
		}
		return key, nil
	}
}

func (verifier *AccessVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), accessHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.jwksURL, nil)
	if err != nil {
		return nil, ErrAccessUnavailable
	}
	request.Header.Set("Accept", "application/json")
	response, err := verifier.client.Do(request)
	if err != nil {
		return nil, ErrAccessUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrAccessUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAccessJWKSBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxAccessJWKSBytes {
		return nil, ErrAccessUnavailable
	}
	keys, err := parseAccessJWKSet(body)
	if err != nil {
		return nil, ErrAccessUnavailable
	}
	return keys, nil
}

func parseAccessJWKSet(data []byte) (map[string]*rsa.PublicKey, error) {
	if validateNoDuplicateJSONKeys(data, 6) != nil || validateExactObjectKeys(data, accessJWKSetKeys) != nil {
		return nil, ErrAccessUnavailable
	}
	var wire accessJWKSetWire
	if decodeStrictJSON(data, &wire) != nil || len(wire.Keys) == 0 || len(wire.Keys) > maxAccessJWKCount {
		return nil, ErrAccessUnavailable
	}
	if len(wire.PublicCert) != 0 && !validAccessPEMCert(wire.PublicCert) {
		return nil, ErrAccessUnavailable
	}
	if len(wire.PublicCerts) != 0 {
		var certificates []json.RawMessage
		if decodeStrictJSON(wire.PublicCerts, &certificates) != nil || certificates == nil {
			return nil, ErrAccessUnavailable
		}
		for _, raw := range certificates {
			if !validAccessPEMCert(raw) {
				return nil, ErrAccessUnavailable
			}
		}
	}

	keys := make(map[string]*rsa.PublicKey, len(wire.Keys))
	for _, raw := range wire.Keys {
		if validateExactObjectKeys(raw, accessJWKKeys) != nil {
			return nil, ErrAccessUnavailable
		}
		var jwk accessJWKWire
		if decodeStrictJSON(raw, &jwk) != nil || jwk.KeyType != "RSA" ||
			jwk.Algorithm != "RS256" || jwk.Use != "sig" || !validAccessKeyID(jwk.KeyID) {
			return nil, ErrAccessUnavailable
		}
		if _, duplicate := keys[jwk.KeyID]; duplicate {
			return nil, ErrAccessUnavailable
		}
		key, ok := accessRSAKey(jwk)
		if !ok {
			return nil, ErrAccessUnavailable
		}
		keys[jwk.KeyID] = key
	}
	return keys, nil
}

func validAccessPEMCert(data []byte) bool {
	if validateExactObjectKeys(data, accessPEMCertKeys) != nil {
		return false
	}
	var certificate accessPEMCertWire
	return decodeStrictJSON(data, &certificate) == nil && validAccessKeyID(certificate.KeyID) &&
		len(certificate.Certificate) > 0 && len(certificate.Certificate) <= 32<<10 &&
		utf8.ValidString(certificate.Certificate)
}

func accessRSAKey(jwk accessJWKWire) (*rsa.PublicKey, bool) {
	modulusBytes, ok := decodeCanonicalAccessSegment(jwk.Modulus, 512)
	if !ok || modulusBytes[0] == 0 {
		return nil, false
	}
	modulus := new(big.Int).SetBytes(modulusBytes)
	if modulus.BitLen() < 2048 || modulus.BitLen() > 4096 || modulus.Bit(0) == 0 {
		return nil, false
	}
	exponentBytes, ok := decodeCanonicalAccessSegment(jwk.Exponent, 4)
	if !ok || exponentBytes[0] == 0 {
		return nil, false
	}
	exponent := 0
	for _, value := range exponentBytes {
		exponent = exponent<<8 | int(value)
	}
	if exponent != 65537 {
		return nil, false
	}
	return &rsa.PublicKey{N: modulus, E: exponent}, true
}

func accessUnknownKIDCooldown(cacheTTL time.Duration) time.Duration {
	if cacheTTL < 5*time.Second {
		return cacheTTL
	}
	return 5 * time.Second
}

func accessRefreshFailureCooldown(cacheTTL time.Duration) time.Duration {
	// CacheTTL is configuration-bounded to [1s, 24h]. Reusing that lower
	// bound keeps deterministic short-TTL tests meaningful while capping a
	// real outage cooldown at five seconds.
	if cacheTTL < 5*time.Second {
		return cacheTTL
	}
	return 5 * time.Second
}

func validAccessAudience(value string) bool {
	return len(value) > 0 && len(value) <= 256 && accessAudiencePattern.MatchString(value)
}

func validAccessKeyID(value string) bool {
	return len(value) > 0 && len(value) <= 256 && accessOpaquePattern.MatchString(value)
}

func validAccessEmail(value string) bool {
	if len(value) < 3 || len(value) > 254 || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\t ") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func validAccessOpaque(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validAccessCountry(value string) bool {
	if value == "" {
		return true
	}
	return len(value) == 2 && value[0] >= 'A' && value[0] <= 'Z' && value[1] >= 'A' && value[1] <= 'Z'
}

type redactedAccessConfig struct {
	TeamDomainCount   int           `json:"team_domain_count"`
	AudienceCount     int           `json:"audience_count"`
	AllowedEmailCount int           `json:"allowed_email_count"`
	CacheTTL          time.Duration `json:"cache_ttl"`
}

func (config AccessConfig) redacted() redactedAccessConfig {
	teamCount, audienceCount := 0, 0
	if config.TeamDomain != "" {
		teamCount = 1
	}
	if config.Audience != "" {
		audienceCount = 1
	}
	return redactedAccessConfig{
		TeamDomainCount: teamCount, AudienceCount: audienceCount,
		AllowedEmailCount: len(config.AllowedEmails), CacheTTL: config.CacheTTL,
	}
}

func (config AccessConfig) MarshalJSON() ([]byte, error) { return json.Marshal(config.redacted()) }
func (config AccessConfig) String() string {
	data, err := json.Marshal(config.redacted())
	if err != nil {
		return "publicedge.AccessConfig{redacted}"
	}
	return string(data)
}
func (config AccessConfig) GoString() string { return config.String() }

type redactedAccessIdentity struct {
	Email   string `json:"email"`
	Subject string `json:"subject"`
}

func (identity AccessIdentity) redacted() redactedAccessIdentity {
	return redactedAccessIdentity{Email: redactIfSet(identity.Email), Subject: redactIfSet(identity.Subject)}
}

func (identity AccessIdentity) MarshalJSON() ([]byte, error) {
	return json.Marshal(identity.redacted())
}
func (identity AccessIdentity) String() string {
	data, err := json.Marshal(identity.redacted())
	if err != nil {
		return "publicedge.AccessIdentity{redacted}"
	}
	return string(data)
}
func (identity AccessIdentity) GoString() string { return identity.String() }

func (verifier *AccessVerifier) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Configured bool `json:"configured"`
	}{Configured: verifier != nil})
}
func (verifier *AccessVerifier) String() string   { return "publicedge.AccessVerifier{redacted}" }
func (verifier *AccessVerifier) GoString() string { return verifier.String() }
