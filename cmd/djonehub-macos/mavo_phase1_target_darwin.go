//go:build darwin && cgo

package main

import (
	"errors"
	"time"
)

const mavoPhase1ReadOnlyADBProbe = "printf 'uid='; id -u; " +
	"printf 'kernel='; uname -r; " +
	"printf 'arch='; uname -m; " +
	"if test -e /dev/ttyGS0; then printf 'ttygs0=1\\n'; else printf 'ttygs0=0\\n'; fi; " +
	"if test -e /run/voc_svr; then printf 'voc_server=1\\n'; else printf 'voc_server=0\\n'; fi; " +
	"if test -e /usr/bin/alsaucm_test; then printf 'alsaucm_test=1\\n'; else printf 'alsaucm_test=0\\n'; fi; " +
	"if test -e /usr/lib/libql_lib_audio.so.1 || test -e /usr/lib/libql_lib_audio.so; then printf 'audio_library=1\\n'; else printf 'audio_library=0\\n'; fi; " +
	"if test -e /lib/ld-linux.so.3; then printf 'armel_loader=1\\n'; else printf 'armel_loader=0\\n'; fi; " +
	"if grep -q '^qdc507_' /proc/modules; then printf 'qdc_modules_absent=0\\n'; else printf 'qdc_modules_absent=1\\n'; fi"

func inspectMavoPhase1Target(locationID uint32) (mavoPhase1TargetInspection, error) {
	if err := validateDirectUACUSB(locationID); err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	session := &voiceADBSession{locationID: locationID}
	defer session.close()
	output, status, err := session.query(mavoPhase1ReadOnlyADBProbe, 12*time.Second)
	if err != nil {
		return mavoPhase1TargetInspection{}, err
	}
	if status != 0 {
		return mavoPhase1TargetInspection{}, errors.New("read-only ADB target probe did not exit successfully")
	}
	inspection, err := parseMavoPhase1TargetOutput(output)
	if err != nil {
		return inspection, err
	}
	// Reaching this point proves both descriptor-safe ADB and the exact UAC
	// layout are present on the same physical module.
	inspection.ADBDescriptorValid = true
	inspection.UACDescriptorValid = true
	return inspection, nil
}
