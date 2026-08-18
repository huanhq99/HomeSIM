package main

import "testing"

func TestParseGPSLocation(t *testing.T) {
	fix, err := parseGPSLocation("AT+QGPSLOC=2\r\n+QGPSLOC: 120000.0,39.900000,116.400000,0.8,42.1,1,0.0,0.0,0.0,01012026,12\r\nOK")
	if err != nil {
		t.Fatalf("parseGPSLocation() error = %v", err)
	}
	if fix.Latitude != "39.900000" || fix.Longitude != "116.400000" || fix.Satellites != "12" {
		t.Fatalf("parseGPSLocation() = %+v", fix)
	}
}

func TestParseGPSLocationWithoutFix(t *testing.T) {
	if _, err := parseGPSLocation("AT+QGPSLOC=2\r\nERROR"); err == nil {
		t.Fatal("parseGPSLocation() error = nil, want no-fix error")
	}
}
