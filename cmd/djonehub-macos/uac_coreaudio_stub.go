//go:build !darwin

package main

import "errors"

func validateDirectUACCoreAudio(uint32) error {
	return errors.New("CoreAudio UAC validation is available only on macOS")
}
