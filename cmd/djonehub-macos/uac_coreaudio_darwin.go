//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const uacPreflightHelperEnvironment = "MACCELLULAR_UAC_PREFLIGHT_HELPER"

func locateUACPreflightHelper() (string, error) {
	candidates := []string{}
	if explicit := os.Getenv(uacPreflightHelperEnvironment); explicit != "" {
		candidates = append(candidates, explicit)
	}
	candidates = append(candidates,
		"/Applications/MacCellular.app/Contents/MacOS/DJOneHubNotifier",
		filepath.Join(os.Getenv("HOME"), "Applications", "MacCellular.app", "Contents", "MacOS", "DJOneHubNotifier"),
	)
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			continue
		}
		return candidate, nil
	}
	return "", errors.New("MacCellular CoreAudio preflight helper is not installed or trusted")
}

// validateDirectUACCoreAudio launches the already-installed native app in a
// non-UI, read-only mode. The Swift helper selects the exact VID/PID/location
// CoreAudio pair and exits without creating an IOProc.
func validateDirectUACCoreAudio(locationID uint32) error {
	if locationID == 0 {
		return errors.New("CoreAudio UAC validation requires a non-zero USB location")
	}
	helper, err := locateUACPreflightHelper()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, helper, "--uac-preflight", "0x2c7c", "0x0125", fmt.Sprintf("0x%08x", locationID))
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("CoreAudio UAC preflight timed out")
	}
	if err != nil {
		return fmt.Errorf("CoreAudio UAC preflight failed: %s", firstNonEmpty(string(output), err.Error()))
	}
	return nil
}
