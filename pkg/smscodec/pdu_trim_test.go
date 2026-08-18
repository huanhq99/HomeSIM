package smscodec

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	smspdu "github.com/warthog618/sms"
)

const syntheticSMSBody = "Synthetic privacy-safe SMS fixture"

func syntheticDeliverPDU(t *testing.T, body string) ([]byte, string, int) {
	t.Helper()
	tpdus, err := smspdu.Encode([]byte(body), smspdu.AsDeliver, smspdu.From("101"))
	if err != nil {
		t.Fatalf("encode synthetic SMS fixture: %v", err)
	}
	if len(tpdus) != 1 {
		t.Fatalf("synthetic fixture segments=%d want 1", len(tpdus))
	}
	tpduBytes, err := tpdus[0].MarshalBinary()
	if err != nil {
		t.Fatalf("marshal synthetic SMS fixture: %v", err)
	}
	full := append([]byte{0}, tpduBytes...)
	return tpduBytes, strings.ToUpper(hex.EncodeToString(full)), len(tpduBytes)
}

func syntheticConcatDeliverPDU(t *testing.T) (string, int) {
	t.Helper()
	body := strings.Repeat("SYNTHETIC-FIXTURE-", 30)
	tpdus, err := smspdu.Encode([]byte(body), smspdu.AsDeliver, smspdu.From("101"))
	if err != nil {
		t.Fatalf("encode synthetic multipart SMS fixture: %v", err)
	}
	if len(tpdus) != 4 {
		t.Fatalf("synthetic multipart segments=%d want 4", len(tpdus))
	}
	tpduBytes, err := tpdus[1].MarshalBinary()
	if err != nil {
		t.Fatalf("marshal synthetic multipart SMS fixture: %v", err)
	}
	full := append([]byte{0}, tpduBytes...)
	return strings.ToUpper(hex.EncodeToString(full)), len(tpduBytes)
}

func TestTrimFullPDUHexByTPDULengthRemovesStoragePadding(t *testing.T) {
	_, pdu, tpduLen := syntheticDeliverPDU(t, syntheticSMSBody)
	padded := pdu + strings.Repeat("00", 128)
	got, trimmed := TrimFullPDUHexByTPDULength(padded, tpduLen)
	if !trimmed {
		t.Fatal("TrimFullPDUHexByTPDULength trimmed=false, want true")
	}
	if got != pdu {
		t.Fatalf("trimmed PDU mismatch\ngot  %s\nwant %s", got, pdu)
	}
}

func TestTrimFullPDUHexByTPDULengthFallsBackToTPDUDeclaredLength(t *testing.T) {
	pdu, tpduLen := syntheticConcatDeliverPDU(t)
	padded := pdu + strings.Repeat("00", 128)
	got, trimmed := TrimFullPDUHexByTPDULength(padded, tpduLen+64)
	if !trimmed {
		t.Fatal("TrimFullPDUHexByTPDULength trimmed=false, want true")
	}
	if got != pdu {
		t.Fatalf("trimmed PDU mismatch\ngot  %s\nwant %s", got, pdu)
	}
}

func TestDecodeDeliverTPDUTrimsFixedSlotPadding(t *testing.T) {
	pdu, _ := syntheticConcatDeliverPDU(t)
	padded := pdu + strings.Repeat("00", 128)
	b, err := hexStringToBytesForTest(padded)
	if err != nil {
		t.Fatal(err)
	}
	smscLen := int(b[0])
	tpduBytes := b[1+smscLen:]

	_, text, _, concat, err := DecodeDeliverTPDU(tpduBytes)
	if err != nil {
		t.Fatalf("DecodeDeliverTPDU() error = %v", err)
	}
	if text == "" {
		t.Fatal("DecodeDeliverTPDU() text is empty")
	}
	if !concat.IsConcat || concat.Total != 4 || concat.Seq != 2 {
		t.Fatalf("concat=%+v, want total=4 seq=2", concat)
	}
}

func TestDecodeDeliverTPDUAcceptsNonZeroGSM7SpareBits(t *testing.T) {
	tpduBytes, _, _ := syntheticDeliverPDU(t, syntheticSMSBody)
	tpduBytes[len(tpduBytes)-1] |= 0x80

	sender, text, _, concat, err := DecodeDeliverTPDU(tpduBytes)
	if err != nil {
		t.Fatalf("DecodeDeliverTPDU() error = %v", err)
	}
	if sender != "+101" {
		t.Fatalf("sender=%q want synthetic short code", sender)
	}
	if text != syntheticSMSBody {
		t.Fatalf("text=%q want synthetic fixture", text)
	}
	if concat.IsConcat {
		t.Fatalf("concat=%+v, want non-concat", concat)
	}
}

func TestParseATSMSHeaderTPDULengthUsesLastNumericField(t *testing.T) {
	got, ok := ParseATSMSHeaderTPDULength(`+CMGL: 7,1,,38`)
	if !ok || got != 38 {
		t.Fatalf("ParseATSMSHeaderTPDULength()=(%d,%v), want (38,true)", got, ok)
	}
}

func TestTrimFullPDUHexByATHeaderKeepsRawWhenHeaderLengthMissing(t *testing.T) {
	_, pdu, _ := syntheticDeliverPDU(t, syntheticSMSBody)
	padded := pdu + "00"
	got, trimmed := TrimFullPDUHexByATHeader(padded, `+CMGR: 0`)
	if trimmed {
		t.Fatal("TrimFullPDUHexByATHeader trimmed=true, want false")
	}
	if got != padded {
		t.Fatalf("got %q want original %q", got, padded)
	}
}

func TestTrimFullPDUHexByATHeaderUsesSyntheticLength(t *testing.T) {
	_, pdu, tpduLen := syntheticDeliverPDU(t, syntheticSMSBody)
	padded := pdu + strings.Repeat("00", 16)
	header := fmt.Sprintf("+CMGR: 0,,%d", tpduLen)
	got, trimmed := TrimFullPDUHexByATHeader(padded, header)
	if !trimmed || got != pdu {
		t.Fatalf("synthetic header trim=(%t,%d bytes), want true and exact fixture", trimmed, len(got)/2)
	}
}

func hexStringToBytesForTest(s string) ([]byte, error) {
	return hex.DecodeString(s)
}
