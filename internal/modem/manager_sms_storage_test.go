package modem

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHandleCMTIUsesIndicatedStorageForReadAndDelete(t *testing.T) {
	m := newRunningTestManager(t)
	validPDU, tpduLen := syntheticDeliverFullPDU(t, "Synthetic privacy-safe SMS fixture")

	done := make(chan []string, 1)
	go func() {
		done <- respondToCommands(t, m, 4, func(req commandRequest) {
			switch req.cmd {
			case "AT+CPMS?":
				req.respChan <- "\r\n+CPMS: \"SM\",0,10,\"SM\",0,10,\"SM\",0,10\r\n\r\nOK\r\n"
			case `AT+CPMS="ME","ME","ME"`:
				req.respChan <- "OK"
			case "AT+CMGR=7":
				req.respChan <- fmt.Sprintf("\r\n+CMGR: 0,,%d\r\n%s\r\n\r\nOK\r\n", tpduLen, validPDU)
			case `AT+CPMS="SM","SM","SM"`:
				req.respChan <- "OK"
			default:
				req.errChan <- nil
			}
		})
	}()

	m.handleURC(`+CMTI: "ME",7`)

	var got []string
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for storage-aware CMTI handling")
	}
	want := []string{
		"AT+CPMS?",
		`AT+CPMS="ME","ME","ME"`,
		"AT+CMGR=7",
		`AT+CPMS="SM","SM","SM"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands=%#v want %#v", got, want)
	}
}

func TestDecodePDUFailureDoesNotExposeRawPayload(t *testing.T) {
	m := newRunningTestManager(t)
	marker := "not-hex-private-payload"
	sender, content, _ := m.decodePDU(marker)
	if sender != "" || content != "" {
		t.Fatalf("invalid PDU became public SMS: sender=%q content_contains_marker=%t", sender, strings.Contains(content, marker))
	}
}

func TestCheckAllSMSRetainsModuleCopiesAfterCallback(t *testing.T) {
	m := newRunningTestManager(t)
	validPDU, tpduLen := syntheticDeliverFullPDU(t, "Synthetic durable SMS fixture")
	callback := make(chan struct{}, 1)
	m.SetSMSCallback(func(_, _ string, _ time.Time) { callback <- struct{}{} })

	done := make(chan []string, 1)
	go func() {
		done <- respondToCommands(t, m, 1, func(req commandRequest) {
			if req.cmd != "AT+CMGL=4" {
				t.Errorf("command=%q want AT+CMGL=4", req.cmd)
			}
			req.respChan <- fmt.Sprintf("\r\n+CMGL: 1,0,,%d\r\n%s\r\n\r\nOK\r\n", tpduLen, validPDU)
		})
	}()

	m.CheckAllSMS()
	select {
	case <-callback:
	case <-time.After(time.Second):
		t.Fatal("SMS callback was not invoked")
	}
	<-done
	select {
	case req := <-m.cmdChan:
		t.Fatalf("automatic command after durable callback is forbidden: %q", req.cmd)
	case <-time.After(50 * time.Millisecond):
	}
}
