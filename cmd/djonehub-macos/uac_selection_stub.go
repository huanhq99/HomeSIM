//go:build !darwin || !cgo

package main

import "errors"

func validateDirectUACUSB(uint32) error {
	return errors.New("UAC USB descriptor validation is available only on macOS")
}
