import Darwin
import Foundation

struct RemoteMediaControlClaim {
    let mediaSessionID: String
    let mediaLeaseGeneration: UInt64
    let purpose: String
    let expectedCallGeneration: UInt64
    let pcmLease: RemotePCMBridgeLease
    let vendorID: UInt16
    let productID: UInt16
    let locationID: UInt32
    let expiresAt: Date
}

struct RemoteMediaControlActivity: Equatable {
    let mediaSessionID: String
    let mediaLeaseGeneration: UInt64
    let activationEpoch: UInt64
    let captureFrames: UInt64
    let playbackFrames: UInt64
}

extension RemoteMediaControlClaim: CustomStringConvertible, CustomDebugStringConvertible {
    var description: String { "RemoteMediaControlClaim{redacted}" }
    var debugDescription: String { description }
}

extension RemotePCMBridgeLease: CustomStringConvertible, CustomDebugStringConvertible {
    var description: String { "RemotePCMBridgeLease{redacted}" }
    var debugDescription: String { description }
}

enum RemoteMediaControlError: LocalizedError {
    case invalidSocketPath
    case insecureSocket
    case oversizedMessage
    case malformedMessage
    case unknownResponseField
    case invalidClaim
    case expiredClaim
    case invalidPreparedDigest
    case invalidActivation
    case invalidActivity
    case rejected
    case systemCall(String, Int32)

    var errorDescription: String? {
        switch self {
        case .invalidSocketPath:
            return "远程媒体控制 socket 路径无效。"
        case .insecureSocket:
            return "远程媒体控制 socket 的类型、权限、所有者或 peer 不安全。"
        case .oversizedMessage:
            return "远程媒体控制消息超过 64 KiB。"
        case .malformedMessage:
            return "远程媒体控制消息不是严格的单行 JSON。"
        case .unknownResponseField:
            return "远程媒体控制响应包含未知字段。"
        case .invalidClaim:
            return "远程媒体控制响应中的 lease 无效。"
        case .expiredClaim:
            return "远程媒体 lease 已过期。"
        case .invalidPreparedDigest:
            return "UAC UID digest 必须是 64 位小写 SHA-256 十六进制。"
        case .invalidActivation:
            return "远程媒体 activation 身份或 epoch 无效。"
        case .invalidActivity:
            return "远程媒体 activity 身份或计数无效。"
        case .rejected:
            return "远程媒体控制请求被拒绝。"
        case let .systemCall(operation, code):
            return "远程媒体控制 " + operation + " 失败（errno " + String(code) + "）。"
        }
    }
}

/// Same-UID, short-connection AF_UNIX client for the local media coordinator.
/// It never logs or persists the socket path, opaque session ID, or PCM token.
final class RemoteMediaControlClient {
    static let maximumMessageBytes = 64 * 1_024
    private static let activityMinimumIntervalNanoseconds: UInt64 = 1_000_000_000

    static var defaultSocketPath: String {
        (NSHomeDirectory() as NSString).appendingPathComponent(
            "Library/Application Support/MacCellular/remote/media-control.sock"
        )
    }

    private struct ClaimRequest: Encodable {
        let action = "claim"
    }

    private struct PreparedRequest: Encodable {
        let action = "prepared"
        let mediaSessionID: String
        let leaseGeneration: UInt64
        let uacUIDDigest: String

        enum CodingKeys: String, CodingKey {
            case action
            case mediaSessionID = "media_session_id"
            case leaseGeneration = "lease_generation"
            case uacUIDDigest = "uac_uid_digest"
        }
    }

    private struct ActivationRequest: Encodable {
        let action = "activation"
        let mediaSessionID: String
        let leaseGeneration: UInt64

        enum CodingKeys: String, CodingKey {
            case action
            case mediaSessionID = "media_session_id"
            case leaseGeneration = "lease_generation"
        }
    }

    private struct ActivityRequest: Encodable, Equatable {
        let action = "activity"
        let mediaSessionID: String
        let leaseGeneration: UInt64
        let activationEpoch: UInt64
        let captureFrames: UInt64
        let playbackFrames: UInt64

        enum CodingKeys: String, CodingKey {
            case action
            case mediaSessionID = "media_session_id"
            case leaseGeneration = "lease_generation"
            case activationEpoch = "activation_epoch"
            case captureFrames = "capture_frames"
            case playbackFrames = "playback_frames"
        }
    }

    private struct ClaimResponse: Decodable {
        let ok: Bool
        let lease: ClaimLeaseResponse?

        enum CodingKeys: String, CodingKey, CaseIterable {
            case ok
            case lease
        }
    }

    private struct ClaimLeaseResponse: Decodable {
        let mediaSessionID: String
        let leaseGeneration: UInt64
        let purpose: String
        let expectedCallGeneration: UInt64
        let pcmSocketPath: String
        let pcmTokenBase64: String
        let vendorID: UInt16
        let productID: UInt16
        let locationID: UInt32
        let expiresAt: String

        enum CodingKeys: String, CodingKey, CaseIterable {
            case mediaSessionID = "media_session_id"
            case leaseGeneration = "lease_generation"
            case purpose
            case expectedCallGeneration = "expected_call_generation"
            case pcmSocketPath = "pcm_socket_path"
            case pcmTokenBase64 = "pcm_token_base64"
            case vendorID = "vendor_id"
            case productID = "product_id"
            case locationID = "location_id"
            case expiresAt = "expires_at"
        }
    }

    private struct PreparedResponse: Decodable {
        let ok: Bool

        enum CodingKeys: String, CodingKey, CaseIterable {
            case ok
        }
    }

    private struct ActivationResponse: Decodable {
        let ok: Bool
        let activationEpoch: UInt64?

        enum CodingKeys: String, CodingKey, CaseIterable {
            case ok
            case activationEpoch = "activation_epoch"
        }
    }

    private struct ActivityBinding: Equatable {
        let mediaSessionID: String
        let leaseGeneration: UInt64
        let activationEpoch: UInt64

        func matches(_ request: ActivityRequest) -> Bool {
            mediaSessionID == request.mediaSessionID &&
                leaseGeneration == request.leaseGeneration &&
                activationEpoch == request.activationEpoch
        }
    }

    private struct FileIdentity: Equatable {
        let device: dev_t
        let inode: ino_t
    }

    private let socketPath: String
    private let callbackQueue: DispatchQueue
    private let workerQueue = DispatchQueue(
        label: "io.maccellular.remote-media-control",
        qos: .userInitiated
    )
    private let activityQueue = DispatchQueue(
        label: "io.maccellular.remote-media-activity",
        qos: .utility
    )
    private let now: () -> Date
    private var activityBinding: ActivityBinding?
    private var pendingActivity: ActivityRequest?
    private var activityInFlight: ActivityRequest?
    private var activityWakeScheduled = false
    private var lastActivityStartedAt: UInt64?
    private let activityFailureLock = NSLock()
    private var activityFailureHandler: ((String, UInt64, UInt64, Error) -> Void)?

    init(
        socketPath: String = RemoteMediaControlClient.defaultSocketPath,
        callbackQueue: DispatchQueue = .main,
        now: @escaping () -> Date = Date.init
    ) {
        self.socketPath = socketPath
        self.callbackQueue = callbackQueue
        self.now = now
    }

    /// Claims at most one prepared media lease. Only a strict
    /// `{\"ok\":true,\"lease\":null}` means no lease is currently available.
    func claim(
        completion: @escaping (Result<RemoteMediaControlClaim?, Error>) -> Void
    ) {
        workerQueue.async { [self] in
            do {
                var response = try transact(ClaimRequest())
                defer { response.resetBytes(in: response.indices) }
                let claim = try Self.decodeClaimResponse(response, now: now())
                finish(.success(claim), completion)
            } catch {
                finish(.failure(error), completion)
            }
        }
    }

    /// Marks exact UAC descriptor validation complete for one media lease.
    /// The digest is non-reversible metadata; never pass the raw CoreAudio UID.
    func prepared(
        mediaSessionID: String,
        mediaLeaseGeneration: UInt64,
        uacUIDDigest: String,
        completion: @escaping (Result<Void, Error>) -> Void
    ) {
        guard Self.validMediaSessionID(mediaSessionID),
              mediaLeaseGeneration != 0 else {
            callbackQueue.async { completion(.failure(RemoteMediaControlError.invalidClaim)) }
            return
        }
        guard Self.validSHA256Digest(uacUIDDigest) else {
            callbackQueue.async {
                completion(.failure(RemoteMediaControlError.invalidPreparedDigest))
            }
            return
        }
        let request = PreparedRequest(
            mediaSessionID: mediaSessionID,
            leaseGeneration: mediaLeaseGeneration,
            uacUIDDigest: uacUIDDigest
        )
        workerQueue.async { [self] in
            do {
                var response = try transact(request)
                defer { response.resetBytes(in: response.indices) }
                try Self.decodePreparedResponse(response)
                finish(.success(()), completion)
            } catch {
                finish(.failure(error), completion)
            }
        }
    }

    /// Activates exactly one prepared media lease. The returned epoch is owned
    /// by Go and must be carried through UAC start, media enable and activity.
    func activation(
        mediaSessionID: String,
        mediaLeaseGeneration: UInt64,
        completion: @escaping (Result<UInt64, Error>) -> Void
    ) {
        guard Self.validMediaSessionID(mediaSessionID),
              mediaLeaseGeneration != 0 else {
            callbackQueue.async {
                completion(.failure(RemoteMediaControlError.invalidActivation))
            }
            return
        }
        let request = ActivationRequest(
            mediaSessionID: mediaSessionID,
            leaseGeneration: mediaLeaseGeneration
        )
        workerQueue.async { [self] in
            do {
                var response = try transact(request, timeoutSeconds: 12)
                defer { response.resetBytes(in: response.indices) }
                let epoch = try Self.decodeActivationResponse(response)
                finish(.success(epoch), completion)
            } catch {
                finish(.failure(error), completion)
            }
        }
    }

    /// Installs the only identity allowed to emit cumulative activity. Binding
    /// performs no IO and does not imply that UAC media has been enabled.
    @discardableResult
    func bindActivity(
        mediaSessionID: String,
        mediaLeaseGeneration: UInt64,
        activationEpoch: UInt64
    ) -> Bool {
        guard Self.validMediaSessionID(mediaSessionID),
              mediaLeaseGeneration != 0,
              activationEpoch != 0 else {
            return false
        }
        let binding = ActivityBinding(
            mediaSessionID: mediaSessionID,
            leaseGeneration: mediaLeaseGeneration,
            activationEpoch: activationEpoch
        )
        activityQueue.async { [self] in
            activityBinding = binding
            pendingActivity = nil
        }
        return true
    }

    /// Nonblocking cumulative submission. At most one short IPC request is in
    /// flight and starts are separated by at least one second; faster snapshots
    /// are merged by taking each monotonic counter's maximum.
    @discardableResult
    func enqueueActivity(_ activity: RemoteMediaControlActivity) -> Bool {
        guard Self.validMediaSessionID(activity.mediaSessionID),
              activity.mediaLeaseGeneration != 0,
              activity.activationEpoch != 0,
              activity.captureFrames != 0 || activity.playbackFrames != 0 else {
            return false
        }
        let request = ActivityRequest(
            mediaSessionID: activity.mediaSessionID,
            leaseGeneration: activity.mediaLeaseGeneration,
            activationEpoch: activity.activationEpoch,
            captureFrames: activity.captureFrames,
            playbackFrames: activity.playbackFrames
        )
        activityQueue.async { [self] in
            guard activityBinding?.matches(request) == true else { return }
            pendingActivity = Self.mergeActivity(pendingActivity, request)
            scheduleActivityIfNeeded()
        }
        return true
    }

    func cancelActivity(
        mediaSessionID: String,
        mediaLeaseGeneration: UInt64,
        activationEpoch: UInt64
    ) {
        activityQueue.async { [self] in
            guard activityBinding == ActivityBinding(
                mediaSessionID: mediaSessionID,
                leaseGeneration: mediaLeaseGeneration,
                activationEpoch: activationEpoch
            ) else {
                return
            }
            activityBinding = nil
            pendingActivity = nil
        }
    }

    func setActivityFailureHandler(
        _ handler: ((String, UInt64, UInt64, Error) -> Void)?
    ) {
        activityFailureLock.lock()
        activityFailureHandler = handler
        activityFailureLock.unlock()
    }

    static func runSyntheticSelfTest() {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]

        let claimRequest = try! encodeLine(ClaimRequest(), encoder: encoder)
        precondition(claimRequest == Data("{\"action\":\"claim\"}\n".utf8))

        let sessionID = "synthetic-media-session-0001"
        let digest = String(repeating: "a", count: 64)
        let preparedRequest = try! encodeLine(
            PreparedRequest(
                mediaSessionID: sessionID,
                leaseGeneration: 19,
                uacUIDDigest: digest
            ),
            encoder: encoder
        )
        let preparedObject = try! JSONSerialization.jsonObject(
            with: preparedRequest.dropLast()
        ) as! [String: Any]
        precondition(Set(preparedObject.keys) == [
            "action", "media_session_id", "lease_generation", "uac_uid_digest",
        ])
        precondition(preparedObject["action"] as? String == "prepared")

        let activationRequest = try! encodeLine(
            ActivationRequest(
                mediaSessionID: sessionID,
                leaseGeneration: 19
            ),
            encoder: encoder
        )
        let activationObject = try! JSONSerialization.jsonObject(
            with: activationRequest.dropLast()
        ) as! [String: Any]
        precondition(Set(activationObject.keys) == [
            "action", "media_session_id", "lease_generation",
        ])
        precondition(try! decodeActivationResponse(
            Data("{\"ok\":true,\"activation_epoch\":23}\n".utf8)
        ) == 23)
        precondition(rejectsActivation("{\"ok\":true,\"activation_epoch\":0}\n"))
        precondition(rejectsActivation("{\"ok\":false}\n"))
        precondition(rejectsActivation(
            "{\"ok\":false,\"activation_epoch\":23}\n"
        ))
        precondition(rejectsActivation(
            "{\"ok\":true,\"activation_epoch\":23,\"extra\":1}\n"
        ))

        let activity = ActivityRequest(
            mediaSessionID: sessionID,
            leaseGeneration: 19,
            activationEpoch: 23,
            captureFrames: 160,
            playbackFrames: 80
        )
        let activityLine = try! encodeLine(activity, encoder: encoder)
        let activityObject = try! JSONSerialization.jsonObject(
            with: activityLine.dropLast()
        ) as! [String: Any]
        precondition(Set(activityObject.keys) == [
            "action", "media_session_id", "lease_generation",
            "activation_epoch", "capture_frames", "playback_frames",
        ])
        let merged = mergeActivity(
            activity,
            ActivityRequest(
                mediaSessionID: sessionID,
                leaseGeneration: 19,
                activationEpoch: 23,
                captureFrames: 120,
                playbackFrames: 320
            )
        )
        precondition(merged.captureFrames == 160)
        precondition(merged.playbackFrames == 320)
        precondition(activityMinimumIntervalNanoseconds == 1_000_000_000)
        let binding = ActivityBinding(
            mediaSessionID: sessionID,
            leaseGeneration: 19,
            activationEpoch: 23
        )
        precondition(binding.matches(activity))
        precondition(!binding.matches(ActivityRequest(
            mediaSessionID: sessionID,
            leaseGeneration: 19,
            activationEpoch: 24,
            captureFrames: 160,
            playbackFrames: 80
        )))
        precondition(activityDelay(
            now: 1_500_000_000,
            lastStartedAt: 1_000_000_000
        ) == 500_000_000)
        precondition(activityDelay(
            now: 2_000_000_000,
            lastStartedAt: 1_000_000_000
        ) == 0)

        let validToken = Data(repeating: 0x42, count: RemotePCMBridgeLease.tokenBytes)
            .base64EncodedString()
        let validResponse = Data((
            "{\"ok\":true,\"lease\":{\"media_session_id\":\"" + sessionID + "\"," +
                "\"lease_generation\":19," +
                "\"purpose\":\"outgoing\",\"expected_call_generation\":41," +
                "\"pcm_socket_path\":\"/tmp/maccellular-media-self-test/pcm.sock\"," +
                "\"pcm_token_base64\":\"" + validToken + "\"," +
                "\"vendor_id\":11388,\"product_id\":293," +
                "\"location_id\":305397760," +
                "\"expires_at\":\"2030-01-02T03:04:05Z\"}}\n"
        ).utf8)
        let now = Date(timeIntervalSince1970: 1_700_000_000)
        let claim = try! decodeClaimResponse(validResponse, now: now)
        precondition(claim?.mediaSessionID == sessionID)
        precondition(claim?.mediaLeaseGeneration == 19)
        precondition(claim?.purpose == "outgoing")
        precondition(claim?.expectedCallGeneration == 41)
        precondition(claim?.pcmLease.mediaLeaseGeneration == 19)
        precondition(claim?.vendorID == 11_388)
        precondition(claim?.productID == 293)
        precondition(claim?.locationID == 305_397_760)

        precondition(
            try! decodeClaimResponse(Data("{\"ok\":true,\"lease\":null}\n".utf8), now: now) == nil
        )
        precondition(rejectsClaim("{\"ok\":true,\"lease\":null,\"extra\":1}\n", now: now))
        precondition(rejectsClaim("{\"ok\":false,\"lease\":null}\n", now: now))
        precondition(rejectsClaim("{\"ok\":true}\n", now: now))
        precondition(rejectsClaim(
            String(decoding: validResponse, as: UTF8.self)
                .replacingOccurrences(of: "}}\n", with: ",\"extra\":1}}\n"),
            now: now
        ))
        precondition(rejectsClaim(
            String(decoding: validResponse, as: UTF8.self)
                .replacingOccurrences(of: validToken, with: Data(repeating: 1, count: 31).base64EncodedString()),
            now: now
        ))
        precondition(rejectsClaim(
            String(decoding: validResponse, as: UTF8.self)
                .replacingOccurrences(of: "2030-01-02T03:04:05Z", with: "2020-01-02T03:04:05Z"),
            now: now
        ))
        precondition(rejectsClaim(String(repeating: "x", count: maximumMessageBytes + 1), now: now))
        precondition(rejectsClaim("{\"ok\":true,\"lease\":null}", now: now))
        precondition(rejectsClaim("{\"ok\":true}\n{\"lease\":null}\n", now: now))
        precondition(!validSHA256Digest(String(repeating: "A", count: 64)))
        precondition(!validSHA256Digest(String(repeating: "a", count: 63)))
        try! decodePreparedResponse(Data("{\"ok\":true}\n".utf8))
        precondition(rejectsPrepared("{\"ok\":false}\n"))
        precondition(rejectsPrepared("{\"ok\":true,\"extra\":1}\n"))
        CallCenter.runRemoteMediaSyntheticSelfTest()
    }

    private func scheduleActivityIfNeeded() {
        dispatchPrecondition(condition: .onQueue(activityQueue))
        guard activityInFlight == nil,
              !activityWakeScheduled,
              let pendingActivity,
              activityBinding?.matches(pendingActivity) == true else {
            return
        }
        let now = DispatchTime.now().uptimeNanoseconds
        let delay = Self.activityDelay(
            now: now,
            lastStartedAt: lastActivityStartedAt
        )
        activityWakeScheduled = true
        activityQueue.asyncAfter(deadline: .now() + .nanoseconds(Int(delay))) { [self] in
            activityWakeScheduled = false
            beginActivityIfPossible()
        }
    }

    private func beginActivityIfPossible() {
        dispatchPrecondition(condition: .onQueue(activityQueue))
        guard activityInFlight == nil,
              let request = pendingActivity,
              activityBinding?.matches(request) == true else {
            return
        }
        let now = DispatchTime.now().uptimeNanoseconds
        if Self.activityDelay(
            now: now,
            lastStartedAt: lastActivityStartedAt
        ) > 0 {
            scheduleActivityIfNeeded()
            return
        }
        pendingActivity = nil
        activityInFlight = request
        lastActivityStartedAt = now
        workerQueue.async { [self] in
            let result: Result<Void, Error>
            do {
                var response = try transact(request)
                defer { response.resetBytes(in: response.indices) }
                try Self.decodePreparedResponse(response)
                result = .success(())
            } catch {
                result = .failure(error)
            }
            activityQueue.async { [self] in
                finishActivity(request, result: result)
            }
        }
    }

    private func finishActivity(
        _ request: ActivityRequest,
        result: Result<Void, Error>
    ) {
        dispatchPrecondition(condition: .onQueue(activityQueue))
        guard activityInFlight == request else { return }
        activityInFlight = nil
        let stillCurrent = activityBinding?.matches(request) == true
        if case .failure(let error) = result, stillCurrent {
            activityBinding = nil
            pendingActivity = nil
            activityFailureLock.lock()
            let handler = activityFailureHandler
            activityFailureLock.unlock()
            if let handler {
                callbackQueue.async {
                    handler(
                        request.mediaSessionID,
                        request.leaseGeneration,
                        request.activationEpoch,
                        error
                    )
                }
            }
            return
        }
        scheduleActivityIfNeeded()
    }

    private static func mergeActivity(
        _ pending: ActivityRequest?,
        _ incoming: ActivityRequest
    ) -> ActivityRequest {
        guard let pending,
              pending.mediaSessionID == incoming.mediaSessionID,
              pending.leaseGeneration == incoming.leaseGeneration,
              pending.activationEpoch == incoming.activationEpoch else {
            return incoming
        }
        return ActivityRequest(
            mediaSessionID: incoming.mediaSessionID,
            leaseGeneration: incoming.leaseGeneration,
            activationEpoch: incoming.activationEpoch,
            captureFrames: max(pending.captureFrames, incoming.captureFrames),
            playbackFrames: max(pending.playbackFrames, incoming.playbackFrames)
        )
    }

    private static func activityDelay(
        now: UInt64,
        lastStartedAt: UInt64?
    ) -> UInt64 {
        guard let lastStartedAt,
              now >= lastStartedAt,
              now - lastStartedAt < activityMinimumIntervalNanoseconds else {
            return 0
        }
        return activityMinimumIntervalNanoseconds - (now - lastStartedAt)
    }

    private func transact<Request: Encodable>(
        _ request: Request,
        timeoutSeconds: Int = 3
    ) throws -> Data {
        guard (1 ... 15).contains(timeoutSeconds) else {
            throw RemoteMediaControlError.malformedMessage
        }
        let requestLine = try Self.encodeLine(request)
        let validated = try validateSocketBeforeConnect()
        let fd = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw RemoteMediaControlError.systemCall("socket", errno)
        }
        defer { Darwin.close(fd) }

        var noSignal: Int32 = 1
        guard Darwin.setsockopt(
            fd,
            SOL_SOCKET,
            SO_NOSIGPIPE,
            &noSignal,
            socklen_t(MemoryLayout.size(ofValue: noSignal))
        ) == 0 else {
            throw RemoteMediaControlError.systemCall("setsockopt(SO_NOSIGPIPE)", errno)
        }
        var timeout = timeval(tv_sec: timeoutSeconds, tv_usec: 0)
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
            throw RemoteMediaControlError.systemCall("setsockopt(timeout)", errno)
        }

        var address = sockaddr_un()
        address.sun_len = UInt8(MemoryLayout<sa_family_t>.size * 2 + socketPath.utf8.count + 1)
        address.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = socketPath.utf8CString
        guard pathBytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
            throw RemoteMediaControlError.invalidSocketPath
        }
        withUnsafeMutableBytes(of: &address.sun_path) { destination in
            pathBytes.withUnsafeBytes { source in destination.copyBytes(from: source) }
        }
        let addressLength = socklen_t(address.sun_len)
        let connected = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                Darwin.connect(fd, socketAddress, addressLength)
            }
        }
        guard connected == 0 else {
            throw RemoteMediaControlError.systemCall("connect", errno)
        }

        var peerUID: uid_t = 0
        var peerGID: gid_t = 0
        guard Darwin.getpeereid(fd, &peerUID, &peerGID) == 0,
              peerUID == Darwin.geteuid(),
              try validateSocketAfterConnect(validated) else {
            throw RemoteMediaControlError.insecureSocket
        }

        try writeAll(requestLine, to: fd)
        guard Darwin.shutdown(fd, SHUT_WR) == 0 else {
            throw RemoteMediaControlError.systemCall("shutdown", errno)
        }
        return try readResponseToEOF(from: fd)
    }

    private func validateSocketBeforeConnect() throws -> (FileIdentity, FileIdentity) {
        let standardized = (socketPath as NSString).standardizingPath
        guard socketPath.hasPrefix("/"), standardized == socketPath,
              socketPath.utf8.count <= 103 else {
            throw RemoteMediaControlError.invalidSocketPath
        }
        let parent = (socketPath as NSString).deletingLastPathComponent
        var parentStatus = stat()
        guard parent.withCString({ Darwin.lstat($0, &parentStatus) }) == 0,
              parentStatus.st_mode & mode_t(S_IFMT) == mode_t(S_IFDIR),
              parentStatus.st_mode & 0o777 == 0o700,
              parentStatus.st_uid == Darwin.geteuid() else {
            throw RemoteMediaControlError.insecureSocket
        }
        var socketStatus = stat()
        guard socketPath.withCString({ Darwin.lstat($0, &socketStatus) }) == 0,
              socketStatus.st_mode & mode_t(S_IFMT) == mode_t(S_IFSOCK),
              socketStatus.st_mode & 0o777 == 0o600,
              socketStatus.st_uid == Darwin.geteuid() else {
            throw RemoteMediaControlError.insecureSocket
        }
        return (
            FileIdentity(device: parentStatus.st_dev, inode: parentStatus.st_ino),
            FileIdentity(device: socketStatus.st_dev, inode: socketStatus.st_ino)
        )
    }

    private func validateSocketAfterConnect(
        _ expected: (FileIdentity, FileIdentity)
    ) throws -> Bool {
        let current = try validateSocketBeforeConnect()
        return current.0 == expected.0 && current.1 == expected.1
    }

    private func writeAll(_ data: Data, to fd: Int32) throws {
        try data.withUnsafeBytes { rawBuffer in
            guard let base = rawBuffer.baseAddress else { return }
            var offset = 0
            while offset < rawBuffer.count {
                let written = Darwin.write(fd, base.advanced(by: offset), rawBuffer.count - offset)
                if written > 0 {
                    offset += written
                } else if written < 0, errno == EINTR {
                    continue
                } else {
                    throw RemoteMediaControlError.systemCall("write", written == 0 ? EPIPE : errno)
                }
            }
        }
    }

    private func readResponseToEOF(from fd: Int32) throws -> Data {
        var response = Data()
        var buffer = [UInt8](repeating: 0, count: 4_096)
        while true {
            let count = buffer.withUnsafeMutableBytes { rawBuffer in
                Darwin.read(fd, rawBuffer.baseAddress, rawBuffer.count)
            }
            if count > 0 {
                guard response.count + count <= Self.maximumMessageBytes else {
                    throw RemoteMediaControlError.oversizedMessage
                }
                response.append(contentsOf: buffer.prefix(count))
            } else if count == 0 {
                break
            } else if errno == EINTR {
                continue
            } else {
                throw RemoteMediaControlError.systemCall("read", errno)
            }
        }
        try Self.validateSingleJSONLine(response)
        return response
    }

    private static func encodeLine<Request: Encodable>(
        _ request: Request,
        encoder: JSONEncoder = JSONEncoder()
    ) throws -> Data {
        var encoded = try encoder.encode(request)
        guard !encoded.isEmpty, encoded.count < maximumMessageBytes,
              !encoded.contains(0x0a), !encoded.contains(0x0d) else {
            throw RemoteMediaControlError.malformedMessage
        }
        encoded.append(0x0a)
        return encoded
    }

    private static func validateSingleJSONLine(_ data: Data) throws {
        guard !data.isEmpty, data.count <= maximumMessageBytes,
              data.last == 0x0a,
              !data.dropLast().contains(0x0a),
              !data.dropLast().contains(0x0d) else {
            throw data.count > maximumMessageBytes
                ? RemoteMediaControlError.oversizedMessage
                : RemoteMediaControlError.malformedMessage
        }
    }

    private static func decodeClaimResponse(
        _ data: Data,
        now: Date
    ) throws -> RemoteMediaControlClaim? {
        try validateSingleJSONLine(data)
        let envelopeAllowed = Set(ClaimResponse.CodingKeys.allCases.map(\.rawValue))
        let object = try objectDictionary(in: data)
        guard Set(object.keys) == envelopeAllowed else {
            throw RemoteMediaControlError.unknownResponseField
        }
        let response: ClaimResponse
        do {
            response = try JSONDecoder().decode(ClaimResponse.self, from: data.dropLast())
        } catch {
            throw RemoteMediaControlError.malformedMessage
        }
        guard response.ok else { throw RemoteMediaControlError.rejected }
        guard let payloadObject = object[ClaimResponse.CodingKeys.lease.rawValue] else {
            throw RemoteMediaControlError.malformedMessage
        }
        if payloadObject is NSNull {
            guard response.lease == nil else { throw RemoteMediaControlError.invalidClaim }
            return nil
        }
        let leaseAllowed = Set(ClaimLeaseResponse.CodingKeys.allCases.map(\.rawValue))
        guard let payloadDictionary = payloadObject as? [String: Any] else {
            throw RemoteMediaControlError.malformedMessage
        }
        guard Set(payloadDictionary.keys) == leaseAllowed else {
            throw RemoteMediaControlError.unknownResponseField
        }
        guard let response = response.lease else {
            throw RemoteMediaControlError.invalidClaim
        }
        let mediaSessionID = response.mediaSessionID
        let generation = response.leaseGeneration
        let purpose = response.purpose
        let expectedCallGeneration = response.expectedCallGeneration
        let pcmPath = response.pcmSocketPath
        let tokenText = response.pcmTokenBase64
        let vendorID = response.vendorID
        let productID = response.productID
        let locationID = response.locationID
        guard validMediaSessionID(mediaSessionID), generation != 0,
              (purpose == "incoming" && expectedCallGeneration == 0 ||
                purpose == "outgoing" && expectedCallGeneration != 0),
              let token = Data(base64Encoded: tokenText),
              token.base64EncodedString() == tokenText,
              token.count == RemotePCMBridgeLease.tokenBytes,
              let lease = RemotePCMBridgeLease(
                  socketPath: pcmPath,
                  mediaLeaseGeneration: generation,
                  sessionToken: token
              ),
              vendorID != 0, productID != 0, locationID != 0,
              let expiresAt = parseRFC3339(response.expiresAt) else {
            throw RemoteMediaControlError.invalidClaim
        }
        guard expiresAt > now else { throw RemoteMediaControlError.expiredClaim }
        return RemoteMediaControlClaim(
            mediaSessionID: mediaSessionID,
            mediaLeaseGeneration: generation,
            purpose: purpose,
            expectedCallGeneration: expectedCallGeneration,
            pcmLease: lease,
            vendorID: vendorID,
            productID: productID,
            locationID: locationID,
            expiresAt: expiresAt
        )
    }

    private static func decodePreparedResponse(_ data: Data) throws {
        try validateSingleJSONLine(data)
        let allowed = Set(PreparedResponse.CodingKeys.allCases.map(\.rawValue))
        guard Set(try objectDictionary(in: data).keys) == allowed else {
            throw RemoteMediaControlError.unknownResponseField
        }
        let response: PreparedResponse
        do {
            response = try JSONDecoder().decode(PreparedResponse.self, from: data.dropLast())
        } catch {
            throw RemoteMediaControlError.malformedMessage
        }
        guard response.ok else { throw RemoteMediaControlError.rejected }
    }

    private static func decodeActivationResponse(_ data: Data) throws -> UInt64 {
        try validateSingleJSONLine(data)
        let object = try objectDictionary(in: data)
        let response: ActivationResponse
        do {
            response = try JSONDecoder().decode(
                ActivationResponse.self,
                from: data.dropLast()
            )
        } catch {
            throw RemoteMediaControlError.malformedMessage
        }
        if response.ok {
            guard Set(object.keys) == Set([
                ActivationResponse.CodingKeys.ok.rawValue,
                ActivationResponse.CodingKeys.activationEpoch.rawValue,
            ]),
            let epoch = response.activationEpoch,
            epoch != 0 else {
                throw RemoteMediaControlError.invalidActivation
            }
            return epoch
        }
        guard Set(object.keys) == Set([ActivationResponse.CodingKeys.ok.rawValue]),
              response.activationEpoch == nil else {
            throw RemoteMediaControlError.unknownResponseField
        }
        throw RemoteMediaControlError.rejected
    }

    private static func objectDictionary(in line: Data) throws -> [String: Any] {
        let object: Any
        do {
            object = try JSONSerialization.jsonObject(with: line.dropLast())
        } catch {
            throw RemoteMediaControlError.malformedMessage
        }
        guard let dictionary = object as? [String: Any] else {
            throw RemoteMediaControlError.malformedMessage
        }
        return dictionary
    }

    private static func validMediaSessionID(_ value: String) -> Bool {
        guard value == value.trimmingCharacters(in: .whitespacesAndNewlines),
              value.utf8.count >= 16, value.utf8.count <= 256 else { return false }
        return value.unicodeScalars.allSatisfy { scalar in
            scalar.value >= 0x21 && scalar.value != 0x7f
        }
    }

    private static func validSHA256Digest(_ value: String) -> Bool {
        value.utf8.count == 64 && value.utf8.allSatisfy {
            ($0 >= 0x30 && $0 <= 0x39) || ($0 >= 0x61 && $0 <= 0x66)
        }
    }

    private static func parseRFC3339(_ value: String) -> Date? {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = fractional.date(from: value) { return date }
        let seconds = ISO8601DateFormatter()
        seconds.formatOptions = [.withInternetDateTime]
        return seconds.date(from: value)
    }

    private static func rejectsClaim(_ value: String, now: Date) -> Bool {
        do {
            _ = try decodeClaimResponse(Data(value.utf8), now: now)
            return false
        } catch {
            return true
        }
    }

    private static func rejectsPrepared(_ value: String) -> Bool {
        do {
            try decodePreparedResponse(Data(value.utf8))
            return false
        } catch {
            return true
        }
    }

    private static func rejectsActivation(_ value: String) -> Bool {
        do {
            _ = try decodeActivationResponse(Data(value.utf8))
            return false
        } catch {
            return true
        }
    }

    private func finish<T>(
        _ result: Result<T, Error>,
        _ completion: @escaping (Result<T, Error>) -> Void
    ) {
        callbackQueue.async { completion(result) }
    }
}
