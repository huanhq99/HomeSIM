package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func defaultSMSStoreRoot() string {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		return ""
	}
	return filepath.Join(base, "MacCellular", "sms")
}

// initializeSMSStore runs before any modem callback, poller or HTTP listener.
// A failed store leaves incoming module copies untouched and disables outgoing
// SMS; it never falls back to pretending process memory is durable.
func (a *app) initializeSMSStore(root string) error {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) {
		err := errors.New("SMS store path must be an absolute private path")
		a.setSMSStoreError(err)
		return err
	}
	store, err := openSMSStore(root)
	if err != nil {
		a.setSMSStoreError(err)
		return err
	}
	if err := store.RecoverPending(); err != nil {
		_ = store.Close()
		a.setSMSStoreError(err)
		return err
	}
	items, err := store.List(smsStoreMaximumLimit)
	if err != nil {
		_ = store.Close()
		a.setSMSStoreError(err)
		return err
	}
	a.smsMu.Lock()
	a.smsStore = store
	a.smsStoreError = ""
	a.smsMu.Unlock()
	legacy := make([]receivedSMS, 0, len(items))
	for _, item := range items {
		if item.Direction != "incoming" {
			continue
		}
		legacy = append(legacy, receivedSMS{
			Sender: item.Peer, Content: item.Content, Code: item.Code, Timestamp: item.Timestamp,
		})
	}
	a.mergeSMSCache(legacy)
	return nil
}

func (a *app) closeSMSStore() {
	if a == nil {
		return
	}
	a.smsMu.Lock()
	store := a.smsStore
	a.smsStore = nil
	if store != nil {
		a.smsStoreError = "sms_store_unavailable"
	}
	a.smsMu.Unlock()
	if store != nil {
		_ = store.Close()
	}
}

func (a *app) setSMSStoreError(err error) {
	a.smsMu.Lock()
	defer a.smsMu.Unlock()
	if err == nil {
		a.smsStoreError = ""
		return
	}
	a.smsStoreError = "sms_store_unavailable"
}

func (a *app) smsStoreStatus() (available bool, code string) {
	a.smsMu.RLock()
	defer a.smsMu.RUnlock()
	return a.smsStore != nil && a.smsStoreError == "", a.smsStoreError
}

func (a *app) smsStoreConfigured() bool {
	a.smsMu.RLock()
	defer a.smsMu.RUnlock()
	return a.smsStore != nil
}

func (a *app) smsStoreSnapshot() *smsStore {
	a.smsMu.RLock()
	defer a.smsMu.RUnlock()
	if a.smsStoreError != "" {
		return nil
	}
	return a.smsStore
}

func (a *app) noteSMSStoreOperationError(err error) {
	if errors.Is(err, ErrSMSStoreClosed) || errors.Is(err, ErrSMSStoreDegraded) ||
		errors.Is(err, ErrSMSStoreCommitUncertain) {
		a.setSMSStoreError(err)
	}
}

func (a *app) ingestIncomingSMS(messages []receivedSMS) (newCount int, total int, err error) {
	if a.demo {
		newCount, total = a.mergeSMSCache(messages)
		return newCount, total, nil
	}
	store := a.smsStoreSnapshot()
	if store == nil {
		err = errors.New("durable SMS store is unavailable")
		a.setSMSStoreError(err)
		return 0, a.smsCacheCount(), err
	}
	persisted := make([]receivedSMS, 0, len(messages))
	for _, item := range messages {
		if item.Code == "" {
			item.Code = extractSMSCode(item.Content)
		}
		stored, _, ingestErr := store.IngestIncoming(item.Sender, item.Content, item.Code, item.Timestamp)
		if ingestErr != nil {
			a.noteSMSStoreOperationError(ingestErr)
			return 0, a.smsCacheCount(), ingestErr
		}
		persisted = append(persisted, receivedSMS{
			Sender: stored.Peer, Content: stored.Content, Code: stored.Code, Timestamp: stored.Timestamp,
		})
	}
	newCount, total = a.mergeSMSCache(persisted)
	return newCount, total, nil
}

func (a *app) smsCacheCount() int {
	a.smsMu.RLock()
	defer a.smsMu.RUnlock()
	return len(a.sms)
}

func (a *app) beginOutgoingSMS(peer, content string, timestamp time.Time) (StoreMessage, error) {
	store := a.smsStoreSnapshot()
	if store == nil {
		err := errors.New("durable SMS store is unavailable")
		a.setSMSStoreError(err)
		return StoreMessage{}, err
	}
	message, err := store.BeginOutgoing(peer, content, timestamp)
	if err != nil {
		a.noteSMSStoreOperationError(err)
	}
	return message, err
}

func (a *app) updateOutgoingSMS(id, status string, segments int) (StoreMessage, error) {
	store := a.smsStoreSnapshot()
	if store == nil {
		return StoreMessage{}, errors.New("durable SMS store is unavailable")
	}
	message, err := store.UpdateStatus(id, status, segments)
	if err != nil {
		a.noteSMSStoreOperationError(err)
	}
	return message, err
}

// markOutgoingSMSUnknown is the last durable step after a modem operation has
// started but its outcome cannot be proven. If even this terminal state cannot
// be committed, fail the whole SMS store closed so no later send can compound
// an unresolved pending operation.
func (a *app) markOutgoingSMSUnknown(id string, segments int) error {
	_, err := a.updateOutgoingSMS(id, "unknown", segments)
	if err != nil {
		a.setSMSStoreError(err)
	}
	return err
}

func (a *app) syncSMSHistory(cursor string, limit int) (SMSSyncResult, error) {
	store := a.smsStoreSnapshot()
	if store == nil {
		return SMSSyncResult{}, errors.New("durable SMS store is unavailable")
	}
	result, err := store.Sync(cursor, limit)
	if err != nil {
		a.noteSMSStoreOperationError(err)
	}
	return result, err
}

func sortedStoreMessages(items []StoreMessage) []StoreMessage {
	result := append([]StoreMessage(nil), items...)
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Timestamp.Equal(result[j].Timestamp) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].Timestamp.After(result[j].Timestamp)
	})
	return result
}
