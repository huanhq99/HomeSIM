//go:build darwin && cgo

package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeVoiceRouteResult struct {
	output string
	status int
	err    error
}

type fakeVoiceRouteSession struct {
	processes    [][]voiceHelperProcess
	processIndex int
	queryResults []fakeVoiceRouteResult
	shellResults []fakeVoiceRouteResult
	commands     []string
}

func (s *fakeVoiceRouteSession) nextProcesses() []voiceHelperProcess {
	if len(s.processes) == 0 {
		return nil
	}
	index := s.processIndex
	if index >= len(s.processes) {
		index = len(s.processes) - 1
	} else {
		s.processIndex++
	}
	return s.processes[index]
}

func (s *fakeVoiceRouteSession) query(command string, _ time.Duration) (string, int, error) {
	s.commands = append(s.commands, command)
	if strings.Contains(command, voiceHelperScanMarker) {
		if len(s.queryResults) > 0 {
			result := s.queryResults[0]
			s.queryResults = s.queryResults[1:]
			return result.output, result.status, result.err
		}
		var output strings.Builder
		for _, process := range s.nextProcesses() {
			output.WriteString(strings.TrimSpace(strings.Join([]string{
				formatVoiceTestUint(process.PID),
				formatVoiceTestUint(process.StartTime),
			}, " ")))
			output.WriteByte('\n')
		}
		return output.String(), 0, nil
	}
	if len(s.queryResults) == 0 {
		return "", 0, nil
	}
	result := s.queryResults[0]
	s.queryResults = s.queryResults[1:]
	return result.output, result.status, result.err
}

func (s *fakeVoiceRouteSession) shell(command string, _ time.Duration) (string, int, error) {
	s.commands = append(s.commands, command)
	if len(s.shellResults) == 0 {
		return "", 0, nil
	}
	result := s.shellResults[0]
	s.shellResults = s.shellResults[1:]
	return result.output, result.status, result.err
}

func formatVoiceTestUint(value uint64) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%10]
		value /= 10
	}
	return string(buffer[index:])
}

func commandContaining(commands []string, marker string) string {
	for _, command := range commands {
		if strings.Contains(command, marker) {
			return command
		}
	}
	return ""
}

func TestOutputHasExactField(t *testing.T) {
	if !outputHasExactField("0\r\n", "0") {
		t.Fatal("exact root field rejected")
	}
	if outputHasExactField("1000\r\n", "0") {
		t.Fatal("uid 1000 accepted as root")
	}
	if !outputHasExactField("3.18.44\r\n", "3.18.44") {
		t.Fatal("exact kernel field rejected")
	}
}

func TestParseVoiceHelperProcesses(t *testing.T) {
	processes, err := parseVoiceHelperProcesses("17 998\r\n42 12345\r\n\r\n__MAVO_STATUS_0123456789abcdef_0__\r\n")
	if err != nil {
		t.Fatalf("parseVoiceHelperProcesses() error = %v", err)
	}
	want := []voiceHelperProcess{{PID: 17, StartTime: 998}, {PID: 42, StartTime: 12345}}
	if len(processes) != len(want) || processes[0] != want[0] || processes[1] != want[1] {
		t.Fatalf("processes = %#v, want %#v", processes, want)
	}
	for _, invalid := range []string{"17", "0 9", "17 0", "x 9", "17 x", "17 9\n17 10\n"} {
		if _, err := parseVoiceHelperProcesses(invalid); err == nil {
			t.Fatalf("parseVoiceHelperProcesses(%q) accepted invalid output", invalid)
		}
	}
}

func TestVoiceHelperScanCommandMatchesExactArgv(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	command := voiceHelperProcessScanCommand(helper)
	for _, required := range []string{
		"for proc in /proc/[0-9]*",
		"test \"$argv0\" = \"$expected_helper\"",
		"grep -q '^--voice-route-session$'",
		"cut -d ' ' -f 22 \"$proc/stat\"",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("scan command missing %q: %s", required, command)
		}
	}
	if strings.Contains(command, "pgrep") || strings.Contains(command, "grep -F "+helper) {
		t.Fatalf("scan command uses an inexact process matcher: %s", command)
	}
	if strings.Contains(command, voiceRoutePIDFile) {
		t.Fatalf("scan command trusts pidfile; absent/corrupt pidfiles must not hide helpers: %s", command)
	}
}

func TestVoiceHelperShellCommandsParse(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	runtime := &externalVoiceRuntime{manifest: voiceRuntimeManifest{Helper: trustedVoiceHelperName}}
	commands := map[string]string{
		"scan":      voiceHelperProcessScanCommand(helper),
		"ready":     voiceRouteReadyCommand(runtime),
		"terminate": voiceHelperTerminateCommand(helper, voiceHelperProcess{PID: 31, StartTime: 4001}),
		"launch":    voiceHelperLaunchCommand(helper),
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			if output, err := exec.Command("/bin/sh", "-n", "-c", command).CombinedOutput(); err != nil {
				t.Fatalf("shell command does not parse: %v\n%s\n%s", err, output, command)
			}
		})
	}
}

func TestVoiceHelperMarkersExecuteInsteadOfCommentingOutCommand(t *testing.T) {
	markers := map[string]string{
		"scan":      voiceHelperScanMarker,
		"ready":     voiceHelperReadyMarker,
		"terminate": voiceHelperTerminateMarker,
		"launch":    voiceHelperLaunchMarker,
		"cleanup":   voiceHelperCleanupMarker,
	}
	for name, marker := range markers {
		t.Run(name, func(t *testing.T) {
			command := marker + "; printf marker-ran"
			output, err := exec.Command("/bin/sh", "-c", command).CombinedOutput()
			if err != nil {
				t.Fatalf("marker is not an executable no-op shell statement: %v\n%s", err, output)
			}
			if string(output) != "marker-ran" {
				t.Fatalf("marker swallowed the following safety command: output=%q command=%q", output, command)
			}
		})
	}
}

func TestPrepareVoiceHelperStartTerminatesStaleHelperWithoutPIDFile(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	process := voiceHelperProcess{PID: 31, StartTime: 4001}
	session := &fakeVoiceRouteSession{
		// Initial scan finds the stale helper. The TERM poll and both pre-launch
		// confirmation scans then see zero helpers. No pidfile response is used.
		processes: [][]voiceHelperProcess{{process}, nil, nil, nil},
	}
	if err := prepareVoiceHelperStart(session, helper); err != nil {
		t.Fatalf("prepareVoiceHelperStart() error = %v", err)
	}
	terminate := commandContaining(session.commands, voiceHelperTerminateMarker)
	if terminate == "" || !strings.Contains(terminate, "kill -TERM 31") {
		t.Fatalf("stale helper was not terminated precisely: %q", terminate)
	}
	if strings.Contains(terminate, "KILL") {
		t.Fatalf("terminate command escalated to SIGKILL: %s", terminate)
	}
	cleanup := commandContaining(session.commands, "rm -f '"+voiceRoutePIDFile+"'")
	if cleanup == "" {
		t.Fatal("missing/corrupt pidfile state was not cleaned after process verification")
	}
}

func TestPrepareVoiceHelperStartFailsClosedOnMultipleHelpers(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	session := &fakeVoiceRouteSession{processes: [][]voiceHelperProcess{{
		{PID: 31, StartTime: 4001},
		{PID: 32, StartTime: 4002},
	}}}
	err := prepareVoiceHelperStart(session, helper)
	if err == nil || !strings.Contains(err.Error(), "2 个") {
		t.Fatalf("prepareVoiceHelperStart() error = %v, want multiple-instance failure", err)
	}
	if commandContaining(session.commands, voiceHelperTerminateMarker) != "" ||
		commandContaining(session.commands, voiceHelperLaunchMarker) != "" {
		t.Fatalf("multiple helpers caused a mutation: %#v", session.commands)
	}
}

func TestPrepareVoiceHelperStartFailsClosedWhenScanCannotBeVerified(t *testing.T) {
	session := &fakeVoiceRouteSession{queryResults: []fakeVoiceRouteResult{{err: errors.New("transport lost")}}}
	_, err := scanVoiceHelperProcesses(session, voiceRemoteDirectory+"/"+trustedVoiceHelperName)
	if err == nil || !strings.Contains(err.Error(), "transport lost") {
		t.Fatalf("scanVoiceHelperProcesses() error = %v, want transport failure", err)
	}
}

func TestStopExternalVoiceRouteFindsHelperWithAbsentOrCorruptPIDFile(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	for _, name := range []string{"absent pidfile", "corrupt pidfile"} {
		t.Run(name, func(t *testing.T) {
			process := voiceHelperProcess{PID: 41, StartTime: 5001}
			session := &fakeVoiceRouteSession{
				processes: [][]voiceHelperProcess{{process}, nil, nil, nil},
			}
			if err := stopExternalVoiceRoute(session, helper); err != nil {
				t.Fatalf("stopExternalVoiceRoute() error = %v", err)
			}
			terminate := commandContaining(session.commands, voiceHelperTerminateMarker)
			if terminate == "" || !strings.Contains(terminate, "kill -TERM 41") {
				t.Fatalf("live helper was not found independently of pidfile: %q", terminate)
			}
			for _, command := range session.commands {
				if strings.Contains(command, voiceHelperTerminateMarker) {
					break
				}
				if strings.Contains(command, voiceRoutePIDFile) {
					t.Fatalf("%s scan trusted the pidfile before finding the helper: %s", name, command)
				}
			}
			cleanup := commandContaining(session.commands, voiceHelperCleanupMarker)
			if cleanup == "" || !strings.Contains(cleanup, "audio_enable)\" = 0") {
				t.Fatalf("audio_enable=0 was not verified: %q", cleanup)
			}
		})
	}
}

func TestStopExternalVoiceRouteWithNoHelperStillVerifiesAudioDisabled(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	session := &fakeVoiceRouteSession{processes: [][]voiceHelperProcess{nil, nil}}
	if err := stopExternalVoiceRoute(session, helper); err != nil {
		t.Fatalf("stopExternalVoiceRoute() error = %v", err)
	}
	if commandContaining(session.commands, voiceHelperTerminateMarker) != "" {
		t.Fatalf("zero-helper stop sent TERM: %#v", session.commands)
	}
	if commandContaining(session.commands, voiceHelperCleanupMarker) == "" {
		t.Fatal("zero-helper stop did not verify audio_enable=0")
	}
}

func TestStopExternalVoiceRouteRejectsMultipleHelpers(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	session := &fakeVoiceRouteSession{processes: [][]voiceHelperProcess{{
		{PID: 51, StartTime: 6001},
		{PID: 52, StartTime: 6002},
	}}}
	err := stopExternalVoiceRoute(session, helper)
	if err == nil || !strings.Contains(err.Error(), "2 个") {
		t.Fatalf("stopExternalVoiceRoute() error = %v, want multiple-instance failure", err)
	}
	if commandContaining(session.commands, voiceHelperTerminateMarker) != "" ||
		commandContaining(session.commands, voiceHelperCleanupMarker) != "" {
		t.Fatalf("ambiguous helpers caused a mutation: %#v", session.commands)
	}
}

func TestStopExternalVoiceRouteDoesNotReportFailedAudioDisableAsSuccess(t *testing.T) {
	helper := voiceRemoteDirectory + "/" + trustedVoiceHelperName
	failed := fakeVoiceRouteResult{output: "read-only", status: 1}
	session := &fakeVoiceRouteSession{
		processes:    [][]voiceHelperProcess{nil},
		shellResults: []fakeVoiceRouteResult{failed, failed, failed, failed, failed},
	}
	err := stopExternalVoiceRoute(session, helper)
	if err == nil || !strings.Contains(err.Error(), "路由回滚未确认") {
		t.Fatalf("stopExternalVoiceRoute() error = %v, want audio rollback failure", err)
	}
}
