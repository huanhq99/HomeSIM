package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/remotevoice"
)

type remoteMediaTestPeer struct {
	done     chan struct{}
	once     sync.Once
	prepared bool
}

type delayedRemoteMediaTestPeer struct {
	done  chan struct{}
	once  sync.Once
	ready atomic.Bool
}

func (p *delayedRemoteMediaTestPeer) Connected() bool { return p.ready.Load() }
func (p *delayedRemoteMediaTestPeer) NetworkActivitySnapshot() remotevoice.NetworkActivitySnapshot {
	if !p.ready.Load() {
		return remotevoice.NetworkActivitySnapshot{}
	}
	now := time.Now()
	return remotevoice.NetworkActivitySnapshot{
		Connected: true, LocalTrackStarted: true, UplinkPackets: 1, LastUplinkAt: now,
		RemoteTrackReady: true, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
		ReceiverReports: 1, LastReceiverReportAt: now,
		SelectedPair: remotevoice.SelectedPairSnapshot{
			Established: true, Protocol: remotevoice.SelectedPairProtocolUDP,
			LocalCandidateType:  remotevoice.SelectedCandidateTypeHost,
			RemoteCandidateType: remotevoice.SelectedCandidateTypeHost,
			LocalAddressAllowed: true, RemoteAddressAllowed: true,
		},
	}
}
func (p *delayedRemoteMediaTestPeer) Activate(uint64) (remotevoice.NetworkActivitySnapshot, error) {
	return p.NetworkActivitySnapshot(), nil
}
func (p *delayedRemoteMediaTestPeer) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return nil
	}
}
func (p *delayedRemoteMediaTestPeer) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *remoteMediaTestPeer) Connected() bool { return p.prepared }
func (p *remoteMediaTestPeer) NetworkActivitySnapshot() remotevoice.NetworkActivitySnapshot {
	if p.prepared {
		now := time.Now()
		return remotevoice.NetworkActivitySnapshot{
			Connected: true, LocalTrackStarted: true, UplinkPackets: 1, LastUplinkAt: now,
			RemoteTrackReady: true, RemoteRTPPackets: 1, LastRemoteRTPAt: now,
			ReceiverReports: 1, LastReceiverReportAt: now,
			SelectedPair: remotevoice.SelectedPairSnapshot{
				Established: true, Protocol: remotevoice.SelectedPairProtocolUDP,
				LocalCandidateType:  remotevoice.SelectedCandidateTypeHost,
				RemoteCandidateType: remotevoice.SelectedCandidateTypeHost,
				LocalAddressAllowed: true, RemoteAddressAllowed: true,
			},
		}
	}
	return remotevoice.NetworkActivitySnapshot{}
}
func (p *remoteMediaTestPeer) Activate(uint64) (remotevoice.NetworkActivitySnapshot, error) {
	return p.NetworkActivitySnapshot(), nil
}
func (p *remoteMediaTestPeer) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return nil
	}
}
func (p *remoteMediaTestPeer) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

type remoteMediaTestBroker struct {
	done     chan struct{}
	once     sync.Once
	prepared bool
}

func (b *remoteMediaTestBroker) ActivitySnapshot() remotevoice.IPCBrokerActivitySnapshot {
	if b.prepared {
		now := time.Now()
		return remotevoice.IPCBrokerActivitySnapshot{
			Consumed: true, AuthenticatedAt: now, Ready: true, ReadyAt: now, Live: true,
		}
	}
	return remotevoice.IPCBrokerActivitySnapshot{}
}
func (b *remoteMediaTestBroker) Activate(uint64) (remotevoice.IPCBrokerActivitySnapshot, error) {
	return b.ActivitySnapshot(), nil
}
func (b *remoteMediaTestBroker) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return nil
	}
}
func (b *remoteMediaTestBroker) Close() error {
	b.once.Do(func() { close(b.done) })
	return nil
}

func newRemoteMediaUnitManager(t *testing.T) *remoteMediaManager {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "maccellular-rm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	manager, err := newRemoteMediaManager(remoteMediaRuntimeConfig{
		Interface: "utun-test", LocalCIDRs: []string{"100.64.0.1/32"},
		RemoteCIDRs: []string{"100.64.0.2/32"}, UDPMin: 49160, UDPMax: 49180,
		RootDir: filepath.Join(root, "pcm"), ControlPath: filepath.Join(root, "media-control.sock"),
		OfferTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.validateNetwork = func(remoteMediaRuntimeConfig) error { return nil }
	manager.validateUAC = func(uint32) error { return nil }
	manager.startBroker = func(remotevoice.LocalPCMBrokerStartConfig) (remotevoice.LocalPCMBroker, error) {
		return &remoteMediaTestBroker{done: make(chan struct{}), prepared: true}, nil
	}
	manager.answerer = func(context.Context, []byte, remotevoice.Config, *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
		return &remoteMediaTestPeer{done: make(chan struct{}), prepared: true}, []byte("v=0\r\n"), nil
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func prepareRemoteMediaUnitLease(t *testing.T, manager *remoteMediaManager) (
	remoteMediaOfferResponse,
	*remoteMediaLease,
	*usbAT,
	usbATPhysicalIdentity,
) {
	t.Helper()
	request := remoteMediaUnitOffer()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := manager.offer(
		context.Background(), "owner@example.com", body, request,
		func(source remoteMediaSource) bool {
			return source.Purpose == "incoming" && source.Call == request.Call
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.active
	manager.mu.Unlock()
	claim, err := entry.coordinator.ClaimIPCLease()
	if err != nil {
		t.Fatal(err)
	}
	for index := range claim.SessionToken {
		claim.SessionToken[index] = 0
	}
	device := &usbAT{}
	identity := usbATPhysicalIdentity{
		VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Location: 0x12340000,
	}
	manager.mu.Lock()
	entry.claimDevice = device
	entry.claimIdentity = identity
	entry.hostPrepared = true
	entry.uacUIDDigest = strings.Repeat("a", 64)
	manager.mu.Unlock()
	return response, entry, device, identity
}

func remoteMediaUnitOffer() remoteMediaOfferRequest {
	return remoteMediaOfferRequest{
		Purpose: "incoming", ClientNonce: "client-nonce-0001", SDPOffer: "v=0\r\n",
		Call: remoteCallExpectation{CallID: "call-1", CallGeneration: 7, CallIndex: 1, CallDirection: "incoming"},
	}
}

func remoteMediaUnitOutgoingOffer() remoteMediaOfferRequest {
	return remoteMediaOfferRequest{
		Purpose: "outgoing", ExpectedCallGeneration: 11, Number: "+86 10086",
		ClientNonce: "client-nonce-outgoing-0001", SDPOffer: "v=0\r\n",
	}
}

func TestRemoteMediaOutgoingOfferBindsIdleGenerationAndNumber(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	request := remoteMediaUnitOutgoingOffer()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	sourceCurrent := func(source remoteMediaSource) bool {
		return source.Purpose == "outgoing" && source.ExpectedCallGeneration == 11 &&
			source.DialNumber == "+8610086"
	}
	response, err := manager.offer(context.Background(), "owner@example.com", body, request, sourceCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if response.CallGeneration != 11 || response.MediaSessionID == "" || response.LeaseGeneration == 0 {
		t.Fatalf("outgoing response = %+v", response)
	}
	manager.mu.Lock()
	entry := manager.active
	manager.mu.Unlock()
	if entry == nil || !sourceCurrent(entry.source) || entry.source.DialNumber != "+8610086" {
		t.Fatalf("outgoing source = %+v", entry)
	}

	changed := request
	changed.ClientNonce = "client-nonce-outgoing-0002"
	changed.Number = "+86 10010"
	changedBody, _ := json.Marshal(changed)
	if _, err := manager.offer(context.Background(), "owner@example.com", changedBody, changed, sourceCurrent); err == nil {
		t.Fatal("outgoing offer for another number passed the idle source fence")
	}
}

func TestRemoteMediaOutgoingFinalGateRequiresExactPreparedOwner(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	request := remoteMediaUnitOutgoingOffer()
	body, _ := json.Marshal(request)
	response, err := manager.offer(context.Background(), "owner@example.com", body, request,
		func(source remoteMediaSource) bool { return source.ExpectedCallGeneration == 11 })
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.active
	manager.mu.Unlock()
	claim, err := entry.coordinator.ClaimIPCLease()
	if err != nil {
		t.Fatal(err)
	}
	for index := range claim.SessionToken {
		claim.SessionToken[index] = 0
	}
	manager.mu.Lock()
	device := &usbAT{}
	identity := usbATPhysicalIdentity{
		VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Location: 0x12340000,
	}
	entry.claimDevice = device
	entry.claimIdentity = identity
	entry.hostPrepared = true
	entry.uacUIDDigest = strings.Repeat("b", 64)
	manager.mu.Unlock()

	if err := manager.authorizeOutgoingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, 11, "+8610086",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.beginOutgoingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, 11, "10010", device, identity,
	); err == nil {
		t.Fatal("different dial number acquired the outgoing action")
	}
	reservation, err := manager.beginOutgoingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, 11, "+8610086", device, identity,
	)
	if err != nil || !reservation.valid() {
		t.Fatalf("outgoing reservation=%+v err=%v", reservation, err)
	}
	manager.finishIncomingAction(reservation, true)
	manager.mu.Lock()
	actionPending := manager.active != nil && manager.active.actionPending
	manager.mu.Unlock()
	if !actionPending {
		t.Fatal("accepted outgoing action did not remain activation-pending")
	}
}

func TestRemoteMediaOutgoingAnswerWaitsForExactHostPreparation(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	request := remoteMediaUnitOutgoingOffer()
	body, _ := json.Marshal(request)
	response, err := manager.offer(
		context.Background(), "owner@example.com", body, request,
		func(source remoteMediaSource) bool { return source.ExpectedCallGeneration == 11 },
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.active
	manager.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		manager.mu.Lock()
		entry.claimDevice = &usbAT{}
		entry.hostPrepared = true
		entry.uacUIDDigest = strings.Repeat("c", 64)
		manager.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.waitHostPrepared(
		ctx, "owner@example.com", response.MediaSessionID, response.LeaseGeneration,
	); err != nil {
		t.Fatal(err)
	}

	timeoutManager := newRemoteMediaUnitManager(t)
	timeoutResponse, err := timeoutManager.offer(
		context.Background(), "owner@example.com", body, request,
		func(source remoteMediaSource) bool { return source.ExpectedCallGeneration == 11 },
	)
	if err != nil {
		t.Fatal(err)
	}
	timeoutContext, timeoutCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer timeoutCancel()
	if err := timeoutManager.waitHostPrepared(
		timeoutContext, "owner@example.com", timeoutResponse.MediaSessionID,
		timeoutResponse.LeaseGeneration,
	); err == nil {
		t.Fatal("unprepared outgoing media host was accepted")
	}
}

func TestRemoteMediaOutgoingDialWaitsForTransportProof(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	peer := &delayedRemoteMediaTestPeer{done: make(chan struct{})}
	manager.answerer = func(
		context.Context, []byte, remotevoice.Config, *remotevoice.FramePort,
	) (remotevoice.MediaPeer, []byte, error) {
		return peer, []byte("v=0\r\n"), nil
	}
	request := remoteMediaUnitOutgoingOffer()
	body, _ := json.Marshal(request)
	response, err := manager.offer(
		context.Background(), "owner@example.com", body, request,
		func(source remoteMediaSource) bool { return source.ExpectedCallGeneration == 11 },
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	entry := manager.active
	manager.mu.Unlock()
	claim, err := entry.coordinator.ClaimIPCLease()
	if err != nil {
		t.Fatal(err)
	}
	for index := range claim.SessionToken {
		claim.SessionToken[index] = 0
	}
	manager.mu.Lock()
	entry.claimDevice = &usbAT{}
	entry.hostPrepared = true
	entry.uacUIDDigest = strings.Repeat("d", 64)
	manager.mu.Unlock()
	go func() {
		time.Sleep(50 * time.Millisecond)
		peer.ready.Store(true)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.waitOutgoingPrepared(
		ctx, "owner@example.com", response.MediaSessionID, response.LeaseGeneration, 11, "+8610086",
	); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteMediaConfigurationRequiresExactTailnetPeer(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "maccellular-rm-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	base := remoteMediaRuntimeConfig{
		Interface: "utun7", LocalCIDRs: []string{"100.64.0.1/32"},
		RemoteCIDRs: []string{"100.64.0.2/32"}, UDPMin: 49160, UDPMax: 49180,
		RootDir: filepath.Join(root, "pcm"), ControlPath: filepath.Join(root, "media-control.sock"),
		OfferTTL: 30 * time.Second,
	}
	if _, err := newRemoteMediaManager(base); err != nil {
		t.Fatalf("exact Tailnet media config rejected: %v", err)
	}
	base.RemoteCIDRs = []string{"100.64.0.0/10"}
	if _, err := newRemoteMediaManager(base); err == nil {
		t.Fatal("broad remote Tailnet CIDR was accepted")
	}
	base.RemoteCIDRs = []string{"0.0.0.0/0"}
	if _, err := newRemoteMediaManager(base); err == nil {
		t.Fatal("public remote media CIDR was accepted")
	}
	base.RemoteCIDRs = []string{"100.64.0.2/32", "100.64.0.3/32"}
	if _, err := newRemoteMediaManager(base); err == nil {
		t.Fatal("multiple unbound remote media peers were accepted")
	}
}

func TestRemoteMediaConfigurationAcceptsOnlyOneCompleteNetworkProfile(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "maccellular-rm-public-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	paths := remoteMediaRuntimeConfig{
		RootDir: filepath.Join(root, "pcm"), ControlPath: filepath.Join(root, "media-control.sock"),
		OfferTTL: 30 * time.Second,
	}
	issueTURN := func() (*remotevoice.TURNRelayConfig, error) {
		return &remotevoice.TURNRelayConfig{}, nil
	}
	public := paths
	public.IssueTURN = issueTURN
	manager, err := newRemoteMediaManager(public)
	if err != nil {
		t.Fatalf("complete public TURN profile rejected: %v", err)
	}
	defer manager.Close()
	if manager.cfg.networkMode() != remoteMediaNetworkPublicTURN || !manager.cfg.enabled() {
		t.Fatal("complete public TURN profile was not enabled as public relay")
	}

	tailnet := paths
	tailnet.Interface = "utun7"
	tailnet.LocalCIDRs = []string{"100.64.0.1/32"}
	tailnet.RemoteCIDRs = []string{"100.64.0.2/32"}
	tailnet.UDPMin, tailnet.UDPMax = 49160, 49180
	if manager, err := newRemoteMediaManager(tailnet); err != nil {
		t.Fatalf("complete legacy Tailnet profile rejected: %v", err)
	} else {
		_ = manager.Close()
	}

	mixedCases := []struct {
		name   string
		mutate func(*remoteMediaRuntimeConfig)
	}{
		{name: "interface", mutate: func(c *remoteMediaRuntimeConfig) { c.Interface = "utun7" }},
		{name: "local CIDR", mutate: func(c *remoteMediaRuntimeConfig) { c.LocalCIDRs = []string{"100.64.0.1/32"} }},
		{name: "remote CIDR", mutate: func(c *remoteMediaRuntimeConfig) { c.RemoteCIDRs = []string{"100.64.0.2/32"} }},
		{name: "UDP minimum", mutate: func(c *remoteMediaRuntimeConfig) { c.UDPMin = 49160 }},
		{name: "UDP maximum", mutate: func(c *remoteMediaRuntimeConfig) { c.UDPMax = 49180 }},
	}
	for _, test := range mixedCases {
		t.Run("mixed "+test.name, func(t *testing.T) {
			candidate := public
			test.mutate(&candidate)
			if candidate.enabled() {
				t.Fatal("mixed public/Tailnet profile reported enabled")
			}
			if _, err := newRemoteMediaManager(candidate); err == nil {
				t.Fatal("mixed public/Tailnet profile was accepted")
			}
		})
	}

	partialCases := []struct {
		name   string
		mutate func(*remoteMediaRuntimeConfig)
	}{
		{name: "no network", mutate: func(*remoteMediaRuntimeConfig) {}},
		{name: "interface only", mutate: func(c *remoteMediaRuntimeConfig) { c.Interface = "utun7" }},
		{name: "missing peer", mutate: func(c *remoteMediaRuntimeConfig) {
			c.Interface = "utun7"
			c.LocalCIDRs = []string{"100.64.0.1/32"}
			c.UDPMin, c.UDPMax = 49160, 49180
		}},
		{name: "missing UDP maximum", mutate: func(c *remoteMediaRuntimeConfig) {
			c.Interface = "utun7"
			c.LocalCIDRs = []string{"100.64.0.1/32"}
			c.RemoteCIDRs = []string{"100.64.0.2/32"}
			c.UDPMin = 49160
		}},
	}
	for _, test := range partialCases {
		t.Run("partial "+test.name, func(t *testing.T) {
			candidate := paths
			test.mutate(&candidate)
			if candidate.enabled() {
				t.Fatal("partial network profile reported enabled")
			}
			if _, err := newRemoteMediaManager(candidate); err == nil {
				t.Fatal("partial network profile was accepted")
			}
		})
	}
}

func TestRemoteMediaPublicTURNOfferForwardsOneDeepCopiedCredential(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "maccellular-rm-public-offer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source := &remotevoice.TURNRelayConfig{
		URLs: []string{
			"turn:turn.example.com:3478?transport=udp",
			"turns:turn.example.com:443?transport=tcp",
		},
		Username: "temporary-user", Password: "temporary-password",
		CredentialType: remotevoice.TURNCredentialTypePassword,
	}
	issueCalls := 0
	manager, err := newRemoteMediaManager(remoteMediaRuntimeConfig{
		IssueTURN: func() (*remotevoice.TURNRelayConfig, error) {
			issueCalls++
			return source, nil
		},
		RootDir: filepath.Join(root, "pcm"), ControlPath: filepath.Join(root, "media-control.sock"),
		OfferTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	manager.validateNetwork = func(remoteMediaRuntimeConfig) error {
		t.Fatal("public TURN offer invoked Tailnet network validation")
		return nil
	}
	manager.startBroker = func(remotevoice.LocalPCMBrokerStartConfig) (remotevoice.LocalPCMBroker, error) {
		return &remoteMediaTestBroker{done: make(chan struct{}), prepared: true}, nil
	}
	answerCalls := 0
	var observed remotevoice.Config
	var encodedConfig []byte
	manager.answerer = func(
		_ context.Context,
		_ []byte,
		cfg remotevoice.Config,
		_ *remotevoice.FramePort,
	) (remotevoice.MediaPeer, []byte, error) {
		answerCalls++
		observed = cfg
		if cfg.TURNRelay == source || &cfg.TURNRelay.URLs[0] == &source.URLs[0] {
			t.Fatal("TURN credential was not deep-copied before reaching the answerer")
		}
		observedRelay := *cfg.TURNRelay
		observedRelay.URLs = append([]string(nil), cfg.TURNRelay.URLs...)
		observed.TURNRelay = &observedRelay
		var marshalErr error
		encodedConfig, marshalErr = json.Marshal(cfg)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		cfg.TURNRelay.URLs[0] = "turn:mutated.invalid:3478?transport=udp"
		cfg.TURNRelay.Password = "mutated-password"
		return &remoteMediaTestPeer{done: make(chan struct{}), prepared: true}, []byte("v=0\r\n"), nil
	}

	request := remoteMediaUnitOffer()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	callCurrent := func(source remoteMediaSource) bool {
		return source.Purpose == "incoming" && source.Call == request.Call
	}
	first, err := manager.offer(context.Background(), "owner@example.com", body, request, callCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if issueCalls != 1 || answerCalls != 1 {
		t.Fatalf("first offer issue calls=%d answer calls=%d", issueCalls, answerCalls)
	}
	if observed.TURNRelay == nil || len(observed.TURNRelay.URLs) != 2 ||
		observed.TURNRelay.URLs[0] != source.URLs[0] || observed.TURNRelay.URLs[1] != source.URLs[1] ||
		observed.TURNRelay.Username != source.Username || observed.TURNRelay.Password != source.Password ||
		observed.TURNRelay.CredentialType != source.CredentialType {
		t.Fatalf("incomplete TURN forwarding: %+v", observed.TURNRelay)
	}
	if remoteMediaPublicUDPMin != 55000 || remoteMediaPublicUDPMax != 55063 {
		t.Fatalf("public TURN UDP constants = %d-%d", remoteMediaPublicUDPMin, remoteMediaPublicUDPMax)
	}
	if observed.UDPMin != remoteMediaPublicUDPMin || observed.UDPMax != remoteMediaPublicUDPMax {
		t.Fatalf("public TURN UDP range = %d-%d", observed.UDPMin, observed.UDPMax)
	}
	if len(observed.AllowedInterfaces) != 0 || len(observed.AllowedLocalCIDRs) != 0 ||
		len(observed.AllowedRemoteCIDRs) != 0 {
		t.Fatal("public TURN profile forwarded Tailnet allowlists")
	}
	if source.URLs[0] != "turn:turn.example.com:3478?transport=udp" ||
		source.Password != "temporary-password" {
		t.Fatal("answerer mutation escaped the deep-copied TURN credential")
	}
	if strings.Contains(string(encodedConfig), source.Username) ||
		strings.Contains(string(encodedConfig), source.Password) ||
		strings.Contains(string(encodedConfig), source.URLs[0]) {
		t.Fatal("TURN credential material entered remotevoice.Config JSON")
	}

	second, err := manager.offer(context.Background(), "owner@example.com", body, request, callCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if first.MediaSessionID != second.MediaSessionID || first.LeaseGeneration != second.LeaseGeneration ||
		issueCalls != 1 || answerCalls != 1 {
		t.Fatalf("exact retry reissued TURN credentials or replaced its lease: issue=%d answer=%d", issueCalls, answerCalls)
	}
}

func TestRemoteMediaPublicTURNIssuerFailureIsFailClosedAndRedacted(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "maccellular-rm-public-issuer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, test := range []struct {
		name  string
		issue func() (*remotevoice.TURNRelayConfig, error)
	}{
		{name: "error", issue: func() (*remotevoice.TURNRelayConfig, error) {
			return nil, errors.New("credential-marker-must-not-escape")
		}},
		{name: "nil credential", issue: func() (*remotevoice.TURNRelayConfig, error) { return nil, nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, err := newRemoteMediaManager(remoteMediaRuntimeConfig{
				IssueTURN:   test.issue,
				RootDir:     filepath.Join(root, strings.ReplaceAll(test.name, " ", "-"), "pcm"),
				ControlPath: filepath.Join(root, strings.ReplaceAll(test.name, " ", "-"), "media-control.sock"),
				OfferTTL:    30 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			manager.startBroker = func(remotevoice.LocalPCMBrokerStartConfig) (remotevoice.LocalPCMBroker, error) {
				return &remoteMediaTestBroker{done: make(chan struct{}), prepared: true}, nil
			}
			answerCalled := false
			manager.answerer = func(context.Context, []byte, remotevoice.Config, *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
				answerCalled = true
				return nil, nil, errors.New("unexpected answer")
			}
			request := remoteMediaUnitOffer()
			body, _ := json.Marshal(request)
			_, offerErr := manager.offer(context.Background(), "owner@example.com", body, request,
				func(source remoteMediaSource) bool {
					return source.Purpose == "incoming" && source.Call == request.Call
				})
			if offerErr == nil || strings.Contains(offerErr.Error(), "credential-marker") || answerCalled {
				t.Fatalf("issuer failure did not fail closed/redacted: err=%v answer=%t", offerErr, answerCalled)
			}
			manager.mu.Lock()
			active := manager.active
			manager.mu.Unlock()
			if active != nil {
				t.Fatal("failed issuer published a media lease")
			}
		})
	}
}

func TestRemoteMediaLegacyTailnetOfferConfigIsUnchanged(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	validateCalls := 0
	manager.validateNetwork = func(cfg remoteMediaRuntimeConfig) error {
		validateCalls++
		if cfg.networkMode() != remoteMediaNetworkTailnet {
			t.Fatal("legacy offer changed network profile")
		}
		return nil
	}
	var observed remotevoice.Config
	manager.answerer = func(_ context.Context, _ []byte, cfg remotevoice.Config, _ *remotevoice.FramePort) (remotevoice.MediaPeer, []byte, error) {
		observed = cfg
		return &remoteMediaTestPeer{done: make(chan struct{}), prepared: true}, []byte("v=0\r\n"), nil
	}
	request := remoteMediaUnitOffer()
	body, _ := json.Marshal(request)
	if _, err := manager.offer(context.Background(), "owner@example.com", body, request,
		func(source remoteMediaSource) bool {
			return source.Purpose == "incoming" && source.Call == request.Call
		}); err != nil {
		t.Fatal(err)
	}
	if validateCalls != 1 || observed.TURNRelay != nil ||
		len(observed.AllowedInterfaces) != 1 || observed.AllowedInterfaces[0] != manager.cfg.Interface ||
		len(observed.AllowedLocalCIDRs) != 1 || observed.AllowedLocalCIDRs[0] != manager.cfg.LocalCIDRs[0] ||
		len(observed.AllowedRemoteCIDRs) != 1 || observed.AllowedRemoteCIDRs[0] != manager.cfg.RemoteCIDRs[0] ||
		observed.UDPMin != manager.cfg.UDPMin || observed.UDPMax != manager.cfg.UDPMax {
		t.Fatalf("legacy Tailnet answer config changed: validate=%d config=%+v", validateCalls, observed)
	}
}

func TestRemoteMediaOfferIsInMemoryIdempotentAndGenerationBound(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	request := remoteMediaUnitOffer()
	body, _ := json.Marshal(request)
	callCurrent := func(source remoteMediaSource) bool {
		return source.Purpose == "incoming" && source.Call == request.Call
	}
	first, err := manager.offer(context.Background(), "owner@example.com", body, request, callCurrent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.offer(context.Background(), "owner@example.com", body, request, callCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if first.MediaSessionID == "" || first.LeaseGeneration == 0 ||
		first.MediaSessionID != second.MediaSessionID || first.LeaseGeneration != second.LeaseGeneration ||
		first.CallGeneration != request.Call.CallGeneration {
		t.Fatalf("exact retry did not replay one opaque lease")
	}
	changed := request
	changed.SDPOffer = "v=1\r\n"
	changedBody, _ := json.Marshal(changed)
	if _, err := manager.offer(context.Background(), "owner@example.com", changedBody, changed, callCurrent); err == nil {
		t.Fatal("same nonce with a different offer was accepted")
	}
	if _, err := manager.offer(context.Background(), "owner@example.com", body, request, func(remoteMediaSource) bool { return false }); err == nil {
		t.Fatal("stale ringing generation was accepted")
	}
	encoded, _ := json.Marshal(manager.snapshot("owner@example.com", callCurrent, nil))
	for _, secret := range []string{first.MediaSessionID, request.SDPOffer, request.ClientNonce} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("credential or SDP leaked into status snapshot")
		}
	}
}

func TestRemoteMediaOfferBypassesHardwareLedgerButKeepsSecurityGates(t *testing.T) {
	instance, handler, csrf := newRemoteTestApp(t, true, "status.read", "calls.read", "calls.media")
	instance.remoteMedia = newRemoteMediaUnitManager(t)
	instance.applyCallPoll([]parsedCall{{Index: 1, Direction: "incoming", State: "incoming"}}, time.Now())
	instance.setCallPollStatus(nil)
	instance.callMu.RLock()
	call := remoteCallExpectation{
		CallID: instance.activeCall.ID, CallGeneration: instance.callGeneration,
		CallIndex: instance.activeCall.Index, CallDirection: instance.activeCall.Direction,
	}
	instance.callMu.RUnlock()
	offer := remoteMediaUnitOffer()
	offer.Call = call
	bodyBytes, _ := json.Marshal(offer)
	request := remoteTestRequest(http.MethodPost, "/api/remote/v1/media/offers", string(bodyBytes),
		"status.read", "calls.read", "calls.media")
	authorizeRemoteMutation(request, csrf, "")
	request.Header.Del("Idempotency-Key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ephemeral media offer status=%d body=%s", response.Code, response.Body.String())
	}

	missingOrigin := remoteTestRequest(http.MethodPost, "/api/remote/v1/media/offers", string(bodyBytes),
		"status.read", "calls.read", "calls.media")
	missingOrigin.Header.Set("Content-Type", "application/json")
	missingOrigin.Header.Set("X-MacCellular-CSRF", csrf)
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingOrigin)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("media offer without same-origin gate status=%d", missingResponse.Code)
	}
}

func TestRemoteMediaControlNoLeaseSchemaAndPermissions(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("same-UID AF_UNIX control server is macOS-only")
	}
	manager := newRemoteMediaUnitManager(t)
	instance := &app{remoteMedia: manager}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := instance.startRemoteMediaControlServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	info, err := os.Lstat(manager.cfg.ControlPath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("control socket mode/type: info=%v err=%v", info, err)
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: manager.cfg.ControlPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("{\"action\":\"claim\"}\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.CloseWrite()
	response, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "{\"ok\":true,\"lease\":null}\n" {
		t.Fatalf("no-lease schema = %q", response)
	}
}

func TestRemoteMediaControlRecoversStaleSocketButRejectsActiveServer(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("AF_UNIX control server recovery is macOS-only")
	}
	manager := newRemoteMediaUnitManager(t)
	if err := ensurePrivateRuntimeDirectory(filepath.Dir(manager.cfg.ControlPath)); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: manager.cfg.ControlPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := secureRemoteMediaControlSocket(manager.cfg.ControlPath); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	instance := &app{remoteMedia: manager}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, err := instance.startRemoteMediaControlServer(ctx)
	if err != nil {
		t.Fatalf("stale socket was not recovered: %v", err)
	}
	defer server.Close()

	second := &app{remoteMedia: manager}
	if duplicate, err := second.startRemoteMediaControlServer(ctx); err == nil || duplicate != nil {
		if duplicate != nil {
			_ = duplicate.Close()
		}
		t.Fatalf("active socket was not rejected: server=%v err=%v", duplicate, err)
	}
}

func TestRemoteMediaControlSimpleResponseHasNoLeaseField(t *testing.T) {
	var output strings.Builder
	if err := writeRemoteMediaControlLine(&output, remoteMediaControlSimpleEnvelope{OK: false}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\"ok\":false}\n" {
		t.Fatalf("simple control response = %q", output.String())
	}
}

func TestRemoteMediaIdentityGenerationIsAlwaysJSSafe(t *testing.T) {
	for iteration := 0; iteration < 1000; iteration++ {
		sessionID, generation, err := newRemoteMediaIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if !remoteMediaSessionPattern.MatchString(sessionID) {
			t.Fatalf("iteration %d produced invalid session ID", iteration)
		}
		if generation == 0 || generation > remoteMediaMaxJSSafeInteger {
			t.Fatalf("iteration %d produced non-JS-safe generation %d", iteration, generation)
		}
	}
}

func TestRemoteMediaFinalActionGateRequiresExactClaimedDeviceAndIdentity(t *testing.T) {
	t.Run("exact owner reserves", func(t *testing.T) {
		manager := newRemoteMediaUnitManager(t)
		response, entry, device, identity := prepareRemoteMediaUnitLease(t, manager)
		reservation, err := manager.beginIncomingAction(
			"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
			entry.source.Call, device, identity,
		)
		if err != nil || !reservation.valid() {
			t.Fatalf("exact final gate reservation=%+v err=%v", reservation, err)
		}
		manager.mu.Lock()
		if manager.active != entry || !entry.actionPending || entry.offerTimer != nil {
			t.Fatalf("final gate did not atomically reserve and stop offer expiry: %+v", entry)
		}
		manager.mu.Unlock()
		manager.finishIncomingAction(reservation, false)
	})

	t.Run("same-location replacement is rejected", func(t *testing.T) {
		manager := newRemoteMediaUnitManager(t)
		response, entry, _, identity := prepareRemoteMediaUnitLease(t, manager)
		replacement := &usbAT{}
		if _, err := manager.beginIncomingAction(
			"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
			entry.source.Call, replacement, identity,
		); err == nil {
			t.Fatal("same-location replacement pointer passed the final action gate")
		}
		manager.mu.Lock()
		defer manager.mu.Unlock()
		if manager.active != entry || entry.actionPending {
			t.Fatal("failed replacement gate mutated the media lease")
		}
	})

	t.Run("physical identity replacement is rejected", func(t *testing.T) {
		manager := newRemoteMediaUnitManager(t)
		response, entry, device, identity := prepareRemoteMediaUnitLease(t, manager)
		identity.Location++
		if _, err := manager.beginIncomingAction(
			"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
			entry.source.Call, device, identity,
		); err == nil {
			t.Fatal("changed physical identity passed the final action gate")
		}
	})

	t.Run("fresh UAC descriptor failure is rejected", func(t *testing.T) {
		manager := newRemoteMediaUnitManager(t)
		manager.validateUAC = func(uint32) error { return errors.New("descriptor unavailable") }
		response, entry, device, identity := prepareRemoteMediaUnitLease(t, manager)
		if _, err := manager.beginIncomingAction(
			"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
			entry.source.Call, device, identity,
		); err == nil {
			t.Fatal("missing fresh UAC descriptor passed the final action gate")
		}
		manager.mu.Lock()
		defer manager.mu.Unlock()
		if manager.active != entry || entry.actionPending {
			t.Fatal("failed UAC descriptor gate mutated the media lease")
		}
	})
}

func TestRemoteMediaActionReservationOwnsExpiryAndActivationTransition(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	manager.actionTTL = 25 * time.Millisecond
	response, entry, device, identity := prepareRemoteMediaUnitLease(t, manager)
	reservation, err := manager.beginIncomingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		entry.source.Call, device, identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.now = func() time.Time { return entry.expiresAt.Add(time.Second) }
	manager.expireOfferIfGeneration(response.LeaseGeneration)
	if status := manager.snapshot(
		"owner@example.com",
		func(remoteMediaSource) bool { return false },
		func(callMediaTicket) bool { return false },
	); status.Phase == "expired" || status.Phase == "stale_call" {
		t.Fatalf("reserved ATA transition was torn down by offer/call snapshot: %+v", status)
	}
	manager.finishIncomingAction(reservation, true)

	ticket := callMediaTicket{
		CallID: entry.source.Call.CallID, Generation: 9, Index: entry.source.Call.CallIndex,
		Direction: entry.source.Call.CallDirection,
	}
	epoch, err := manager.activate(response.MediaSessionID, response.LeaseGeneration, ticket, "module-hash")
	if err != nil || epoch == 0 {
		t.Fatalf("reserved action could not activate after offer expiry: epoch=%d err=%v", epoch, err)
	}
	time.Sleep(2 * manager.actionTTL)
	manager.mu.Lock()
	stillActive := manager.active == entry && entry.activationEpoch == epoch
	manager.mu.Unlock()
	if !stillActive {
		t.Fatal("stopped action timer closed the activated exact generation")
	}
}

func TestRemoteMediaFinalGateRechecksExpiryAfterReadOnlyAuthorization(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	response, entry, device, identity := prepareRemoteMediaUnitLease(t, manager)
	if err := manager.authorizeIncomingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration, entry.source.Call,
	); err != nil {
		t.Fatalf("initial read-only authorization failed: %v", err)
	}
	manager.now = func() time.Time { return entry.expiresAt }
	if reservation, err := manager.beginIncomingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		entry.source.Call, device, identity,
	); err == nil || reservation.valid() {
		t.Fatalf("expired lease crossed final ATA gate: reservation=%+v err=%v", reservation, err)
	}
	manager.expireOfferIfGeneration(response.LeaseGeneration)
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("expired lease remained published after its exact expiry callback")
	}
}

func TestRemoteMediaUnactivatedActionReservationTimesOut(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	manager.actionTTL = 15 * time.Millisecond
	response, entry, device, identity := prepareRemoteMediaUnitLease(t, manager)
	reservation, err := manager.beginIncomingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		entry.source.Call, device, identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.finishIncomingAction(reservation, true)
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		active := manager.active
		manager.mu.Unlock()
		if active == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unactivated successful/ambiguous action outlived its bounded timer")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRemoteMediaActionTimerIsExactGenerationAndTokenScoped(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	response, old, device, identity := prepareRemoteMediaUnitLease(t, manager)
	reservation, err := manager.beginIncomingAction(
		"owner@example.com", response.MediaSessionID, response.LeaseGeneration,
		old.source.Call, device, identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &remoteMediaLease{generation: old.generation + 1}
	manager.mu.Lock()
	manager.active = replacement
	manager.mu.Unlock()
	manager.expireActionIfReservation(reservation)
	manager.mu.Lock()
	got := manager.active
	manager.mu.Unlock()
	if got != replacement {
		t.Fatal("old action finalizer closed a replacement generation")
	}
	closeRemoteMediaLease(old)
}

func TestRemoteMediaActivationRequiresReservedCallAction(t *testing.T) {
	manager := newRemoteMediaUnitManager(t)
	response, entry, _, _ := prepareRemoteMediaUnitLease(t, manager)
	ticket := callMediaTicket{
		CallID: entry.source.Call.CallID, Generation: 10, Index: entry.source.Call.CallIndex,
		Direction: entry.source.Call.CallDirection,
	}
	if epoch, err := manager.activate(response.MediaSessionID, response.LeaseGeneration, ticket, "module-hash"); err == nil || epoch != 0 {
		t.Fatalf("activation without a reserved ATA action succeeded: epoch=%d err=%v", epoch, err)
	}
}

type remoteMediaShortWriter struct{}

func (remoteMediaShortWriter) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	return len(payload) - 1, nil
}

func TestRemoteMediaControlSuccessACKShortWriteRevokesOnlyExactGeneration(t *testing.T) {
	if err := writeRemoteMediaControlLine(remoteMediaShortWriter{}, remoteMediaControlSimpleEnvelope{OK: true}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error=%v, want io.ErrShortWrite", err)
	}
	manager := &remoteMediaManager{now: time.Now}
	entry := &remoteMediaLease{generation: 41}
	manager.active = entry
	server := &remoteMediaControlServer{app: &app{remoteMedia: manager}}
	if err := server.writeRemoteMediaMutationSuccess(
		remoteMediaShortWriter{}, 41, remoteMediaControlSimpleEnvelope{OK: true},
	); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("mutation ACK short write error=%v", err)
	}
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("hidden successful mutation did not revoke its exact media generation")
	}

	replacement := &remoteMediaLease{generation: 42}
	manager.mu.Lock()
	manager.active = replacement
	manager.mu.Unlock()
	_ = server.writeRemoteMediaMutationSuccess(
		remoteMediaShortWriter{}, 41, remoteMediaControlSimpleEnvelope{OK: true},
	)
	manager.mu.Lock()
	active = manager.active
	manager.mu.Unlock()
	if active != replacement {
		t.Fatal("late ACK failure from old generation closed its ABA replacement")
	}
}

func TestRemoteMediaRootTypesRedactFormattingAndJSON(t *testing.T) {
	manager := &remoteMediaManager{}
	lease := &remoteMediaLease{
		identity: "owner-secret@example.com", sessionID: "session-secret",
		clientNonce: "nonce-secret", answerSDP: []byte("sdp-secret"),
		uacUIDDigest: strings.Repeat("d", 64),
	}
	copy(lease.requestHash[:], []byte("request-hash-secret"))
	manager.active = lease
	control := &remoteMediaControlLeaseResponse{
		MediaSessionID: "session-secret", PCMSocketPath: "/tmp/socket-secret",
		PCMTokenBase64: "token-secret",
	}
	formatted := fmt.Sprintf("%+v %#v %+v %#v %+v %#v", manager, manager, lease, lease, control, control)
	managerJSON, err := json.Marshal(manager)
	if err != nil {
		t.Fatal(err)
	}
	leaseJSON, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	combined := formatted + string(managerJSON) + string(leaseJSON)
	for _, secret := range []string{
		"owner-secret", "session-secret", "nonce-secret", "sdp-secret",
		"request-hash-secret", strings.Repeat("d", 64), "/tmp/socket-secret", "token-secret",
	} {
		if strings.Contains(combined, secret) {
			t.Fatalf("root media formatting/JSON leaked %q: %s", secret, combined)
		}
	}
}
