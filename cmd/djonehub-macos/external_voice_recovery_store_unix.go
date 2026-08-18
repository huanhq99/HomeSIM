//go:build !windows

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func validateExternalVoiceRecoveryDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(unix.Geteuid()) {
		return errors.New("external voice recovery directory must be owned by the current user")
	}
	if stat.Mode&0o777 != 0o700 {
		return errors.New("external voice recovery directory must have mode 0700")
	}
	return nil
}

func openExternalVoiceRecoveryLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "external-voice-recovery-lock")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create external voice recovery lock handle")
	}
	if err := validateExternalVoiceRecoveryUnixFile(fd, true, -1, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateExternalVoiceRecoveryUnixFile(fd, true, -1, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func readExternalVoiceRecoveryFile(path string, maxBytes int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "external-voice-recovery-record")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create external voice recovery record handle")
	}
	defer file.Close()
	if err := validateExternalVoiceRecoveryUnixFile(fd, true, maxBytes, true); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("external voice recovery record is too large")
	}
	return data, nil
}

func createExternalVoiceRecoveryTemp(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "external-voice-recovery-temp")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create external voice recovery temporary handle")
	}
	if err := validateExternalVoiceRecoveryUnixFile(fd, true, externalVoiceRecoveryStoreMaxBytes, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateExternalVoiceRecoveryUnixFile(fd, true, externalVoiceRecoveryStoreMaxBytes, true); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func removeExternalVoiceRecoveryTemp(path string) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	if err := validateExternalVoiceRecoveryUnixFile(fd, true, externalVoiceRecoveryStoreMaxBytes, true); err != nil {
		_ = unix.Close(fd)
		return false, err
	}
	if err := unix.Close(fd); err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

func validateExternalVoiceRecoveryUnixFile(fd int, requireSingleLink bool, maxBytes int64, requireMode bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(unix.Geteuid()) {
		return errors.New("external voice recovery file must be a current-user regular file")
	}
	if requireSingleLink && stat.Nlink != 1 {
		return errors.New("external voice recovery file must have exactly one link")
	}
	if requireMode && stat.Mode&0o777 != 0o600 {
		return errors.New("external voice recovery file must have mode 0600")
	}
	if stat.Size < 0 || maxBytes >= 0 && stat.Size > maxBytes {
		return errors.New("external voice recovery file is too large")
	}
	return nil
}

func renameExternalVoiceRecoveryFile(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func syncExternalVoiceRecoveryDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
