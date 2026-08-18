package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const liveGateEnvironment = "MACCELLULAR_PUBLIC_TURN_E2E"

type probeFunc func(string, string, probeTransport) (probeResult, error)
type stunControlFunc func(string) stunControlResult

func main() {
	os.Exit(runCLI(
		os.Args[1:], os.Getenv(liveGateEnvironment), liveProbe, liveSTUNControl, os.Stdout, os.Stderr,
	))
}

func runCLI(
	args []string,
	liveGate string,
	probe probeFunc,
	control stunControlFunc,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		printUsage(stdout)
		return 0
	}

	if liveGate != "1" {
		if liveGate != "" && liveGate != "0" {
			fmt.Fprintf(stderr, "ERROR: %s must be exactly 1 for a live probe\n", liveGateEnvironment)
			return 2
		}
		if len(args) != 0 {
			fmt.Fprintf(stderr, "ERROR: refusing live network input without %s=1\n", liveGateEnvironment)
			return 2
		}
		fmt.Fprintf(stdout, "SKIP: public TURN/STUN network probes require %s=1 and explicit live arguments\n", liveGateEnvironment)
		return 0
	}

	options, ok := parseLiveArgs(args)
	if !ok {
		fmt.Fprintln(stderr, "ERROR: live mode requires --secret-file and/or one or more --stun-control host:port values")
		return 2
	}

	exitCode := 0
	for index, endpoint := range options.stunControls {
		result := control(endpoint)
		if result.Mapped {
			fmt.Fprintf(stdout, "PASS: stun_control=%d udp_tx=%d udp_rx=%d mapped=verified\n", index+1, result.Sent, result.Received)
		} else {
			fmt.Fprintf(stderr, "ERROR: stun_control=%d udp_tx=%d udp_rx=%d mapped=not-verified\n", index+1, result.Sent, result.Received)
			exitCode = 1
		}
	}

	if options.secretFile != "" {
		result, err := probe(options.secretFile, options.connectAddress, options.transport)
		if err != nil {
			stage := safeFailureStage(err)
			if sent, received, ok := safeTrafficEvidence(err); ok {
				fmt.Fprintf(stderr, "ERROR: public TURN probe failed at %s (udp_tx=%d udp_rx=%d)\n", stage, sent, received)
			} else {
				fmt.Fprintf(stderr, "ERROR: public TURN probe failed at %s\n", stage)
			}
			return 1
		}
		fmt.Fprintf(stdout, "PASS: transport=%s allocate=verified relay_port=%d peer_to_relay=verified relay_to_peer=verified cleanup=deallocate-sent+sockets-closed\n", options.transport, result.RelayPort)
	}
	return exitCode
}

type liveCLIOptions struct {
	secretFile     string
	connectAddress string
	stunControls   []string
	transport      probeTransport
}

func parseLiveArgs(args []string) (liveCLIOptions, bool) {
	options := liveCLIOptions{transport: probeTransportUDP}
	transportSet := false
	for len(args) > 0 {
		if args[0] == "--secret-stdin" {
			if options.secretFile != "" {
				return liveCLIOptions{}, false
			}
			options.secretFile = "-"
			args = args[1:]
			continue
		}
		if len(args) < 2 || args[1] == "" {
			return liveCLIOptions{}, false
		}
		switch args[0] {
		case "--secret-file":
			if options.secretFile != "" {
				return liveCLIOptions{}, false
			}
			options.secretFile = args[1]
		case "--connect-address":
			if options.connectAddress != "" {
				return liveCLIOptions{}, false
			}
			options.connectAddress = args[1]
		case "--transport":
			if transportSet || (args[1] != string(probeTransportUDP) && args[1] != string(probeTransportTCP) && args[1] != string(probeTransportTLS)) {
				return liveCLIOptions{}, false
			}
			options.transport = probeTransport(args[1])
			transportSet = true
		case "--stun-control":
			if len(options.stunControls) >= 4 {
				return liveCLIOptions{}, false
			}
			options.stunControls = append(options.stunControls, args[1])
		default:
			return liveCLIOptions{}, false
		}
		args = args[2:]
	}
	if options.connectAddress != "" && options.secretFile == "" {
		return liveCLIOptions{}, false
	}
	if options.transport != probeTransportUDP && options.secretFile == "" {
		return liveCLIOptions{}, false
	}
	return options, options.secretFile != "" || len(options.stunControls) > 0
}

func printUsage(output io.Writer) {
	fmt.Fprintf(output, `Usage:
  scripts/tests/public-turn-e2e.sh
  %s=1 scripts/tests/public-turn-e2e.sh \
    --secret-file /absolute/mode-0600/turn-auth-secret \
    [--connect-address PUBLIC_TURN_IPV4] \
    [--transport udp|tcp|tls]
  %s=1 scripts/tests/public-turn-e2e.sh \
    --secret-stdin \
    [--connect-address PUBLIC_TURN_IPV4] \
    [--transport udp|tcp|tls]
  %s=1 scripts/tests/public-turn-e2e.sh \
    --stun-control 74.125.250.129:19302 \
    --stun-control 18.141.157.136:3478

The default invocation runs hermetic checks only. Live transport udp uses TURN
UDP 3478; transport tcp uses TURN TCP 3478; transport tls verifies
turn.example.com's public certificate and uses TURN TLS/TCP 443. All verify a
UDP relay allocation and bidirectional payloads.
The optional numeric connect address bypasses local DNS without changing TLS
ServerName, credential host, or authentication realm. --secret-stdin reads one
0600-equivalent secret value from stdin and never writes it to disk or places it
in argv; this is useful for an encrypted SSH pipe. STUN controls are independent
Binding-only UDP checks; they never print the mapped public address.
`, liveGateEnvironment, liveGateEnvironment, liveGateEnvironment)
}

func safeFailureStage(err error) string {
	var failure *probeFailure
	if errors.As(err, &failure) && failure != nil && validFailureStages[failure.stage] {
		return string(failure.stage)
	}
	return "internal"
}

func safeTrafficEvidence(err error) (int, int, bool) {
	var failure *probeFailure
	if !errors.As(err, &failure) || failure == nil || failure.stage != stageTURNNoResponse ||
		failure.udpSent < 1 || failure.udpSent > 100 || failure.udpReceived < 0 || failure.udpReceived > 100 {
		return 0, 0, false
	}
	return failure.udpSent, failure.udpReceived, true
}
