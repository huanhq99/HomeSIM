//go:build darwin

package remotevoice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIPCProtocolGoldenHeaderMatchesSwift(t *testing.T) {
	t.Parallel()
	var encoded bytes.Buffer
	if err := writeIPCMessage(&encoded, ipcMessage{
		kind:       ipcMessageDeviceToRemote,
		generation: 7,
		sequence:   1,
		payload:    make([]byte, FrameBytes),
	}); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x45, 0x56, 0x50, 0x31, 0x01, 0x03, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x00, 0x01, 0x40,
	}
	if !bytes.Equal(encoded.Bytes()[:ipcHeaderBytes], want) {
		t.Fatalf("PCM IPC header = %x, want %x", encoded.Bytes()[:ipcHeaderBytes], want)
	}
}

func TestSyntheticIPCLeaseIsRandomAndGenerationBound(t *testing.T) {
	t.Parallel()
	first, err := NewSyntheticIPCLease(7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSyntheticIPCLease(7)
	if err != nil {
		t.Fatal(err)
	}
	if first.CallGeneration != 7 || second.CallGeneration != 7 {
		t.Fatal("lease generation was not retained")
	}
	if first.SessionToken == second.SessionToken || allZero(first.SessionToken[:]) || allZero(second.SessionToken[:]) {
		t.Fatal("session tokens are equal or all-zero")
	}
	if _, err := NewSyntheticIPCLease(0); err == nil {
		t.Fatal("zero generation unexpectedly accepted")
	}
}

func TestSyntheticIPCBrokerRoundTripPermissionsAndBackpressure(t *testing.T) {
	t.Parallel()
	broker, port, lease, socketPath := newSyntheticBrokerForTest(t, 2, nil)
	defer broker.Close()

	parentInfo, err := os.Lstat(filepath.Dir(socketPath))
	if err != nil {
		t.Fatal(err)
	}
	if parentInfo.Mode().Perm() != 0o700 || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("parent mode = %v, want real 0700 directory", parentInfo.Mode())
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if socketInfo.Mode().Perm() != 0o600 || socketInfo.Mode()&os.ModeSocket == 0 ||
		socketInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("socket mode = %v, want real 0600 socket", socketInfo.Mode())
	}

	conn := dialSyntheticBroker(t, socketPath)
	clientHandshake(t, conn, lease)
	if !broker.Consumed() {
		t.Fatal("authenticated one-use token was not marked consumed")
	}
	// Authentication consumes the listener immediately: the bearer token can
	// authorize exactly one local client even while that session remains open.
	if second, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond); err == nil {
		_ = second.Close()
		t.Fatal("second client connected after the one-use lease was consumed")
	}

	for value := int16(1); value <= 3; value++ {
		if err := writeIPCMessage(conn, ipcMessage{
			kind:       ipcMessageDeviceToRemote,
			generation: lease.CallGeneration,
			sequence:   uint64(value),
			payload:    encodePCM16LE(filledFrame(value)),
		}); err != nil {
			t.Fatal(err)
		}
		response, err := readIPCMessage(conn)
		if err != nil {
			t.Fatal(err)
		}
		if response.kind != ipcMessageRemoteToDevice || response.sequence != uint64(value) ||
			response.generation != lease.CallGeneration || len(response.payload) != FrameBytes {
			t.Fatalf("response = %+v, want matched remote-to-device frame", response)
		}
		if samples := decodePCM16LE(response.payload); samples[0] != 0 {
			t.Fatalf("empty downlink frame starts with %d, want silence", samples[0])
		}
	}
	activity := waitBrokerActivity(t, broker, func(activity IPCBrokerActivitySnapshot) bool {
		return activity.DeviceUplinkFrames == 3
	})
	if !activity.Consumed || !activity.Ready || !activity.Live || activity.AuthenticatedAt.IsZero() || activity.ReadyAt.IsZero() ||
		activity.DeviceUplinkFrames != 3 || activity.LastDeviceUplinkAt.IsZero() ||
		activity.RemoteDownlinkFrames != 0 || !activity.LastRemoteDownlinkAt.IsZero() {
		t.Fatalf("activity after device-only frames = %+v", activity)
	}

	first, ok := port.takeUplink()
	if !ok || first[0] != 2 {
		t.Fatalf("oldest retained device frame = %v, want value 2", first)
	}
	second, ok := port.takeUplink()
	if !ok || second[0] != 3 {
		t.Fatalf("newest retained device frame = %v, want value 3", second)
	}
	stats := port.Stats()
	if stats.UplinkOverflow != 1 || stats.DownlinkUnderrun != 3 {
		t.Fatalf("frame stats = %+v, want one uplink overflow and three downlink underruns", stats)
	}

	if err := port.pushDownlink(filledFrame(44)); err != nil {
		t.Fatal(err)
	}
	if err := writeIPCMessage(conn, ipcMessage{
		kind:       ipcMessageDeviceToRemote,
		generation: lease.CallGeneration,
		sequence:   4,
		payload:    encodePCM16LE(filledFrame(4)),
	}); err != nil {
		t.Fatal(err)
	}
	response, err := readIPCMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodePCM16LE(response.payload)[0]; got != 44 {
		t.Fatalf("remote-to-device sample = %d, want 44", got)
	}
	activity = waitBrokerActivity(t, broker, func(activity IPCBrokerActivitySnapshot) bool {
		return activity.DeviceUplinkFrames == 4 && activity.RemoteDownlinkFrames == 1
	})
	if activity.DeviceUplinkFrames != 4 || activity.RemoteDownlinkFrames != 1 || activity.LastRemoteDownlinkAt.IsZero() {
		t.Fatalf("bidirectional activity = %+v", activity)
	}

	_ = conn.Close()
	waitSyntheticBroker(t, broker, nil)
	if activity := broker.ActivitySnapshot(); activity.Live {
		t.Fatalf("broker remained live after authenticated client exit: %+v", activity)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after session: %v", err)
	}
}

func TestSyntheticIPCBrokerActivityUsesInjectedClock(t *testing.T) {
	t.Parallel()
	parent, err := os.MkdirTemp("/tmp", "maccellular-rvipc-clock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := NewSyntheticIPCLease(52)
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}
	clock := &lockedClock{now: time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)}
	broker, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath: filepath.Join(parent, "pcm.sock"), Lease: lease, Port: port,
		IOTimeout: 2 * time.Second, ActivityNow: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	conn := dialSyntheticBroker(t, broker.SocketPath())
	clientHandshake(t, conn, lease)
	initial := waitBrokerActivity(t, broker, func(activity IPCBrokerActivitySnapshot) bool {
		return activity.Ready && activity.Live
	})
	if !initial.AuthenticatedAt.Equal(clock.Now()) || !initial.ReadyAt.Equal(clock.Now()) {
		t.Fatalf("handshake times = %+v, want injected %v", initial, clock.Now())
	}

	next := clock.Advance(250 * time.Millisecond)
	if err := port.pushDownlink(filledFrame(9)); err != nil {
		t.Fatal(err)
	}
	if err := writeIPCMessage(conn, ipcMessage{
		kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration, sequence: 1,
		payload: encodePCM16LE(filledFrame(8)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readIPCMessage(conn); err != nil {
		t.Fatal(err)
	}
	activity := waitBrokerActivity(t, broker, func(activity IPCBrokerActivitySnapshot) bool {
		return activity.DeviceUplinkFrames == 1 && activity.RemoteDownlinkFrames == 1
	})
	if !activity.LastDeviceUplinkAt.Equal(next) || !activity.LastRemoteDownlinkAt.Equal(next) {
		t.Fatalf("frame times = %+v, want injected %v", activity, next)
	}
	_ = conn.Close()
	waitSyntheticBroker(t, broker, nil)
}

func TestSyntheticIPCBrokerReadyWriteFailureNeverPublishesReadyOrLive(t *testing.T) {
	t.Parallel()
	parent, err := os.MkdirTemp("/tmp", "maccellular-rvipc-ready-fail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewSyntheticIPCLease(53)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath: filepath.Join(parent, "pcm.sock"), Lease: lease, Port: port,
		writeMessage: func(io.Writer, ipcMessage) error { return io.ErrClosedPipe },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	conn := dialSyntheticBroker(t, broker.SocketPath())
	writeClientHello(t, conn, lease)
	assertConnectionClosed(t, conn)
	waitSyntheticBroker(t, broker, ErrIPCProtocolViolation)
	activity := broker.ActivitySnapshot()
	if !activity.Consumed || activity.Ready || activity.Live || !activity.ReadyAt.IsZero() {
		t.Fatalf("failed Ready write published authorization state: %+v", activity)
	}
}

func TestSyntheticIPCBrokerSeparatesPreparedIdleAndActiveFrameTimeouts(t *testing.T) {
	t.Parallel()
	parent, err := os.MkdirTemp("/tmp", "maccellular-rvipc-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := NewSyntheticIPCLease(61)
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath: filepath.Join(parent, "pcm.sock"), Lease: lease, Port: port,
		IOTimeout: 100 * time.Millisecond, AuthenticatedIdleTimeout: 800 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	conn := dialSyntheticBroker(t, broker.SocketPath())
	clientHandshake(t, conn, lease)

	// This exceeds the active frame timeout but remains inside the distinct
	// prepared/pre-call idle window.
	time.Sleep(250 * time.Millisecond)
	if err := writeIPCMessage(conn, ipcMessage{
		kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration, sequence: 1,
		payload: encodePCM16LE(filledFrame(1)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readIPCMessage(conn); err != nil {
		t.Fatalf("first frame inside authenticated idle window: %v", err)
	}

	// After the first frame the short active stall timeout applies again.
	time.Sleep(250 * time.Millisecond)
	assertConnectionClosed(t, conn)
	waitSyntheticBroker(t, broker, ErrIPCProtocolViolation)
}

func TestSyntheticIPCBrokerActivationEpochExcludesInFlightExchange(t *testing.T) {
	t.Parallel()
	parent, err := os.MkdirTemp("/tmp", "maccellular-rvipc-epoch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(4)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := NewSyntheticIPCLease(63)
	if err != nil {
		t.Fatal(err)
	}
	readStarted := make(chan uint64, 2)
	broker, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath: filepath.Join(parent, "pcm.sock"), Lease: lease, Port: port,
		IOTimeout:       2 * time.Second,
		beforeFrameRead: func(epoch uint64) { readStarted <- epoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	conn := dialSyntheticBroker(t, broker.SocketPath())
	clientHandshake(t, conn, lease)
	if epoch := <-readStarted; epoch != 0 {
		t.Fatalf("first read epoch = %d, want zero", epoch)
	}
	if err := port.pushDownlink(filledFrame(5)); err != nil {
		t.Fatal(err)
	}

	// The server is already blocked in a read that began under epoch zero.
	// Activate before sending the request so the exchange is deterministically
	// in-flight across the barrier rather than racing a completed exchange and
	// the next loop's epoch capture.
	baseline, err := broker.Activate(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIPCMessage(conn, ipcMessage{
		kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration, sequence: 1,
		payload: encodePCM16LE(filledFrame(6)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readIPCMessage(conn); err != nil {
		t.Fatal(err)
	}
	activity := waitBrokerActivity(t, broker, func(activity IPCBrokerActivitySnapshot) bool {
		return activity.DeviceUplinkFrames >= baseline.DeviceUplinkFrames && activity.LastDeviceUplinkEpoch == 0
	})
	if activity.LastDeviceUplinkEpoch != 0 || activity.LastRemoteDownlinkEpoch != 0 {
		t.Fatalf("in-flight exchange crossed activation epoch: %+v", activity)
	}
	if epoch := <-readStarted; epoch != 1 {
		t.Fatalf("second read epoch = %d, want one", epoch)
	}
	if frame, ok, err := port.takeUplinkAtEpoch(1); err != nil || ok {
		t.Fatalf("in-flight epoch-zero device frame crossed into epoch one: frame=%v ok=%v err=%v", frame, ok, err)
	}
	if frame, ok, err := port.takeDownlinkAtEpoch(1); err != nil || ok {
		t.Fatalf("flushed epoch-zero remote frame crossed into epoch one: frame=%v ok=%v err=%v", frame, ok, err)
	}

	if err := port.pushDownlinkAtEpoch(filledFrame(7), 1); err != nil {
		t.Fatal(err)
	}
	if err := writeIPCMessage(conn, ipcMessage{
		kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration, sequence: 2,
		payload: encodePCM16LE(filledFrame(8)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readIPCMessage(conn); err != nil {
		t.Fatal(err)
	}
	activity = waitBrokerActivity(t, broker, func(activity IPCBrokerActivitySnapshot) bool {
		return activity.LastDeviceUplinkEpoch == 1 && activity.LastRemoteDownlinkEpoch == 1
	})
	if activity.DeviceUplinkFrames <= baseline.DeviceUplinkFrames || activity.RemoteDownlinkFrames <= baseline.RemoteDownlinkFrames {
		t.Fatalf("post-activation exchange counters did not advance: baseline=%+v activity=%+v", baseline, activity)
	}
	_ = conn.Close()
	waitSyntheticBroker(t, broker, nil)
}

func TestSyntheticIPCBrokerRejectsInvalidAuthenticatedIdleTimeout(t *testing.T) {
	t.Parallel()
	lease, err := NewSyntheticIPCLease(62)
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "ipc")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath: filepath.Join(parent, "pcm.sock"), Lease: lease, Port: port,
		IOTimeout: time.Second, AuthenticatedIdleTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("authenticated idle timeout shorter than frame timeout was accepted")
	}
}

func TestSyntheticIPCBrokerRejectsWrongGenerationAndTokenWithoutConsumingLease(t *testing.T) {
	t.Parallel()
	broker, _, lease, socketPath := newSyntheticBrokerForTest(t, 2, nil)
	defer broker.Close()

	wrongGeneration := lease
	wrongGeneration.CallGeneration++
	conn := dialSyntheticBroker(t, socketPath)
	writeClientHello(t, conn, wrongGeneration)
	assertConnectionClosed(t, conn)

	wrongToken := lease
	wrongToken.SessionToken[0] ^= 0xff
	conn = dialSyntheticBroker(t, socketPath)
	writeClientHello(t, conn, wrongToken)
	assertConnectionClosed(t, conn)

	conn = dialSyntheticBroker(t, socketPath)
	clientHandshake(t, conn, lease)
	_ = conn.Close()
	waitSyntheticBroker(t, broker, nil)
}

func TestSyntheticIPCBrokerRejectsDifferentPeerEffectiveUID(t *testing.T) {
	t.Parallel()
	wrongUID := func(*net.UnixConn) (uint32, error) {
		return uint32(os.Geteuid()) + 1, nil
	}
	broker, _, lease, socketPath := newSyntheticBrokerForTest(t, 2, wrongUID)

	conn := dialSyntheticBroker(t, socketPath)
	// The kernel credential check runs before reading the bearer. Either the
	// write races with the close or it succeeds into the socket buffer; both
	// must result in an unauthenticated disconnect.
	_ = writeIPCMessage(conn, ipcMessage{
		kind:       ipcMessageHello,
		generation: lease.CallGeneration,
		payload:    lease.SessionToken[:],
	})
	assertConnectionClosed(t, conn)
	_ = broker.Close()
	waitSyntheticBroker(t, broker, nil)
}

func TestSyntheticIPCBrokerCloseWaitsForSocketCleanupAndAllowsImmediateReuse(t *testing.T) {
	t.Parallel()
	broker, port, lease, socketPath := newSyntheticBrokerForTest(t, 2, nil)
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket exists after Close returned: %v", err)
	}
	replacementLease, err := NewSyntheticIPCLease(lease.CallGeneration + 1)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{
		SocketPath: socketPath, Lease: replacementLease, Port: port,
	})
	if err != nil {
		t.Fatalf("immediate socket path reuse after Close: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSyntheticIPCBrokerCloseUnblocksAuthenticatedClient(t *testing.T) {
	t.Parallel()
	broker, _, lease, socketPath := newSyntheticBrokerForTest(t, 2, nil)
	conn := dialSyntheticBroker(t, socketPath)
	clientHandshake(t, conn, lease)
	done := make(chan error, 1)
	go func() { done <- broker.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock authenticated client")
	}
	assertConnectionClosed(t, conn)
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket exists after authenticated Close: %v", err)
	}
}

func TestSyntheticIPCBrokerDisconnectsOnProtocolViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		message func(SyntheticIPCLease) ipcMessage
	}{
		{
			name: "out of order sequence",
			message: func(lease SyntheticIPCLease) ipcMessage {
				return ipcMessage{kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration, sequence: 2, payload: make([]byte, FrameBytes)}
			},
		},
		{
			name: "generation mismatch",
			message: func(lease SyntheticIPCLease) ipcMessage {
				return ipcMessage{kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration + 1, sequence: 1, payload: make([]byte, FrameBytes)}
			},
		},
		{
			name: "unknown type",
			message: func(lease SyntheticIPCLease) ipcMessage {
				return ipcMessage{kind: 99, generation: lease.CallGeneration, sequence: 1, payload: make([]byte, FrameBytes)}
			},
		},
		{
			name: "short frame",
			message: func(lease SyntheticIPCLease) ipcMessage {
				return ipcMessage{kind: ipcMessageDeviceToRemote, generation: lease.CallGeneration, sequence: 1, payload: make([]byte, FrameBytes-2)}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			broker, _, lease, socketPath := newSyntheticBrokerForTest(t, 2, nil)
			defer broker.Close()
			conn := dialSyntheticBroker(t, socketPath)
			clientHandshake(t, conn, lease)
			if err := writeIPCMessage(conn, test.message(lease)); err != nil {
				t.Fatal(err)
			}
			assertConnectionClosed(t, conn)
			waitSyntheticBroker(t, broker, ErrIPCProtocolViolation)
		})
	}
}

func TestSyntheticIPCBrokerDisconnectsBeforeAllocatingOversizedPayload(t *testing.T) {
	t.Parallel()
	broker, _, lease, socketPath := newSyntheticBrokerForTest(t, 2, nil)
	defer broker.Close()
	conn := dialSyntheticBroker(t, socketPath)
	clientHandshake(t, conn, lease)

	header := make([]byte, ipcHeaderBytes)
	copy(header[:4], ipcMagic)
	header[4] = IPCProtocolVersion
	header[5] = ipcMessageDeviceToRemote
	putUint64(header[8:16], lease.CallGeneration)
	putUint64(header[16:24], 1)
	putUint32(header[24:28], IPCMaxPayloadBytes+1)
	if err := writeFull(conn, header); err != nil {
		t.Fatal(err)
	}
	assertConnectionClosed(t, conn)
	waitSyntheticBroker(t, broker, ErrIPCProtocolViolation)
}

func TestSyntheticIPCBrokerRejectsUnsafeSocketPaths(t *testing.T) {
	t.Parallel()
	lease, err := NewSyntheticIPCLease(9)
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(2)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("non-absolute", func(t *testing.T) {
		_, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{SocketPath: "relative.sock", Lease: lease, Port: port})
		if err == nil {
			t.Fatal("relative socket path unexpectedly accepted")
		}
	})
	t.Run("parent wrong mode", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "ipc")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{SocketPath: filepath.Join(parent, "pcm.sock"), Lease: lease, Port: port})
		if err == nil {
			t.Fatal("mode-0755 parent unexpectedly accepted")
		}
	})
	t.Run("symlink parent", func(t *testing.T) {
		root := t.TempDir()
		realParent := filepath.Join(root, "real")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(root, "linked")
		if err := os.Symlink(realParent, linkedParent); err != nil {
			t.Fatal(err)
		}
		_, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{SocketPath: filepath.Join(linkedParent, "pcm.sock"), Lease: lease, Port: port})
		if err == nil {
			t.Fatal("symlink parent unexpectedly accepted")
		}
	})
	t.Run("file placeholder", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "ipc")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "pcm.sock")
		if err := os.WriteFile(path, []byte("do not replace"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{SocketPath: path, Lease: lease, Port: port})
		if err == nil {
			t.Fatal("file placeholder unexpectedly replaced")
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != "do not replace" {
			t.Fatalf("placeholder changed: data=%q err=%v", data, readErr)
		}
	})
	t.Run("symlink placeholder", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "ipc")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(parent, "target")
		if err := os.WriteFile(target, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "pcm.sock")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		_, err := StartSyntheticIPCBroker(SyntheticIPCBrokerConfig{SocketPath: path, Lease: lease, Port: port})
		if err == nil {
			t.Fatal("symlink placeholder unexpectedly replaced")
		}
	})
}

func newSyntheticBrokerForTest(
	t *testing.T,
	capacity int,
	peerLookup func(*net.UnixConn) (uint32, error),
) (*SyntheticIPCBroker, *FramePort, SyntheticIPCLease, string) {
	t.Helper()
	parent, err := os.MkdirTemp("/tmp", "maccellular-rvipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "pcm.sock")
	lease, err := NewSyntheticIPCLease(41)
	if err != nil {
		t.Fatal(err)
	}
	port, err := NewFramePort(capacity)
	if err != nil {
		t.Fatal(err)
	}
	cfg := SyntheticIPCBrokerConfig{SocketPath: path, Lease: lease, Port: port, IOTimeout: 2 * time.Second}
	var broker *SyntheticIPCBroker
	if peerLookup == nil {
		broker, err = StartSyntheticIPCBroker(cfg)
	} else {
		broker, err = startSyntheticIPCBroker(cfg, peerLookup)
	}
	if err != nil {
		t.Fatal(err)
	}
	return broker, port, lease, path
}

func dialSyntheticBroker(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func writeClientHello(t *testing.T, conn *net.UnixConn, lease SyntheticIPCLease) {
	t.Helper()
	if err := writeIPCMessage(conn, ipcMessage{
		kind:       ipcMessageHello,
		generation: lease.CallGeneration,
		sequence:   0,
		payload:    lease.SessionToken[:],
	}); err != nil {
		t.Fatal(err)
	}
}

func clientHandshake(t *testing.T, conn *net.UnixConn, lease SyntheticIPCLease) {
	t.Helper()
	writeClientHello(t, conn, lease)
	ready, err := readIPCMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if ready.kind != ipcMessageReady || ready.generation != lease.CallGeneration ||
		ready.sequence != 0 || len(ready.payload) != 0 {
		t.Fatalf("ready = %+v, want empty matched response", ready)
	}
}

func assertConnectionClosed(t *testing.T, conn *net.UnixConn) {
	t.Helper()
	defer conn.Close()
	if _, err := readIPCMessage(conn); err == nil {
		t.Fatal("protocol-violating client remained connected")
	}
}

func waitSyntheticBroker(t *testing.T, broker *SyntheticIPCBroker, target error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := broker.Wait(ctx)
	if target == nil && err != nil {
		t.Fatalf("broker wait = %v, want nil", err)
	}
	if target != nil && !errors.Is(err, target) {
		t.Fatalf("broker wait = %v, want %v", err, target)
	}
}

func waitBrokerActivity(
	t *testing.T,
	broker *SyntheticIPCBroker,
	predicate func(IPCBrokerActivitySnapshot) bool,
) IPCBrokerActivitySnapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		activity := broker.ActivitySnapshot()
		if predicate(activity) {
			return activity
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker activity did not reach expected state: %+v", activity)
		}
		time.Sleep(time.Millisecond)
	}
}

func putUint64(target []byte, value uint64) {
	for index := 7; index >= 0; index-- {
		target[index] = byte(value)
		value >>= 8
	}
}

func putUint32(target []byte, value int) {
	for index := 3; index >= 0; index-- {
		target[index] = byte(value)
		value >>= 8
	}
}
