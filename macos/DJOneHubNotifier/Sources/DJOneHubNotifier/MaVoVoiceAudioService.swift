import AVFoundation
import CModemBridge
import CUACProbe
import Foundation

private func maVoAudioLog(_ stage: String) {
    guard let data = "mavo-audio-stage=\(stage)\n".data(using: .utf8) else { return }
    try? FileHandle.standardError.write(contentsOf: data)
}

struct RemoteMediaActivitySnapshot: Equatable {
    let mediaLeaseGeneration: UInt64
    let activationEpoch: UInt64
    let captureFrames: UInt64
    let playbackFrames: UInt64
}

/// Bridges modem PCM either to the Mac microphone/speaker or to one explicit,
/// generation-bound remote media lease. USB/socket waits and sample-rate
/// conversion never run on a CoreAudio render thread.
final class VoiceAudioService {
    var onError: ((String) -> Void)?
    /// A transport failure revokes only this media lease. The owner may update
    /// UI/backend lease state, but must not infer that the cellular call ended.
    var onRemoteMediaRevoked: ((UInt64) -> Void)?

    private struct DeferredUACCleanup {
        let pointer: OpaquePointer
        var lastCallbackSequence: UInt64
        var removedQuietPasses: Int
    }

    private struct RemoteActivityCounter {
        let sessionGeneration: UInt64
        let mediaLeaseGeneration: UInt64
        let activationEpoch: UInt64
        let playbackConsumedBaseline: UInt64
        var captureAcceptedFrames: UInt64
    }

    private let ioQueue = DispatchQueue(label: "app.mavo.mac.voice.usb", qos: .userInteractive)
    private let captureQueue = DispatchQueue(label: "app.mavo.mac.voice.capture", qos: .userInteractive)
    private let playbackQueue = DispatchQueue(label: "app.mavo.mac.voice.playback", qos: .userInteractive)
    private let remoteMediaCallbackQueue = DispatchQueue(
        label: "io.maccellular.voice.remote-media-callback",
        qos: .userInitiated
    )
    private let stateLock = NSLock()
    private let uploadLock = NSLock()

    private var voice: OpaquePointer?
    private var activeUAC: OpaquePointer?
    private var engine: AVAudioEngine?
    private var player: AVAudioPlayerNode?
    private var running = false
    private var startInProgress = false
    private var mediaEnabled = false
    private var pcmFlowReady = true
    private var muted = false
    private var uacCleanupPending = false
    private var sessionGeneration: UInt64 = 0
    private var remoteMediaEpoch: UInt64 = 0
    private var remoteMediaBridge: RemotePCMBridge?
    private var remoteMediaGeneration: UInt64?
    private var remoteMediaReady = false
    // Persists for the lifetime of a remote-mode UAC session even if its
    // transport is revoked, preventing a silent fallback/relabel as local.
    private var activeRemoteMediaGeneration: UInt64?
    private var activeRemoteActivationEpoch: UInt64?
    private var remoteActivityCounter: RemoteActivityCounter?
    private var rawRemotePlaybackFrames: UInt64 = 0
    private var uploadBytes = Data()
    private var captureSamples: [Float] = []
    private var capturePosition = 0.0
    private var captureSampleRate = 0.0
    private var captureConditioner = VoiceCaptureConditioner()
    private let recorder = MaVoCallRecorder()
    // Accessed only on ioQueue. A CoreAudio stop failure must retain the C
    // callback context until a later retry confirms IOProc quiescence.
    private var deferredUACCleanup: [DeferredUACCleanup] = []
    // A removed USB ancestor can make AudioDeviceStop permanently return
    // BadDevice. Once that exact IORegistry ancestor is definitively absent
    // and callbacks stay quiet, keep the context alive for process lifetime
    // without blocking a newly enumerated physical module.
    private var retiredUACTombstones: [OpaquePointer] = []
    private var uacCleanupRetryScheduled = false
    // Accessed only on playbackQueue. Caps queued audio at 400 ms so small
    // clock differences between the modem and CoreAudio cannot grow forever.
    private var scheduledPlaybackFrames = 0

    private let modemSampleRate = 8_000.0
    private let receiveChunkBytes = 640
    // ttyGS0 normally emits 256-byte PCM periods.  When the host is briefly
    // descheduled, g_serial can coalesce several periods into one bulk IN
    // transaction.  A 640-byte USB request then fails with kIOReturnOverrun
    // before any of that transaction is usable.  Keep the 40 ms playback
    // chunk above, but submit a larger transport buffer for backlog bursts.
    private let usbReceiveBufferBytes = 4_096
    private let transmitChunkBytes = 1_600
    private let maximumUploadBytes = 6_400
    // Drain up to four 512-frame device periods per worker pass. This lets the
    // bridge catch up after normal macOS scheduling pauses instead of leaving
    // the realtime callback to overflow a very short ring.
    private let uacTransferFrames = 2_048
    private let uacIdleInterval = 0.005
    private let uacCallbackStallNanoseconds: UInt64 = 3_000_000_000
    private let maximumScheduledPlaybackFrames = 3_200
    private let remoteFrameNanoseconds: UInt64 = 20_000_000

    /// Authenticates a remote PCM bridge without opening UAC or changing the
    /// cellular call. The lease generation is independent of CLCC topology.
    func prepareRemoteMedia(
        lease: RemotePCMBridgeLease,
        completion: @escaping (Result<UInt64, Error>) -> Void
    ) {
        guard let bridge = RemotePCMBridge.makeIfConfigured(
            lease,
            callbackQueue: remoteMediaCallbackQueue
        ) else {
            DispatchQueue.main.async {
                completion(.failure(RemotePCMBridgeError.invalidLease))
            }
            return
        }

        let prepared: (epoch: UInt64, previous: RemotePCMBridge?)? = stateLock.withLock {
            guard !running, !startInProgress,
                  activeRemoteMediaGeneration == nil else { return nil }
            remoteMediaEpoch &+= 1
            let previous = remoteMediaBridge
            remoteMediaBridge = bridge
            remoteMediaGeneration = lease.mediaLeaseGeneration
            remoteMediaReady = false
            return (remoteMediaEpoch, previous)
        }
        guard let prepared else {
            bridge.stop()
            DispatchQueue.main.async {
                completion(.failure(VoiceRemoteMediaError.mediaAlreadyActive))
            }
            return
        }
        prepared.previous?.stop()
        let preparationEpoch = prepared.epoch

        bridge.start(
            onDisconnect: { [weak self, weak bridge] generation in
                guard let bridge else { return }
                self?.handleRemoteMediaDisconnect(
                    bridge: bridge,
                    generation: generation,
                    epoch: preparationEpoch
                )
            },
            completion: { [weak self] result in
                guard let self else {
                    bridge.stop()
                    return
                }
                switch result {
                case .success:
                    let accepted = self.stateLock.withLock {
                        guard self.remoteMediaEpoch == preparationEpoch,
                              self.remoteMediaBridge === bridge,
                              self.remoteMediaGeneration == lease.mediaLeaseGeneration,
                              !self.running,
                              !self.startInProgress,
                              bridge.isReady(
                                  mediaLeaseGeneration: lease.mediaLeaseGeneration
                              ) else {
                            return false
                        }
                        self.remoteMediaReady = true
                        return true
                    }
                    guard accepted else {
                        bridge.stop()
                        DispatchQueue.main.async {
                            completion(.failure(VoiceRemoteMediaError.staleGeneration))
                        }
                        return
                    }
                    DispatchQueue.main.async {
                        let stillCurrent = self.stateLock.withLock {
                            self.remoteMediaEpoch == preparationEpoch &&
                                self.remoteMediaBridge === bridge &&
                                self.remoteMediaGeneration == lease.mediaLeaseGeneration &&
                                self.remoteMediaReady
                        }
                        if stillCurrent,
                           bridge.isReady(
                               mediaLeaseGeneration: lease.mediaLeaseGeneration
                           ) {
                            completion(.success(lease.mediaLeaseGeneration))
                        } else {
                            completion(.failure(VoiceRemoteMediaError.staleGeneration))
                        }
                    }
                case .failure(let error):
                    self.stateLock.withLock {
                        guard self.remoteMediaEpoch == preparationEpoch,
                              self.remoteMediaBridge === bridge else { return }
                        self.remoteMediaBridge = nil
                        self.remoteMediaGeneration = nil
                        self.remoteMediaReady = false
                    }
                    bridge.stop()
                    DispatchQueue.main.async { completion(.failure(error)) }
                }
            }
        )
    }

    /// Revokes exactly one media lease and leaves UAC/cellular call teardown to
    /// their existing owners. A stale generation cannot revoke a newer lease.
    @discardableResult
    func revokeRemoteMedia(mediaLeaseGeneration generation: UInt64) -> Bool {
        let bridge: RemotePCMBridge? = stateLock.withLock {
            guard generation != 0, remoteMediaGeneration == generation else { return nil }
            remoteMediaEpoch &+= 1
            let current = remoteMediaBridge
            remoteMediaBridge = nil
            remoteMediaGeneration = nil
            remoteMediaReady = false
            if activeRemoteMediaGeneration == generation {
                activeRemoteActivationEpoch = nil
                remoteActivityCounter = nil
            }
            return current
        }
        bridge?.stop()
        return bridge != nil
    }

    var preparedRemoteMediaGeneration: UInt64? {
        stateLock.withLock {
            guard remoteMediaReady,
                  let generation = remoteMediaGeneration,
                  remoteMediaBridge?.isReady(mediaLeaseGeneration: generation) == true else {
                return nil
            }
            return generation
        }
    }

    func requestMicrophoneAccess(completion: @escaping (Bool) -> Void) {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized:
            completion(true)
        case .notDetermined:
            AVCaptureDevice.requestAccess(for: .audio) { granted in
                DispatchQueue.main.async { completion(granted) }
            }
        case .denied, .restricted:
            completion(false)
        @unknown default:
            completion(false)
        }
    }

    func start(
        matchingLocationID: UInt32,
        remoteMediaGeneration requestedRemoteGeneration: UInt64? = nil,
        remoteActivationEpoch requestedActivationEpoch: UInt64? = nil,
        completion: @escaping (ModemActionResult) -> Void
    ) {
        guard matchingLocationID != 0 else {
            completion(.failure("USB 语音接口缺少可绑定的 locationID。"))
            return
        }
        guard (requestedRemoteGeneration == nil) == (requestedActivationEpoch == nil),
              requestedActivationEpoch.map({ $0 != 0 }) ?? true else {
            completion(.failure("远程媒体 generation 与 activation epoch 必须成对且非零。"))
            return
        }
        let selectedRemoteBridge: RemotePCMBridge?
        if let requestedRemoteGeneration {
            selectedRemoteBridge = stateLock.withLock {
                guard requestedRemoteGeneration != 0,
                      remoteMediaReady,
                      remoteMediaGeneration == requestedRemoteGeneration else { return nil }
                return remoteMediaBridge
            }
            guard let selectedRemoteBridge,
                  selectedRemoteBridge.isReady(mediaLeaseGeneration: requestedRemoteGeneration) else {
                completion(.failure("远程媒体 lease 未连接或 generation 已失效。"))
                return
            }
        } else {
            selectedRemoteBridge = nil
        }
        var reservationError: String?
        let session: UInt64? = stateLock.withLock {
            if startInProgress {
                reservationError = "另一项语音启动仍在进行。"
                return nil
            }
            if running { return nil }
            startInProgress = true
            sessionGeneration &+= 1
            return sessionGeneration
        }
        guard let session else {
            DispatchQueue.main.async {
                if let reservationError {
                    completion(.failure(reservationError))
                } else {
                    completion(.success())
                }
            }
            return
        }
        ioQueue.async { [weak self] in
            guard let self else { return }
            defer { self.releaseStartReservation(session: session) }
            maVoAudioLog("start-worker")
            guard self.isCurrentSession(session) else {
                DispatchQueue.main.async { completion(.failure("语音启动已取消。")) }
                return
            }
            guard let voice = mavo_voice_create() else {
                DispatchQueue.main.async { completion(.failure("无法初始化 USB 语音接口。")) }
                return
            }
            let openResult = mavo_voice_open_for_location(voice, matchingLocationID)
            guard openResult == MAVO_MODEM_OK else {
                let error = String(cString: mavo_voice_last_error(voice))
                maVoAudioLog("raw-open-failed-\(openResult)-\(error)")
                mavo_voice_destroy(voice)
                DispatchQueue.main.async {
                    completion(.failure(error.isEmpty ? "无法打开 USB interface 1 语音通道。" : error))
                }
                return
            }
            maVoAudioLog(String(
                format: "raw-opened-out-%02X-in-%02X",
                mavo_voice_output_endpoint(voice),
                mavo_voice_input_endpoint(voice)
            ))
            guard self.isCurrentSession(session) else {
                mavo_voice_destroy(voice)
                DispatchQueue.main.async { completion(.failure("语音启动已取消。")) }
                return
            }

            var voiceProcessingEnabled = false
            if selectedRemoteBridge == nil {
                do {
                    voiceProcessingEnabled = try self.startAudioEngine(session: session)
                } catch {
                    mavo_voice_destroy(voice)
                    DispatchQueue.main.async {
                        completion(.failure("无法启动 Mac 麦克风/扬声器：\(error.localizedDescription)"))
                    }
                    return
                }
            }

            guard self.isCurrentSession(session) else {
                self.stopAudioEngine()
                mavo_voice_destroy(voice)
                DispatchQueue.main.async { completion(.failure("语音启动已取消。")) }
                return
            }

            self.stateLock.withLock {
                self.voice = voice
                self.running = true
                self.startInProgress = false
                self.mediaEnabled = false
                self.muted = false
                if let requestedRemoteGeneration, let requestedActivationEpoch {
                    self.activeRemoteMediaGeneration = requestedRemoteGeneration
                    self.activeRemoteActivationEpoch = requestedActivationEpoch
                    self.rawRemotePlaybackFrames = 0
                    self.remoteActivityCounter = RemoteActivityCounter(
                        sessionGeneration: session,
                        mediaLeaseGeneration: requestedRemoteGeneration,
                        activationEpoch: requestedActivationEpoch,
                        playbackConsumedBaseline: 0,
                        captureAcceptedFrames: 0
                    )
                }
            }
            self.resetBuffers()
            let description = String(
                format: "语音 #1 · OUT 0x%02X · IN 0x%02X",
                mavo_voice_output_endpoint(voice),
                mavo_voice_input_endpoint(voice)
            ) + (voiceProcessingEnabled ? " · 回声消除" : "")
            DispatchQueue.main.async { completion(.success(description)) }
            self.runUSBLoop(
                voice,
                session: session,
                remoteBridge: selectedRemoteBridge,
                remoteGeneration: requestedRemoteGeneration
            )
            self.stopAudioEngine()
            mavo_voice_destroy(voice)
            self.stateLock.withLock {
                if self.voice == voice {
                    self.voice = nil
                }
                if self.sessionGeneration == session {
                    self.running = false
                    self.mediaEnabled = false
                    self.activeRemoteMediaGeneration = nil
                    self.activeRemoteActivationEpoch = nil
                    self.remoteActivityCounter = nil
                }
            }
            self.resetBuffers()
            if let requestedRemoteGeneration {
                _ = self.revokeRemoteMedia(mediaLeaseGeneration: requestedRemoteGeneration)
            }
        }
    }

    func validateUAC(
        vendorID: UInt16,
        productID: UInt16,
        matchingLocationID: UInt32,
        completion: @escaping (ModemActionResult) -> Void
    ) {
        guard vendorID != 0, productID != 0, matchingLocationID != 0 else {
            completion(.failure("UAC 语音接口缺少完整的 USB 身份。"))
            return
        }
        guard stateLock.withLock({ !running && !startInProgress && activeUAC == nil }) else {
            completion(.failure("已有语音媒体正在运行。"))
            return
        }
        ioQueue.async { [weak self] in
            guard let self else { return }
            self.reapDeferredUACCleanupNow()
            guard self.deferredUACCleanup.isEmpty else {
                DispatchQueue.main.async {
                    completion(.failure("上一次 UAC IOProc 尚未完成清理，请稍后重试。"))
                }
                return
            }
            guard let uac = mavo_uac_probe_create() else {
                DispatchQueue.main.async {
                    completion(.failure("无法初始化模块 UAC 音频通道。"))
                }
                return
            }
            maVoAudioLog("uac-open-requested")
            let openResult = self.openUAC(
                uac,
                vendorID: vendorID,
                productID: productID,
                matchingLocationID: matchingLocationID,
                preferredUID: nil
            )
            guard openResult == MAVO_UAC_OK else {
                maVoAudioLog("uac-open-failed")
                let error = self.lastUACError(
                    uac,
                    fallback: "没有找到与模块匹配的 8 kHz UAC 设备。"
                )
                let cleanupError = self.disposeUACOrDefer(uac)
                DispatchQueue.main.async {
                    completion(.failure(cleanupError.map { "\(error)\nUAC 清理失败：\($0)" } ?? error))
                }
                return
            }
            maVoAudioLog("uac-opened")
            let uid = mavo_uac_probe_uid(uac).map(String.init(cString:)) ?? ""
            guard !uid.isEmpty, mavo_uac_probe_usb_binding_verified(uac) != 0 else {
                let cleanupError = self.disposeUACOrDefer(uac)
                DispatchQueue.main.async {
                    completion(.failure(
                        cleanupError.map { "UAC USB 身份校验失败，且清理未完成：\($0)" }
                            ?? "UAC USB 身份校验失败。"
                    ))
                }
                return
            }
            let cleanupError = self.disposeUACOrDefer(uac)
            DispatchQueue.main.async {
                if let cleanupError {
                    completion(.failure("UAC 预检后的清理未完成：\(cleanupError)"))
                } else {
                    completion(.success(uid))
                }
            }
        }
    }

    func startUAC(
        vendorID: UInt16,
        productID: UInt16,
        matchingLocationID: UInt32,
        preferredUID: String? = nil,
        remoteMediaGeneration requestedRemoteGeneration: UInt64? = nil,
        remoteActivationEpoch requestedActivationEpoch: UInt64? = nil,
        completion: @escaping (ModemActionResult) -> Void
    ) {
        guard vendorID != 0, productID != 0, matchingLocationID != 0 else {
            completion(.failure("UAC 语音接口缺少完整的 USB 身份。"))
            return
        }
        guard (requestedRemoteGeneration == nil) == (requestedActivationEpoch == nil),
              requestedActivationEpoch.map({ $0 != 0 }) ?? true else {
            completion(.failure("远程媒体 generation 与 activation epoch 必须成对且非零。"))
            return
        }
        let selectedRemoteBridge: RemotePCMBridge?
        if let requestedRemoteGeneration {
            selectedRemoteBridge = stateLock.withLock {
                guard requestedRemoteGeneration != 0,
                      remoteMediaReady,
                      remoteMediaGeneration == requestedRemoteGeneration,
                      let bridge = remoteMediaBridge else { return nil }
                return bridge
            }
            guard let selectedRemoteBridge,
                  selectedRemoteBridge.isReady(
                      mediaLeaseGeneration: requestedRemoteGeneration
                  ) else {
                completion(.failure("远程媒体 lease 未连接或 generation 已失效。"))
                return
            }
        } else {
            selectedRemoteBridge = nil
        }
        var reservationError: String?
        let session: UInt64? = stateLock.withLock {
            if startInProgress {
                reservationError = "另一项 UAC 语音启动仍在进行。"
                return nil
            }
            if running {
                if activeRemoteMediaGeneration != requestedRemoteGeneration ||
                    activeRemoteActivationEpoch != requestedActivationEpoch {
                    reservationError = "现有语音会话不属于请求的远程媒体 generation。"
                }
                return nil
            }
            startInProgress = true
            sessionGeneration &+= 1
            return sessionGeneration
        }
        guard let session else {
            DispatchQueue.main.async {
                if let reservationError {
                    completion(.failure(reservationError))
                } else {
                    completion(.success())
                }
            }
            return
        }

        ioQueue.async { [weak self] in
            guard let self else { return }
            defer { self.releaseStartReservation(session: session) }
            guard self.isCurrentSession(session) else {
                DispatchQueue.main.async { completion(.failure("语音启动已取消。")) }
                return
            }
            self.reapDeferredUACCleanupNow()
            guard self.deferredUACCleanup.isEmpty else {
                DispatchQueue.main.async {
                    completion(.failure("上一次 UAC IOProc 尚未完成清理，请稍后重试。"))
                }
                return
            }
            guard let uac = mavo_uac_probe_create() else {
                DispatchQueue.main.async { completion(.failure("无法初始化模块 UAC 音频通道。")) }
                return
            }
            var audioEngineStarted = false
            var voiceProcessingEnabled = false

            func failStart(_ message: String) {
                mavo_uac_probe_flush_pcm(uac)
                if audioEngineStarted {
                    self.stopAudioEngine()
                }
                let cleanupError = self.disposeUACOrDefer(uac)
                self.resetBuffersSynchronously()
                let detail = cleanupError.map {
                    "\(message)\nUAC 清理将在后台重试：\($0)"
                } ?? message
                DispatchQueue.main.async { completion(.failure(detail)) }
            }

            let openResult = self.openUAC(
                uac,
                vendorID: vendorID,
                productID: productID,
                matchingLocationID: matchingLocationID,
                preferredUID: preferredUID
            )
            guard openResult == MAVO_UAC_OK else {
                failStart(self.lastUACError(uac, fallback: "没有找到与模块匹配的 8 kHz UAC 设备。"))
                return
            }
            guard self.isCurrentSession(session) else {
                failStart("语音启动已取消。")
                return
            }

            self.resetBuffersSynchronously()
            if selectedRemoteBridge == nil {
                do {
                    voiceProcessingEnabled = try self.startAudioEngine(session: session)
                    audioEngineStarted = true
                } catch {
                    failStart("无法启动 Mac 麦克风/扬声器：\(error.localizedDescription)")
                    return
                }
            }
            guard self.isCurrentSession(session) else {
                failStart("语音启动已取消。")
                return
            }
            if let requestedRemoteGeneration, let selectedRemoteBridge {
                guard self.isPreparedRemoteMedia(
                    bridge: selectedRemoteBridge,
                    generation: requestedRemoteGeneration
                ) else {
                    failStart("远程媒体 lease 在 UAC 启动前已失效。")
                    return
                }
            }

            maVoAudioLog("pcm-start-requested")
            let startResult = mavo_uac_probe_start_pcm_bridge(uac)
            guard startResult == MAVO_UAC_OK else {
                maVoAudioLog("pcm-start-failed")
                failStart(self.lastUACError(uac, fallback: "无法启动模块 UAC PCM 通道。"))
                return
            }
            maVoAudioLog("pcm-started")
            guard self.isCurrentSession(session) else {
                failStart("语音启动已取消。")
                return
            }

            mavo_uac_probe_flush_pcm(uac)
            let mediaStateAccepted = self.stateLock.withLock {
                if let requestedRemoteGeneration {
                    guard let requestedActivationEpoch,
                          self.remoteMediaReady,
                          self.remoteMediaGeneration == requestedRemoteGeneration,
                          self.remoteMediaBridge === selectedRemoteBridge,
                          selectedRemoteBridge?.isReady(
                              mediaLeaseGeneration: requestedRemoteGeneration
                          ) == true else {
                        return false
                    }
                    self.activeRemoteMediaGeneration = requestedRemoteGeneration
                    self.activeRemoteActivationEpoch = requestedActivationEpoch
                    self.remoteActivityCounter = RemoteActivityCounter(
                        sessionGeneration: session,
                        mediaLeaseGeneration: requestedRemoteGeneration,
                        activationEpoch: requestedActivationEpoch,
                        playbackConsumedBaseline:
                            mavo_uac_probe_remote_payload_consumed_frames(uac),
                        captureAcceptedFrames: 0
                    )
                } else {
                    self.activeRemoteMediaGeneration = nil
                    self.activeRemoteActivationEpoch = nil
                    self.remoteActivityCounter = nil
                }
                self.running = true
                self.startInProgress = false
                self.mediaEnabled = false
                self.muted = false
                self.activeUAC = uac
                return true
            }
            guard mediaStateAccepted else {
                failStart("远程媒体 lease 在 UAC 接线时已失效。")
                return
            }
            let uacName: String
            if let rawName = mavo_uac_probe_name(uac) {
                uacName = String(cString: rawName)
            } else {
                uacName = "QDC507 UAC"
            }
            DispatchQueue.main.async { [weak self] in
                guard let self, self.isActiveSession(session) else {
                    completion(.failure("语音启动已取消。"))
                    return
                }
                if let requestedRemoteGeneration {
                    let stillOwned = self.stateLock.withLock {
                        self.activeRemoteMediaGeneration == requestedRemoteGeneration &&
                            self.activeRemoteActivationEpoch == requestedActivationEpoch
                    }
                    guard stillOwned else {
                        completion(.failure("远程媒体 lease 已在启动完成前撤销。"))
                        return
                    }
                    guard selectedRemoteBridge?.isReady(
                        mediaLeaseGeneration: requestedRemoteGeneration
                    ) == true else {
                        completion(.failure("远程媒体 bridge 已在启动完成前断开。"))
                        return
                    }
                }
                completion(.success(
                    "UAC · \(uacName) · 8 kHz" +
                        (selectedRemoteBridge == nil
                            ? (voiceProcessingEnabled ? " · 本机 · 回声消除" : " · 本机")
                            : " · 远程媒体")
                ))
            }

            maVoAudioLog("uac-loop-started")

            let fatalError = self.runUACLoop(
                uac,
                session: session,
                remoteBridge: selectedRemoteBridge,
                remoteGeneration: requestedRemoteGeneration
            )
            self.stateLock.withLock {
                if self.activeUAC == uac {
                    self.activeUAC = nil
                }
                if self.sessionGeneration == session {
                    self.running = false
                    self.mediaEnabled = false
                    if self.activeRemoteMediaGeneration == requestedRemoteGeneration {
                        self.activeRemoteMediaGeneration = nil
                        self.activeRemoteActivationEpoch = nil
                        self.remoteActivityCounter = nil
                    }
                }
            }
            self.clearUploadBytes()
            mavo_uac_probe_flush_pcm(uac)
            self.stopAudioEngine()
            let stopError = self.disposeUACOrDefer(uac)
            self.resetBuffersSynchronously()

            if let requestedRemoteGeneration {
                _ = self.revokeRemoteMedia(
                    mediaLeaseGeneration: requestedRemoteGeneration
                )
            }

            if let fatalError {
                self.reportUACError(fatalError, session: session)
            } else if let stopError, self.isCurrentSession(session) {
                self.reportUACError(stopError, session: session)
            }
        }
    }

    func stop(completion: (() -> Void)? = nil) {
        let remoteBridge = stateLock.withLock {
            sessionGeneration &+= 1
            running = false
            startInProgress = false
            mediaEnabled = false
            muted = false
            activeUAC = nil
            activeRemoteMediaGeneration = nil
            activeRemoteActivationEpoch = nil
            remoteActivityCounter = nil
            remoteMediaEpoch &+= 1
            let bridge = remoteMediaBridge
            remoteMediaBridge = nil
            remoteMediaGeneration = nil
            remoteMediaReady = false
            return bridge
        }
        remoteBridge?.stop()
        clearUploadBytes()
        guard let completion else { return }
        ioQueue.async(execute: completion)
    }

    @discardableResult
    func setMediaEnabled(
        _ enabled: Bool,
        remoteMediaGeneration requestedGeneration: UInt64? = nil,
        remoteActivationEpoch requestedActivationEpoch: UInt64? = nil
    ) -> Bool {
        var remote: (RemotePCMBridge, UInt64)?
        let accepted = stateLock.withLock {
            guard running, activeUAC != nil || voice != nil else { return false }
            if let activeGeneration = activeRemoteMediaGeneration {
                guard requestedGeneration == activeGeneration,
                      let activeEpoch = activeRemoteActivationEpoch,
                      activeEpoch != 0,
                      requestedActivationEpoch == activeEpoch,
                      remoteMediaGeneration == activeGeneration,
                      remoteMediaReady,
                      let bridge = remoteMediaBridge else {
                    return false
                }
                remote = (bridge, activeGeneration)
                if enabled {
                    let existing = remoteActivityCounter
                    if existing?.sessionGeneration != sessionGeneration ||
                        existing?.mediaLeaseGeneration != activeGeneration ||
                        existing?.activationEpoch != activeEpoch {
                    let baseline = activeUAC.map {
                        mavo_uac_probe_remote_payload_consumed_frames($0)
                    } ?? rawRemotePlaybackFrames
                    remoteActivityCounter = RemoteActivityCounter(
                            sessionGeneration: sessionGeneration,
                            mediaLeaseGeneration: activeGeneration,
                            activationEpoch: activeEpoch,
                        playbackConsumedBaseline: baseline,
                            captureAcceptedFrames: 0
                        )
                    }
                }
            } else {
                guard requestedGeneration == nil, requestedActivationEpoch == nil else {
                    return false
                }
                remoteActivityCounter = nil
            }
            mediaEnabled = enabled
            return true
        }
        guard accepted else { return false }
        if let remote,
           !remote.0.setMediaFlowEnabled(
               enabled,
               mediaLeaseGeneration: remote.1
           ) {
            stateLock.withLock {
                mediaEnabled = false
            }
            clearUploadBytes()
            return false
        }
        if !enabled { clearUploadBytes() }
        return true
    }

    func remoteMediaActivitySnapshot(
        mediaLeaseGeneration generation: UInt64,
        activationEpoch: UInt64
    ) -> RemoteMediaActivitySnapshot? {
        stateLock.withLock {
            guard generation != 0, activationEpoch != 0,
                  running, mediaEnabled,
                  activeRemoteMediaGeneration == generation,
                  activeRemoteActivationEpoch == activationEpoch,
                  let counter = remoteActivityCounter,
                  counter.sessionGeneration == sessionGeneration,
                  counter.mediaLeaseGeneration == generation,
                  counter.activationEpoch == activationEpoch else {
                return nil
            }
            let consumed = activeUAC.map {
                mavo_uac_probe_remote_payload_consumed_frames($0)
            } ?? rawRemotePlaybackFrames
            let playbackFrames = consumed >= counter.playbackConsumedBaseline
                ? consumed - counter.playbackConsumedBaseline
                : 0
            return RemoteMediaActivitySnapshot(
                mediaLeaseGeneration: generation,
                activationEpoch: activationEpoch,
                captureFrames: counter.captureAcceptedFrames,
                playbackFrames: playbackFrames
            )
        }
    }

    func setPCMFlowReady(_ ready: Bool) {
        stateLock.withLock { pcmFlowReady = ready }
        trimUploadBytesToLatestChunk()
    }

    func setMuted(_ value: Bool) {
        stateLock.withLock { muted = value }
        if value { clearUploadBytes() }
    }

    var isRunning: Bool {
        stateLock.withLock { running }
    }

    var isRecording: Bool { recorder.isRecording }

    func startRecording() throws -> String { try recorder.start() }

    func stopRecording(completion: @escaping (String?) -> Void) { recorder.stop(completion: completion) }

    var hasUnresolvedUACCleanup: Bool {
        stateLock.withLock { uacCleanupPending }
    }

    var uacMediaSnapshot: UACMediaSnapshot? {
        stateLock.withLock {
            guard let uac = activeUAC else { return nil }
            return UACMediaSnapshot(
                inputFrames: mavo_uac_probe_input_frames(uac),
                outputFrames: mavo_uac_probe_output_frames(uac),
                inputTotalSamples: mavo_uac_probe_input_total_samples(uac),
                inputSignalSamples: mavo_uac_probe_input_signal_samples(uac),
                inputPeakPCM16: mavo_uac_probe_input_peak_pcm16(uac),
                inputSignalThresholdPCM16: mavo_uac_probe_input_signal_threshold_pcm16(uac),
                downlinkDroppedFrames: mavo_uac_probe_downlink_dropped_frames(uac),
                uplinkDroppedFrames: mavo_uac_probe_uplink_dropped_frames(uac)
            )
        }
    }

    private func openUAC(
        _ uac: OpaquePointer,
        vendorID: UInt16,
        productID: UInt16,
        matchingLocationID: UInt32,
        preferredUID: String?
    ) -> Int32 {
        if let preferredUID {
            return preferredUID.withCString { uid in
                mavo_uac_probe_open_for_usb(
                    uac,
                    vendorID,
                    productID,
                    matchingLocationID,
                    uid
                )
            }
        }
        return mavo_uac_probe_open_for_usb(
            uac,
            vendorID,
            productID,
            matchingLocationID,
            nil
        )
    }

    private func startAudioEngine(session: UInt64) throws -> Bool {
        try playbackQueue.sync {
            scheduledPlaybackFrames = 0
            let engine = AVAudioEngine()
            let player = AVAudioPlayerNode()
            let playbackFormat = AVAudioFormat(
                standardFormatWithSampleRate: modemSampleRate,
                channels: 1
            )!
            engine.attach(player)
            engine.connect(player, to: engine.mainMixerNode, format: playbackFormat)

            let input = engine.inputNode
            let voiceProcessingEnabled: Bool
            do {
                try input.setVoiceProcessingEnabled(true)
                input.isVoiceProcessingBypassed = false
                input.isVoiceProcessingAGCEnabled = true
                voiceProcessingEnabled = input.isVoiceProcessingEnabled
            } catch {
                // Some split Bluetooth/USB device combinations cannot enter
                // VoiceProcessingIO. Keep calls usable and rely on the local
                // telephone-band conditioner instead.
                voiceProcessingEnabled = false
            }
            let inputFormat = input.outputFormat(forBus: 0)
            guard inputFormat.sampleRate > 0, inputFormat.channelCount > 0 else {
                throw VoiceAudioError.microphoneUnavailable
            }
            // 20 ms capture periods reduce conversational latency compared
            // with the previous 50 ms microphone batches.
            let tapFrames = AVAudioFrameCount(max(256, Int(inputFormat.sampleRate / 50)))
            input.installTap(onBus: 0, bufferSize: tapFrames, format: inputFormat) { [weak self] buffer, _ in
                self?.copyMicrophoneSamples(buffer, session: session)
            }

            engine.prepare()
            try engine.start()
            player.play()
            self.engine = engine
            self.player = player
            return voiceProcessingEnabled
        }
    }

    private func stopAudioEngine() {
        playbackQueue.sync {
            if let engine {
                engine.inputNode.removeTap(onBus: 0)
                player?.stop()
                engine.stop()
            }
            self.player = nil
            self.engine = nil
            scheduledPlaybackFrames = 0
        }
    }

    private func copyMicrophoneSamples(_ buffer: AVAudioPCMBuffer, session: UInt64) {
        guard isCurrentSession(session) else { return }
        guard let channels = buffer.floatChannelData else { return }
        let frameCount = Int(buffer.frameLength)
        let channelCount = Int(buffer.format.channelCount)
        guard frameCount > 0, channelCount > 0 else { return }

        var mono = [Float](repeating: 0, count: frameCount)
        for channel in 0 ..< channelCount {
            let source = channels[channel]
            for frame in 0 ..< frameCount {
                mono[frame] += source[frame] / Float(channelCount)
            }
        }
        let sampleRate = buffer.format.sampleRate
        captureQueue.async { [weak self] in
            self?.resampleAndQueue(mono, sampleRate: sampleRate, session: session)
        }
    }

    private func resampleAndQueue(_ samples: [Float], sampleRate: Double, session: UInt64) {
        let state = stateLock.withLock {
            (sessionGeneration == session, running, mediaEnabled, muted)
        }
        guard state.0, state.1, state.2 else { return }
        if captureSampleRate != sampleRate {
            captureSamples.removeAll(keepingCapacity: true)
            capturePosition = 0
            captureSampleRate = sampleRate
        }
        let conditioned = captureConditioner.process(samples, sampleRate: sampleRate)
        captureSamples.append(contentsOf: conditioned)
        let step = sampleRate / modemSampleRate
        var pcm = Data()
        pcm.reserveCapacity(Int(Double(samples.count) / step) * 2 + 4)

        while capturePosition + 1 < Double(captureSamples.count) {
            let lower = Int(capturePosition)
            let fraction = Float(capturePosition - Double(lower))
            let value = captureSamples[lower] * (1 - fraction) + captureSamples[lower + 1] * fraction
            let scaled: Int16
            if state.3 {
                scaled = 0
            } else {
                scaled = Int16(max(-1, min(1, value)) * Float(Int16.max))
            }
            var littleEndian = scaled.littleEndian
            withUnsafeBytes(of: &littleEndian) { pcm.append(contentsOf: $0) }
            capturePosition += step
        }

        let consumed = max(0, min(Int(capturePosition), captureSamples.count - 1))
        if consumed > 0 {
            captureSamples.removeFirst(consumed)
            capturePosition -= Double(consumed)
        }
        appendUploadBytes(pcm)
    }

    private func runUACLoop(
        _ uac: OpaquePointer,
        session: UInt64,
        remoteBridge: RemotePCMBridge?,
        remoteGeneration: UInt64?
    ) -> String? {
        var downlinkSamples = [Int16](repeating: 0, count: uacTransferFrames)
        var uplinkSamples = [Int16](repeating: 0, count: uacTransferFrames)
        var remoteFramer = RemotePCM20msFramer()
        var lastInputCallbacks = mavo_uac_probe_input_callbacks(uac)
        var lastOutputCallbacks = mavo_uac_probe_output_callbacks(uac)
        var inputProgressAt = DispatchTime.now().uptimeNanoseconds
        var outputProgressAt = inputProgressAt
        var mediaWasEnabled = false
        var muteWasEnabled = false
        let usesRemoteMedia = remoteBridge != nil && remoteGeneration != nil

        while isActiveSession(session) {
            let state = stateLock.withLock {
                (sessionGeneration == session && running, mediaEnabled, muted)
            }
            guard state.0 else { break }

            let now = DispatchTime.now().uptimeNanoseconds
            guard state.1 else {
                clearUploadBytes()
                mavo_uac_probe_flush_pcm(uac)
                lastInputCallbacks = mavo_uac_probe_input_callbacks(uac)
                lastOutputCallbacks = mavo_uac_probe_output_callbacks(uac)
                inputProgressAt = now
                outputProgressAt = now
                mediaWasEnabled = false
                muteWasEnabled = false
                remoteFramer.reset()
                Thread.sleep(forTimeInterval: uacIdleInterval)
                continue
            }
            if !mediaWasEnabled {
                mavo_uac_probe_flush_pcm(uac)
                clearUploadBytes()
                lastInputCallbacks = mavo_uac_probe_input_callbacks(uac)
                lastOutputCallbacks = mavo_uac_probe_output_callbacks(uac)
                inputProgressAt = now
                outputProgressAt = now
                mediaWasEnabled = true
                remoteFramer.reset()
            }
            if state.2 != muteWasEnabled {
                clearUploadBytes()
                mavo_uac_probe_flush_uplink_pcm(uac)
            }
            muteWasEnabled = state.2

            var uplinkPCM = Data()
            if let remoteBridge, let remoteGeneration {
                // Do not pre-fill the UAC ring with synthetic silence. Its
                // realtime output callback already zero-fills a genuine
                // underrun. Pre-filling here put silence ahead of real phone
                // audio when CoreAudio requested a large USB period.
                let maximumRemoteFrames = uplinkSamples.count /
                    (RemotePCM20msFramer.frameBytes / MemoryLayout<Int16>.size)
                var receivedRemoteFrames = 0
                while receivedRemoteFrames < maximumRemoteFrames {
                    let remoteRead = remoteBridge.takeRemoteUplinkFrameOrSilence(
                        mediaLeaseGeneration: remoteGeneration
                    )
                    guard remoteRead.wasRemoteFrame else { break }
                    let sampleOffset = receivedRemoteFrames *
                        (RemotePCM20msFramer.frameBytes / MemoryLayout<Int16>.size)
                    remoteRead.data.withUnsafeBytes { rawBuffer in
                        let bytes = rawBuffer.bindMemory(to: UInt8.self)
                        for frame in 0 ..< (RemotePCM20msFramer.frameBytes / MemoryLayout<Int16>.size) {
                            let bits = UInt16(bytes[frame * 2]) |
                                UInt16(bytes[frame * 2 + 1]) << 8
                            uplinkSamples[sampleOffset + frame] = Int16(bitPattern: bits)
                        }
                    }
                    if !state.2 {
                        uplinkPCM.append(remoteRead.data)
                    }
                    receivedRemoteFrames += 1
                }
                let remoteSamples = receivedRemoteFrames *
                    (RemotePCM20msFramer.frameBytes / MemoryLayout<Int16>.size)
                if remoteSamples > 0, !state.2 {
                    _ = uplinkSamples.withUnsafeBufferPointer { samples in
                        mavo_uac_probe_write_remote_uplink_pcm16(
                            uac,
                            samples.baseAddress,
                            remoteSamples
                        )
                    }
                }
            } else {
                let uplinkFrames = takeUploadSamples(into: &uplinkSamples)
                if uplinkFrames > 0 {
                    uplinkPCM = uplinkSamples.prefix(uplinkFrames).withUnsafeBytes { Data($0) }
                    _ = uplinkSamples.withUnsafeBufferPointer { samples in
                        mavo_uac_probe_write_uplink_pcm16(
                            uac,
                            samples.baseAddress,
                            uplinkFrames
                        )
                    }
                }
            }

            let downlinkFrames = downlinkSamples.withUnsafeMutableBufferPointer { samples in
                Int(mavo_uac_probe_read_downlink_pcm16(
                    uac,
                    samples.baseAddress,
                    samples.count
                ))
            }
            if downlinkFrames > 0 {
                let frameCount = min(downlinkFrames, downlinkSamples.count)
                let byteCount = frameCount * MemoryLayout<Int16>.size
                let pcm = downlinkSamples.withUnsafeBytes { bytes in
                    Data(bytes: bytes.baseAddress!, count: byteCount)
                }
                if usesRemoteMedia, let remoteBridge, let remoteGeneration {
                    for frame in remoteFramer.append(pcm) {
                        let accepted = remoteBridge.offerModemDownlink(
                            frame,
                            mediaLeaseGeneration: remoteGeneration
                        )
                        if accepted {
                            recordRemoteCaptureAccepted(
                                frames: frame.count / MemoryLayout<Int16>.size,
                                mediaLeaseGeneration: remoteGeneration,
                                session: session
                            )
                        }
                    }
                } else {
                    schedulePlayback(pcm, session: session)
                }
                recorder.enqueue(far: pcm, near: uplinkPCM)
            }

            if mavo_uac_probe_is_running(uac) == 0 {
                return lastUACError(uac, fallback: "模块 UAC IOProc 已停止。")
            }
            let inputCallbacks = mavo_uac_probe_input_callbacks(uac)
            let outputCallbacks = mavo_uac_probe_output_callbacks(uac)
            let progressNow = DispatchTime.now().uptimeNanoseconds
            if inputCallbacks != lastInputCallbacks {
                lastInputCallbacks = inputCallbacks
                inputProgressAt = progressNow
            }
            if outputCallbacks != lastOutputCallbacks {
                lastOutputCallbacks = outputCallbacks
                outputProgressAt = progressNow
            }
            if progressNow >= inputProgressAt,
               progressNow - inputProgressAt >= uacCallbackStallNanoseconds {
                return "模块 UAC 下行 IOProc 长时间没有推进。"
            }
            if progressNow >= outputProgressAt,
               progressNow - outputProgressAt >= uacCallbackStallNanoseconds {
                return "模块 UAC 上行 IOProc 长时间没有推进。"
            }

            Thread.sleep(forTimeInterval: uacIdleInterval)
        }
        return nil
    }

    private func runUSBLoop(
        _ voice: OpaquePointer,
        session: UInt64,
        remoteBridge: RemotePCMBridge? = nil,
        remoteGeneration: UInt64? = nil
    ) {
        var receiveBuffer = [UInt8](repeating: 0, count: usbReceiveBufferBytes)
        var downlinkBytes = Data()
        var remoteFramer = RemotePCM20msFramer()
        var nextTransmit = DispatchTime.now().uptimeNanoseconds + 100_000_000
        var nextRemoteUplink = DispatchTime.now().uptimeNanoseconds + remoteFrameNanoseconds
        let usesRemoteMedia = remoteBridge != nil && remoteGeneration != nil
        var loggedFirstWrite = false
        var loggedFirstRead = false
        var readTimeouts = 0

        while isActiveSession(session) {
            let beforeRead = DispatchTime.now().uptimeNanoseconds
            let state = stateLock.withLock {
                (
                    sessionGeneration == session && running,
                    mediaEnabled,
                    pcmFlowReady
                )
            }
            guard state.0 else { break }

            if state.1, state.2, usesRemoteMedia,
               let remoteBridge, let remoteGeneration,
               beforeRead >= nextRemoteUplink {
                let remoteRead = remoteBridge.takeRemoteUplinkFrameOrSilence(
                    mediaLeaseGeneration: remoteGeneration
                )
                appendUploadBytes(remoteRead.data)
                if remoteRead.wasRemoteFrame {
                    stateLock.withLock {
                        rawRemotePlaybackFrames &+= UInt64(
                            remoteRead.data.count / MemoryLayout<Int16>.size
                        )
                    }
                }
                nextRemoteUplink &+= remoteFrameNanoseconds
                if beforeRead > nextRemoteUplink + remoteFrameNanoseconds {
                    nextRemoteUplink = beforeRead + remoteFrameNanoseconds
                }
            }

            let uplinkReady = !usesRemoteMedia || loggedFirstRead
            // The modem begins the call by emitting 640-byte downlink frames.
            // Do not let an unavailable uplink pipe tear down reception before
            // the first real downlink frame has arrived.
            if state.1, state.2, uplinkReady, beforeRead >= nextTransmit {
                let chunk = takeUploadChunkOrSilence()
                let writeResult = chunk.withUnsafeBytes { rawBuffer -> Int32 in
                    let bytes = rawBuffer.bindMemory(to: UInt8.self)
                    return mavo_voice_write(voice, 80, bytes.baseAddress, bytes.count)
                }
                if writeResult != MAVO_MODEM_OK {
                    maVoAudioLog("raw-write-failed-\(writeResult)")
                    reportTransportError(voice, session: session)
                    break
                }
                if !loggedFirstWrite {
                    loggedFirstWrite = true
                    maVoAudioLog("raw-first-write-ok")
                }
                nextTransmit &+= 100_000_000
                if beforeRead > nextTransmit + 100_000_000 {
                    nextTransmit = beforeRead + 100_000_000
                }
            } else if !state.1 || !state.2 {
                nextTransmit = beforeRead + 100_000_000
            }

            guard state.1 else {
                Thread.sleep(forTimeInterval: 0.01)
                continue
            }
            let readStart = DispatchTime.now().uptimeNanoseconds
            let untilTransmit = uplinkReady && nextTransmit > readStart
                ? nextTransmit - readStart
                : (uplinkReady ? 0 : 75_000_000)
            if state.2, uplinkReady, untilTransmit < 45_000_000 {
                if untilTransmit > 0 {
                    Thread.sleep(forTimeInterval: Double(untilTransmit) / 1_000_000_000)
                }
                continue
            }
            // Normal downlink cadence is 40 ms. A 75 ms ceiling gives USB and
            // scheduler jitter room; the short interval before each 100 ms
            // uplink deadline is slept instead of forcing a guaranteed timeout.
            let timeoutMs = state.2
                ? max(45, min(75, Int((untilTransmit + 999_999) / 1_000_000)))
                : 75
            let readResult = receiveBuffer.withUnsafeMutableBufferPointer { pointer in
                mavo_voice_read(voice, Int32(timeoutMs), pointer.baseAddress, pointer.count)
            }
            if readResult < 0 || (readResult > 0 && mavo_voice_is_open(voice) == 0) {
                maVoAudioLog("raw-read-failed-\(readResult)")
                reportTransportError(voice, session: session)
                break
            }
            if readResult > 0 {
                readTimeouts = 0
                if !loggedFirstRead {
                    loggedFirstRead = true
                    maVoAudioLog("raw-first-read-bytes-\(readResult)")
                }
                downlinkBytes.append(contentsOf: receiveBuffer.prefix(Int(readResult)))
                while downlinkBytes.count >= receiveChunkBytes {
                    let chunk = Data(downlinkBytes.prefix(receiveChunkBytes))
                    downlinkBytes.removeFirst(receiveChunkBytes)
                    if usesRemoteMedia, let remoteBridge, let remoteGeneration {
                        for frame in remoteFramer.append(chunk) {
                            if remoteBridge.offerModemDownlink(
                                frame,
                                mediaLeaseGeneration: remoteGeneration
                            ) {
                                recordRemoteCaptureAccepted(
                                    frames: frame.count / MemoryLayout<Int16>.size,
                                    mediaLeaseGeneration: remoteGeneration,
                                    session: session
                                )
                            }
                        }
                    } else {
                        schedulePlayback(chunk, session: session)
                    }
                }
                if downlinkBytes.count > receiveChunkBytes * 8 {
                    downlinkBytes = Data(downlinkBytes.suffix(receiveChunkBytes))
                }
            } else {
                readTimeouts += 1
                if readTimeouts == 40 {
                    maVoAudioLog("raw-no-downlink-after-timeout-window")
                }
            }

        }
        stateLock.withLock {
            if sessionGeneration == session { running = false }
        }
        maVoAudioLog("raw-loop-ended")
    }

    private func schedulePlayback(_ pcm: Data, session: UInt64) {
        let shouldPlay = stateLock.withLock {
            sessionGeneration == session && running && mediaEnabled
        }
        guard shouldPlay else { return }
        playbackQueue.async { [weak self] in
            guard let self,
                  self.isActiveSession(session),
                  let player = self.player else { return }
            let format = AVAudioFormat(
                standardFormatWithSampleRate: self.modemSampleRate,
                channels: 1
            )!
            let frames = pcm.count / 2
            guard frames > 0,
                  self.scheduledPlaybackFrames + frames <= self.maximumScheduledPlaybackFrames else {
                return
            }
            guard let buffer = AVAudioPCMBuffer(
                pcmFormat: format,
                frameCapacity: AVAudioFrameCount(frames)
            ), let output = buffer.floatChannelData?[0] else {
                return
            }
            buffer.frameLength = AVAudioFrameCount(frames)
            pcm.withUnsafeBytes { rawBuffer in
                let bytes = rawBuffer.bindMemory(to: UInt8.self)
                for frame in 0 ..< frames {
                    let raw = UInt16(bytes[frame * 2]) | UInt16(bytes[frame * 2 + 1]) << 8
                    output[frame] = Float(Int16(bitPattern: raw)) / 32_768.0
                }
            }
            self.scheduledPlaybackFrames += frames
            player.scheduleBuffer(buffer, completionCallbackType: .dataPlayedBack) { [weak self] _ in
                self?.playbackQueue.async { [weak self] in
                    guard let self, self.isCurrentSession(session) else { return }
                    self.scheduledPlaybackFrames = max(0, self.scheduledPlaybackFrames - frames)
                }
            }
        }
    }

    private func appendUploadBytes(_ bytes: Data) {
        guard !bytes.isEmpty else { return }
        let limit = stateLock.withLock {
            pcmFlowReady ? maximumUploadBytes : transmitChunkBytes
        }
        uploadLock.withLock {
            uploadBytes.append(bytes)
            if uploadBytes.count > limit {
                uploadBytes.removeFirst(uploadBytes.count - limit)
            }
        }
    }

    private func takeUploadChunkOrSilence() -> Data {
        uploadLock.withLock {
            if uploadBytes.count >= transmitChunkBytes {
                let chunk = Data(uploadBytes.prefix(transmitChunkBytes))
                uploadBytes.removeFirst(transmitChunkBytes)
                return chunk
            }
            var chunk = uploadBytes
            uploadBytes.removeAll(keepingCapacity: true)
            chunk.append(Data(repeating: 0, count: transmitChunkBytes - chunk.count))
            return chunk
        }
    }

    private func takeUploadSamples(into samples: inout [Int16]) -> Int {
        guard !samples.isEmpty else { return 0 }
        return uploadLock.withLock {
            let frameCount = min(
                samples.count,
                uploadBytes.count / MemoryLayout<Int16>.size
            )
            guard frameCount > 0 else { return 0 }
            uploadBytes.withUnsafeBytes { rawBuffer in
                let bytes = rawBuffer.bindMemory(to: UInt8.self)
                for frame in 0 ..< frameCount {
                    let bits = UInt16(bytes[frame * 2]) |
                        UInt16(bytes[frame * 2 + 1]) << 8
                    samples[frame] = Int16(bitPattern: bits)
                }
            }
            uploadBytes.removeFirst(frameCount * MemoryLayout<Int16>.size)
            return frameCount
        }
    }

    private func clearUploadBytes() {
        uploadLock.withLock { uploadBytes.removeAll(keepingCapacity: true) }
    }

    private func trimUploadBytesToLatestChunk() {
        uploadLock.withLock {
            if uploadBytes.count > transmitChunkBytes {
                uploadBytes = Data(uploadBytes.suffix(transmitChunkBytes))
            }
        }
    }

    private func resetBuffers() {
        clearUploadBytes()
        playbackQueue.async { [weak self] in
            self?.scheduledPlaybackFrames = 0
        }
        captureQueue.async { [weak self] in
            self?.captureSamples.removeAll(keepingCapacity: true)
            self?.capturePosition = 0
            self?.captureSampleRate = 0
            self?.captureConditioner.reset()
        }
    }

    private func resetBuffersSynchronously() {
        clearUploadBytes()
        playbackQueue.sync {
            scheduledPlaybackFrames = 0
        }
        captureQueue.sync {
            captureSamples.removeAll(keepingCapacity: true)
            capturePosition = 0
            captureSampleRate = 0
            captureConditioner.reset()
        }
    }

    private func reportTransportError(_ voice: OpaquePointer, session: UInt64) {
        guard isCurrentSession(session) else { return }
        let raw = String(cString: mavo_voice_last_error(voice))
        let message = raw.isEmpty ? "USB 语音接口已断开。" : raw
        maVoAudioLog("raw-transport-error-\(message)")
        DispatchQueue.main.async { [weak self] in
            guard let self, self.isCurrentSession(session) else { return }
            self.onError?(message)
        }
    }

    private func lastUACError(_ uac: OpaquePointer, fallback: String) -> String {
        guard let raw = mavo_uac_probe_last_error(uac) else { return fallback }
        let message = String(cString: raw)
        return message.isEmpty ? fallback : message
    }

    /// Must run on ioQueue. Returns nil only when the pointer was freed.
    private func disposeUACOrDefer(_ uac: OpaquePointer) -> String? {
        let result = mavo_uac_probe_try_destroy(uac)
        guard result != MAVO_UAC_OK else { return nil }
        let error = lastUACError(
            uac,
            fallback: "模块 UAC IOProc 或采样率恢复尚未完成。"
        )
        if !deferredUACCleanup.contains(where: { $0.pointer == uac }) {
            deferredUACCleanup.append(
                DeferredUACCleanup(
                    pointer: uac,
                    lastCallbackSequence: mavo_uac_probe_callback_sequence(uac),
                    removedQuietPasses: 0
                )
            )
        }
        stateLock.withLock { uacCleanupPending = true }
        scheduleDeferredUACCleanup()
        return error
    }

    private func scheduleDeferredUACCleanup() {
        guard !uacCleanupRetryScheduled, !deferredUACCleanup.isEmpty else { return }
        uacCleanupRetryScheduled = true
        ioQueue.asyncAfter(deadline: .now() + 1) { [weak self] in
            guard let self else { return }
            self.uacCleanupRetryScheduled = false
            self.reapDeferredUACCleanupNow()
            self.scheduleDeferredUACCleanup()
        }
    }

    private func reapDeferredUACCleanupNow() {
        var remaining: [DeferredUACCleanup] = []
        for var entry in deferredUACCleanup {
            let uac = entry.pointer
            let originalUSBPresence = mavo_uac_probe_original_usb_present(uac)
            if originalUSBPresence > 0 {
                entry.removedQuietPasses = 0
                if mavo_uac_probe_try_destroy(uac) == MAVO_UAC_OK {
                    continue
                }
                entry.lastCallbackSequence = mavo_uac_probe_callback_sequence(uac)
                remaining.append(entry)
                continue
            }
            if originalUSBPresence < 0 {
                entry.removedQuietPasses = 0
                remaining.append(entry)
                continue
            }

            let sequence = mavo_uac_probe_callback_sequence(uac)
            let callbacksInFlight = mavo_uac_probe_callbacks_in_flight(uac)
            if callbacksInFlight == 0, sequence == entry.lastCallbackSequence {
                entry.removedQuietPasses += 1
            } else {
                entry.removedQuietPasses = 0
            }
            entry.lastCallbackSequence = sequence
            if entry.removedQuietPasses >= 3 {
                retiredUACTombstones.append(uac)
            } else {
                remaining.append(entry)
            }
        }
        deferredUACCleanup = remaining
        stateLock.withLock { uacCleanupPending = !remaining.isEmpty }
    }

    private func reportUACError(_ message: String, session: UInt64) {
        guard isCurrentSession(session) else { return }
        DispatchQueue.main.async { [weak self] in
            guard let self, self.isCurrentSession(session) else { return }
            self.onError?(message)
        }
    }

    private func recordRemoteCaptureAccepted(
        frames: Int,
        mediaLeaseGeneration generation: UInt64,
        session: UInt64
    ) {
        guard frames > 0 else { return }
        stateLock.withLock {
            guard sessionGeneration == session,
                  running, mediaEnabled,
                  activeRemoteMediaGeneration == generation,
                  let activeEpoch = activeRemoteActivationEpoch,
                  var counter = remoteActivityCounter,
                  counter.sessionGeneration == session,
                  counter.mediaLeaseGeneration == generation,
                  counter.activationEpoch == activeEpoch else {
                return
            }
            let increment = UInt64(frames)
            let (total, overflow) = counter.captureAcceptedFrames
                .addingReportingOverflow(increment)
            counter.captureAcceptedFrames = overflow ? UInt64.max : total
            remoteActivityCounter = counter
        }
    }

    private func isPreparedRemoteMedia(
        bridge: RemotePCMBridge,
        generation: UInt64
    ) -> Bool {
        let ownsLease = stateLock.withLock {
            remoteMediaReady &&
                remoteMediaGeneration == generation &&
                remoteMediaBridge === bridge
        }
        return ownsLease && bridge.isReady(mediaLeaseGeneration: generation)
    }

    private func handleRemoteMediaDisconnect(
        bridge: RemotePCMBridge,
        generation: UInt64,
        epoch: UInt64
    ) {
        let revoked = stateLock.withLock {
            guard remoteMediaEpoch == epoch,
                  remoteMediaGeneration == generation,
                  remoteMediaBridge === bridge else { return false }
            remoteMediaEpoch &+= 1
            remoteMediaBridge = nil
            remoteMediaGeneration = nil
            remoteMediaReady = false
            if activeRemoteMediaGeneration == generation {
                activeRemoteActivationEpoch = nil
                remoteActivityCounter = nil
            }
            return true
        }
        guard revoked else { return }
        // Do not stop UAC or issue any modem/call command here. The C UAC
        // output ring zero-fills, and the loop also supplies 20 ms silence.
        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            let replacedBySameGeneration = self.stateLock.withLock {
                self.remoteMediaGeneration == generation
            }
            if !replacedBySameGeneration {
                self.onRemoteMediaRevoked?(generation)
            }
        }
    }

    private func isCurrentSession(_ session: UInt64) -> Bool {
        stateLock.withLock { sessionGeneration == session }
    }

    private func releaseStartReservation(session: UInt64) {
        stateLock.withLock {
            if sessionGeneration == session {
                startInProgress = false
            }
        }
    }

    private func isActiveSession(_ session: UInt64) -> Bool {
        stateLock.withLock { sessionGeneration == session && running }
    }
}

private enum VoiceAudioError: LocalizedError {
    case microphoneUnavailable

    var errorDescription: String? {
        "没有可用的麦克风输入设备"
    }
}

enum VoiceRemoteMediaError: LocalizedError {
    case mediaAlreadyActive
    case staleGeneration

    var errorDescription: String? {
        switch self {
        case .mediaAlreadyActive:
            return "已有本机或远程媒体会话，不能替换 lease。"
        case .staleGeneration:
            return "远程媒体 lease 在连接过程中已被替换或撤销。"
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
