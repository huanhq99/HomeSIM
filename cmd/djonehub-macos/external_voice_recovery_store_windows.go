//go:build windows

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

func validateExternalVoiceRecoveryDirectory(path string) error {
	handle, err := openExternalVoiceRecoveryWindowsPath(
		path,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		true,
	)
	if err != nil {
		return err
	}
	return windows.CloseHandle(handle)
}

func openExternalVoiceRecoveryLock(path string) (*os.File, error) {
	handle, err := openExternalVoiceRecoveryWindowsPath(
		path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, false,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "external-voice-recovery-lock")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create external voice recovery lock handle")
	}
	return file, nil
}

func readExternalVoiceRecoveryFile(path string, maxBytes int64) ([]byte, error) {
	handle, err := openExternalVoiceRecoveryWindowsPath(
		path, windows.GENERIC_READ, windows.FILE_SHARE_READ, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, false,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "external-voice-recovery-record")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create external voice recovery record handle")
	}
	defer file.Close()
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
	handle, err := openExternalVoiceRecoveryWindowsPath(
		path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, false,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "external-voice-recovery-temp")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("create external voice recovery temporary handle")
	}
	return file, nil
}

func removeExternalVoiceRecoveryTemp(path string) (bool, error) {
	handle, err := openExternalVoiceRecoveryWindowsPath(
		path, windows.GENERIC_READ|windows.DELETE, 0, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, false,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	if err := validateExternalVoiceRecoveryWindowsSize(handle, externalVoiceRecoveryStoreMaxBytes); err != nil {
		_ = windows.CloseHandle(handle)
		return false, err
	}
	if err := windows.CloseHandle(handle); err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

func validateExternalVoiceRecoveryWindowsSize(handle windows.Handle, maxBytes int64) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size > uint64(maxBytes) {
		return errors.New("external voice recovery file is too large")
	}
	return nil
}

func renameExternalVoiceRecoveryFile(oldPath, newPath string) error {
	oldName, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newName, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(oldName, newName, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func syncExternalVoiceRecoveryDirectory(path string) error {
	return validateExternalVoiceRecoveryDirectory(path)
}

func openExternalVoiceRecoveryWindowsPath(
	path string,
	access uint32,
	shareMode uint32,
	creation uint32,
	flags uint32,
	requireDirectory bool,
) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	handle, err := windows.CreateFile(name, access, shareMode, nil, creation, flags, 0)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, err
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		isDirectory != requireDirectory || !requireDirectory && info.NumberOfLinks != 1 {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, errors.New("external voice recovery path must be a non-reparse single-link object of the expected type")
	}
	return handle, nil
}
