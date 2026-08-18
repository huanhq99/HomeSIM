//go:build !darwin || !cgo

package main

import "errors"

func inspectMavoPhase1Target(uint32) (mavoPhase1TargetInspection, error) {
	return mavoPhase1TargetInspection{}, errors.New("MaVo phase-1 target inspection requires macOS arm64-cgo with libusb")
}
