package manager

import (
	"bytes"
	"strings"
	"testing"

	smspdu "github.com/warthog618/sms"
)

func syntheticPaddedDeliverTPDU(t *testing.T) ([]byte, []byte) {
	t.Helper()
	body := strings.Repeat("SYNTHETIC-FIXTURE-", 30)
	tpdus, err := smspdu.Encode([]byte(body), smspdu.AsDeliver, smspdu.From("101"))
	if err != nil {
		t.Fatalf("encode synthetic multipart SMS fixture: %v", err)
	}
	if len(tpdus) != 4 {
		t.Fatalf("synthetic multipart segments=%d want 4", len(tpdus))
	}
	declared, err := tpdus[1].MarshalBinary()
	if err != nil {
		t.Fatalf("marshal synthetic multipart SMS fixture: %v", err)
	}
	full := append([]byte{0}, declared...)
	padded := append(full, make([]byte, 64)...)
	return padded, declared
}

func TestTrimDeliverTPDUToDeclaredLengthRemovesFixedSlotPadding(t *testing.T) {
	fullPadded, declared := syntheticPaddedDeliverTPDU(t)
	got, trimmed := trimDeliverTPDUToDeclaredLength(fullPadded[1:])
	if !trimmed {
		t.Fatal("trimDeliverTPDUToDeclaredLength trimmed=false, want true")
	}
	if !bytes.Equal(got, declared) {
		t.Fatalf("trimmed TPDU length=%d want %d", len(got), len(declared))
	}
}
