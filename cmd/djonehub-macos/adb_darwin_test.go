//go:build darwin && cgo

package main

import (
	"encoding/hex"
	"testing"
)

func TestADBTokenIsRandomHex(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		token := adbToken()
		if len(token) < 16 {
			t.Fatalf("token too short: %q", token)
		}
		if _, err := hex.DecodeString(token); err != nil {
			t.Fatalf("token is not hex: %q: %v", token, err)
		}
		if _, exists := seen[token]; exists {
			t.Fatalf("duplicate token: %q", token)
		}
		seen[token] = struct{}{}
	}
}
