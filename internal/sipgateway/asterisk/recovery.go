package asterisk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/iniwex5/vohive/internal/sipgateway"
)

const recoveryTokenVersion = 1

// RecoveryInspector is an HTTP-only Asterisk recovery client. It never opens
// an ARI event WebSocket and has no mutation methods.
type RecoveryInspector struct {
	mu         sync.Mutex
	cfg        normalizedConfig
	http       *ariHTTPClient
	pbx        pbxIdentity
	runCtx     context.Context
	cancelRun  context.CancelFunc
	operations sync.WaitGroup
	closeDone  chan struct{}
	closed     bool
}

type recoveryIdentity struct {
	GatewayID       string `json:"gateway_id"`
	PBXEntityID     string `json:"pbx_entity_id"`
	PBXVersion      string `json:"pbx_version"`
	PBXStartupTime  string `json:"pbx_startup_time"`
	PolicyID        string `json:"policy_id"`
	ChannelID       string `json:"channel_id"`
	ChannelName     string `json:"channel_name"`
	Context         string `json:"context"`
	Extension       string `json:"extension"`
	Priority        int64  `json:"priority"`
	Application     string `json:"application"`
	ApplicationData string `json:"application_data"`
	CreationTime    string `json:"creation_time"`
	ProtocolDigest  string `json:"protocol_digest,omitempty"`
}

type recoveryEnvelope struct {
	Version  int              `json:"version"`
	Identity recoveryIdentity `json:"identity"`
}

type ariGETClient interface {
	do(context.Context, string, []string, url.Values) (httpResponse, error)
	inspectPBX(context.Context, string, string, string) (pbxIdentity, error)
}

// NewRecoveryInspector validates configuration and the exact PBX incarnation
// using GET /asterisk/info. It does not connect to the ARI event WebSocket.
func NewRecoveryInspector(ctx context.Context, cfg Config) (*RecoveryInspector, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	httpClient := newARIHTTPClient(normalized)
	pbx, err := httpClient.inspectPBX(
		ctx, normalized.expectedVersion, normalized.expectedEntityID, normalized.expectedStartupTime,
	)
	if err != nil {
		httpClient.close()
		zeroRecoverySecret(&normalized)
		if errors.Is(err, ErrProtocol) {
			return nil, errors.Join(sipgateway.ErrReconcileRequired, err)
		}
		return nil, err
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	return &RecoveryInspector{
		cfg: normalized, http: httpClient, pbx: pbx, runCtx: runCtx, cancelRun: cancelRun,
		closeDone: make(chan struct{}),
	}, nil
}

func (*RecoveryInspector) String() string   { return "asterisk.RecoveryInspector{redacted}" }
func (*RecoveryInspector) GoString() string { return "asterisk.RecoveryInspector{redacted}" }

func (a *Adapter) recoveryIdentityFor(channel ariChannel) (recoveryIdentity, error) {
	return recoveryIdentityFor(a.cfg, a.pbx, channel)
}

func recoveryIdentityFor(cfg normalizedConfig, pbx pbxIdentity, channel ariChannel) (recoveryIdentity, error) {
	if err := validateRecoveryChannelIdentity(cfg, channel); err != nil {
		return recoveryIdentity{}, ErrProtocol
	}
	contextName, extension, priority, appData, policyID := channel.Dialplan.Context,
		channel.Dialplan.Exten, channel.Dialplan.Priority, channel.Dialplan.AppData,
		channel.ChannelVars[incomingPolicyVariable]
	if strings.HasPrefix(channel.Name, "PJSIP/") && !strings.HasPrefix(channel.Name, "PJSIP/"+cfg.incomingEndpoint+"-") {
		if contextName == "" {
			contextName = "outgoing"
		}
		if extension == "" {
			extension = outgoingChannelStem(channel.Name)
		}
		if priority == 0 {
			priority = 1
		}
		if appData == "" {
			appData = cfg.application + ",outgoing"
		}
		if policyID == "" {
			policyID = cfg.incomingPolicyID
		}
	}
	if !validOpaque(channel.CreationTime, 1, 128) || !validAsteriskTime(channel.CreationTime) ||
		!validOpaque(contextName, 1, 64) || !validOpaque(extension, 1, 128) ||
		priority < 1 || priority > 1_000_000 || !validOpaque(appData, 1, 256) ||
		!validToken(policyID, 1, 64) {
		return recoveryIdentity{}, ErrProtocol
	}
	protocolID := ""
	if len(cfg.recoverySecret) != 0 {
		if !validOpaque(channel.ProtocolID, 1, 512) {
			return recoveryIdentity{}, ErrProtocol
		}
		protocolID = protocolDigest(cfg.recoverySecret, channel.ProtocolID)
	}
	return recoveryIdentity{
		GatewayID: cfg.gatewayID, PBXEntityID: pbx.EntityID, PBXVersion: pbx.Version,
		PBXStartupTime: pbx.StartupTime, PolicyID: policyID,
		ChannelID: channel.ID, ChannelName: channel.Name,
		Context: contextName, Extension: extension,
		Priority: priority, Application: channel.Dialplan.AppName,
		ApplicationData: appData, CreationTime: channel.CreationTime,
		ProtocolDigest: protocolID,
	}, nil
}

func outgoingChannelStem(name string) string {
	if !strings.HasPrefix(name, "PJSIP/") {
		return ""
	}
	rest := strings.TrimPrefix(name, "PJSIP/")
	separator := strings.LastIndexByte(rest, '-')
	if separator <= 0 {
		return ""
	}
	return rest[:separator]
}

func validateRecoveryChannelIdentity(cfg normalizedConfig, channel ariChannel) error {
	if validateIncomingChannelIdentity(cfg, channel) == nil {
		return nil
	}
	if cfg.outgoingEndpoint == "" || !outgoingChannelShapeMatches(cfg, channel.Name) {
		return ErrProtocol
	}
	if channel.Dialplan.AppName != "Stasis" ||
		(channel.Dialplan.AppData != "" && channel.Dialplan.AppData != cfg.application+",outgoing") ||
		(channel.ChannelVars[incomingPolicyVariable] != "" && channel.ChannelVars[incomingPolicyVariable] != cfg.incomingPolicyID) {
		return ErrProtocol
	}
	return nil
}

// RecoveryToken serializes a bounded strict identity token captured from a
// locally verified StasisStart event. The outer recovery store is responsible
// for token integrity and private file access. This method performs no request.
func (a *Adapter) RecoveryToken(ref sipgateway.CallRef) (sipgateway.RecoveryToken, error) {
	if err := a.validateRef(ref); err != nil {
		return sipgateway.RecoveryToken{}, err
	}
	a.mu.Lock()
	current, callExists := a.calls[ref.ProviderHandle]
	identity, identityExists := a.recoveryIDs[ref.ProviderHandle]
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return sipgateway.RecoveryToken{}, sipgateway.ErrClosed
	}
	if !callExists || !current.Ref.SameDialog(ref) || !identityExists {
		return sipgateway.RecoveryToken{}, sipgateway.ErrReconcileRequired
	}
	payload, err := json.Marshal(recoveryEnvelope{Version: recoveryTokenVersion, Identity: identity})
	if err != nil || len(payload) == 0 || len(payload) > sipgateway.MaxRecoveryTokenBytes {
		return sipgateway.RecoveryToken{}, sipgateway.ErrReconcileRequired
	}
	var token sipgateway.RecoveryToken
	if err := token.UnmarshalBinary(payload); err != nil {
		return sipgateway.RecoveryToken{}, sipgateway.ErrReconcileRequired
	}
	return token, nil
}

// InspectRecovery performs exactly one GET for the token's channel ID,
// bracketed by two read-only exact-incarnation info GETs on this Adapter's
// already bound event WebSocket. It never adds the recovered channel to calls
// and never performs POST or DELETE.
func (a *Adapter) InspectRecovery(
	ctx context.Context,
	token sipgateway.RecoveryToken,
) (sipgateway.RecoverySnapshot, error) {
	ctx, releaseContext, err := a.operationContext(ctx)
	if err != nil {
		return sipgateway.RecoverySnapshot{}, err
	}
	defer releaseContext()
	if err := a.acquireOperation(ctx); err != nil {
		return sipgateway.RecoverySnapshot{}, err
	}
	defer a.releaseOperation()
	return inspectRecovery(ctx, a.cfg, a.pbx, a.ari, token)
}

// InspectRecovery performs one HTTP-only, incarnation-bracketed exact-channel
// inspection. Its three independent HTTP GETs are deliberately not described
// as atomic; deployment must still prohibit request-level A -> B -> A routing.
func (i *RecoveryInspector) InspectRecovery(
	ctx context.Context,
	token sipgateway.RecoveryToken,
) (sipgateway.RecoverySnapshot, error) {
	if err := contextErr(ctx); err != nil {
		return sipgateway.RecoverySnapshot{}, err
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrClosed
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(i.runCtx, cancel)
	i.operations.Add(1)
	cfg, pbx, httpClient := i.cfg, i.pbx, i.http
	i.mu.Unlock()
	defer func() {
		stop()
		cancel()
		i.operations.Done()
	}()
	return inspectRecovery(operation, cfg, pbx, httpClient, token)
}

// Close releases HTTP resources and is idempotent.
func (i *RecoveryInspector) Close() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	if i.closed {
		done := i.closeDone
		i.mu.Unlock()
		<-done
		return nil
	}
	i.closed = true
	i.cancelRun()
	i.mu.Unlock()
	i.operations.Wait()
	i.http.close()
	i.mu.Lock()
	zeroRecoverySecret(&i.cfg)
	close(i.closeDone)
	i.mu.Unlock()
	return nil
}

func inspectRecovery(
	ctx context.Context,
	cfg normalizedConfig,
	pbx pbxIdentity,
	client ariGETClient,
	token sipgateway.RecoveryToken,
) (sipgateway.RecoverySnapshot, error) {
	identity, err := openRecoveryToken(token)
	if err != nil || identity.GatewayID != cfg.gatewayID ||
		identity.PBXEntityID != pbx.EntityID || identity.PBXVersion != pbx.Version ||
		identity.PBXStartupTime != pbx.StartupTime || identity.PolicyID != cfg.incomingPolicyID ||
		(len(identity.ProtocolDigest) == 0) != (len(cfg.recoverySecret) == 0) {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	// The standalone inspector may remain open while Asterisk restarts or a
	// local proxy switches backends, so it checks the exact captured PBX on both
	// sides of the channel GET. Those independent HTTP reads are not atomic and
	// deployment must still prohibit A -> B -> A routing. The live Adapter calls
	// this same GET-only sequence over its one incarnation-bound event socket.
	if _, err := client.inspectPBX(
		ctx, pbx.Version, pbx.EntityID, pbx.StartupTime,
	); err != nil {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	response, err := client.do(ctx, http.MethodGet, []string{"channels", identity.ChannelID}, nil)
	if err != nil {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	if _, err := client.inspectPBX(
		ctx, pbx.Version, pbx.EntityID, pbx.StartupTime,
	); err != nil {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	if response.status == http.StatusNotFound {
		return sipgateway.RecoverySnapshot{State: sipgateway.ProviderCallEnded}, nil
	}
	if response.status != http.StatusOK {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	var channel ariChannel
	if err := decodeOneJSON(response.body, &channel); err != nil {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	observed, err := recoveryIdentityFor(cfg, pbx, channel)
	if err != nil || !sameRecoveryIdentity(identity, observed) {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	state, err := providerStateFromARI(channel.State)
	if err != nil {
		return sipgateway.RecoverySnapshot{}, sipgateway.ErrReconcileRequired
	}
	return sipgateway.RecoverySnapshot{State: state}, nil
}

func openRecoveryToken(token sipgateway.RecoveryToken) (recoveryIdentity, error) {
	payload, err := token.MarshalBinary()
	if err != nil || len(payload) == 0 || len(payload) > sipgateway.MaxRecoveryTokenBytes {
		return recoveryIdentity{}, sipgateway.ErrReconcileRequired
	}
	var envelope recoveryEnvelope
	if err := decodeExactJSON(payload, &envelope); err != nil || envelope.Version != recoveryTokenVersion ||
		validateRecoveryIdentity(envelope.Identity) != nil {
		return recoveryIdentity{}, sipgateway.ErrReconcileRequired
	}
	return envelope.Identity, nil
}

func validateRecoveryIdentity(identity recoveryIdentity) error {
	if !validToken(identity.GatewayID, 1, 64) || !validOpaque(identity.PBXEntityID, 1, 256) ||
		!validOpaque(identity.PBXVersion, 1, 128) || !validAsteriskTime(identity.PBXStartupTime) ||
		!validToken(identity.PolicyID, 1, 64) || !validOpaque(identity.ChannelID, 1, 256) ||
		!validOpaque(identity.ChannelName, 1, 512) || !validOpaque(identity.Context, 1, 64) ||
		!validOpaque(identity.Extension, 1, 128) || identity.Priority < 1 || identity.Priority > 1_000_000 ||
		!validOpaque(identity.Application, 1, 64) || !validOpaque(identity.ApplicationData, 1, 256) ||
		!validAsteriskTime(identity.CreationTime) ||
		(identity.ProtocolDigest != "" && !validOpaque(identity.ProtocolDigest, 43, 43)) {
		return ErrProtocol
	}
	return nil
}

func sameRecoveryIdentity(want, got recoveryIdentity) bool {
	if want.GatewayID != got.GatewayID || want.PBXEntityID != got.PBXEntityID ||
		want.PBXVersion != got.PBXVersion || want.PBXStartupTime != got.PBXStartupTime ||
		want.PolicyID != got.PolicyID || want.ChannelID != got.ChannelID ||
		want.ChannelName != got.ChannelName || want.Context != got.Context ||
		want.Extension != got.Extension || want.Priority != got.Priority ||
		want.Application != got.Application || want.ApplicationData != got.ApplicationData ||
		want.CreationTime != got.CreationTime {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want.ProtocolDigest), []byte(got.ProtocolDigest)) == 1
}

func protocolDigest(secret []byte, protocolID string) string {
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write([]byte("asterisk-recovery-protocol-id-v1\x00"))
	_, _ = digest.Write([]byte(protocolID))
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func decodeOneJSON(data []byte, target any) error {
	return decodeJSON(data, target, false)
}

func decodeExactJSON(data []byte, target any) error {
	return decodeJSON(data, target, true)
}

func decodeJSON(data []byte, target any, disallowUnknown bool) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ErrProtocol
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkStrictJSONValue(decoder, 0); err != nil {
		return ErrProtocol
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrProtocol
	}
	return nil
}

func walkStrictJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrProtocol
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrProtocol
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrProtocol
			}
			seen[key] = struct{}{}
			if err := walkStrictJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrProtocol
		}
		return nil
	case '[':
		for decoder.More() {
			if err := walkStrictJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrProtocol
		}
		return nil
	default:
		return ErrProtocol
	}
}
