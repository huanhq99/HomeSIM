package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPublicWebPushLiveDelivery is an explicit live gate for the installed
// browser subscription. It sends only the fixed incoming_call envelope and
// never logs endpoint, key, principal, or VAPID material. Normal test runs
// skip it and therefore remain hermetic.
func TestPublicWebPushLiveDelivery(t *testing.T) {
	if os.Getenv("MACCELLULAR_PUBLIC_WEB_PUSH_E2E") != "1" {
		t.Skip("set MACCELLULAR_PUBLIC_WEB_PUSH_E2E=1 for the explicit live push gate")
	}
	privateKeyPath := os.Getenv("MACCELLULAR_PUBLIC_WEB_PUSH_VAPID_KEY_FILE")
	storePath := os.Getenv("MACCELLULAR_PUBLIC_WEB_PUSH_SUBSCRIPTIONS_FILE")
	if !filepath.IsAbs(privateKeyPath) || filepath.Clean(privateKeyPath) != privateKeyPath ||
		!filepath.IsAbs(storePath) || filepath.Clean(storePath) != storePath {
		t.Fatal("live push paths must be clean and absolute")
	}
	privateKey, publicKey, err := readPublicWebPushVAPIDPrivateKey(privateKeyPath)
	if err != nil {
		t.Fatal("live push VAPID key is unavailable")
	}
	data, err := readPrivateRegular(storePath, publicWebPushMaximumStoreBytes)
	if err != nil {
		t.Fatal("live push subscription store is unavailable")
	}
	var store publicWebPushDiskStore
	if decodePublicWebPushJSON(data, &store) != nil || store.Version != publicWebPushAPIVersion ||
		len(store.Subscriptions) == 0 || len(store.Subscriptions) > publicWebPushMaximumTotal {
		t.Fatal("live push subscription store is invalid or empty")
	}
	sender := &publicWebPushDeliveryClient{
		privateKey: privateKey,
		publicKey:  publicKey,
		subject:    "https://phone.example.com",
		httpClient: newPublicWebPushHTTPClient(),
	}
	for _, subscription := range store.Subscriptions {
		if validatePublicWebPushDiskSubscription(subscription, time.Now()) != nil {
			t.Fatal("live push subscription is invalid")
		}
		ctx, cancel := context.WithTimeout(context.Background(), publicWebPushDeliveryTimeout)
		status, sendErr := sender.Send(ctx, subscription, publicWebIncomingCallPayload)
		cancel()
		if sendErr != nil || status < 200 || status >= 300 {
			t.Fatalf("live push delivery failed: provider=%s status=%d reason=%s",
				publicWebPushProvider(subscription.Endpoint), status,
				publicWebPushProviderFailureReason(sendErr))
		}
	}
	t.Logf("live push delivery accepted for %d subscription(s)", len(store.Subscriptions))
}
