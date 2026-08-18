package main

import (
	"strings"
	"testing"
	"time"
)

func TestRemoteRescueHangupRejectsAmbiguousCLCCBeforeATH(t *testing.T) {
	tests := []struct {
		name string
		clcc string
	}{
		{name: "valid prefix trailing garbage", clcc: "+CLCC: 1,1,0,0,0,\"\",129 garbage\r\nOK"},
		{name: "unknown direction", clcc: "+CLCC: 1,9,0,0,0,\"\",129\r\nOK"},
		{name: "unknown state", clcc: "+CLCC: 1,1,9,0,0,\"\",129\r\nOK"},
		{name: "malformed extra row", clcc: "+CLCC: 1,1,0,0,0,\"\",129\r\n+CLCC: garbage\r\nOK"},
		{name: "unrelated line", clcc: "+CLCC: 1,1,0,0,0,\"\",129\r\nNO CARRIER\r\nOK"},
		{name: "valid second call", clcc: "+CLCC: 1,1,0,0,0,\"\",129\r\n+CLCC: 2,0,1,0,0,\"\",129\r\nOK"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRemoteRescueExecutionFixture(t)
			fixture.app.usbATPinnedSessionOverride = func(
				_ uint32,
				operation func(*usbAT, usbATPhysicalIdentity, usbATCommandFunc) error,
			) error {
				return operation(fixture.device, fixture.physical, func(command string, _ time.Duration) (string, error) {
					fixture.commands = append(fixture.commands, command)
					switch command {
					case "AT+CGSN":
						return remoteRescueTestIMEI + "\r\nOK\r\n", nil
					case "AT+CLCC":
						return test.clcc, nil
					default:
						return "ERROR\r\n", nil
					}
				})
			}

			reservation, err := fixture.execute()
			if err == nil || reservation.valid() {
				t.Fatalf("ambiguous CLCC crossed rescue gate: reservation=%+v err=%v", reservation, err)
			}
			for _, command := range fixture.commands {
				if command == "ATH" || command == "AT+CHUP" {
					t.Fatalf("ambiguous CLCC emitted mutation %q; commands=%s", command, strings.Join(fixture.commands, ","))
				}
			}
		})
	}
}
