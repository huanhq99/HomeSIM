package asterisk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iniwex5/vohive/internal/sipgateway"
	"golang.org/x/net/websocket"
)

const (
	mediaEndpoint          = "WebSocket/INCOMING/c(ulaw)f(json)"
	incomingPolicyVariable = "DJONEHUB_POLICY_ID"
)

var processBootCounter atomic.Uint64

// Adapter is one connected Asterisk ARI process incarnation.
type Adapter struct {
	idMu   sync.Mutex
	mu     sync.Mutex
	opGate chan struct{}

	cfg         normalizedConfig
	ari         *ariWSClient
	mediaDialer *ariHTTPClient
	bootEpoch   string
	pbx         pbxIdentity
	events      chan sipgateway.Event
	done        chan struct{}
	runCtx      context.Context
	cancelRun   context.CancelFunc
	readDone    chan struct{}
	closeOnce   sync.Once
	closed      bool
	terminalErr error
	// cleanStartupCutoff is a PBX-clock timestamp taken from the second ARI
	// UserEvent. A channel created at or before it belongs to the pre-start
	// uncertainty set even if its StasisStart is delivered later.
	cleanStartupCutoff time.Time

	calls       map[string]sipgateway.CallSnapshot
	recoveryIDs map[string]recoveryIdentity
	media       map[string]*mediaSession
	mediaByCall map[string]string
	pending     map[string]*pendingMedia
	pendingDial map[string]*pendingDial
	commands    map[string]commandRecord
	stateSignal chan struct{}
	// beforeMediaRegister is a package-private deterministic race-test seam.
	// Production leaves it nil.
	afterMediaPrepared  func(*mediaSession)
	beforeMediaRegister func(*mediaSession)
	cleanupWG           sync.WaitGroup
}

type pendingMedia struct {
	connection chan dialConnection
}

type pendingDial struct {
	number string
	result chan sipgateway.CallSnapshot
	err    chan error
}

type dialConnection struct {
	id  string
	err error
}

type commandKind uint8

const (
	answerCommand commandKind = iota + 1
	endCommand
	rejectCommand
	dtmfCommand
)

type commandFingerprint struct {
	kind    commandKind
	call    sipgateway.CallRef
	leaseID string
	digits  string
}

type commandRecord struct {
	fingerprint commandFingerprint
	result      sipgateway.CommandResult
}

type ariChannel struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	State        string            `json:"state"`
	CreationTime string            `json:"creationtime"`
	ProtocolID   string            `json:"protocol_id"`
	ChannelVars  map[string]string `json:"channelvars"`
	Dialplan     struct {
		Context  string `json:"context"`
		Exten    string `json:"exten"`
		Priority int64  `json:"priority"`
		AppName  string `json:"app_name"`
		AppData  string `json:"app_data"`
	} `json:"dialplan"`
}

type ariEvent struct {
	Type        string      `json:"type"`
	Application string      `json:"application"`
	AsteriskID  string      `json:"asterisk_id"`
	Timestamp   string      `json:"timestamp"`
	EventName   string      `json:"eventname"`
	Args        []string    `json:"args"`
	DialStatus  string      `json:"dialstatus"`
	Channel     *ariChannel `json:"channel"`
	Caller      *ariChannel `json:"caller"`
	Peer        *ariChannel `json:"peer"`
}

type ariBridge struct {
	ID       string   `json:"id"`
	Channels []string `json:"channels"`
}

type framedMessage struct {
	payloadType byte
	data        []byte
}

var receiveFrame = websocket.Codec{
	Unmarshal: func(data []byte, payloadType byte, target any) error {
		message, ok := target.(*framedMessage)
		if !ok {
			return websocket.ErrNotSupported
		}
		message.payloadType = payloadType
		message.data = append(message.data[:0], data...)
		return nil
	},
}

// New connects to the ARI event WebSocket, allocates a never-reused random
// boot epoch, and queues the gateway_restart event before processing calls.
func New(ctx context.Context, cfg Config) (*Adapter, error) {
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
		return nil, err
	}
	eventWS, err := httpClient.dialEvents(ctx, normalized.application)
	if err != nil {
		httpClient.close()
		zeroRecoverySecret(&normalized)
		return nil, err
	}
	ari, err := newARIWSClient(eventWS, normalized, pbx.EntityID)
	if err != nil {
		_ = eventWS.Close()
		httpClient.close()
		zeroRecoverySecret(&normalized)
		return nil, err
	}
	// The initial HTTP GET only supplies a candidate identity before upgrade.
	// Re-read exact version/entity/startup through the event socket itself; all
	// subsequent live REST work remains on this socket with no HTTP fallback.
	if _, err = ari.inspectPBX(ctx, pbx.Version, pbx.EntityID, pbx.StartupTime); err != nil {
		ari.close()
		httpClient.close()
		zeroRecoverySecret(&normalized)
		return nil, cleanStartupFailure(err)
	}
	var cleanStartupCutoff time.Time
	if normalized.requireCleanStartup {
		cleanStartupCutoff, err = proveCleanStartup(ctx, normalized, pbx, ari)
		if err != nil {
			ari.close()
			httpClient.close()
			zeroRecoverySecret(&normalized)
			return nil, err
		}
	}
	adapter := &Adapter{
		cfg: normalized, ari: ari, mediaDialer: httpClient, pbx: pbx,
		cleanStartupCutoff: cleanStartupCutoff,
		events:             make(chan sipgateway.Event, normalized.eventCapacity), done: make(chan struct{}),
		readDone: make(chan struct{}),
		calls:    make(map[string]sipgateway.CallSnapshot), recoveryIDs: make(map[string]recoveryIdentity),
		media:       make(map[string]*mediaSession),
		mediaByCall: make(map[string]string), pending: make(map[string]*pendingMedia),
		pendingDial: make(map[string]*pendingDial),
		commands:    make(map[string]commandRecord), stateSignal: make(chan struct{}),
		opGate: make(chan struct{}, 1),
	}
	adapter.opGate <- struct{}{}
	adapter.runCtx, adapter.cancelRun = context.WithCancel(context.Background())
	adapter.bootEpoch, err = adapter.randomID("boot_", 24)
	if err != nil {
		adapter.cancelRun()
		ari.close()
		httpClient.close()
		zeroRecoverySecret(&adapter.cfg)
		return nil, err
	}
	adapter.bootEpoch += "_" + strconv.FormatUint(processBootCounter.Add(1), 36)
	restart := sipgateway.Event{
		Kind: sipgateway.EventGatewayRestart, GatewayID: normalized.gatewayID,
		BootEpoch: adapter.bootEpoch, ObservedAt: normalized.now(),
	}
	adapter.events <- restart
	ari.setFailureHandler(adapter.stopWithError)
	go adapter.readEvents()
	return adapter, nil
}

func (a *Adapter) String() string   { return "asterisk.Adapter{redacted}" }
func (a *Adapter) GoString() string { return "asterisk.Adapter{redacted}" }

// Capabilities reports the deliberately narrow incoming PCMU slice.
func (a *Adapter) Capabilities() sipgateway.Capabilities {
	return sipgateway.Capabilities{
		Incoming: true, Dial: a.cfg.outgoingEndpoint != "", PrepareMedia: true, AnswerIncoming: true, EndActive: true,
		Codecs: []sipgateway.Codec{sipgateway.CodecPCMU},
	}
}

// Observe returns one bounded, validated adapter event.
func (a *Adapter) Observe(ctx context.Context) (sipgateway.Event, error) {
	if err := contextErr(ctx); err != nil {
		return sipgateway.Event{}, err
	}
	select {
	case <-a.done:
		return sipgateway.Event{}, a.terminalError()
	default:
	}
	select {
	case <-ctx.Done():
		return sipgateway.Event{}, ctx.Err()
	case event := <-a.events:
		a.mu.Lock()
		closed := a.closed
		a.mu.Unlock()
		if closed {
			return sipgateway.Event{}, a.terminalError()
		}
		return cloneEvent(event), nil
	case <-a.done:
		return sipgateway.Event{}, a.terminalError()
	}
}

func (a *Adapter) readEvents() {
	defer func() {
		// stop waits for all gated operations before readEvents returns. Wipe the
		// adapter-owned persistent-secret copy before signaling Close completion.
		zeroRecoverySecret(&a.cfg)
		close(a.readDone)
	}()
	for {
		event, err := a.ari.nextEvent(a.runCtx)
		if err != nil {
			a.stopWithError(err)
			return
		}
		if err := a.applyARIEvent(event); err != nil {
			a.stopWithError(err)
			return
		}
	}
}

func (a *Adapter) applyARIEvent(event ariEvent) error {
	if !validOpaque(event.AsteriskID, 1, 256) || event.AsteriskID != a.pbx.EntityID {
		return sipgateway.ErrRecoveryRequired
	}
	if _, ok := parseAsteriskTime(event.Timestamp); !ok {
		return ErrProtocol
	}
	if event.Application != a.cfg.application {
		if event.Type == "ApplicationReplaced" || event.Type == "ApplicationUnregistered" {
			return cleanStartupFailure(ErrProtocol)
		}
		return ErrProtocol
	}
	switch event.Type {
	case "ApplicationReplaced", "ApplicationUnregistered":
		return sipgateway.ErrRecoveryRequired
	case "StasisStart":
		if event.Channel == nil || event.Channel.ID == "" {
			return ErrProtocol
		}
		if len(event.Args) == 1 && event.Args[0] == a.cfg.incomingArgument {
			return a.startIncoming(*event.Channel)
		}
		if a.cfg.outgoingEndpoint != "" && len(event.Args) == 1 && event.Args[0] == "outgoing" {
			return a.startOutgoing(*event.Channel)
		}
		if slices.Contains(event.Args, a.cfg.incomingArgument) {
			return ErrProtocol
		}
	case "ChannelStateChange":
		if event.Channel == nil || event.Channel.ID == "" {
			return ErrProtocol
		}
		if !a.hasCall(event.Channel.ID) {
			return nil
		}
		call, err := a.currentCallForHandle(event.Channel.ID)
		if err != nil {
			return err
		}
		if err := a.validateObservedCallIdentity(call.Ref, *event.Channel); err != nil {
			return err
		}
		state, err := providerStateFromARI(event.Channel.State)
		if err != nil {
			return err
		}
		if state == sipgateway.ProviderCallActive {
			return a.transitionCall(event.Channel.ID, state, true)
		}
		// The one WebSocket reader demultiplexes responses directly to request
		// waiters while events pass through a bounded queue. An earlier Ring event
		// can therefore reach this consumer after Inspect already proved Up. Treat
		// that valid but older event as stale; a later REST inspection still rejects
		// an actual active -> ringing provider rollback.
		return nil
	case "StasisEnd", "ChannelDestroyed":
		if event.Channel == nil || event.Channel.ID == "" {
			return ErrProtocol
		}
		if !a.hasCall(event.Channel.ID) {
			return nil
		}
		call, err := a.currentCallForHandle(event.Channel.ID)
		if err != nil {
			return err
		}
		if err := a.validateObservedCallIdentity(call.Ref, *event.Channel); err != nil {
			return err
		}
		return a.transitionCall(event.Channel.ID, sipgateway.ProviderCallEnded, true)
	case "Dial":
		return a.applyDialEvent(event)
	}
	return nil
}

func (a *Adapter) startIncoming(channel ariChannel) error {
	if a.cfg.requireCleanStartup {
		createdAt, ok := parseAsteriskTime(channel.CreationTime)
		if !ok || a.cleanStartupCutoff.IsZero() || !createdAt.After(a.cleanStartupCutoff) {
			return sipgateway.ErrRecoveryRequired
		}
	}
	if err := a.validateIncomingChannel(channel); err != nil {
		return err
	}
	ref := sipgateway.CallRef{
		GatewayID: a.cfg.gatewayID, BootEpoch: a.bootEpoch,
		ProviderHandle: channel.ID, Revision: 1,
	}
	if ref.Validate() != nil {
		return ErrProtocol
	}
	snapshot := sipgateway.CallSnapshot{Ref: ref, State: sipgateway.ProviderCallIncoming}
	recoveryID, recoveryErr := a.recoveryIdentityFor(channel)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return sipgateway.ErrClosed
	}
	if prior, exists := a.calls[channel.ID]; exists {
		knownRecoveryID, hasRecoveryID := a.recoveryIDs[channel.ID]
		a.mu.Unlock()
		if prior.State == sipgateway.ProviderCallEnded {
			return ErrProtocol
		}
		if recoveryErr == nil && hasRecoveryID && knownRecoveryID != recoveryID {
			return ErrProtocol
		}
		return nil
	}
	a.calls[channel.ID] = snapshot
	if recoveryErr == nil {
		a.recoveryIDs[channel.ID] = recoveryID
	}
	a.signalStateLocked()
	a.mu.Unlock()
	return a.enqueueCall(snapshot)
}

func (a *Adapter) startOutgoing(channel ariChannel) error {
	a.mu.Lock()
	pending := a.pendingDial[channel.ID]
	a.mu.Unlock()
	if pending == nil || a.cfg.outgoingEndpoint == "" {
		return nil
	}
	if err := validateOutgoingChannelIdentity(a.cfg, channel, pending.number); err != nil {
		select {
		case pending.err <- err:
		default:
		}
		return nil
	}
	ref := sipgateway.CallRef{GatewayID: a.cfg.gatewayID, BootEpoch: a.bootEpoch, ProviderHandle: channel.ID, Revision: 1}
	snapshot := sipgateway.CallSnapshot{Ref: ref, State: sipgateway.ProviderCallIncoming, Direction: sipgateway.CallDirectionOutgoing}
	if err := snapshot.Validate(); err != nil {
		select {
		case pending.err <- ErrProtocol:
		default:
		}
		return nil
	}
	recoveryID, recoveryErr := a.recoveryIdentityFor(channel)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return sipgateway.ErrClosed
	}
	if _, exists := a.calls[channel.ID]; exists {
		a.mu.Unlock()
		return nil
	}
	a.calls[channel.ID] = snapshot
	if recoveryErr == nil {
		a.recoveryIDs[channel.ID] = recoveryID
	}
	a.signalStateLocked()
	delete(a.pendingDial, channel.ID)
	a.mu.Unlock()
	if err := a.enqueueCall(snapshot); err != nil {
		select {
		case pending.err <- err:
		default:
		}
		return nil
	}
	select {
	case pending.result <- snapshot:
	default:
	}
	return nil
}

// Dial creates one outbound PJSIP dialog. The call is deliberately admitted
// only when an outgoing endpoint was explicitly configured; the subsequent
// StasisStart is still the authority that publishes the dialog identity.
func (a *Adapter) Dial(ctx context.Context, number string) (sipgateway.CallSnapshot, error) {
	if !validOutgoingDialNumber(number) || a.cfg.outgoingEndpoint == "" {
		return sipgateway.CallSnapshot{}, sipgateway.ErrInvalidCommand
	}
	ctx, releaseContext, err := a.operationContext(ctx)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	defer releaseContext()
	if err := a.acquireOperation(ctx); err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	defer a.releaseOperation()
	a.mu.Lock()
	for _, current := range a.calls {
		if current.State != sipgateway.ProviderCallEnded {
			a.mu.Unlock()
			return sipgateway.CallSnapshot{}, sipgateway.ErrConcurrentCall
		}
	}
	a.mu.Unlock()
	channelID, err := a.randomID("dj1o_", 18)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	pending := &pendingDial{number: number, result: make(chan sipgateway.CallSnapshot, 1), err: make(chan error, 1)}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return sipgateway.CallSnapshot{}, sipgateway.ErrClosed
	}
	a.pendingDial[channelID] = pending
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.pendingDial, channelID); a.mu.Unlock() }()
	query := url.Values{
		"endpoint": {"PJSIP/" + number + "@" + a.cfg.outgoingEndpoint},
		"app":      {a.cfg.application}, "appArgs": {"outgoing"}, "channelId": {channelID},
		"variables[" + incomingPolicyVariable + "]": {a.cfg.incomingPolicyID},
	}
	response, err := a.ari.do(ctx, http.MethodPost, []string{"channels", "create"}, query)
	if err != nil || !status2xx(response.status) {
		return sipgateway.CallSnapshot{}, ErrTransport
	}
	var created ariChannel
	if err := json.Unmarshal(response.body, &created); err != nil || created.ID != channelID || created.State != "Down" {
		return sipgateway.CallSnapshot{}, ErrProtocol
	}
	response, err = a.ari.do(ctx, http.MethodPost, []string{"channels", channelID, "dial"}, url.Values{"timeout": {"30"}})
	if err != nil || !status2xx(response.status) {
		a.scheduleCleanup("", channelID)
		return sipgateway.CallSnapshot{}, ErrTransport
	}
	select {
	case snapshot := <-pending.result:
		return snapshot, nil
	case err := <-pending.err:
		a.scheduleCleanup("", channelID)
		return sipgateway.CallSnapshot{}, err
	case <-ctx.Done():
		a.scheduleCleanup("", channelID)
		return sipgateway.CallSnapshot{}, ctx.Err()
	case <-a.done:
		return sipgateway.CallSnapshot{}, sipgateway.ErrClosed
	}
}

func (a *Adapter) transitionCall(handle string, state sipgateway.ProviderCallState, emit bool) error {
	var (
		changed sipgateway.CallSnapshot
		media   *mediaSession
	)
	a.mu.Lock()
	current, exists := a.calls[handle]
	if !exists || current.State == state {
		a.mu.Unlock()
		return nil
	}
	if !validTransition(current.State, state) {
		a.mu.Unlock()
		return ErrProtocol
	}
	current.Ref.Revision++
	current.State = state
	a.calls[handle] = current
	changed = current
	if state == sipgateway.ProviderCallEnded {
		if leaseID := a.mediaByCall[handle]; leaseID != "" {
			media = a.media[leaseID]
		}
	}
	a.signalStateLocked()
	a.mu.Unlock()
	if media != nil {
		media.closeLocal()
	}
	if emit {
		return a.enqueueCall(changed)
	}
	return nil
}

func (a *Adapter) applyDialEvent(event ariEvent) error {
	if event.Peer == nil || event.Peer.ID == "" || event.DialStatus != "" {
		return nil
	}
	a.mu.Lock()
	pending := a.pending[event.Peer.ID]
	a.mu.Unlock()
	if pending == nil {
		// The ARI application may receive Dial events for other channels. Only a
		// server-generated media channel currently owned by this adapter is part
		// of the media preparation protocol.
		return nil
	}
	if !strings.HasPrefix(event.Peer.Name, "WebSocket/") {
		return ErrProtocol
	}
	connectionID := event.Peer.ChannelVars["MEDIA_WEBSOCKET_CONNECTION_ID"]
	if !validOpaque(connectionID, 8, 128) {
		select {
		case pending.connection <- dialConnection{err: ErrProtocol}:
		default:
		}
		return nil
	}
	select {
	case pending.connection <- dialConnection{id: connectionID}:
	default:
	}
	return nil
}

func (a *Adapter) enqueueCall(snapshot sipgateway.CallSnapshot) error {
	event := sipgateway.Event{
		Kind: sipgateway.EventCallChanged, Call: cloneSnapshot(&snapshot), ObservedAt: a.cfg.now(),
	}
	select {
	case a.events <- event:
		return nil
	default:
		return sipgateway.ErrObservationQueueFull
	}
}

func (a *Adapter) hasCall(handle string) bool {
	a.mu.Lock()
	_, exists := a.calls[handle]
	a.mu.Unlock()
	return exists
}

func (a *Adapter) currentCallForHandle(handle string) (sipgateway.CallSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, exists := a.calls[handle]
	if !exists {
		return sipgateway.CallSnapshot{}, sipgateway.ErrCallNotFound
	}
	return current, nil
}

// Inspect performs only a read. It accepts an old revision so it can reconcile
// one ambiguous mutation, but it never accepts another boot epoch or dialog.
func (a *Adapter) Inspect(ctx context.Context, ref sipgateway.CallRef) (sipgateway.CallSnapshot, error) {
	ctx, releaseContext, err := a.operationContext(ctx)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	defer releaseContext()
	if err := a.validateRef(ref); err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	if err := a.acquireOperation(ctx); err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	defer a.releaseOperation()
	return a.inspectOnce(ctx, ref)
}

func (a *Adapter) inspectOnce(ctx context.Context, ref sipgateway.CallRef) (sipgateway.CallSnapshot, error) {
	current, err := a.currentDialog(ref)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	if current.State == sipgateway.ProviderCallEnded {
		return current, nil
	}
	response, err := a.ari.do(ctx, http.MethodGet, []string{"channels", ref.ProviderHandle}, nil)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	if response.status == http.StatusNotFound {
		if err := a.transitionCall(ref.ProviderHandle, sipgateway.ProviderCallEnded, false); err != nil {
			return sipgateway.CallSnapshot{}, err
		}
		return a.currentDialog(ref)
	}
	if response.status != http.StatusOK {
		return sipgateway.CallSnapshot{}, ErrTransport
	}
	var channel ariChannel
	if err := decodeOneJSON(response.body, &channel); err != nil || channel.ID != ref.ProviderHandle {
		return sipgateway.CallSnapshot{}, ErrProtocol
	}
	if err := a.validateObservedCallIdentity(ref, channel); err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	wanted, err := providerStateFromARI(channel.State)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	if current.State != wanted {
		if err := a.transitionCall(ref.ProviderHandle, wanted, false); err != nil {
			return sipgateway.CallSnapshot{}, err
		}
	}
	return a.currentDialog(ref)
}

// PrepareMedia creates and dials one inbound JSON chan_websocket channel,
// validates MEDIA_START, creates a mixing bridge, and adds both exact channels.
// It never answers the incoming channel.
func (a *Adapter) PrepareMedia(ctx context.Context, request sipgateway.PrepareMediaRequest) (sipgateway.MediaSession, error) {
	ctx, releaseContext, err := a.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseContext()
	ctx, cancel := withMaxTimeout(ctx, a.cfg.prepareTimeout)
	defer cancel()
	if err := request.Call.Validate(); err != nil {
		return nil, err
	}
	if !containsPCMUOnly(request.Codecs) {
		return nil, sipgateway.ErrCodecMismatch
	}
	if err := a.acquireOperation(ctx); err != nil {
		return nil, err
	}
	defer a.releaseOperation()
	current, err := a.requireExact(request.Call, sipgateway.ProviderCallIncoming)
	if err != nil {
		return nil, err
	}
	current, err = a.requireExactOnPBX(ctx, request.Call, sipgateway.ProviderCallIncoming)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	existingLease := a.mediaByCall[request.Call.ProviderHandle]
	a.mu.Unlock()
	if existingLease != "" {
		return nil, sipgateway.ErrMediaLeaseMismatch
	}
	mediaChannelID, err := a.randomID("dj1m_", 18)
	if err != nil {
		return nil, err
	}
	bridgeID, err := a.randomID("dj1b_", 18)
	if err != nil {
		return nil, err
	}
	leaseID, err := a.randomID("dj1l_", 24)
	if err != nil {
		return nil, err
	}
	mediaArgument := "media-" + mediaChannelID
	pending := &pendingMedia{connection: make(chan dialConnection, 1)}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, sipgateway.ErrClosed
	}
	a.pending[mediaChannelID] = pending
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, mediaChannelID)
		a.mu.Unlock()
	}()

	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			a.scheduleCleanup(bridgeID, mediaChannelID)
		}
	}()
	createQuery := url.Values{
		"endpoint": {mediaEndpoint}, "app": {a.cfg.application}, "appArgs": {mediaArgument},
		"channelId": {mediaChannelID}, "originator": {request.Call.ProviderHandle},
	}
	response, err := a.ari.do(ctx, http.MethodPost, []string{"channels", "create"}, createQuery)
	if err != nil || !status2xx(response.status) {
		return nil, sipgateway.ErrMediaNotPrepared
	}
	var created ariChannel
	if err := json.Unmarshal(response.body, &created); err != nil || created.ID != mediaChannelID ||
		created.State != "Down" || !strings.HasPrefix(created.Name, "WebSocket/") {
		return nil, ErrProtocol
	}
	dialQuery := url.Values{"caller": {request.Call.ProviderHandle}, "timeout": {"5"}}
	response, err = a.ari.do(ctx, http.MethodPost, []string{"channels", mediaChannelID, "dial"}, dialQuery)
	if err != nil || !status2xx(response.status) {
		return nil, sipgateway.ErrMediaNotPrepared
	}
	var dial dialConnection
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.done:
		return nil, sipgateway.ErrClosed
	case dial = <-pending.connection:
	}
	if dial.err != nil || dial.id == "" {
		return nil, ErrProtocol
	}
	mediaWS, err := a.mediaDialer.dialMedia(ctx, dial.id)
	if err != nil {
		return nil, err
	}
	session := newMediaSession(mediaSessionConfig{
		id: leaseID, call: current.Ref, channelID: mediaChannelID,
		connectionID: dial.id, ws: mediaWS, now: a.cfg.now,
		capacity: a.cfg.mediaQueueCapacity, ioTimeout: a.cfg.ioTimeout,
		cleanup: func() {
			a.scheduleCleanup(bridgeID, mediaChannelID)
			// Register cleanup before removing map ownership. stop() uses that
			// ownership as the barrier that prevents cleanupWG.Wait from racing a
			// late cleanup Add.
			a.dropMedia(leaseID, request.Call.ProviderHandle)
		},
	})
	if err := session.waitPrepared(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	if a.afterMediaPrepared != nil {
		a.afterMediaPrepared(session)
	}
	bridgeQuery := url.Values{"type": {"mixing"}}
	response, err = a.ari.do(ctx, http.MethodPost, []string{"bridges", bridgeID}, bridgeQuery)
	if err != nil || !status2xx(response.status) {
		_ = session.Close()
		return nil, sipgateway.ErrMediaNotPrepared
	}
	for _, channelID := range []string{request.Call.ProviderHandle, mediaChannelID} {
		response, err = a.ari.do(ctx, http.MethodPost, []string{"bridges", bridgeID, "addChannel"}, url.Values{"channel": {channelID}})
		if err != nil || !status2xx(response.status) {
			_ = session.Close()
			return nil, sipgateway.ErrMediaNotPrepared
		}
	}
	response, err = a.ari.do(ctx, http.MethodGet, []string{"bridges", bridgeID}, nil)
	if err != nil || response.status != http.StatusOK {
		_ = session.Close()
		return nil, sipgateway.ErrMediaNotPrepared
	}
	var bridge ariBridge
	if err := json.Unmarshal(response.body, &bridge); err != nil || bridge.ID != bridgeID || len(bridge.Channels) != 2 ||
		!slices.Contains(bridge.Channels, request.Call.ProviderHandle) || !slices.Contains(bridge.Channels, mediaChannelID) {
		_ = session.Close()
		return nil, ErrProtocol
	}
	if _, err := a.requireExactOnPBX(ctx, request.Call, sipgateway.ProviderCallIncoming); err != nil {
		_ = session.Close()
		return nil, err
	}
	if a.beforeMediaRegister != nil {
		a.beforeMediaRegister(session)
	}
	a.mu.Lock()
	call, callExists := a.calls[request.Call.ProviderHandle]
	if a.closed || a.mediaByCall[request.Call.ProviderHandle] != "" || !callExists ||
		call.Ref != request.Call || call.State != sipgateway.ProviderCallIncoming {
		a.mu.Unlock()
		_ = session.Close()
		return nil, sipgateway.ErrMediaLeaseMismatch
	}
	a.media[leaseID] = session
	a.mediaByCall[request.Call.ProviderHandle] = leaseID
	a.mu.Unlock()
	// MEDIA_START can be followed by a peer close while bridge construction is
	// still in flight. The session cleanup callback cannot remove an entry that
	// has not yet been registered, so publish first and then recheck Closed. If
	// close races after this snapshot, the callback observes the registered
	// lease and removes it; if close won earlier, this explicit drop does.
	if session.Snapshot().Closed {
		_ = session.Close()
		a.dropMedia(leaseID, request.Call.ProviderHandle)
		return nil, sipgateway.ErrMediaNotPrepared
	}
	cleanupNeeded = false
	return session, nil
}

// AnswerIncoming sends exactly one ARI answer mutation for a new CommandID.
func (a *Adapter) AnswerIncoming(ctx context.Context, request sipgateway.AnswerIncomingRequest) (sipgateway.CommandResult, error) {
	fingerprint := commandFingerprint{kind: answerCommand, call: request.Call, leaseID: request.MediaLeaseID}
	return a.executeMutation(ctx, request.CommandID, fingerprint)
}

// EndActive sends exactly one ARI hangup mutation for a new CommandID.
func (a *Adapter) EndActive(ctx context.Context, request sipgateway.EndActiveRequest) (sipgateway.CommandResult, error) {
	fingerprint := commandFingerprint{kind: endCommand, call: request.Call}
	return a.executeMutation(ctx, request.CommandID, fingerprint)
}

// RejectIncoming removes an exact ringing channel without answering it. It is
// exposed through the optional sipgateway.CallController surface and never
// accepts a provider handle from a remote client.
func (a *Adapter) RejectIncoming(ctx context.Context, ref sipgateway.CallRef, commandID string) (sipgateway.CommandResult, error) {
	return a.executeMutation(ctx, commandID, commandFingerprint{kind: rejectCommand, call: ref})
}

// SendDTMF sends a bounded digit sequence to one exact active channel. The
// coordinator separately binds this operation to the browser media lease.
func (a *Adapter) SendDTMF(ctx context.Context, ref sipgateway.CallRef, commandID, digits string) (sipgateway.CommandResult, error) {
	if !validDTMFDigits(digits) {
		return sipgateway.CommandResult{}, sipgateway.ErrInvalidCommand
	}
	return a.executeMutation(ctx, commandID, commandFingerprint{kind: dtmfCommand, call: ref, digits: digits})
}

func (a *Adapter) executeMutation(
	ctx context.Context,
	commandID string,
	fingerprint commandFingerprint,
) (sipgateway.CommandResult, error) {
	if !validOpaque(commandID, 8, 128) || fingerprint.call.Validate() != nil ||
		(fingerprint.kind == answerCommand && !validOpaque(fingerprint.leaseID, 1, 128)) ||
		(fingerprint.kind == dtmfCommand && !validDTMFDigits(fingerprint.digits)) {
		return sipgateway.CommandResult{}, sipgateway.ErrInvalidCommand
	}
	ctx, releaseContext, err := a.operationContext(ctx)
	if err != nil {
		// A response loss terminally closes the incarnation-bound socket, but the
		// already stored Unknown must remain replayable so callers never resend it.
		return a.replayStoredCommand(commandID, fingerprint, err)
	}
	defer releaseContext()
	if err := a.acquireOperation(ctx); err != nil {
		return a.replayStoredCommand(commandID, fingerprint, err)
	}
	defer a.releaseOperation()
	a.mu.Lock()
	if existing, ok := a.commands[commandID]; ok {
		a.mu.Unlock()
		if existing.fingerprint != fingerprint {
			return sipgateway.CommandResult{}, sipgateway.ErrCommandConflict
		}
		return cloneCommandResult(existing.result), nil
	}
	if len(a.commands) >= a.cfg.maxCommands {
		a.mu.Unlock()
		return sipgateway.CommandResult{}, sipgateway.ErrCommandStoreFull
	}
	a.mu.Unlock()
	wantedBefore := sipgateway.ProviderCallIncoming
	if fingerprint.kind == endCommand || fingerprint.kind == dtmfCommand {
		wantedBefore = sipgateway.ProviderCallActive
	}
	before, err := a.requireExact(fingerprint.call, wantedBefore)
	if err != nil {
		return sipgateway.CommandResult{}, err
	}
	before, err = a.requireExactOnPBX(ctx, fingerprint.call, wantedBefore)
	if err != nil {
		return sipgateway.CommandResult{}, err
	}
	if fingerprint.kind == answerCommand {
		a.mu.Lock()
		media := a.media[fingerprint.leaseID]
		a.mu.Unlock()
		if media == nil || media.CallRef() != fingerprint.call {
			return sipgateway.CommandResult{}, sipgateway.ErrMediaLeaseMismatch
		}
		snapshot := media.Snapshot()
		if !snapshot.Prepared || snapshot.Closed || snapshot.Activated {
			return sipgateway.CommandResult{}, sipgateway.ErrMediaLeaseMismatch
		}
	}
	unknown := sipgateway.CommandResult{CommandID: commandID, Outcome: sipgateway.CommandUnknown}
	a.mu.Lock()
	a.commands[commandID] = commandRecord{fingerprint: fingerprint, result: unknown}
	a.mu.Unlock()

	method := http.MethodPost
	segments := []string{"channels", fingerprint.call.ProviderHandle, "answer"}
	wantedAfter := sipgateway.ProviderCallActive
	if fingerprint.kind == endCommand || fingerprint.kind == rejectCommand {
		method = http.MethodDelete
		segments = []string{"channels", fingerprint.call.ProviderHandle}
		wantedAfter = sipgateway.ProviderCallEnded
	} else if fingerprint.kind == dtmfCommand {
		segments = []string{"channels", fingerprint.call.ProviderHandle, "dtmf"}
		query := url.Values{"dtmf": {fingerprint.digits}}
		response, requestErr := a.ari.do(ctx, method, segments, query)
		if requestErr != nil || !status2xx(response.status) {
			if requestErr == nil && status4xx(response.status) {
				inspected, inspectErr := a.inspectOnce(ctx, fingerprint.call)
				if inspectErr == nil && inspected.State == wantedBefore && inspected.Ref == before.Ref {
					rejected := sipgateway.CommandResult{CommandID: commandID, Outcome: sipgateway.CommandRejected, Current: cloneSnapshot(&inspected)}
					a.storeCommand(commandID, fingerprint, rejected)
					return rejected, nil
				}
			}
			return unknown, nil
		}
		current, inspectErr := a.currentDialog(fingerprint.call)
		if inspectErr != nil || current.State != sipgateway.ProviderCallActive || current.Ref != before.Ref {
			return unknown, nil
		}
		applied := sipgateway.CommandResult{CommandID: commandID, Outcome: sipgateway.CommandApplied, Current: cloneSnapshot(&current)}
		a.storeCommand(commandID, fingerprint, applied)
		return applied, nil
	}
	response, requestErr := a.ari.do(ctx, method, segments, nil)
	if requestErr != nil || !status2xx(response.status) {
		if requestErr == nil && status4xx(response.status) {
			inspected, inspectErr := a.inspectOnce(ctx, fingerprint.call)
			if inspectErr == nil && inspected.State == wantedBefore && inspected.Ref == before.Ref {
				rejected := sipgateway.CommandResult{
					CommandID: commandID, Outcome: sipgateway.CommandRejected, Current: cloneSnapshot(&inspected),
				}
				a.storeCommand(commandID, fingerprint, rejected)
				return rejected, nil
			}
		}
		return unknown, nil
	}
	confirmCtx, cancel := withMaxTimeout(ctx, a.cfg.confirmTimeout)
	defer cancel()
	confirmed, err := a.waitForState(confirmCtx, fingerprint.call, wantedAfter, before.Ref.Revision)
	if err != nil || confirmed.State != wantedAfter || confirmed.Ref.Revision <= before.Ref.Revision {
		return unknown, nil
	}
	applied := sipgateway.CommandResult{
		CommandID: commandID, Outcome: sipgateway.CommandApplied, Current: cloneSnapshot(&confirmed),
	}
	a.storeCommand(commandID, fingerprint, applied)
	return applied, nil
}

func (a *Adapter) replayStoredCommand(
	commandID string,
	fingerprint commandFingerprint,
	fallback error,
) (sipgateway.CommandResult, error) {
	a.mu.Lock()
	existing, ok := a.commands[commandID]
	a.mu.Unlock()
	if !ok {
		return sipgateway.CommandResult{}, fallback
	}
	if existing.fingerprint != fingerprint {
		return sipgateway.CommandResult{}, sipgateway.ErrCommandConflict
	}
	return cloneCommandResult(existing.result), nil
}

func (a *Adapter) waitForState(
	ctx context.Context,
	ref sipgateway.CallRef,
	wanted sipgateway.ProviderCallState,
	minimumRevision uint64,
) (sipgateway.CallSnapshot, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := a.currentDialog(ref)
		if err != nil {
			return sipgateway.CallSnapshot{}, err
		}
		if current.State == wanted && current.Ref.Revision > minimumRevision {
			return current, nil
		}
		if (wanted == sipgateway.ProviderCallActive && current.State == sipgateway.ProviderCallEnded) ||
			(wanted == sipgateway.ProviderCallEnded && current.State == sipgateway.ProviderCallIncoming) {
			return sipgateway.CallSnapshot{}, ErrProtocol
		}
		a.mu.Lock()
		signal := a.stateSignal
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return sipgateway.CallSnapshot{}, ctx.Err()
		case <-a.done:
			return sipgateway.CallSnapshot{}, sipgateway.ErrClosed
		case <-signal:
		case <-ticker.C:
			inspected, inspectErr := a.inspectOnce(ctx, ref)
			if inspectErr == nil && inspected.State == wanted && inspected.Ref.Revision > minimumRevision {
				return inspected, nil
			}
		}
	}
}

func (a *Adapter) storeCommand(commandID string, fingerprint commandFingerprint, result sipgateway.CommandResult) {
	a.mu.Lock()
	a.commands[commandID] = commandRecord{fingerprint: fingerprint, result: cloneCommandResult(result)}
	a.mu.Unlock()
}

func (a *Adapter) currentDialog(ref sipgateway.CallRef) (sipgateway.CallSnapshot, error) {
	if err := a.validateRef(ref); err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return sipgateway.CallSnapshot{}, sipgateway.ErrClosed
	}
	current, ok := a.calls[ref.ProviderHandle]
	if !ok || !current.Ref.SameDialog(ref) {
		return sipgateway.CallSnapshot{}, sipgateway.ErrCallNotFound
	}
	return current, nil
}

func (a *Adapter) requireExact(ref sipgateway.CallRef, state sipgateway.ProviderCallState) (sipgateway.CallSnapshot, error) {
	current, err := a.currentDialog(ref)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	if current.Ref != ref {
		return sipgateway.CallSnapshot{}, sipgateway.ErrStaleRevision
	}
	if current.State != state {
		return sipgateway.CallSnapshot{}, sipgateway.ErrWrongCallPhase
	}
	return current, nil
}

func (a *Adapter) requireExactOnPBX(
	ctx context.Context,
	ref sipgateway.CallRef,
	state sipgateway.ProviderCallState,
) (sipgateway.CallSnapshot, error) {
	current, err := a.inspectOnce(ctx, ref)
	if err != nil {
		return sipgateway.CallSnapshot{}, err
	}
	if current.Ref != ref {
		return sipgateway.CallSnapshot{}, sipgateway.ErrStaleRevision
	}
	if current.State != state {
		return sipgateway.CallSnapshot{}, sipgateway.ErrWrongCallPhase
	}
	return current, nil
}

func (a *Adapter) validateRef(ref sipgateway.CallRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.GatewayID != a.cfg.gatewayID || ref.BootEpoch != a.bootEpoch {
		return sipgateway.ErrBootEpochMismatch
	}
	return nil
}

func (a *Adapter) randomID(prefix string, byteCount int) (string, error) {
	buffer := make([]byte, byteCount)
	a.idMu.Lock()
	_, err := io.ReadFull(a.cfg.idReader, buffer)
	a.idMu.Unlock()
	if err != nil {
		return "", ErrConfiguration
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (a *Adapter) cleanupResources(bridgeID, mediaChannelID string) {
	// Cleanup is an internal compensating action. It must survive cancellation
	// of public adapter operations, otherwise an ARI WebSocket loss or Close
	// would strand the adapter-owned mixing bridge and media channel. Keep it
	// independently bounded and delete only the random IDs created here.
	timeout := min(a.cfg.httpTimeout, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if bridgeID != "" {
		_, _ = a.ari.do(ctx, http.MethodDelete, []string{"bridges", bridgeID}, nil)
	}
	if mediaChannelID != "" {
		_, _ = a.ari.do(ctx, http.MethodDelete, []string{"channels", mediaChannelID}, nil)
	}
}

func (a *Adapter) scheduleCleanup(bridgeID, mediaChannelID string) {
	if bridgeID == "" && mediaChannelID == "" {
		return
	}
	a.cleanupWG.Add(1)
	go func() {
		defer a.cleanupWG.Done()
		a.cleanupResources(bridgeID, mediaChannelID)
	}()
}

func (a *Adapter) dropMedia(leaseID, providerHandle string) {
	a.mu.Lock()
	delete(a.media, leaseID)
	if a.mediaByCall[providerHandle] == leaseID {
		delete(a.mediaByCall, providerHandle)
	}
	a.mu.Unlock()
}

func (a *Adapter) signalStateLocked() {
	close(a.stateSignal)
	a.stateSignal = make(chan struct{})
}

// Close first unblocks Observe and media readers/writers, then performs
// best-effort cleanup of only adapter-owned bridge and media resources.
func (a *Adapter) Close() error {
	a.stop()
	<-a.readDone
	return nil
}

func (a *Adapter) terminalError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.terminalErr != nil {
		return a.terminalErr
	}
	return sipgateway.ErrClosed
}

func (a *Adapter) stopWithError(err error) {
	a.mu.Lock()
	if !a.closed && a.terminalErr == nil {
		if err == nil {
			err = ErrProtocol
		}
		a.terminalErr = err
	}
	a.mu.Unlock()
	a.stop()
}

func (a *Adapter) stop() {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		media := make([]*mediaSession, 0, len(a.media))
		for _, session := range a.media {
			media = append(media, session)
		}
		a.signalStateLocked()
		a.mu.Unlock()
		a.cancelRun()
		close(a.done)
		for _, session := range media {
			_ = session.Close()
		}
		// Cancellation makes every in-flight operation return and release this
		// gate. Waiting here prevents a preparation failure from scheduling its
		// compensating delete after cleanupWG.Wait has begun.
		<-a.opGate
		a.cleanupWG.Wait()
		// Healthy owned-resource cleanup is sent over the original event socket.
		// Only after every bounded cleanup finishes do we close that incarnation
		// boundary. If the socket already failed, ari.do cannot reconnect.
		a.ari.close()
		a.mediaDialer.close()
	})
}

func (a *Adapter) operationContext(ctx context.Context) (context.Context, func(), error) {
	if err := contextErr(ctx); err != nil {
		return nil, nil, err
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.runCtx, cancel)
	if a.runCtx.Err() != nil {
		cancel()
	}
	release := func() {
		stop()
		cancel()
	}
	if err := contextErr(operation); err != nil {
		release()
		return nil, nil, sipgateway.ErrClosed
	}
	return operation, release, nil
}

func (a *Adapter) acquireOperation(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
		return sipgateway.ErrClosed
	case <-a.opGate:
		return nil
	}
}

func (a *Adapter) releaseOperation() { a.opGate <- struct{}{} }

func (a *Adapter) validateIncomingChannel(channel ariChannel) error {
	if err := a.validateIncomingChannelIdentity(channel); err != nil {
		return err
	}
	if channel.State != "Ring" && channel.State != "Ringing" {
		return ErrProtocol
	}
	return nil
}

func (a *Adapter) validateIncomingChannelIdentity(channel ariChannel) error {
	return validateIncomingChannelIdentity(a.cfg, channel)
}

func (a *Adapter) validateObservedCallIdentity(ref sipgateway.CallRef, channel ariChannel) error {
	a.mu.Lock()
	wanted, exists := a.recoveryIDs[ref.ProviderHandle]
	a.mu.Unlock()
	observed, err := a.recoveryIdentityFor(channel)
	if !exists || err != nil || !sameRecoveryIdentity(wanted, observed) {
		// A channel ID can be reused after a PBX restart. Even on the bound event
		// socket, never adopt a GET result unless every immutable call and policy
		// field matches the original StasisStart evidence.
		failure := errors.Join(sipgateway.ErrRecoveryRequired, ErrProtocol)
		go a.stopWithError(failure)
		return failure
	}
	return nil
}

func validateIncomingChannelIdentity(cfg normalizedConfig, channel ariChannel) error {
	prefix := "PJSIP/" + cfg.incomingEndpoint + "-"
	suffix := strings.TrimPrefix(channel.Name, prefix)
	if !validOpaque(channel.ID, 1, 256) || !strings.HasPrefix(channel.Name, prefix) ||
		!validAsteriskChannelSuffix(suffix) || len(channel.Name) > 512 ||
		channel.Dialplan.Context != cfg.incomingContext || channel.Dialplan.AppName != "Stasis" ||
		channel.Dialplan.AppData != cfg.application+","+cfg.incomingArgument ||
		channel.ChannelVars[incomingPolicyVariable] != cfg.incomingPolicyID {
		return ErrProtocol
	}
	return nil
}

func validateOutgoingChannelIdentity(cfg normalizedConfig, channel ariChannel, number string) error {
	if cfg.outgoingEndpoint == "" || !validOutgoingDialNumber(number) ||
		!validOpaque(channel.ID, 1, 256) || !outgoingChannelNameMatches(cfg, channel.Name, number) || len(channel.Name) > 512 ||
		channel.Dialplan.AppName != "Stasis" ||
		(channel.Dialplan.AppData != "" && channel.Dialplan.AppData != cfg.application+",outgoing") ||
		(channel.ChannelVars[incomingPolicyVariable] != "" && channel.ChannelVars[incomingPolicyVariable] != cfg.incomingPolicyID) {
		return ErrProtocol
	}
	return nil
}

func outgoingChannelNameMatches(cfg normalizedConfig, name, number string) bool {
	for _, prefix := range []string{"PJSIP/" + number + "-", "PJSIP/" + cfg.outgoingEndpoint + "-"} {
		if strings.HasPrefix(name, prefix) && validAsteriskChannelSuffix(strings.TrimPrefix(name, prefix)) {
			return true
		}
	}
	return false
}

func outgoingChannelShapeMatches(cfg normalizedConfig, name string) bool {
	if cfg.outgoingEndpoint == "" || !strings.HasPrefix(name, "PJSIP/") {
		return false
	}
	rest := strings.TrimPrefix(name, "PJSIP/")
	separator := strings.LastIndexByte(rest, '-')
	return separator > 0 && validAsteriskChannelSuffix(rest[separator+1:]) &&
		(rest[:separator] == cfg.outgoingEndpoint || validOutgoingDialNumber(rest[:separator]))
}

func validOutgoingDialNumber(value string) bool {
	if len(value) == 0 || len(value) > 32 || value != strings.TrimSpace(value) || value == "+" {
		return false
	}
	for index, char := range value {
		if index == 0 && char == '+' {
			continue
		}
		if !(char >= '0' && char <= '9') && char != '*' && char != '#' {
			return false
		}
	}
	return true
}

func validAsteriskChannelSuffix(value string) bool {
	if len(value) != 8 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') ||
			(char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func providerStateFromARI(state string) (sipgateway.ProviderCallState, error) {
	switch state {
	case "Ring", "Ringing":
		return sipgateway.ProviderCallIncoming, nil
	case "Up":
		return sipgateway.ProviderCallActive, nil
	default:
		return "", ErrProtocol
	}
}

func validTransition(from, to sipgateway.ProviderCallState) bool {
	switch from {
	case sipgateway.ProviderCallIncoming:
		return to == sipgateway.ProviderCallActive || to == sipgateway.ProviderCallEnded
	case sipgateway.ProviderCallActive:
		return to == sipgateway.ProviderCallEnded
	default:
		return false
	}
}

func containsPCMUOnly(codecs []sipgateway.Codec) bool {
	if len(codecs) == 0 || len(codecs) > 16 {
		return false
	}
	found := false
	seen := make(map[sipgateway.Codec]struct{}, len(codecs))
	for _, codec := range codecs {
		if codec.Validate() != nil {
			return false
		}
		if _, duplicate := seen[codec]; duplicate {
			return false
		}
		seen[codec] = struct{}{}
		if codec == sipgateway.CodecPCMU {
			found = true
		}
	}
	return found
}

func cloneEvent(event sipgateway.Event) sipgateway.Event {
	copyEvent := event
	copyEvent.Call = cloneSnapshot(event.Call)
	return copyEvent
}

func cloneSnapshot(snapshot *sipgateway.CallSnapshot) *sipgateway.CallSnapshot {
	if snapshot == nil {
		return nil
	}
	copySnapshot := *snapshot
	return &copySnapshot
}

func cloneCommandResult(result sipgateway.CommandResult) sipgateway.CommandResult {
	copyResult := result
	copyResult.Current = cloneSnapshot(result.Current)
	return copyResult
}

func validDTMFDigits(value string) bool {
	if len(value) == 0 || len(value) > 32 || value != strings.TrimSpace(value) {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && char != '*' && char != '#' &&
			!(char >= 'A' && char <= 'D') && !(char >= 'a' && char <= 'd') {
			return false
		}
	}
	return true
}
