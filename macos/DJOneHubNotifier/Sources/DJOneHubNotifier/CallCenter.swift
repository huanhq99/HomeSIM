import Combine
import CryptoKit
import Foundation

private func remoteMediaLog(_ stage: String) {
    guard let data = "remote-media-stage=\(stage)\n".data(using: .utf8) else { return }
    try? FileHandle.standardError.write(contentsOf: data)
}

private struct RemoteMediaCallIdentity: Equatable {
    let callID: String
    let callIndex: Int
    let callDirection: String

    init?(_ call: CallRecord?) {
        guard let call, !call.id.isEmpty, call.index >= 0,
              call.direction == "incoming" || call.direction == "outgoing" else {
            return nil
        }
        callID = call.id
        callIndex = call.index
        callDirection = call.direction
    }

    func matches(_ call: CallRecord?) -> Bool {
        guard let call else { return false }
        return call.id == callID && call.index == callIndex && call.direction == callDirection
    }
}

private enum RemoteMediaCallBinding: Equatable {
    case incoming(identity: RemoteMediaCallIdentity, ringingGeneration: UInt64)
    case outgoing(expectedIdleGeneration: UInt64)

    init?(call: CallRecord?, callGeneration: UInt64?) {
        guard let call,
              let callGeneration,
              callGeneration != 0,
              let identity = RemoteMediaCallIdentity(call),
              call.direction == "incoming",
              call.state == "incoming" || call.state == "waiting" else {
            return nil
        }
        self = .incoming(identity: identity, ringingGeneration: callGeneration)
    }

    init?(claim: RemoteMediaControlClaim, call: CallRecord?, callGeneration: UInt64?) {
        switch claim.purpose {
        case "incoming":
            self.init(call: call, callGeneration: callGeneration)
        case "outgoing":
            guard call == nil,
                  let callGeneration,
                  callGeneration == claim.expectedCallGeneration,
                  callGeneration != 0 else { return nil }
            self = .outgoing(expectedIdleGeneration: callGeneration)
        default:
            return nil
        }
    }

    func matchesRinging(_ call: CallRecord?, callGeneration: UInt64?) -> Bool {
        guard case let .incoming(identity, ringingGeneration) = self,
              identity.matches(call),
              let call,
              call.state == "incoming" || call.state == "waiting" else {
            return false
        }
        return callGeneration == ringingGeneration
    }

    func matchesPreparation(_ call: CallRecord?, callGeneration: UInt64?) -> Bool {
        switch self {
        case .incoming:
            return matchesRinging(call, callGeneration: callGeneration)
        case let .outgoing(expectedIdleGeneration):
            return call == nil && callGeneration == expectedIdleGeneration
        }
    }

    func canAdoptAfterPreparedAcknowledgement(
        _ call: CallRecord?,
        callGeneration: UInt64?
    ) -> Bool {
        switch self {
        case let .incoming(identity, _):
            return identity.matches(call) && call?.state == "active" &&
                callGeneration.map { $0 != 0 } == true
        case let .outgoing(expectedIdleGeneration):
            guard let call, let callGeneration,
                  call.direction == "outgoing",
                  ["dialing", "alerting", "active"].contains(call.state) else { return false }
            return callGeneration > expectedIdleGeneration
        }
    }

    var initialIdentity: RemoteMediaCallIdentity? {
        if case let .incoming(identity, _) = self { return identity }
        return nil
    }

    var initialPhase: RemoteMediaCallPhase {
        switch self {
        case .incoming: return .ringing
        case .outgoing: return .outgoingIdle
        }
    }
}

private enum RemoteMediaCallPhase {
    case ringing
    case outgoingIdle
    case outgoingDialing
    case active
}

/// Pure, generation-bound routing state. The lease generation deliberately
/// does not equal CLCC generation: answering a ringing call changes the latter.
private struct PreparedRemoteMediaRoute {
    let mediaSessionID: String
    let mediaLeaseGeneration: UInt64
    let preferredUID: String
    let vendorID: UInt16
    let productID: UInt16
    let locationID: UInt32
    let expiresAt: Date
    let callBinding: RemoteMediaCallBinding
    var phase: RemoteMediaCallPhase
    var callIdentity: RemoteMediaCallIdentity?
    var activeCallGeneration: UInt64? = nil
    var activationEpoch: UInt64? = nil

    init(
        mediaSessionID: String,
        mediaLeaseGeneration: UInt64,
        preferredUID: String,
        vendorID: UInt16,
        productID: UInt16,
        locationID: UInt32,
        expiresAt: Date,
        callBinding: RemoteMediaCallBinding
    ) {
        self.mediaSessionID = mediaSessionID
        self.mediaLeaseGeneration = mediaLeaseGeneration
        self.preferredUID = preferredUID
        self.vendorID = vendorID
        self.productID = productID
        self.locationID = locationID
        self.expiresAt = expiresAt
        self.callBinding = callBinding
        phase = callBinding.initialPhase
        callIdentity = callBinding.initialIdentity
    }

    mutating func reconcile(
        call: CallRecord?,
        callGeneration: UInt64?,
        mediaEligible: Bool?,
        preparedGeneration: UInt64?,
        now: Date
    ) -> Bool {
        guard preparedGeneration == mediaLeaseGeneration else { return false }
        switch phase {
        case .ringing:
            guard callBinding.matchesRinging(call, callGeneration: callGeneration) else {
                guard callBinding.canAdoptAfterPreparedAcknowledgement(
                    call, callGeneration: callGeneration
                ), let callGeneration else { return false }
                phase = .active
                activeCallGeneration = callGeneration
                return true
            }
            guard phase == .ringing,
                  activationEpoch == nil,
                  now < expiresAt else { return false }
            return true
        case .outgoingIdle:
            if call == nil {
                guard case let .outgoing(expectedIdleGeneration) = callBinding else { return false }
                return callGeneration == expectedIdleGeneration && activationEpoch == nil && now < expiresAt
            }
            guard callBinding.canAdoptAfterPreparedAcknowledgement(
                call, callGeneration: callGeneration
            ), let call, let callGeneration,
               let identity = RemoteMediaCallIdentity(call) else { return false }
            callIdentity = identity
            activeCallGeneration = callGeneration
            phase = call.state == "active" ? .active : .outgoingDialing
            return true
        case .outgoingDialing:
            guard callIdentity?.matches(call) == true,
                  let call, let callGeneration, let previousGeneration = activeCallGeneration,
                  callGeneration >= previousGeneration else { return false }
            if call.state == "active" {
                guard mediaEligible != false else { return false }
                activeCallGeneration = callGeneration
                phase = .active
                return true
            }
            guard call.state == "dialing" || call.state == "alerting" else { return false }
            activeCallGeneration = callGeneration
            return true
        case .active:
            guard callIdentity?.matches(call) == true,
                  call?.state == "active" else { return false }
            return mediaEligible != false && activeCallGeneration == callGeneration
        }
    }

    func matchesActiveIdentity(
        call: CallRecord,
        config: MaVoAudioHostConfig,
        preparedGeneration: UInt64?
    ) -> Bool {
        phase == .active &&
            call.state == "active" &&
            callIdentity?.matches(call) == true &&
            preparedGeneration == mediaLeaseGeneration &&
            config.callGeneration == activeCallGeneration &&
            !preferredUID.isEmpty &&
            config.vendorID == vendorID &&
            config.productID == productID &&
            config.locationID == locationID
    }

    func matchesActive(
        call: CallRecord,
        config: MaVoAudioHostConfig,
        preparedGeneration: UInt64?
    ) -> Bool {
        matchesActiveIdentity(
            call: call,
            config: config,
            preparedGeneration: preparedGeneration
        )
    }

    func activeMismatchStage(
        call: CallRecord,
        config: MaVoAudioHostConfig,
        preparedGeneration: UInt64?
    ) -> String {
        if phase != .active { return "phase" }
        if call.state != "active" { return "call-state" }
        if callIdentity?.matches(call) != true { return "call-identity" }
        if preparedGeneration != mediaLeaseGeneration { return "prepared-generation" }
        if config.callGeneration != activeCallGeneration { return "call-generation" }
        if preferredUID.isEmpty { return "uac-uid" }
        if config.vendorID != vendorID || config.productID != productID { return "usb-product" }
        if config.locationID != locationID { return "usb-location" }
        return "unknown"
    }

    func reconcileFailureStage(
        call: CallRecord?,
        callGeneration: UInt64?,
        mediaEligible: Bool?,
        preparedGeneration: UInt64?
    ) -> String {
        if preparedGeneration != mediaLeaseGeneration { return "prepared-generation" }
        switch phase {
        case .ringing: return "ringing-transition"
        case .outgoingIdle:
            if call == nil { return "idle-generation" }
            if call?.direction != "outgoing" { return "outgoing-direction" }
            if !["dialing", "alerting", "active"].contains(call?.state ?? "") {
                return "outgoing-state"
            }
            return "outgoing-generation"
        case .outgoingDialing:
            if callIdentity?.matches(call) != true { return "dialing-identity" }
            if let callGeneration, let activeCallGeneration,
               callGeneration < activeCallGeneration { return "dialing-generation" }
            if call?.state == "active" && mediaEligible == false { return "active-ineligible" }
            return "dialing-state"
        case .active:
            if callIdentity?.matches(call) != true { return "active-identity" }
            if call?.state != "active" { return "active-state" }
            if activeCallGeneration != callGeneration { return "active-generation" }
            return "active-ineligible"
        }
    }

    mutating func acceptActivation(epoch: UInt64) -> Bool {
        guard phase == .active, activationEpoch == nil, epoch != 0 else {
            return false
        }
        activationEpoch = epoch
        return true
    }
}

private struct RemoteMediaPreparation {
    let epoch: UInt64
    let claim: RemoteMediaControlClaim
    let callBinding: RemoteMediaCallBinding
}

/// 原生主窗口的数据中心：轮询后台 /api/calls/status，驱动拨号、通话与最近通话界面。
@MainActor
final class CallCenter: ObservableObject {
    @Published var activeCall: CallRecord?
    @Published var history: [CallRecord] = []
    @Published var isOnline = false
    @Published var lastError: String?
    @Published var lastActionError: String?
    @Published var isMuted = false
    @Published var isRecording = false
    @Published var numberInput = ""
    @Published var isDialing = false

    private let api: DJOneHubAPI
    private let remoteMediaOnly: Bool
    var apiClient: DJOneHubAPI { api }
    private var pollTask: Task<Void, Never>?
    private var lastActiveID: String?
    private var pollInFlight = false
    private let maVoAudio = VoiceAudioService()
    private var maVoAudioStarting = false
    private var maVoAudioCallID: String?
    private var maVoAudioGeneration: UInt64 = 0
    private var maVoBackendCallGeneration: UInt64?
    private var maVoHostRegistered = false
    private let remoteMediaControl = RemoteMediaControlClient()
    private var remoteMediaEpoch: UInt64 = 0
    private var remoteMediaClaimEpoch: UInt64?
    private var remoteMediaPreparation: RemoteMediaPreparation?
    private var remoteMediaPreparedAckEpoch: UInt64?
    private var preparedRemoteMediaRoute: PreparedRemoteMediaRoute?
    private var latestCallGeneration: UInt64?
    private var latestMediaEligible: Bool?
    private var latestCallPollHealthy = false

    /// 新来电（呼入且响铃/等待）时回调，用于弹窗/聚焦主窗口。
    var onIncoming: ((CallRecord) -> Void)?

    init(api: DJOneHubAPI, remoteMediaOnly: Bool = false) {
        self.api = api
        self.remoteMediaOnly = remoteMediaOnly
        maVoAudio.onRemoteMediaRevoked = { [weak self] generation in
            Task { @MainActor [weak self] in
                self?.remoteMediaWasRevoked(generation: generation)
            }
        }
        remoteMediaControl.setActivityFailureHandler {
            [weak self] mediaSessionID, generation, activationEpoch, _ in
            Task { @MainActor [weak self] in
                self?.remoteActivityFailed(
                    mediaSessionID: mediaSessionID,
                    generation: generation,
                    activationEpoch: activationEpoch
                )
            }
        }
    }

    func start() {
        guard pollTask == nil else { return }
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.pollOnce()
                try? await Task.sleep(nanoseconds: 1_000_000_000)
            }
        }
    }

    /// The background helper uses the module's USB UAC input, which macOS
    /// classifies under the microphone privacy service. Ask once when the
    /// helper starts instead of interrupting an already connected phone call.
    func startRemoteMediaHelper() {
        guard remoteMediaOnly else {
            start()
            return
        }
        maVoAudio.requestMicrophoneAccess { [weak self] granted in
            remoteMediaLog(granted ? "microphone-authorized" : "microphone-denied")
            self?.start()
        }
    }

    func stop() {
        pollTask?.cancel()
        pollTask = nil
        latestCallPollHealthy = false
        invalidateAllRemoteMedia()
        maVoAudioGeneration &+= 1
        maVoAudioStarting = false
        maVoAudioCallID = nil
        maVoBackendCallGeneration = nil
        maVoAudio.stop()
        maVoHostRegistered = false
        Task { _ = try? await api.setMaVoAudioHostEnabled(false) }
    }

    private func pollOnce() async {
        guard !pollInFlight else { return }
        pollInFlight = true
        defer { pollInFlight = false }
        do {
            let status = try await api.callStatus()
            isOnline = true
            if !maVoHostRegistered {
                do {
                    try await api.setMaVoAudioHostEnabled(true)
                    maVoHostRegistered = true
                } catch {
                    // The backend can take tens of seconds to enumerate USB
                    // after a restart. Keep the old route until registration
                    // succeeds, then retry on the next poll.
                    lastError = error.localizedDescription
                }
            }
            let pollError = status.lastPollError.trimmingCharacters(in: .whitespacesAndNewlines)
            lastError = pollError.isEmpty ? nil : pollError
            let previousID = activeCall?.id
            activeCall = status.active
            history = status.history ?? []
            latestCallGeneration = status.callGeneration
            latestMediaEligible = status.mediaEligible
            latestCallPollHealthy = pollError.isEmpty
            reconcileRemoteMedia(with: status, now: Date())

            if let previousID, status.active?.id != previousID {
                if isRecording { maVoAudio.stopRecording { _ in } }
                isRecording = false
                maVoAudio.stop()
                maVoAudioCallID = nil
                maVoBackendCallGeneration = nil
                maVoAudioStarting = false
                maVoAudioGeneration &+= 1
            }
            // The server publishes its topology generation with every poll.
            // Stop even a same-ID UAC session when a second/held call or any
            // other CLCC topology change invalidates its route token.
            let backendGenerationChanged = maVoBackendCallGeneration.map {
                status.callGeneration != nil && status.callGeneration != $0
            } ?? false
            let backendMediaInvalid = status.mediaEligible == false
            if (backendGenerationChanged || backendMediaInvalid),
               maVoAudio.isRunning || maVoAudioStarting || maVoAudioCallID != nil {
                if isRecording { maVoAudio.stopRecording { _ in } }
                isRecording = false
                maVoAudio.stop()
                maVoAudioCallID = nil
                maVoBackendCallGeneration = nil
                maVoAudioStarting = false
                maVoAudioGeneration &+= 1
            }
            // A cached active call is not authority to keep media alive when
            // the backend could not obtain a fresh CLCC snapshot. The backend
            // has already invalidated the route generation; stop the local
            // zero-media/UAC session too and wait for a successful poll.
            if !pollError.isEmpty {
                let hadMediaSession = maVoAudio.isRunning || maVoAudioStarting || maVoAudioCallID != nil
                if isRecording { maVoAudio.stopRecording { _ in } }
                isRecording = false
                maVoAudio.stop()
                maVoAudioCallID = nil
                maVoBackendCallGeneration = nil
                maVoAudioStarting = false
                if hadMediaSession { maVoAudioGeneration &+= 1 }
                return
            }
            if status.active == nil, previousID != nil, isRecording {
                _ = try? await api.setCallRecording(false)
                isRecording = false
            }

            if let active = status.active,
               active.direction == "incoming",
               active.state == "incoming" || active.state == "waiting",
               active.id != lastActiveID {
                lastActiveID = active.id
                onIncoming?(active)
            } else if status.active == nil {
                lastActiveID = nil
            }
            if status.active?.id != previousID, status.active == nil || previousID == nil {
                isMuted = false
            }
            claimRemoteMediaIfNeeded(for: status)
            enqueueRemoteActivityIfAvailable()
            if let active = status.active,
               active.state == "active",
               status.mediaEligible != false {
                startMaVoAudioIfNeeded(for: active)
            }
        } catch {
            isOnline = false
            lastError = error.localizedDescription
            latestCallPollHealthy = false
            invalidateAllRemoteMedia()
        }
    }

    func dial() {
        let number = numberInput.trimmingCharacters(in: .whitespaces)
        guard !number.isEmpty else { return }
        isDialing = true
        Task {
            do {
                try await api.dial(number: number)
                await MainActor.run {
                    self.isDialing = false
                    self.lastActionError = nil
                }
            } catch {
                await MainActor.run {
                    self.isDialing = false
                    self.lastActionError = "拨号失败：\(error.localizedDescription)"
                }
            }
        }
    }

    func answer() {
        Task {
            do {
                try await api.answerCall()
                lastActionError = nil
            } catch {
                lastActionError = "接听失败：\(error.localizedDescription)"
            }
        }
    }

    func reject() {
        Task {
            do {
                _ = try await api.rejectCall()
                lastActionError = nil
            } catch {
                lastActionError = "拒接失败：\(error.localizedDescription)"
            }
        }
    }

    func hangup() {
        isMuted = false
        if isRecording { maVoAudio.stopRecording { _ in } }
        isRecording = false
        maVoAudioGeneration &+= 1
        maVoAudioStarting = false
        maVoAudioCallID = nil
        maVoBackendCallGeneration = nil
        invalidateAllRemoteMedia()
        maVoAudio.stop()
        Task {
            do {
                try await api.hangupCall()
                lastActionError = nil
            } catch {
                lastActionError = "挂断失败：\(error.localizedDescription)"
            }
        }
    }

    func sendDTMF(_ digit: String) {
        Task { _ = try? await api.sendDTMF(digit: digit) }
    }

    func toggleMute() {
        let muted = !isMuted
        isMuted = muted
        maVoAudio.setMuted(muted)
    }

    func toggleRecording() {
        guard maVoAudio.isRunning else {
            lastActionError = "通话音频尚未连接，无法开始录音。"
            return
        }
        if isRecording {
            maVoAudio.stopRecording { [weak self] _ in self?.isRecording = false }
            return
        }
        do {
            _ = try maVoAudio.startRecording()
            isRecording = true
            lastActionError = nil
        } catch {
            lastActionError = "无法创建录音：\(error.localizedDescription)"
        }
    }

    private func claimRemoteMediaIfNeeded(for status: CallStatus) {
        guard latestCallPollHealthy,
              remoteMediaClaimEpoch == nil,
              remoteMediaPreparation == nil,
              preparedRemoteMediaRoute == nil else {
            return
        }
        remoteMediaEpoch &+= 1
        let epoch = remoteMediaEpoch
        remoteMediaClaimEpoch = epoch
        remoteMediaControl.claim { [weak self] result in
            guard let self, self.remoteMediaClaimEpoch == epoch else { return }
            self.remoteMediaClaimEpoch = nil
            guard case .success(let optionalClaim) = result,
                  let claim = optionalClaim,
                  let binding = RemoteMediaCallBinding(
                      claim: claim,
                      call: self.activeCall,
                      callGeneration: self.latestCallGeneration
                  ),
                  self.latestCallPollHealthy,
                  binding.matchesPreparation(
                      self.activeCall,
                      callGeneration: self.latestCallGeneration
                  ),
                  Date() < claim.expiresAt,
                  self.remoteMediaPreparation == nil,
                  self.preparedRemoteMediaRoute == nil else {
                return
            }
            let preparation = RemoteMediaPreparation(
                epoch: epoch,
                claim: claim,
                callBinding: binding
            )
            remoteMediaLog("claim-accepted")
            self.remoteMediaPreparation = preparation
            self.maVoAudio.prepareRemoteMedia(lease: claim.pcmLease) { [weak self] result in
                self?.remotePCMPreparationFinished(preparation, result: result)
            }
        }
    }

    private func remotePCMPreparationFinished(
        _ preparation: RemoteMediaPreparation,
        result: Result<UInt64, Error>
    ) {
        guard remotePreparationIsCurrent(preparation, requirePreparedBridge: false) else {
            failRemotePreparation(preparation)
            return
        }
        guard case .success(let generation) = result,
              generation == preparation.claim.mediaLeaseGeneration,
              remotePreparationIsCurrent(preparation, requirePreparedBridge: true) else {
            failRemotePreparation(preparation)
            return
        }
        maVoAudio.validateUAC(
            vendorID: preparation.claim.vendorID,
            productID: preparation.claim.productID,
            matchingLocationID: preparation.claim.locationID
        ) { [weak self] result in
            self?.remoteUACValidationFinished(preparation, result: result)
        }
    }

    private func remoteUACValidationFinished(
        _ preparation: RemoteMediaPreparation,
        result: ModemActionResult
    ) {
        guard remotePreparationIsCurrent(preparation, requirePreparedBridge: true) else {
            failRemotePreparation(preparation)
            return
        }
        guard case .success(let optionalUID) = result,
              let uid = optionalUID,
              !uid.isEmpty else {
            failRemotePreparation(preparation)
            return
        }
        let digest = Self.sha256Hex(uid)
        remoteMediaPreparedAckEpoch = preparation.epoch
        remoteMediaControl.prepared(
            mediaSessionID: preparation.claim.mediaSessionID,
            mediaLeaseGeneration: preparation.claim.mediaLeaseGeneration,
            uacUIDDigest: digest
        ) { [weak self] result in
            self?.remotePreparedAcknowledgementFinished(
                preparation,
                preferredUID: uid,
                result: result
            )
        }
    }

    private func remotePreparedAcknowledgementFinished(
        _ preparation: RemoteMediaPreparation,
        preferredUID: String,
        result: Result<Void, Error>
    ) {
        guard remotePreparationOwnsLease(preparation, requirePreparedBridge: true),
              latestCallPollHealthy else {
            failRemotePreparation(preparation)
            return
        }
        guard case .success = result else {
            failRemotePreparation(preparation)
            return
        }
        let claim = preparation.claim
        var route = PreparedRemoteMediaRoute(
            mediaSessionID: claim.mediaSessionID,
            mediaLeaseGeneration: claim.mediaLeaseGeneration,
            preferredUID: preferredUID,
            vendorID: claim.vendorID,
            productID: claim.productID,
            locationID: claim.locationID,
            expiresAt: claim.expiresAt,
            callBinding: preparation.callBinding
        )
        // The prepared response may race the legitimate ringing -> active
        // transition that it just authorized. Reconcile by stable call
        // identity; never compare media generation to the new CLCC generation.
        guard route.reconcile(
            call: activeCall,
            callGeneration: latestCallGeneration,
            mediaEligible: latestMediaEligible,
            preparedGeneration: maVoAudio.preparedRemoteMediaGeneration,
            now: Date()
        ) else {
            failRemotePreparation(preparation)
            return
        }
        remoteMediaPreparation = nil
        remoteMediaPreparedAckEpoch = nil
        preparedRemoteMediaRoute = route
        remoteMediaLog("route-prepared")
    }

    private func remotePreparationIsCurrent(
        _ preparation: RemoteMediaPreparation,
        requirePreparedBridge: Bool
    ) -> Bool {
        guard remotePreparationOwnsLease(
            preparation,
            requirePreparedBridge: requirePreparedBridge
        ),
              latestCallPollHealthy,
              Date() < preparation.claim.expiresAt,
              preparation.callBinding.matchesPreparation(
                  activeCall,
                  callGeneration: latestCallGeneration
              ) else {
            return false
        }
        return true
    }

    private func remotePreparationOwnsLease(
        _ preparation: RemoteMediaPreparation,
        requirePreparedBridge: Bool
    ) -> Bool {
        guard remoteMediaEpoch == preparation.epoch,
              let current = remoteMediaPreparation,
              current.epoch == preparation.epoch,
              current.claim.mediaLeaseGeneration == preparation.claim.mediaLeaseGeneration,
              current.claim.mediaSessionID == preparation.claim.mediaSessionID else {
            return false
        }
        return !requirePreparedBridge ||
            maVoAudio.preparedRemoteMediaGeneration == preparation.claim.mediaLeaseGeneration
    }

    private func failRemotePreparation(_ preparation: RemoteMediaPreparation) {
        guard remoteMediaEpoch == preparation.epoch,
              remoteMediaPreparation?.claim.mediaSessionID == preparation.claim.mediaSessionID,
              remoteMediaPreparation?.claim.mediaLeaseGeneration ==
                preparation.claim.mediaLeaseGeneration else {
            return
        }
        remoteMediaEpoch &+= 1
        remoteMediaPreparation = nil
        if remoteMediaPreparedAckEpoch == preparation.epoch {
            remoteMediaPreparedAckEpoch = nil
        }
        _ = maVoAudio.revokeRemoteMedia(
            mediaLeaseGeneration: preparation.claim.mediaLeaseGeneration
        )
    }

    private func reconcileRemoteMedia(with status: CallStatus, now: Date) {
        let pollHealthy = status.lastPollError
            .trimmingCharacters(in: .whitespacesAndNewlines)
            .isEmpty
        guard pollHealthy else {
            invalidateAllRemoteMedia()
            return
        }
        if let preparation = remoteMediaPreparation,
           !remotePreparationIsCurrent(preparation, requirePreparedBridge: false) {
            let mayBeAuthorizedActiveTransition =
                remoteMediaPreparedAckEpoch == preparation.epoch &&
                remotePreparationOwnsLease(preparation, requirePreparedBridge: true) &&
                preparation.callBinding.canAdoptAfterPreparedAcknowledgement(
                    status.active,
                    callGeneration: status.callGeneration
                )
            if !mayBeAuthorizedActiveTransition {
                failRemotePreparation(preparation)
            }
        }
        guard var route = preparedRemoteMediaRoute else { return }
        guard route.reconcile(
            call: status.active,
            callGeneration: status.callGeneration,
            mediaEligible: status.mediaEligible,
            preparedGeneration: maVoAudio.preparedRemoteMediaGeneration,
            now: now
        ) else {
            remoteMediaLog("route-reconcile-" + route.reconcileFailureStage(
                call: status.active,
                callGeneration: status.callGeneration,
                mediaEligible: status.mediaEligible,
                preparedGeneration: maVoAudio.preparedRemoteMediaGeneration
            ))
            invalidateRemoteMedia(generation: route.mediaLeaseGeneration)
            return
        }
        preparedRemoteMediaRoute = route
    }

    private func invalidateRemoteMedia(generation: UInt64) {
        guard generation != 0 else { return }
        var matched = false
        if remoteMediaPreparation?.claim.mediaLeaseGeneration == generation {
            if remoteMediaPreparedAckEpoch == remoteMediaPreparation?.epoch {
                remoteMediaPreparedAckEpoch = nil
            }
            remoteMediaPreparation = nil
            matched = true
        }
        if let route = preparedRemoteMediaRoute,
           route.mediaLeaseGeneration == generation {
            cancelActivity(for: route)
            preparedRemoteMediaRoute = nil
            matched = true
        }
        guard matched else { return }
        remoteMediaEpoch &+= 1
        _ = maVoAudio.revokeRemoteMedia(mediaLeaseGeneration: generation)
    }

    private func invalidateAllRemoteMedia() {
        guard remoteMediaClaimEpoch != nil ||
                remoteMediaPreparation != nil ||
                preparedRemoteMediaRoute != nil else {
            return
        }
        var generations = Set<UInt64>()
        if let generation = remoteMediaPreparation?.claim.mediaLeaseGeneration {
            generations.insert(generation)
        }
        if let route = preparedRemoteMediaRoute {
            generations.insert(route.mediaLeaseGeneration)
            cancelActivity(for: route)
        }
        remoteMediaEpoch &+= 1
        remoteMediaClaimEpoch = nil
        remoteMediaPreparation = nil
        remoteMediaPreparedAckEpoch = nil
        preparedRemoteMediaRoute = nil
        for generation in generations {
            _ = maVoAudio.revokeRemoteMedia(mediaLeaseGeneration: generation)
        }
    }

    private func remoteMediaWasRevoked(generation: UInt64) {
        guard generation != 0,
              remoteMediaPreparation?.claim.mediaLeaseGeneration == generation ||
                preparedRemoteMediaRoute?.mediaLeaseGeneration == generation else {
            return
        }
        remoteMediaEpoch &+= 1
        if remoteMediaPreparation?.claim.mediaLeaseGeneration == generation {
            if remoteMediaPreparedAckEpoch == remoteMediaPreparation?.epoch {
                remoteMediaPreparedAckEpoch = nil
            }
            remoteMediaPreparation = nil
        }
        if let route = preparedRemoteMediaRoute,
           route.mediaLeaseGeneration == generation {
            cancelActivity(for: route)
            preparedRemoteMediaRoute = nil
        }
        // Transport revocation never implies hangup. A running remote UAC
        // remains fail-closed and emits silence until the normal call owner
        // tears it down.
    }

    private func cancelActivity(for route: PreparedRemoteMediaRoute) {
        guard let activationEpoch = route.activationEpoch else { return }
        remoteMediaControl.cancelActivity(
            mediaSessionID: route.mediaSessionID,
            mediaLeaseGeneration: route.mediaLeaseGeneration,
            activationEpoch: activationEpoch
        )
    }

    private func remoteActivityFailed(
        mediaSessionID: String,
        generation: UInt64,
        activationEpoch: UInt64
    ) {
        guard let route = preparedRemoteMediaRoute,
              route.mediaSessionID == mediaSessionID,
              route.mediaLeaseGeneration == generation,
              route.activationEpoch == activationEpoch else {
            return
        }
        // A failed exact activity receipt revokes only remote media. It never
        // issues a modem hangup or changes cellular call control.
        invalidateRemoteMedia(generation: generation)
    }

    private func enqueueRemoteActivityIfAvailable() {
        guard latestCallPollHealthy,
              latestMediaEligible != false,
              let route = preparedRemoteMediaRoute,
              let activationEpoch = route.activationEpoch,
              let snapshot = maVoAudio.remoteMediaActivitySnapshot(
                  mediaLeaseGeneration: route.mediaLeaseGeneration,
                  activationEpoch: activationEpoch
              ) else {
            return
        }
        _ = remoteMediaControl.enqueueActivity(RemoteMediaControlActivity(
            mediaSessionID: route.mediaSessionID,
            mediaLeaseGeneration: snapshot.mediaLeaseGeneration,
            activationEpoch: snapshot.activationEpoch,
            captureFrames: snapshot.captureFrames,
            playbackFrames: snapshot.playbackFrames
        ))
        if let uac = maVoAudio.uacMediaSnapshot {
            remoteMediaLog(
                "activity-capture-\(snapshot.captureFrames)" +
                "-playback-\(snapshot.playbackFrames)" +
                "-input-total-\(uac.inputTotalSamples)" +
                "-input-signal-\(uac.inputSignalSamples)" +
                "-input-peak-\(uac.inputPeakPCM16)" +
                "-threshold-\(uac.inputSignalThresholdPCM16)" +
                "-downlink-dropped-\(uac.downlinkDroppedFrames)" +
                "-uplink-dropped-\(uac.uplinkDroppedFrames)"
            )
        }
    }

    private func preparedRemoteRoute(
        matching call: CallRecord,
        config: MaVoAudioHostConfig
    ) -> PreparedRemoteMediaRoute? {
        guard latestCallPollHealthy,
              latestMediaEligible != false,
              let route = preparedRemoteMediaRoute else {
            return nil
        }
        guard route.matchesActive(
            call: call,
            config: config,
            preparedGeneration: maVoAudio.preparedRemoteMediaGeneration
        ) else {
            remoteMediaLog("route-mismatch-" + route.activeMismatchStage(
                call: call,
                config: config,
                preparedGeneration: maVoAudio.preparedRemoteMediaGeneration
            ))
            invalidateRemoteMedia(generation: route.mediaLeaseGeneration)
            return nil
        }
        return route
    }

    private func remoteRouteRemainsUsable(
        generation: UInt64?,
        activationEpoch: UInt64?,
        call: CallRecord,
        config: MaVoAudioHostConfig
    ) -> Bool {
        guard (generation == nil) == (activationEpoch == nil) else {
            if let generation {
                invalidateRemoteMedia(generation: generation)
            }
            return false
        }
        guard let generation, let activationEpoch else { return true }
        guard let route = preparedRemoteMediaRoute,
              route.mediaLeaseGeneration == generation,
              route.activationEpoch == activationEpoch,
              latestCallPollHealthy,
              latestMediaEligible != false,
              route.matchesActive(
                  call: call,
                  config: config,
                  preparedGeneration: maVoAudio.preparedRemoteMediaGeneration
              ) else {
            invalidateRemoteMedia(generation: generation)
            return false
        }
        return true
    }

    private func startMaVoAudioIfNeeded(for call: CallRecord) {
        // If the prepared acknowledgement itself authorized the active
        // transition, wait for its exact result. Never race that remote lease
        // with a local AVAudioEngine fallback.
        guard remoteMediaPreparation == nil,
              !maVoAudio.isRunning,
              !maVoAudioStarting,
              maVoAudioCallID != call.id else {
            return
        }
        maVoAudioStarting = true
        maVoAudioCallID = call.id
        maVoAudioGeneration &+= 1
        remoteMediaLog("active-call-starting")
        let generation = maVoAudioGeneration
        Task { [weak self] in
            guard let self else { return }
            do {
                let config = try await self.waitForMaVoRoute(call: call, generation: generation)
                remoteMediaLog("voice-route-ready")
                guard let callGeneration = config.callGeneration,
                      let callID = config.callID,
                      let callIndex = config.callIndex,
                      let callDirection = config.callDirection,
                      callGeneration != 0,
                      callID == call.id,
                      callIndex == call.index,
                      callDirection == call.direction else {
                    throw APIError.http(409, "音频路由不再属于当前通话")
                }
                let ticket = MaVoCallMediaTicket(
                    callGeneration: callGeneration,
                    callID: callID,
                    callIndex: callIndex,
                    callDirection: callDirection
                )
                guard self.audioStartIsCurrent(call: call, generation: generation) else {
                    self.cancelAudioStart(generation: generation, stopAudio: false)
                    return
                }
                self.maVoBackendCallGeneration = callGeneration
                if let remote = self.preparedRemoteRoute(matching: call, config: config) {
                    self.activateAndStartRemoteAudio(
                        call: call,
                        config: config,
                        ticket: ticket,
                        generation: generation,
                        route: remote
                    )
                } else if self.remoteMediaOnly {
                    // The LaunchAgent helper exists only for a browser-owned
                    // media lease. Never turn a failed/expired remote route
                    // into AVAudioEngine capture from the Mac microphone: it
                    // creates a convincing but false remote-audio result.
                    remoteMediaLog("remote-route-required")
                    self.maVoAudioStarting = false
                    self.maVoBackendCallGeneration = nil
                    self.lastActionError = "远程音频会话已失效，请结束当前通话后重新拨号。"
                } else {
                    self.startLocalAudio(
                        call: call,
                        config: config,
                        ticket: ticket,
                        generation: generation
                    )
                }
            } catch {
                self.cancelAudioStart(
                    generation: generation,
                    stopAudio: false,
                    message: "MaVo 音频准备失败：\(error.localizedDescription)"
                )
            }
        }
    }

    private func waitForMaVoRoute(
        call: CallRecord,
        generation: UInt64
    ) async throws -> MaVoAudioHostConfig {
        let deadline = Date().addingTimeInterval(40)
        while Date() < deadline {
            guard audioStartIsCurrent(call: call, generation: generation) else {
                throw APIError.http(409, "模块音频准备已取消")
            }
            let config = try await api.maVoAudioHostConfig()
            if config.routeReady {
                return config
            }
            if let routeError = config.routeError, !routeError.isEmpty {
                throw APIError.http(409, "模块语音路由准备失败：\(routeError)")
            }
            try await Task.sleep(for: .milliseconds(500))
        }
        throw APIError.http(409, "等待模块 UAC 语音路由超过 40 秒")
    }

    private func activateAndStartRemoteAudio(
        call: CallRecord,
        config: MaVoAudioHostConfig,
        ticket: MaVoCallMediaTicket,
        generation: UInt64,
        route: PreparedRemoteMediaRoute
    ) {
        if let activationEpoch = route.activationEpoch {
            startValidatedUAC(
                call: call,
                config: config,
                ticket: ticket,
                generation: generation,
                preferredUID: route.preferredUID,
                remoteMediaGeneration: route.mediaLeaseGeneration,
                remoteActivationEpoch: activationEpoch
            )
            return
        }
        remoteMediaLog("activation-requested")
        remoteMediaControl.activation(
            mediaSessionID: route.mediaSessionID,
            mediaLeaseGeneration: route.mediaLeaseGeneration
        ) { [weak self] result in
            guard let self else { return }
            guard self.audioStartIsCurrent(call: call, generation: generation),
                  self.latestCallPollHealthy,
                  self.latestMediaEligible != false,
                  var current = self.preparedRemoteMediaRoute,
                  current.mediaSessionID == route.mediaSessionID,
                  current.mediaLeaseGeneration == route.mediaLeaseGeneration,
                  current.activationEpoch == nil,
                  current.matchesActiveIdentity(
                      call: call,
                      config: config,
                      preparedGeneration: self.maVoAudio.preparedRemoteMediaGeneration
                  ) else {
                remoteMediaLog("activation-context-stale")
                self.cancelAudioStart(
                    generation: generation,
                    stopAudio: false,
                    remoteMediaGeneration: route.mediaLeaseGeneration
                )
                return
            }
            guard case .success(let activationEpoch) = result,
                  current.acceptActivation(epoch: activationEpoch) else {
                if case .failure(let error) = result {
                    remoteMediaLog("activation-rejected-\(error.localizedDescription)")
                } else {
                    remoteMediaLog("activation-rejected-invalid-epoch")
                }
                self.cancelAudioStart(
                    generation: generation,
                    stopAudio: false,
                    remoteMediaGeneration: route.mediaLeaseGeneration
                )
                return
            }
            // Go's successful activation becomes the temporal authority for
            // this exact lease. Offer expiry no longer tears down its route.
            self.preparedRemoteMediaRoute = current
            remoteMediaLog("activation-accepted")
            self.startValidatedUAC(
                call: call,
                config: config,
                ticket: ticket,
                generation: generation,
                preferredUID: current.preferredUID,
                remoteMediaGeneration: current.mediaLeaseGeneration,
                remoteActivationEpoch: activationEpoch
            )
        }
    }

    private func startLocalAudio(
        call: CallRecord,
        config: MaVoAudioHostConfig,
        ticket: MaVoCallMediaTicket,
        generation: UInt64
    ) {
        remoteMediaLog("local-audio-fallback")
        // The remote branch never reaches this permission request and never
        // starts AVAudioEngine. A nil remote lease preserves the old behavior.
        maVoAudio.requestMicrophoneAccess { [weak self] granted in
            guard let self else { return }
            guard self.audioStartIsCurrent(call: call, generation: generation) else {
                self.cancelAudioStart(generation: generation, stopAudio: false)
                return
            }
            guard granted else {
                self.cancelAudioStart(
                    generation: generation,
                    stopAudio: false,
                    message: "需要麦克风权限才能进行双向通话。"
                )
                return
            }
            self.maVoAudio.validateUAC(
                vendorID: config.vendorID,
                productID: config.productID,
                matchingLocationID: config.locationID
            ) { [weak self] result in
                guard let self else { return }
                guard case .success(let optionalUID) = result,
                      let uid = optionalUID,
                      !uid.isEmpty else {
                    let detail: String
                    if case .failure(let message) = result {
                        detail = message
                    } else {
                        detail = "UAC 未返回可用的精确 UID。"
                    }
                    self.cancelAudioStart(
                        generation: generation,
                        stopAudio: false,
                        message: "MaVo UAC 预检失败：\(detail)"
                    )
                    return
                }
                guard self.audioStartIsCurrent(call: call, generation: generation) else {
                    self.cancelAudioStart(generation: generation, stopAudio: false)
                    return
                }
                self.startValidatedUAC(
                    call: call,
                    config: config,
                    ticket: ticket,
                    generation: generation,
                    preferredUID: uid,
                    remoteMediaGeneration: nil,
                    remoteActivationEpoch: nil
                )
            }
        }
    }

    private func startValidatedUAC(
        call: CallRecord,
        config: MaVoAudioHostConfig,
        ticket: MaVoCallMediaTicket,
        generation: UInt64,
        preferredUID: String,
        remoteMediaGeneration: UInt64?,
        remoteActivationEpoch: UInt64?
    ) {
        guard audioStartIsCurrent(call: call, generation: generation),
              remoteRouteRemainsUsable(
                  generation: remoteMediaGeneration,
                  activationEpoch: remoteActivationEpoch,
                  call: call,
                  config: config
              ) else {
            cancelAudioStart(
                generation: generation,
                stopAudio: false,
                remoteMediaGeneration: remoteMediaGeneration
            )
            return
        }
        let completion: (ModemActionResult) -> Void = { [weak self] result in
            guard let self else { return }
            guard self.audioStartIsCurrent(call: call, generation: generation),
                  self.remoteRouteRemainsUsable(
                      generation: remoteMediaGeneration,
                      activationEpoch: remoteActivationEpoch,
                      call: call,
                      config: config
                  ) else {
                self.cancelAudioStart(
                    generation: generation,
                    stopAudio: true,
                    remoteMediaGeneration: remoteMediaGeneration
                )
                return
            }
            switch result {
            case .success:
                remoteMediaLog("uac-open")
                // The USB route is open but still zero-media. A fresh backend CLCC read
                // for this exact call ticket is required for both local and
                // remote routes before any PCM flow is enabled.
                Task { [weak self] in
                    guard let self else { return }
                    do {
                        try await self.api.confirmMaVoAudioHost(ticket: ticket)
                        guard self.audioStartIsCurrent(call: call, generation: generation),
                              self.maVoAudio.isRunning,
                              self.remoteRouteRemainsUsable(
                                  generation: remoteMediaGeneration,
                                  activationEpoch: remoteActivationEpoch,
                                  call: call,
                                  config: config
                              ) else {
                            self.cancelAudioStart(
                                generation: generation,
                                stopAudio: true,
                                remoteMediaGeneration: remoteMediaGeneration
                            )
                            return
                        }
                        let mediaEnabled: Bool
                        if let remoteMediaGeneration,
                           let remoteActivationEpoch,
                           let route = self.preparedRemoteMediaRoute,
                           route.mediaLeaseGeneration == remoteMediaGeneration,
                           route.activationEpoch == remoteActivationEpoch {
                            mediaEnabled = self.maVoAudio.setMediaEnabled(
                                true,
                                remoteMediaGeneration: remoteMediaGeneration,
                                remoteActivationEpoch: remoteActivationEpoch
                            ) && self.remoteMediaControl.bindActivity(
                                mediaSessionID: route.mediaSessionID,
                                mediaLeaseGeneration: remoteMediaGeneration,
                                activationEpoch: remoteActivationEpoch
                            )
                        } else if remoteMediaGeneration == nil,
                                  remoteActivationEpoch == nil {
                            mediaEnabled = self.maVoAudio.setMediaEnabled(true)
                        } else {
                            mediaEnabled = false
                        }
                        guard mediaEnabled else {
                            remoteMediaLog("media-enable-rejected")
                            self.cancelAudioStart(
                                generation: generation,
                                stopAudio: true,
                                remoteMediaGeneration: remoteMediaGeneration
                            )
                            return
                        }
                        self.maVoAudioStarting = false
                        self.lastActionError = nil
                        remoteMediaLog("media-live")
                        self.enqueueRemoteActivityIfAvailable()
                    } catch {
                        remoteMediaLog("fresh-gate-rejected")
                        self.cancelAudioStart(
                            generation: generation,
                            stopAudio: true,
                            remoteMediaGeneration: remoteMediaGeneration,
                            message: "MaVo 音频 fresh gate 失败：\(error.localizedDescription)"
                        )
                    }
                }
            case .failure(let message):
                remoteMediaLog("uac-open-rejected")
                self.cancelAudioStart(
                    generation: generation,
                    stopAudio: false,
                    remoteMediaGeneration: remoteMediaGeneration,
                    message: "MaVo 音频启动失败：\(message)"
                )
            }
        }
        maVoAudio.startUAC(
            vendorID: config.vendorID,
            productID: config.productID,
            matchingLocationID: config.locationID,
            preferredUID: preferredUID,
            remoteMediaGeneration: remoteMediaGeneration,
            remoteActivationEpoch: remoteActivationEpoch,
            completion: completion
        )
    }

    private func audioStartIsCurrent(call: CallRecord, generation: UInt64) -> Bool {
        maVoAudioGeneration == generation &&
            maVoAudioStarting &&
            activeCall?.id == call.id &&
            activeCall?.index == call.index &&
            activeCall?.direction == call.direction &&
            activeCall?.state == "active"
    }

    private func cancelAudioStart(
        generation: UInt64,
        stopAudio: Bool,
        remoteMediaGeneration: UInt64? = nil,
        message: String? = nil
    ) {
        guard maVoAudioGeneration == generation else { return }
        if let remoteMediaGeneration {
            invalidateRemoteMedia(generation: remoteMediaGeneration)
        }
        maVoAudioStarting = false
        maVoAudioCallID = nil
        maVoBackendCallGeneration = nil
        if stopAudio { maVoAudio.stop() }
        if let message { lastActionError = message }
    }

    nonisolated private static func sha256Hex(_ value: String) -> String {
        SHA256.hash(data: Data(value.utf8)).map {
            String(format: "%02x", $0)
        }.joined()
    }

    nonisolated static func runRemoteMediaSyntheticSelfTest() {
        let base = Date(timeIntervalSince1970: 1_700_000_000)
        let ringing = CallRecord(
            id: "synthetic-call",
            index: 1,
            direction: "incoming",
            state: "incoming",
            number: nil,
            startedAt: base,
            updatedAt: base,
            endedAt: nil,
            missed: false
        )
        let active = CallRecord(
            id: ringing.id,
            index: ringing.index,
            direction: ringing.direction,
            state: "active",
            number: nil,
            startedAt: base,
            updatedAt: base,
            endedAt: nil,
            missed: false
        )
        let binding = RemoteMediaCallBinding(call: ringing, callGeneration: 41)!
        let config = MaVoAudioHostConfig(
            vendorID: 11_388,
            productID: 293,
            locationID: 0x1234_0000,
            routeReady: true,
            routeError: nil,
            callGeneration: 42,
            callID: active.id,
            callIndex: active.index,
            callDirection: active.direction
        )
        var route = PreparedRemoteMediaRoute(
            mediaSessionID: "synthetic-media-session-0001",
            mediaLeaseGeneration: 73,
            preferredUID: "synthetic-uac-uid",
            vendorID: config.vendorID,
            productID: config.productID,
            locationID: config.locationID,
            expiresAt: base.addingTimeInterval(45),
            callBinding: binding
        )
        var expiredRingingRoute = route
        precondition(!expiredRingingRoute.reconcile(
            call: ringing,
            callGeneration: 41,
            mediaEligible: false,
            preparedGeneration: 73,
            now: base.addingTimeInterval(46)
        ))
        precondition(route.reconcile(
            call: ringing,
            callGeneration: 41,
            mediaEligible: false,
            preparedGeneration: 73,
            now: base
        ))
        precondition(!route.reconcile(
            call: ringing,
            callGeneration: 99,
            mediaEligible: false,
            preparedGeneration: 73,
            now: base
        ))
        precondition(route.reconcile(
            call: active,
            callGeneration: 42,
            mediaEligible: false,
            preparedGeneration: 73,
            now: base
        ))
        precondition(route.matchesActive(
            call: active,
            config: config,
            preparedGeneration: 73
        ))
        precondition(!route.matchesActive(
            call: active,
            config: config,
            preparedGeneration: 74
        ))
        let wrongUSBConfig = MaVoAudioHostConfig(
            vendorID: config.vendorID,
            productID: config.productID,
            locationID: config.locationID + 1,
            routeReady: true,
            routeError: nil,
            callGeneration: config.callGeneration,
            callID: config.callID,
            callIndex: config.callIndex,
            callDirection: config.callDirection
        )
        precondition(!route.matchesActive(
            call: active,
            config: wrongUSBConfig,
            preparedGeneration: 73
        ))
        let replacementCall = CallRecord(
            id: "replacement-call",
            index: active.index,
            direction: active.direction,
            state: active.state,
            number: nil,
            startedAt: base,
            updatedAt: base,
            endedAt: nil,
            missed: false
        )
        precondition(!route.matchesActive(
            call: replacementCall,
            config: config,
            preparedGeneration: 73
        ))
        var changedActiveGeneration = route
        precondition(!changedActiveGeneration.reconcile(
            call: active,
            callGeneration: 43,
            mediaEligible: true,
            preparedGeneration: 73,
            now: base
        ))
        precondition(!route.reconcile(
            call: active,
            callGeneration: 42,
            mediaEligible: false,
            preparedGeneration: 73,
            now: base
        ))
        // Offer expiry applies only while ringing. Once the exact call becomes
        // active, Go's activation response is the authority for the route.
        precondition(route.reconcile(
            call: active,
            callGeneration: 42,
            mediaEligible: true,
            preparedGeneration: 73,
            now: base.addingTimeInterval(46)
        ))
        precondition(route.acceptActivation(epoch: 91))
        precondition(!route.acceptActivation(epoch: 92))
        precondition(route.activationEpoch == 91)
        precondition(route.matchesActive(
            call: active,
            config: config,
            preparedGeneration: 73
        ))
        precondition(route.reconcile(
            call: active,
            callGeneration: 42,
            mediaEligible: true,
            preparedGeneration: 73,
            now: base.addingTimeInterval(46)
        ))
        let outgoingBinding = RemoteMediaCallBinding.outgoing(expectedIdleGeneration: 51)
        var outgoingRoute = PreparedRemoteMediaRoute(
            mediaSessionID: "synthetic-media-session-0002",
            mediaLeaseGeneration: 74,
            preferredUID: "synthetic-uac-uid",
            vendorID: config.vendorID,
            productID: config.productID,
            locationID: config.locationID,
            expiresAt: base.addingTimeInterval(45),
            callBinding: outgoingBinding
        )
        precondition(outgoingRoute.reconcile(
            call: nil,
            callGeneration: 51,
            mediaEligible: false,
            preparedGeneration: 74,
            now: base
        ))
        let dialing = CallRecord(
            id: "synthetic-outgoing-call",
            index: 1,
            direction: "outgoing",
            state: "dialing",
            number: nil,
            startedAt: base,
            updatedAt: base,
            endedAt: nil,
            missed: false
        )
        precondition(outgoingRoute.reconcile(
            call: dialing,
            callGeneration: 52,
            mediaEligible: false,
            preparedGeneration: 74,
            now: base
        ))
        let outgoingActive = CallRecord(
            id: dialing.id,
            index: dialing.index,
            direction: dialing.direction,
            state: "active",
            number: nil,
            startedAt: base,
            updatedAt: base,
            endedAt: nil,
            missed: false
        )
        precondition(outgoingRoute.reconcile(
            call: outgoingActive,
            callGeneration: 53,
            mediaEligible: true,
            preparedGeneration: 74,
            now: base
        ))
        let outgoingConfig = MaVoAudioHostConfig(
            vendorID: config.vendorID,
            productID: config.productID,
            locationID: config.locationID,
            routeReady: true,
            routeError: nil,
            callGeneration: 53,
            callID: outgoingActive.id,
            callIndex: outgoingActive.index,
            callDirection: outgoingActive.direction
        )
        precondition(outgoingRoute.matchesActive(
            call: outgoingActive,
            config: outgoingConfig,
            preparedGeneration: 74
        ))
        precondition(
            sha256Hex("synthetic-uac-uid") ==
                "70e54a8cba3e077f5388685e00fae493d29e7fa948d2af4dbfc0d8bed9f61d9c"
        )
    }

    func callDuration(now: Date = Date()) -> TimeInterval {
        guard let call = activeCall else { return 0 }
        let end = call.endedAt ?? now
        return max(0, end.timeIntervalSince(call.startedAt))
    }

    func dialNumber(_ number: String) {
        numberInput = number
        dial()
    }
}
