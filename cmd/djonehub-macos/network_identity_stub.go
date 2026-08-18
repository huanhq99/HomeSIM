//go:build !darwin || !cgo

package main

func isVerifiedDJINetworkInterfaceAtLocation(_ string, _ uint32) bool { return false }
