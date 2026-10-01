//go:build linux

package main

import (
	"net"
	"syscall"
	"time"
)

func boundDialer(iface string) *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		if e := c.Control(func(fd uintptr) {
			err = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
		}); e != nil {
			return e
		}
		return err
	}}
}
