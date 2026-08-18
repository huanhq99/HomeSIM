import Darwin
import Foundation

/// One in-memory authorization for the local PCM bridge.
///
/// This value must come from the verified media lease owner. Do not log,
/// persist, pass through argv/environment, or reuse `sessionToken`. There is no
/// challenge-response exchange in protocol v1: its boundary is same effective
/// UID + a mode-0600 socket inside a mode-0700 directory + this one-use token.
struct RemotePCMBridgeLease {
    static let tokenBytes = 32

    let socketPath: String
    let callGeneration: UInt64
    let sessionToken: Data

    /// The broker's media-lease generation. This is deliberately not named
    /// `activeCallGeneration`: CLCC topology may advance while an incoming
    /// call becomes active, whereas this value identifies one media lease.
    var mediaLeaseGeneration: UInt64 { callGeneration }

    init?(socketPath: String, callGeneration: UInt64, sessionToken: Data) {
        let standardized = (socketPath as NSString).standardizingPath
        guard socketPath.hasPrefix("/"), standardized == socketPath,
              socketPath.utf8.count <= 103,
              callGeneration != 0,
              sessionToken.count == Self.tokenBytes,
              sessionToken.contains(where: { $0 != 0 }) else {
            return nil
        }
        self.socketPath = socketPath
        self.callGeneration = callGeneration
        self.sessionToken = sessionToken
    }

    init?(socketPath: String, mediaLeaseGeneration: UInt64, sessionToken: Data) {
        self.init(
            socketPath: socketPath,
            callGeneration: mediaLeaseGeneration,
            sessionToken: sessionToken
        )
    }
}

enum RemotePCMBridgeError: LocalizedError {
    case alreadyStopped
    case invalidLease
    case insecureSocket
    case protocolViolation
    case disconnected
    case systemCall(String, Int32)

    var errorDescription: String? {
        switch self {
        case .alreadyStopped:
            return "远程 PCM 桥已停止。"
        case .invalidLease:
            return "远程 PCM 桥 lease 无效。"
        case .insecureSocket:
            return "远程 PCM socket 的权限、所有者或类型不安全。"
        case .protocolViolation:
            return "远程 PCM 桥协议不匹配，连接已关闭。"
        case .disconnected:
            return "远程 PCM 桥连接已断开。"
        case let .systemCall(operation, code):
            return "远程 PCM 桥 " + operation + " 失败（errno " + String(code) + "）。"
        }
    }
}

struct RemotePCMUplinkRead {
    let data: Data
    /// True only when `data` came from an authenticated remote-to-device
    /// protocol frame. Stale, disconnected and underrun silence is false even
    /// though it has the same byte shape.
    let wasRemoteFrame: Bool
}

/// Generation-bound Swift client for the Go `remotevoice` Unix-socket broker.
///
/// The bridge can authenticate before UAC starts. Audio exchange begins only
/// when its owner explicitly offers frames for the same media-lease generation.
/// All connect/read/write work is dispatched to a private non-real-time queue;
/// UAC callers only perform bounded, zero-wait admission and mailbox access.
final class RemotePCMBridge {
    static let integrationState = "generation-bound local PCM transport; call control remains external"

    private enum MessageKind: UInt8 {
        case hello = 1
        case ready = 2
        case deviceToRemote = 3
        case remoteToDevice = 4
    }

    private struct Message {
        let kind: MessageKind
        let generation: UInt64
        let sequence: UInt64
        let payload: Data
    }

    private struct PendingExchange {
        let frame: Data
        let mediaFlowEpoch: UInt64
    }

    private struct FileIdentity: Equatable {
        let device: dev_t
        let inode: ino_t
    }

    private static let magic = Data([0x45, 0x56, 0x50, 0x31]) // EVP1
    private static let protocolVersion: UInt8 = 1
    private static let headerBytes = 28
    private static let frameBytes = 320
    private static let maximumPayloadBytes = frameBytes
    // CoreAudio commonly delivers several 20 ms frames in one callback. Keep
    // enough room for two such bursts and write them to the local broker as a
    // short pipeline instead of paying one queue wakeup and round trip per
    // frame. The oldest pending audio is discarded on an actual overrun so a
    // scheduling hiccup does not turn into growing conversational latency.
    private static let maximumPendingExchanges = 16
    private static let maximumExchangeBatch = 8
    // The module output IOProc consumes a large CoreAudio period at once.
    // Preserve up to one full second of returned 20 ms frames so the UAC
    // worker can fill that period with real phone audio instead of silence.
    private static let maximumReadyFrames = 64

    private let socketPath: String
    private let callGeneration: UInt64
    private let callbackQueue: DispatchQueue
    private let workerQueue = DispatchQueue(
        label: "io.maccellular.remote-pcm.bridge",
        qos: .userInitiated
    )
    private let mediaLock = NSLock()

    // Short critical sections only: no socket IO or callback dispatch while
    // holding this lock. It is safe for the non-realtime UAC worker to inspect.
    private var readySnapshot = false
    private var stoppedSnapshot = false
    private var mediaFlowEnabled = false
    private var mediaFlowEpoch: UInt64 = 0
    private var readyFrames: [Data] = []
    private var pendingExchanges: [PendingExchange] = []
    private var exchangeDrainScheduled = false

    // All mutable fields below are workerQueue-confined.
    private var socketFD: Int32 = -1
    private var nextSequence: UInt64 = 1
    private var token: [UInt8]?
    private var stopped = false
    private var handshakeAttempted = false
    private var terminalReported = false
    private var peerMonitorScheduled = false
    private var onDisconnect: ((UInt64) -> Void)?

    private init(lease: RemotePCMBridgeLease, callbackQueue: DispatchQueue) {
        socketPath = lease.socketPath
        callGeneration = lease.callGeneration
        self.callbackQueue = callbackQueue
        token = Array(lease.sessionToken)
    }

    /// Returns nil unless an explicit lease was supplied. Passing nil remains
    /// the default and leaves local microphone/speaker behavior unchanged.
    static func makeIfConfigured(
        _ lease: RemotePCMBridgeLease?,
        callbackQueue: DispatchQueue = .main
    ) -> RemotePCMBridge? {
        guard let lease else { return nil }
        return RemotePCMBridge(lease: lease, callbackQueue: callbackQueue)
    }

    @available(*, deprecated, renamed: "makeIfConfigured(_:callbackQueue:)")
    static func makeSyntheticIfConfigured(
        _ lease: RemotePCMBridgeLease?,
        callbackQueue: DispatchQueue = .main
    ) -> RemotePCMBridge? {
        makeIfConfigured(lease, callbackQueue: callbackQueue)
    }

    /// Connects/authenticates only. It does not start UAC or touch call state.
    func start(
        onDisconnect: @escaping (UInt64) -> Void,
        completion: @escaping (Result<Void, Error>) -> Void
    ) {
        workerQueue.async { [self] in
            if stopped {
                finish(.failure(RemotePCMBridgeError.alreadyStopped), completion)
                return
            }
            if socketFD >= 0 {
                finish(.success(()), completion)
                return
            }
            if handshakeAttempted {
                finish(.failure(RemotePCMBridgeError.invalidLease), completion)
                return
            }
            do {
                self.onDisconnect = onDisconnect
                try connectAndAuthenticate()
                let activated = mediaLock.withLock {
                    guard !stoppedSnapshot else { return false }
                    readySnapshot = true
                    return true
                }
                guard activated else {
                    closeSocket()
                    finish(.failure(RemotePCMBridgeError.alreadyStopped), completion)
                    return
                }
                schedulePeerMonitor()
                finish(.success(()), completion)
            } catch {
                closeSocket()
                finish(.failure(error), completion)
            }
        }
    }

    func start(completion: @escaping (Result<Void, Error>) -> Void) {
        start(onDisconnect: { _ in }, completion: completion)
    }

    /// Nonblocking UAC-facing admission. `false` means the frame was rejected
    /// because the lease is stale, disconnected, or the bounded queue is full.
    /// No socket IO runs on the caller.
    @discardableResult
    func offerModemDownlink(
        _ frame: Data,
        mediaLeaseGeneration generation: UInt64
    ) -> Bool {
        guard generation == callGeneration, frame.count == Self.frameBytes else {
            return false
        }
        let shouldSchedule: Bool? = mediaLock.withLock {
            guard readySnapshot, !stoppedSnapshot, mediaFlowEnabled else { return nil }
            if pendingExchanges.count == Self.maximumPendingExchanges {
                pendingExchanges.removeFirst()
            }
            pendingExchanges.append(PendingExchange(
                frame: frame,
                mediaFlowEpoch: mediaFlowEpoch
            ))
            if exchangeDrainScheduled {
                return false
            }
            exchangeDrainScheduled = true
            return true
        }
        guard let shouldSchedule else { return false }
        if shouldSchedule {
            workerQueue.async { [self] in
                drainPendingExchanges()
            }
        }
        return true
    }

    /// Runs only on workerQueue. The broker protocol remains ordered request /
    /// response, but a small group of requests is written before their replies
    /// are read. Unix socket buffers comfortably hold the bounded batch and the
    /// reduced wakeup/context-switch rate keeps up with the 8 kHz UAC clock.
    private func drainPendingExchanges() {
        while !stopped, socketFD >= 0 {
            let batch: [PendingExchange] = mediaLock.withLock {
                guard readySnapshot, !stoppedSnapshot, mediaFlowEnabled else {
                    pendingExchanges.removeAll(keepingCapacity: true)
                    exchangeDrainScheduled = false
                    return []
                }
                pendingExchanges.removeAll {
                    $0.mediaFlowEpoch != mediaFlowEpoch
                }
                guard !pendingExchanges.isEmpty else {
                    exchangeDrainScheduled = false
                    return []
                }
                let count = min(Self.maximumExchangeBatch, pendingExchanges.count)
                let result = Array(pendingExchanges.prefix(count))
                pendingExchanges.removeFirst(count)
                return result
            }
            guard !batch.isEmpty else { return }

            do {
                guard UInt64(batch.count) <= UInt64.max - nextSequence else {
                    throw RemotePCMBridgeError.protocolViolation
                }
                let firstSequence = nextSequence
                for (offset, exchange) in batch.enumerated() {
                    try writeMessage(Message(
                        kind: .deviceToRemote,
                        generation: callGeneration,
                        sequence: firstSequence + UInt64(offset),
                        payload: exchange.frame
                    ))
                }
                for (offset, exchange) in batch.enumerated() {
                    let expectedSequence = firstSequence + UInt64(offset)
                    let response = try readMessage()
                    guard response.kind == .remoteToDevice,
                          response.generation == callGeneration,
                          response.sequence == expectedSequence,
                          response.payload.count == Self.frameBytes else {
                        throw RemotePCMBridgeError.protocolViolation
                    }
                    mediaLock.withLock {
                        guard readySnapshot, !stoppedSnapshot, mediaFlowEnabled,
                              mediaFlowEpoch == exchange.mediaFlowEpoch else { return }
                        if readyFrames.count == Self.maximumReadyFrames {
                            readyFrames.removeFirst()
                        }
                        readyFrames.append(response.payload)
                    }
                }
                nextSequence += UInt64(batch.count)
            } catch {
                transitionToDisconnected(notify: true)
                return
            }
        }
        mediaLock.withLock {
            pendingExchanges.removeAll(keepingCapacity: false)
            exchangeDrainScheduled = false
        }
    }

    /// Opens or closes frame flow without reconnecting the one-use lease.
    /// Advancing the epoch prevents an in-flight response from a prior media
    /// gate from becoming audio after a disable/re-enable transition.
    @discardableResult
    func setMediaFlowEnabled(
        _ enabled: Bool,
        mediaLeaseGeneration generation: UInt64
    ) -> Bool {
        guard generation == callGeneration else { return false }
        return mediaLock.withLock {
            guard readySnapshot, !stoppedSnapshot else { return false }
            if mediaFlowEnabled != enabled {
                mediaFlowEpoch &+= 1
                mediaFlowEnabled = enabled
                readyFrames.removeAll(keepingCapacity: true)
                pendingExchanges.removeAll(keepingCapacity: true)
            }
            return true
        }
    }

    /// Returns exactly one 20 ms remote-microphone-to-modem frame and its
    /// provenance. A stale generation, disconnect, or underrun returns
    /// deterministic silence explicitly marked as synthetic.
    func takeRemoteUplinkFrameOrSilence(
        mediaLeaseGeneration generation: UInt64
    ) -> RemotePCMUplinkRead {
        guard generation == callGeneration else {
            return RemotePCMUplinkRead(data: Self.silenceFrame, wasRemoteFrame: false)
        }
        return mediaLock.withLock {
            guard readySnapshot, !stoppedSnapshot, mediaFlowEnabled,
                  !readyFrames.isEmpty else {
                return RemotePCMUplinkRead(data: Self.silenceFrame, wasRemoteFrame: false)
            }
            return RemotePCMUplinkRead(data: readyFrames.removeFirst(), wasRemoteFrame: true)
        }
    }

    /// Compatibility wrapper for callers that do not need provenance.
    func takeRemoteUplinkOrSilence(
        mediaLeaseGeneration generation: UInt64
    ) -> Data {
        takeRemoteUplinkFrameOrSilence(
            mediaLeaseGeneration: generation
        ).data
    }

    func isReady(mediaLeaseGeneration generation: UInt64) -> Bool {
        guard generation == callGeneration else { return false }
        return mediaLock.withLock { readySnapshot && !stoppedSnapshot }
    }

    func stop(completion: (() -> Void)? = nil) {
        mediaLock.withLock {
            stoppedSnapshot = true
            readySnapshot = false
            mediaFlowEnabled = false
            mediaFlowEpoch &+= 1
            readyFrames.removeAll(keepingCapacity: false)
            pendingExchanges.removeAll(keepingCapacity: false)
        }
        workerQueue.async { [self] in
            stopped = true
            discardToken()
            closeSocket()
            guard let completion else { return }
            callbackQueue.async(execute: completion)
        }
    }

    /// Pure in-process contract checks. No socket is opened and no hardware or
    /// existing runtime is accessed.
    static func runSyntheticSelfTest() {
        precondition(makeIfConfigured(nil) == nil)
        precondition(RemotePCMBridgeLease(
            socketPath: "relative.sock",
            callGeneration: 1,
            sessionToken: Data(repeating: 1, count: RemotePCMBridgeLease.tokenBytes)
        ) == nil)
        precondition(RemotePCMBridgeLease(
            socketPath: "/tmp/maccellular-rvipc-self-test/pcm.sock",
            callGeneration: 0,
            sessionToken: Data(repeating: 1, count: RemotePCMBridgeLease.tokenBytes)
        ) == nil)
        precondition(RemotePCMBridgeLease(
            socketPath: "/tmp/maccellular-rvipc-self-test/pcm.sock",
            callGeneration: 1,
            sessionToken: Data(repeating: 0, count: RemotePCMBridgeLease.tokenBytes)
        ) == nil)
        guard let lease = RemotePCMBridgeLease(
            socketPath: "/tmp/maccellular-rvipc-self-test/pcm.sock",
            callGeneration: 7,
            sessionToken: Data(repeating: 1, count: RemotePCMBridgeLease.tokenBytes)
        ) else {
            preconditionFailure("valid synthetic PCM lease rejected")
        }
        guard let bridge = makeIfConfigured(
            lease,
            callbackQueue: DispatchQueue(label: "io.maccellular.remote-pcm.self-test")
        ) else {
            preconditionFailure("valid synthetic PCM bridge rejected")
        }
        precondition(!bridge.isReady(mediaLeaseGeneration: 7))
        precondition(!bridge.offerModemDownlink(
            Data(repeating: 1, count: Self.frameBytes),
            mediaLeaseGeneration: 7
        ))

        var encoded = Data()
        encoded.appendBigEndian(UInt64(0x0102_0304_0506_0708))
        encoded.appendBigEndian(UInt32(0x090a_0b0c))
        precondition(encoded.bigEndianUInt64(at: 0) == 0x0102_0304_0506_0708)
        precondition(encoded.bigEndianUInt32(at: 8) == 0x090a_0b0c)

        let golden = encodeMessage(Message(
            kind: .deviceToRemote,
            generation: 7,
            sequence: 1,
            payload: Data(repeating: 0, count: Self.frameBytes)
        ))
        precondition(golden.count == Self.headerBytes + Self.frameBytes)
        precondition(golden.prefix(Self.headerBytes) == Data([
            0x45, 0x56, 0x50, 0x31, 0x01, 0x03, 0x00, 0x00,
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07,
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
            0x00, 0x00, 0x01, 0x40,
        ]))

        // Arbitrary UAC chunks are reframed into exact 160-sample/20 ms
        // packets without swapping device-to-remote direction.
        var framer = RemotePCM20msFramer()
        let firstPart = Data(repeating: 0x11, count: 100)
        let secondPart = Data(repeating: 0x22, count: 540)
        precondition(framer.append(firstPart).isEmpty)
        let reframed = framer.append(secondPart)
        precondition(reframed.count == 2)
        precondition(reframed[0].count == Self.frameBytes)
        precondition(reframed[0].prefix(100) == firstPart)
        precondition(reframed[0].suffix(220) == Data(repeating: 0x22, count: 220))
        precondition(reframed[1] == Data(repeating: 0x22, count: Self.frameBytes))

        // A remote-to-device frame is returned only to its own lease. A stale
        // generation and a disconnect both fail closed to silence.
        bridge.mediaLock.withLock {
            bridge.readySnapshot = true
            bridge.readyFrames = [Data(repeating: 0x5a, count: Self.frameBytes)]
        }
        let disabledRead = bridge.takeRemoteUplinkFrameOrSilence(
            mediaLeaseGeneration: 7
        )
        precondition(disabledRead.data == Self.silenceFrame)
        precondition(!disabledRead.wasRemoteFrame)
        precondition(bridge.setMediaFlowEnabled(true, mediaLeaseGeneration: 7))
        bridge.mediaLock.withLock {
            bridge.readyFrames = [Data(repeating: 0x5a, count: Self.frameBytes)]
        }
        precondition(!bridge.setMediaFlowEnabled(true, mediaLeaseGeneration: 8))
        let staleRead = bridge.takeRemoteUplinkFrameOrSilence(
            mediaLeaseGeneration: 8
        )
        precondition(staleRead.data == Self.silenceFrame)
        precondition(!staleRead.wasRemoteFrame)
        let remoteRead = bridge.takeRemoteUplinkFrameOrSilence(
            mediaLeaseGeneration: 7
        )
        precondition(remoteRead.data == Data(repeating: 0x5a, count: Self.frameBytes))
        precondition(remoteRead.wasRemoteFrame)
        let disconnected = DispatchSemaphore(value: 0)
        bridge.onDisconnect = { generation in
            precondition(generation == 7)
            disconnected.signal()
        }
        bridge.transitionToDisconnected(notify: true)
        precondition(disconnected.wait(timeout: .now() + 1) == .success)
        let disconnectedRead = bridge.takeRemoteUplinkFrameOrSilence(
            mediaLeaseGeneration: 7
        )
        precondition(disconnectedRead.data == Self.silenceFrame)
        precondition(!disconnectedRead.wasRemoteFrame)
        bridge.stop()
        precondition(!bridge.isReady(mediaLeaseGeneration: 7))
    }

    deinit {
        if socketFD >= 0 {
            Darwin.close(socketFD)
        }
    }

    private func connectAndAuthenticate() throws {
        guard token?.count == RemotePCMBridgeLease.tokenBytes,
              token?.contains(where: { $0 != 0 }) == true else {
            throw RemotePCMBridgeError.invalidLease
        }
        handshakeAttempted = true
        // A connect or handshake may consume the one-use server lease even if
        // this client never receives READY. Clear the local copy on every path
        // after an attempt begins; start() cannot retry this instance.
        defer { discardToken() }
        let initialIdentity = try validateSocketPath()
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw RemotePCMBridgeError.systemCall("socket", errno)
        }
        socketFD = fd

        var noSignal: Int32 = 1
        guard Darwin.setsockopt(
            fd,
            SOL_SOCKET,
            SO_NOSIGPIPE,
            &noSignal,
            socklen_t(MemoryLayout.size(ofValue: noSignal))
        ) == 0 else {
            throw RemotePCMBridgeError.systemCall("setsockopt(SO_NOSIGPIPE)", errno)
        }
        var timeout = timeval(tv_sec: 2, tv_usec: 0)
        guard Darwin.setsockopt(
            fd,
            SOL_SOCKET,
            SO_RCVTIMEO,
            &timeout,
            socklen_t(MemoryLayout.size(ofValue: timeout))
        ) == 0,
        Darwin.setsockopt(
            fd,
            SOL_SOCKET,
            SO_SNDTIMEO,
            &timeout,
            socklen_t(MemoryLayout.size(ofValue: timeout))
        ) == 0 else {
            throw RemotePCMBridgeError.systemCall("setsockopt(timeout)", errno)
        }

        var address = sockaddr_un()
        address.sun_len = UInt8(MemoryLayout<sa_family_t>.size * 2 + socketPath.utf8.count + 1)
        address.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = socketPath.utf8CString
        guard pathBytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
            throw RemotePCMBridgeError.invalidLease
        }
        withUnsafeMutableBytes(of: &address.sun_path) { destination in
            pathBytes.withUnsafeBytes { source in
                destination.copyBytes(from: source)
            }
        }
        let addressLength = socklen_t(address.sun_len)
        let connectResult = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                Darwin.connect(fd, socketAddress, addressLength)
            }
        }
        guard connectResult == 0 else {
            throw RemotePCMBridgeError.systemCall("connect", errno)
        }

        var peerUID: uid_t = 0
        var peerGID: gid_t = 0
        guard Darwin.getpeereid(fd, &peerUID, &peerGID) == 0,
              peerUID == Darwin.geteuid(),
              try validateSocketPath() == initialIdentity else {
            throw RemotePCMBridgeError.insecureSocket
        }

        try writeMessage(Message(
            kind: .hello,
            generation: callGeneration,
            sequence: 0,
            payload: Data(token!)
        ))
        discardToken()
        let ready = try readMessage()
        guard ready.kind == .ready,
              ready.generation == callGeneration,
              ready.sequence == 0,
              ready.payload.isEmpty else {
            throw RemotePCMBridgeError.protocolViolation
        }
    }

    private func validateSocketPath() throws -> FileIdentity {
        let parent = (socketPath as NSString).deletingLastPathComponent
        var parentStatus = stat()
        guard parent.withCString({ Darwin.lstat($0, &parentStatus) }) == 0,
              parentStatus.st_mode & mode_t(S_IFMT) == mode_t(S_IFDIR),
              parentStatus.st_mode & 0o777 == 0o700,
              parentStatus.st_uid == Darwin.geteuid() else {
            throw RemotePCMBridgeError.insecureSocket
        }

        var socketStatus = stat()
        guard socketPath.withCString({ Darwin.lstat($0, &socketStatus) }) == 0,
              socketStatus.st_mode & mode_t(S_IFMT) == mode_t(S_IFSOCK),
              socketStatus.st_mode & 0o777 == 0o600,
              socketStatus.st_uid == Darwin.geteuid() else {
            throw RemotePCMBridgeError.insecureSocket
        }
        return FileIdentity(device: socketStatus.st_dev, inode: socketStatus.st_ino)
    }

    private func writeMessage(_ message: Message) throws {
        guard message.payload.count <= Self.maximumPayloadBytes else {
            throw RemotePCMBridgeError.protocolViolation
        }
        try writeAll(Self.encodeMessage(message))
    }

    private static func encodeMessage(_ message: Message) -> Data {
        var encoded = Data()
        encoded.reserveCapacity(Self.headerBytes + message.payload.count)
        encoded.append(Self.magic)
        encoded.append(Self.protocolVersion)
        encoded.append(message.kind.rawValue)
        encoded.append(contentsOf: [0, 0])
        encoded.appendBigEndian(message.generation)
        encoded.appendBigEndian(message.sequence)
        encoded.appendBigEndian(UInt32(message.payload.count))
        encoded.append(message.payload)
        return encoded
    }

    private func readMessage() throws -> Message {
        let header = try readExactly(Self.headerBytes)
        guard header.prefix(4) == Self.magic,
              header[4] == Self.protocolVersion,
              header[6] == 0,
              header[7] == 0,
              let kind = MessageKind(rawValue: header[5]) else {
            throw RemotePCMBridgeError.protocolViolation
        }
        let generation = header.bigEndianUInt64(at: 8)
        let sequence = header.bigEndianUInt64(at: 16)
        let payloadBytes = Int(header.bigEndianUInt32(at: 24))
        guard payloadBytes <= Self.maximumPayloadBytes else {
            throw RemotePCMBridgeError.protocolViolation
        }
        return Message(
            kind: kind,
            generation: generation,
            sequence: sequence,
            payload: try readExactly(payloadBytes)
        )
    }

    private func writeAll(_ data: Data) throws {
        try data.withUnsafeBytes { rawBuffer in
            guard let base = rawBuffer.baseAddress else { return }
            var offset = 0
            while offset < rawBuffer.count {
                let written = Darwin.write(socketFD, base.advanced(by: offset), rawBuffer.count - offset)
                if written > 0 {
                    offset += written
                } else if written < 0, errno == EINTR {
                    continue
                } else if written == 0 {
                    throw RemotePCMBridgeError.disconnected
                } else {
                    throw RemotePCMBridgeError.systemCall("write", errno)
                }
            }
        }
    }

    private func readExactly(_ count: Int) throws -> Data {
        if count == 0 { return Data() }
        var result = Data(count: count)
        try result.withUnsafeMutableBytes { rawBuffer in
            guard let base = rawBuffer.baseAddress else {
                throw RemotePCMBridgeError.disconnected
            }
            var offset = 0
            while offset < count {
                let readCount = Darwin.read(socketFD, base.advanced(by: offset), count - offset)
                if readCount > 0 {
                    offset += readCount
                } else if readCount < 0, errno == EINTR {
                    continue
                } else if readCount == 0 {
                    throw RemotePCMBridgeError.disconnected
                } else {
                    throw RemotePCMBridgeError.systemCall("read", errno)
                }
            }
        }
        return result
    }

    private func closeSocket() {
        guard socketFD >= 0 else { return }
        Darwin.close(socketFD)
        socketFD = -1
    }

    /// Detects an idle broker close without consuming protocol bytes. Socket
    /// IO remains confined to workerQueue; an active exchange simply delays
    /// this check until its bounded read finishes.
    private func schedulePeerMonitor() {
        guard !peerMonitorScheduled, !stopped, socketFD >= 0 else { return }
        peerMonitorScheduled = true
        workerQueue.asyncAfter(deadline: .now() + 0.25) { [self] in
            peerMonitorScheduled = false
            guard !stopped, socketFD >= 0 else { return }
            var byte: UInt8 = 0
            let result = Darwin.recv(socketFD, &byte, 1, MSG_PEEK | MSG_DONTWAIT)
            if result == 0 {
                transitionToDisconnected(notify: true)
                return
            }
            if result > 0 {
                // Protocol v1 is strict request/response. With workerQueue
                // serialization, readable idle bytes are unsolicited.
                transitionToDisconnected(notify: true)
                return
            }
            if result < 0, errno != EAGAIN, errno != EWOULDBLOCK, errno != EINTR {
                transitionToDisconnected(notify: true)
                return
            }
            schedulePeerMonitor()
        }
    }

    private static let silenceFrame = Data(repeating: 0, count: frameBytes)

    private func transitionToDisconnected(notify: Bool) {
        closeSocket()
        let wasUnexpected = mediaLock.withLock {
            let value = readySnapshot && !stoppedSnapshot
            readySnapshot = false
            mediaFlowEnabled = false
            mediaFlowEpoch &+= 1
            readyFrames.removeAll(keepingCapacity: false)
            pendingExchanges.removeAll(keepingCapacity: false)
            return value
        }
        guard notify, wasUnexpected, !stopped, !terminalReported else { return }
        terminalReported = true
        guard let onDisconnect else { return }
        let generation = callGeneration
        callbackQueue.async { onDisconnect(generation) }
    }

    private func discardToken() {
        guard var bytes = token else { return }
        token = nil
        bytes.withUnsafeMutableBufferPointer { buffer in
            buffer.initialize(repeating: 0)
        }
    }

    private func finish<T>(
        _ result: Result<T, Error>,
        _ completion: @escaping (Result<T, Error>) -> Void
    ) {
        callbackQueue.async { completion(result) }
    }
}

/// Reframes arbitrary even-byte PCM16LE chunks into 8 kHz mono 20 ms frames.
/// It is owned by the existing non-realtime UAC worker, so no lock is needed.
struct RemotePCM20msFramer {
    static let frameBytes = 160 * MemoryLayout<Int16>.size
    private static let maximumBufferedBytes = frameBytes * 8
    private var buffered = Data()

    mutating func append(_ pcm16LE: Data) -> [Data] {
        guard !pcm16LE.isEmpty, pcm16LE.count.isMultiple(of: 2) else { return [] }
        buffered.append(pcm16LE)
        if buffered.count > Self.maximumBufferedBytes {
            let excess = buffered.count - Self.maximumBufferedBytes
            let alignedExcess = excess + (excess.isMultiple(of: 2) ? 0 : 1)
            buffered.removeFirst(min(alignedExcess, buffered.count))
        }
        var frames: [Data] = []
        while buffered.count >= Self.frameBytes {
            frames.append(Data(buffered.prefix(Self.frameBytes)))
            buffered.removeFirst(Self.frameBytes)
        }
        return frames
    }

    mutating func reset() {
        buffered.removeAll(keepingCapacity: true)
    }
}

private extension Data {
    mutating func appendBigEndian(_ value: UInt64) {
        var encoded = value.bigEndian
        Swift.withUnsafeBytes(of: &encoded) { append(contentsOf: $0) }
    }

    mutating func appendBigEndian(_ value: UInt32) {
        var encoded = value.bigEndian
        Swift.withUnsafeBytes(of: &encoded) { append(contentsOf: $0) }
    }

    func bigEndianUInt64(at offset: Int) -> UInt64 {
        self[offset ..< offset + MemoryLayout<UInt64>.size].reduce(UInt64(0)) {
            ($0 << 8) | UInt64($1)
        }
    }

    func bigEndianUInt32(at offset: Int) -> UInt32 {
        self[offset ..< offset + MemoryLayout<UInt32>.size].reduce(UInt32(0)) {
            ($0 << 8) | UInt32($1)
        }
    }
}

private extension NSLock {
    @discardableResult
    func withLock<T>(_ body: () throws -> T) rethrows -> T {
        lock()
        defer { unlock() }
        return try body()
    }
}
