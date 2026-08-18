//go:build !windows

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readPrivateEdgeFile(path string, maximum int64) ([]byte, error) {
	if path == "" || maximum <= 0 {
		return nil, errEdgeConfiguration
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, errEdgeConfiguration
	}
	file := os.NewFile(uintptr(fd), "public-edge-private-input")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errEdgeConfiguration
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(unix.Geteuid()) || stat.Mode&0o777 != 0o600 ||
		stat.Size < 1 || stat.Size > maximum {
		return nil, errEdgeConfiguration
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		zeroEdgeBytes(data)
		return nil, errors.Join(errEdgeConfiguration, err)
	}
	return data, nil
}
