//go:build !windows

package main

func initPlatformRuntime() func() { return func() {} }

func openPlatformUI(string) {}

func platformOpenExistingUI(string) bool { return false }
