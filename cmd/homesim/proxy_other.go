//go:build !linux

package main

import (
	"errors"
	"net"
	"syscall"
)

func boundDialer(iface string) *net.Dialer {
	return &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error { return errors.New("USB interface binding requires Linux") }}
}
