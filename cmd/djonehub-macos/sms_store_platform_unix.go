//go:build !windows

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func openSMSStoreLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create SMS store lock file handle")
	}
	if err := validateUnixRegularFD(fd, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func readPrivateRegular(path string, maxBytes int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create SMS store file handle")
	}
	defer file.Close()
	if err := validateUnixRegularFD(fd, true); err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("store file is too large")
	}
	return data, nil
}

func validatePrivateTemp(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return errors.New("temporary artifact must be a single-link regular file")
	}
	if stat.Mode&0o777 != 0o600 {
		return errors.New("temporary artifact must have mode 0600")
	}
	if stat.Size > smsStoreMaxFileBytes {
		return errors.New("temporary artifact is too large")
	}
	return nil
}

func validateUnixRegularFD(fd int, requireSingleLink bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("store file must be a regular file")
	}
	if requireSingleLink && stat.Nlink != 1 {
		return errors.New("store file must have exactly one link")
	}
	return nil
}

func renameSMSStoreFile(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func syncSMSStoreDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
