//go:build windows

package main

import "errors"

func readExternalVoicePasswordFile(string, int64) ([]byte, error) {
	return nil, errors.New("external voice password files are not supported on Windows")
}
