package org.maccellular.gateway.nativeclient;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;
import org.json.JSONTokener;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.time.Instant;
import java.time.format.DateTimeParseException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;

/** Native, cookie-free client for enrollment and explicitly foreground API calls. */
public final class NativeGatewayClient {
    static final String DEVICE_ID_HEADER = "X-MacCellular-Device-ID";
    static final String CHALLENGE_ID_HEADER = "X-MacCellular-Challenge-ID";
    static final String SIGNATURE_HEADER = "X-MacCellular-Signature";
    static final String IDEMPOTENCY_HEADER = "Idempotency-Key";

    private static final byte[] EMPTY_BODY = new byte[0];
    private static final String EMPTY_BODY_SHA256 =
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
    private static final Pattern LOWER_HEX_SHA256 = Pattern.compile("[0-9a-f]{64}");
    private static final Pattern JSON_CONTENT_TYPE = Pattern.compile(
            "application/json(?:\\s*;\\s*charset=utf-8)?");
    private static final Set<String> NATIVE_SCOPES =
            setOf("sms.read", "sms.send", "status.read");

    private final NativeGatewayUrlPolicy urlPolicy;
    private final DeviceIdentity identity;
    private final NativeTransport transport;

    public NativeGatewayClient(NativeGatewayUrlPolicy urlPolicy, DeviceIdentity identity) {
        this(urlPolicy, identity, new NativeHttpTransport(urlPolicy));
    }

    NativeGatewayClient(
            NativeGatewayUrlPolicy urlPolicy,
            DeviceIdentity identity,
            NativeTransport transport) {
        this.urlPolicy = java.util.Objects.requireNonNull(urlPolicy, "urlPolicy");
        this.identity = java.util.Objects.requireNonNull(identity, "identity");
        this.transport = java.util.Objects.requireNonNull(transport, "transport");
    }

    /**
     * Performs prepare/sign/complete exactly once. This method never retries either POST.
     * Any transport loss means the ticket must not be reused.
     */
    public EnrollmentResult enroll(String ticketId, char[] ticketSecret)
            throws NativeGatewayException {
        validateOpaqueId(ticketId, "enr_", "ticket_id");
        if (ticketSecret == null) {
            throw new NativeGatewayException("ticket_secret is required");
        }
        String secret = new String(ticketSecret);
        Arrays.fill(ticketSecret, '\0');
        if (!isRawBase64Url(secret, 32)) {
            throw new NativeGatewayException("ticket_secret is invalid");
        }

        try {
            JSONObject prepare = new JSONObject();
            prepare.put("step", "prepare");
            prepare.put("ticket_id", ticketId);
            prepare.put("ticket_secret", secret);
            prepare.put("algorithm", DeviceIdentity.ALGORITHM);
            prepare.put("public_key_spki", identity.publicKeySpki());

            JSONObject prepared = executeJson(
                    post(urlPolicy.enrollmentsUri(), prepare), 200);
            requireExactKeys(prepared,
                    setOf("version", "enrollment_id", "expires_at", "signing_input"));
            requireVersionOne(prepared);
            String enrollmentId = requireOpaqueId(
                    prepared, "enrollment_id", "npe_");
            requireTimestamp(prepared, "expires_at");
            String signingInput = requireString(prepared, "signing_input", 1, 16 * 1024);
            String signature = identity.signServerInput(signingInput);

            JSONObject complete = new JSONObject();
            complete.put("step", "complete");
            complete.put("enrollment_id", enrollmentId);
            complete.put("signature", signature);
            JSONObject completed = executeJson(
                    post(urlPolicy.enrollmentsUri(), complete), 201);
            return parseEnrollmentResult(completed);
        } catch (IOException error) {
            throw new NativeGatewayException(
                    "Enrollment transport outcome is unknown; do not reuse this ticket",
                    error,
                    true);
        } catch (GeneralSecurityException | JSONException error) {
            throw new NativeGatewayException("Enrollment contract validation failed", error);
        } finally {
            // The Java/JSON string cannot be wiped, but it is neither persisted nor logged.
            secret = null;
        }
    }

    public SessionResult getSession() throws NativeGatewayException {
        JSONObject response = executeSignedGet(
                urlPolicy.sessionUri(), NativeGatewayUrlPolicy.SESSION_PATH);
        try {
            return parseSession(response);
        } catch (JSONException error) {
            throw new NativeGatewayException("Session contract validation failed", error);
        }
    }

    public String getSessionJson() throws NativeGatewayException {
        return getSession().json();
    }

    public String syncSmsJson(String cursor, int limit) throws NativeGatewayException {
        return syncSms(cursor, limit).json();
    }

    public SmsSyncPage syncSms(String cursor, int limit) throws NativeGatewayException {
        String target = urlPolicy.smsSyncTarget(cursor, limit);
        JSONObject response = executeSignedGet(urlPolicy.smsSyncUri(cursor, limit), target);
        try {
            return validateSmsSync(response);
        } catch (JSONException error) {
            throw new NativeGatewayException("SMS sync contract validation failed", error);
        }
    }

    /**
     * Sends one already-built immutable SMS request. The callback must durably mark the
     * operation unknown immediately before the final POST. This method never retries.
     */
    public SmsSendResult sendSms(SmsSendRequest request, BeforeSmsPost beforePost)
            throws NativeGatewayException {
        java.util.Objects.requireNonNull(request, "request");
        java.util.Objects.requireNonNull(beforePost, "beforePost");
        byte[] exactBody = null;
        boolean finalPostMayHaveStarted = false;
        try {
            exactBody = request.exactBody();
            String actualHash = sha256Hex(exactBody);
            if (!actualHash.equals(request.bodySha256())) {
                throw new NativeGatewayException("SMS request body hash changed before signing");
            }
            Map<String, String> headers = signedHeaders(
                    "POST", NativeGatewayUrlPolicy.SMS_SEND_PATH, actualHash, true);
            headers.put(IDEMPOTENCY_HEADER, request.operationId());
            NativeHttpRequest signedPost = new NativeHttpRequest(
                    "POST", urlPolicy.smsSendUri(), exactBody, headers);

            // The durable local UNKNOWN state must exist before any final POST bytes
            // can leave the process. A callback failure prevents the network call.
            beforePost.markUnknownBeforePost();
            finalPostMayHaveStarted = true;
            NativeHttpResponse response = transport.execute(signedPost);
            return parseSmsSendResponse(response);
        } catch (NativeGatewayException error) {
            if (finalPostMayHaveStarted) {
                throw new NativeGatewayException(
                        "SMS submission outcome is unknown; do not send it again",
                        error,
                        false,
                        true);
            }
            throw error;
        } catch (IOException | GeneralSecurityException | JSONException | RuntimeException error) {
            throw new NativeGatewayException(
                    finalPostMayHaveStarted
                            ? "SMS submission outcome is unknown; do not send it again"
                            : "SMS submission did not start",
                    error,
                    false,
                    finalPostMayHaveStarted);
        } finally {
            if (exactBody != null) {
                Arrays.fill(exactBody, (byte) 0);
            }
        }
    }

    /** Performs a signed, empty-body lookup. It never repeats the SMS POST. */
    public SmsOperationResult getSmsOperation(String operationId)
            throws NativeGatewayException {
        String target;
        try {
            target = urlPolicy.smsOperationTarget(operationId);
        } catch (IllegalArgumentException error) {
            throw new NativeGatewayException("SMS operation id is invalid", error);
        }
        JSONObject response = executeSignedGet(urlPolicy.smsOperationUri(operationId), target);
        try {
            return parseSmsOperation(response, operationId);
        } catch (JSONException error) {
            throw new NativeGatewayException("SMS operation contract validation failed", error);
        }
    }

    private JSONObject executeSignedGet(java.net.URI uri, String requestTarget)
            throws NativeGatewayException {
        try {
            Map<String, String> headers = signedHeaders(
                    "GET", requestTarget, EMPTY_BODY_SHA256, false);
            return executeJson(new NativeHttpRequest("GET", uri, EMPTY_BODY, headers), 200);
        } catch (IOException | GeneralSecurityException | JSONException error) {
            throw new NativeGatewayException("Signed native request failed", error);
        }
    }

    private Map<String, String> signedHeaders(
            String method,
            String requestTarget,
            String bodySha256,
            boolean withBody)
            throws IOException, GeneralSecurityException, JSONException, NativeGatewayException {
        JSONObject challengeRequest = new JSONObject();
        challengeRequest.put("version", 1);
        challengeRequest.put("device_id", identity.deviceId());
        challengeRequest.put("method", method);
        challengeRequest.put("request_target", requestTarget);
        challengeRequest.put("body_sha256", bodySha256);

        JSONObject challenge = executeJson(
                post(urlPolicy.challengesUri(), challengeRequest), 200);
        requireExactKeys(challenge,
                setOf("version", "challenge_id", "expires_at", "signing_input"));
        requireVersionOne(challenge);
        String challengeId = requireOpaqueId(challenge, "challenge_id", "nch_");
        requireTimestamp(challenge, "expires_at");
        String signingInput = requireAsciiString(
                challenge, "signing_input", 1, 16 * 1024);
        String signature = identity.signServerInput(signingInput);

        Map<String, String> headers = baseHeaders(withBody);
        headers.put(DEVICE_ID_HEADER, identity.deviceId());
        headers.put(CHALLENGE_ID_HEADER, challengeId);
        headers.put(SIGNATURE_HEADER, signature);
        return headers;
    }

    private NativeHttpRequest post(java.net.URI uri, JSONObject body) {
        return new NativeHttpRequest(
                "POST",
                uri,
                body.toString().getBytes(StandardCharsets.UTF_8),
                baseHeaders(true));
    }

    private JSONObject executeJson(NativeHttpRequest request, int expectedStatus)
            throws IOException, NativeGatewayException, JSONException {
        NativeHttpResponse response = transport.execute(request);
        JSONObject result = parseJsonResponse(response);
        if (response.statusCode != expectedStatus) {
            throw new NativeGatewayException(
                    "Gateway returned unexpected HTTP status " + response.statusCode);
        }
        return result;
    }

    private static JSONObject parseJsonResponse(NativeHttpResponse response)
            throws NativeGatewayException, JSONException {
        if (response.statusCode >= 300 && response.statusCode <= 399) {
            throw new NativeGatewayException("Gateway redirects are forbidden");
        }
        String contentType = response.contentType.trim().toLowerCase(Locale.ROOT);
        if (!JSON_CONTENT_TYPE.matcher(contentType).matches()) {
            throw new NativeGatewayException("Gateway response is not application/json");
        }
        return parseStrictObject(response.body);
    }

    private static SmsSendResult parseSmsSendResponse(NativeHttpResponse response)
            throws NativeGatewayException, JSONException {
        JSONObject result = parseJsonResponse(response);
        Set<String> keys = jsonKeys(result);
        if (keys.equals(setOf("ok", "code"))) {
            requireBoolean(result, "ok");
            boolean ok = (Boolean) result.get("ok");
            String code = requireString(result, "code", 1, 48);
            if (response.statusCode == 200 && ok && "completed".equals(code)) {
                return new SmsSendResult(response.statusCode, code, SmsOutcome.SUBMITTED);
            }
            if (response.statusCode == 400 && !ok && "invalid_request".equals(code)) {
                return new SmsSendResult(response.statusCode, code, SmsOutcome.NOT_SUBMITTED);
            }
            if ((response.statusCode == 409 && !ok
                    && ("unknown_outcome".equals(code) || "conflict".equals(code)))
                    || (response.statusCode == 503 && !ok
                    && "service_unavailable".equals(code))
                    || (response.statusCode == 202 && ok
                    && "completed_audit_unavailable".equals(code))) {
                return new SmsSendResult(response.statusCode, code, SmsOutcome.UNKNOWN_LOCKED);
            }
            throw new NativeGatewayException("Gateway returned an unsafe SMS result combination");
        }
        if (keys.equals(setOf("error", "code"))) {
            String error = requireString(result, "error", 1, 128);
            String code = requireString(result, "code", 1, 64);
            if (!"native request refused".equals(error)
                    || !isKnownPreHardwareNativeError(response.statusCode, code)) {
                throw new NativeGatewayException("Gateway returned an unknown native SMS error");
            }
            // This response arrived only after the final POST was allowed to
            // leave. HTTPS implementations may transparently replay a POST
            // after losing the first response, so a later pre-ledger refusal
            // cannot prove that the first attempt did not reach the modem.
            // Keep the durable guard locked and reconcile by signed GET only.
            return new SmsSendResult(response.statusCode, code, SmsOutcome.UNKNOWN_LOCKED);
        }
        throw new NativeGatewayException("Gateway returned an invalid SMS response shape");
    }

    private static boolean isKnownPreHardwareNativeError(int status, String code) {
        if (status == 400) {
            return "invalid_request".equals(code);
        }
        if (status == 403) {
            return setOf(
                    "native_transport_required",
                    "tailscale_auth_required",
                    "native_control_disabled",
                    "device_proof_invalid",
                    "capability_denied").contains(code);
        }
        if (status == 415) {
            return "json_required".equals(code);
        }
        return status == 503 && setOf(
                "native_auth_unavailable", "operation_ledger_unavailable").contains(code);
    }

    private static JSONObject parseStrictObject(byte[] body)
            throws NativeGatewayException, JSONException {
        if (body.length == 0) {
            throw new NativeGatewayException("Gateway returned an empty JSON response");
        }
        String text;
        try {
            text = StandardCharsets.UTF_8.newDecoder()
                    .onMalformedInput(CodingErrorAction.REPORT)
                    .onUnmappableCharacter(CodingErrorAction.REPORT)
                    .decode(ByteBuffer.wrap(body))
                    .toString();
        } catch (CharacterCodingException error) {
            throw new NativeGatewayException("Gateway returned malformed UTF-8 JSON", error);
        }
        JSONTokener tokener = new JSONTokener(text);
        Object value = tokener.nextValue();
        if (!(value instanceof JSONObject) || tokener.nextClean() != 0) {
            throw new NativeGatewayException("Gateway returned invalid JSON framing");
        }
        return (JSONObject) value;
    }

    private EnrollmentResult parseEnrollmentResult(JSONObject result)
            throws NativeGatewayException, JSONException {
        requireExactKeys(result,
                setOf("version", "device_id", "algorithm", "scopes", "enrolled_at"));
        requireVersionOne(result);
        String deviceId = requireString(result, "device_id", 1, 256);
        if (!identity.deviceId().equals(deviceId)) {
            throw new NativeGatewayException("Gateway enrolled a different device identity");
        }
        if (!DeviceIdentity.ALGORITHM.equals(requireString(result, "algorithm", 1, 16))) {
            throw new NativeGatewayException("Gateway returned an unexpected key algorithm");
        }
        JSONArray scopesJson = requireArray(result, "scopes");
        List<String> scopes = new ArrayList<>();
        String previous = null;
        for (int index = 0; index < scopesJson.length(); index++) {
            Object raw = scopesJson.get(index);
            if (!(raw instanceof String)) {
                throw new NativeGatewayException("Enrollment scope must be a string");
            }
            String scope = (String) raw;
            if (!NATIVE_SCOPES.contains(scope)
                    || (previous != null && previous.compareTo(scope) >= 0)) {
                throw new NativeGatewayException(
                        "Enrollment scopes are not unique, sorted, and permitted");
            }
            scopes.add(scope);
            previous = scope;
        }
        if (scopes.isEmpty()) {
            throw new NativeGatewayException(
                    "Enrollment did not grant a native scope");
        }
        String enrolledAt = requireTimestamp(result, "enrolled_at");
        return new EnrollmentResult(deviceId, scopes, enrolledAt);
    }

    private SessionResult parseSession(JSONObject result)
            throws NativeGatewayException, JSONException {
        requireExactKeys(result,
                setOf(
                        "version", "device_id", "algorithm", "actions", "transport",
                        "sms_sync", "sms_send"));
        requireVersionOne(result);
        if (!identity.deviceId().equals(requireString(result, "device_id", 1, 256))) {
            throw new NativeGatewayException("Session belongs to a different device identity");
        }
        if (!DeviceIdentity.ALGORITHM.equals(requireString(result, "algorithm", 1, 16))) {
            throw new NativeGatewayException("Session returned an unexpected key algorithm");
        }
        if (!"tailscale-serve-device-proof".equals(
                requireString(result, "transport", 1, 64))) {
            throw new NativeGatewayException("Session returned an unexpected transport");
        }
        JSONArray actionsJson = requireArray(result, "actions");
        Set<String> actions = new HashSet<>();
        String previous = null;
        for (int index = 0; index < actionsJson.length(); index++) {
            Object raw = actionsJson.get(index);
            if (!(raw instanceof String)) {
                throw new NativeGatewayException("Session action must be a string");
            }
            String action = (String) raw;
            if (!NATIVE_SCOPES.contains(action)
                    || (previous != null && previous.compareTo(action) >= 0)) {
                throw new NativeGatewayException(
                        "Session actions are not unique, sorted, and permitted");
            }
            actions.add(action);
            previous = action;
        }
        if (!actions.contains("status.read")) {
            throw new NativeGatewayException("Session is missing status.read");
        }
        requireBoolean(result, "sms_sync");
        if ((Boolean) result.get("sms_sync") && !actions.contains("sms.read")) {
            throw new NativeGatewayException("Session SMS readiness exceeds its actions");
        }
        requireBoolean(result, "sms_send");
        if ((Boolean) result.get("sms_send") && !actions.contains("sms.send")) {
            throw new NativeGatewayException("Session SMS send readiness exceeds its actions");
        }
        return new SessionResult(
                result.toString(),
                actions,
                (Boolean) result.get("sms_sync"),
                (Boolean) result.get("sms_send"));
    }

    private static SmsOperationResult parseSmsOperation(
            JSONObject result,
            String expectedOperationId) throws NativeGatewayException, JSONException {
        requireVersionOne(result);
        String operationId = requireOpaqueId(result, "operation_id", "nsm_");
        if (!expectedOperationId.equals(operationId)) {
            throw new NativeGatewayException("Gateway returned a different SMS operation");
        }
        String state = requireString(result, "state", 1, 32);
        if ("completed".equals(state)) {
            requireExactKeys(result,
                    setOf("version", "operation_id", "state", "http_status", "code"));
            int status = requireHTTPStatus(result, "http_status");
            String code = requireString(result, "code", 1, 48);
            SmsOutcome outcome;
            if (status == 200 && "completed".equals(code)) {
                outcome = SmsOutcome.SUBMITTED;
            } else if ((status == 400 && "invalid_request".equals(code))
                    || (status == 503 && "service_unavailable".equals(code))) {
                outcome = SmsOutcome.NOT_SUBMITTED;
            } else {
                // A completed journal record can still authoritatively say that
                // the hardware outcome is unknown. Never infer success from state.
                outcome = SmsOutcome.UNKNOWN_LOCKED;
            }
            return new SmsOperationResult(operationId, state, status, code, outcome);
        }
        requireExactKeys(result, setOf("version", "operation_id", "state"));
        switch (state) {
        case "not_found":
            return new SmsOperationResult(
                    operationId, state, 0, "", SmsOutcome.NOT_FOUND_REQUIRES_CONFIRMATION);
        case "in_flight":
        case "conflict":
        case "unknown_outcome":
            return new SmsOperationResult(
                    operationId, state, 0, "", SmsOutcome.UNKNOWN_LOCKED);
        default:
            throw new NativeGatewayException("Gateway returned an unknown SMS operation state");
        }
    }

    private static SmsSyncPage validateSmsSync(JSONObject result)
            throws NativeGatewayException, JSONException {
        requireExactKeys(result, setOf("messages", "cursor", "bootstrap", "has_more"));
        JSONArray messages = requireArray(result, "messages");
        String cursor = requireString(result, "cursor", 1, 256);
        requireBoolean(result, "bootstrap");
        requireBoolean(result, "has_more");
        Set<String> required = setOf(
                "id", "event_id", "direction", "status", "peer", "content",
                "timestamp", "updated_at");
        Set<String> allowed = new HashSet<>(required);
        allowed.add("code");
        allowed.add("segments");
        for (int index = 0; index < messages.length(); index++) {
            Object raw = messages.get(index);
            if (!(raw instanceof JSONObject)) {
                throw new NativeGatewayException("SMS message entry must be an object");
            }
            JSONObject message = (JSONObject) raw;
            requireOnlyKeys(message, allowed, required);
            requireString(message, "id", 1, 256);
            requireString(message, "event_id", 1, 256);
            requireString(message, "direction", 1, 32);
            requireString(message, "status", 1, 64);
            requireString(message, "peer", 0, 256);
            requireString(message, "content", 0, 64 * 1024);
            requireTimestamp(message, "timestamp");
            requireTimestamp(message, "updated_at");
            if (message.has("code")) {
                requireString(message, "code", 0, 256);
            }
            if (message.has("segments")) {
                Object segments = message.get("segments");
                if (!(segments instanceof Integer) && !(segments instanceof Long)) {
                    throw new NativeGatewayException("SMS segments value is invalid");
                }
                if (((Number) segments).longValue() < 1
                        || ((Number) segments).longValue() > Integer.MAX_VALUE) {
                    throw new NativeGatewayException("SMS segments value is invalid");
                }
            }
        }
        return new SmsSyncPage(
                result.toString(),
                cursor,
                (Boolean) result.get("bootstrap"),
                (Boolean) result.get("has_more"),
                messages.length());
    }

    private static Map<String, String> baseHeaders(boolean withBody) {
        LinkedHashMap<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        headers.put("Cache-Control", "no-store");
        if (withBody) {
            headers.put("Content-Type", "application/json; charset=utf-8");
        }
        return headers;
    }

    private static void requireVersionOne(JSONObject object)
            throws NativeGatewayException, JSONException {
        Object value = object.get("version");
        if (!(value instanceof Integer) && !(value instanceof Long)) {
            throw new NativeGatewayException("Native API version must be integer 1");
        }
        if (((Number) value).longValue() != 1) {
            throw new NativeGatewayException("Native API version must be numeric 1");
        }
    }

    private static String requireOpaqueId(JSONObject object, String key, String prefix)
            throws NativeGatewayException, JSONException {
        String value = requireString(object, key, 1, 256);
        validateOpaqueId(value, prefix, key);
        return value;
    }

    private static void validateOpaqueId(String value, String prefix, String key)
            throws NativeGatewayException {
        if (value == null || !value.startsWith(prefix)
                || !isRawBase64Url(value.substring(prefix.length()), 16)) {
            throw new NativeGatewayException(key + " is invalid");
        }
    }

    private static boolean isRawBase64Url(String value, int decodedLength) {
        if (value == null || value.indexOf('=') >= 0) {
            return false;
        }
        try {
            byte[] decoded = java.util.Base64.getUrlDecoder().decode(value);
            return decoded.length == decodedLength
                    && DeviceIdentity.base64Url(decoded).equals(value);
        } catch (IllegalArgumentException error) {
            return false;
        }
    }

    private static String requireTimestamp(JSONObject object, String key)
            throws NativeGatewayException, JSONException {
        String value = requireString(object, key, 1, 128);
        try {
            Instant.parse(value);
        } catch (DateTimeParseException error) {
            throw new NativeGatewayException(key + " is not an RFC3339 timestamp", error);
        }
        return value;
    }

    private static String requireAsciiString(
            JSONObject object, String key, int minimum, int maximum)
            throws NativeGatewayException, JSONException {
        String value = requireString(object, key, minimum, maximum);
        for (int index = 0; index < value.length(); index++) {
            if (value.charAt(index) > 0x7f) {
                throw new NativeGatewayException(key + " must contain only ASCII");
            }
        }
        return value;
    }

    private static String requireString(
            JSONObject object, String key, int minimum, int maximum)
            throws NativeGatewayException, JSONException {
        Object raw = object.get(key);
        if (!(raw instanceof String)) {
            throw new NativeGatewayException(key + " must be a string");
        }
        String value = (String) raw;
        int bytes = value.getBytes(StandardCharsets.UTF_8).length;
        if (bytes < minimum || bytes > maximum) {
            throw new NativeGatewayException(key + " has an invalid length");
        }
        return value;
    }

    private static JSONArray requireArray(JSONObject object, String key)
            throws NativeGatewayException, JSONException {
        Object raw = object.get(key);
        if (!(raw instanceof JSONArray)) {
            throw new NativeGatewayException(key + " must be an array");
        }
        return (JSONArray) raw;
    }

    private static void requireBoolean(JSONObject object, String key)
            throws NativeGatewayException, JSONException {
        if (!(object.get(key) instanceof Boolean)) {
            throw new NativeGatewayException(key + " must be a boolean");
        }
    }

    private static void requireExactKeys(JSONObject object, Set<String> expected)
            throws NativeGatewayException {
        requireOnlyKeys(object, expected, expected);
    }

    private static void requireOnlyKeys(
            JSONObject object, Set<String> allowed, Set<String> required)
            throws NativeGatewayException {
        Set<String> actual = jsonKeys(object);
        if (!allowed.containsAll(actual) || !actual.containsAll(required)) {
            throw new NativeGatewayException("Native JSON object has unknown or missing fields");
        }
    }

    private static Set<String> jsonKeys(JSONObject object) {
        Set<String> actual = new HashSet<>();
        java.util.Iterator<String> keys = object.keys();
        while (keys.hasNext()) {
            actual.add(keys.next());
        }
        return actual;
    }

    private static int requireHTTPStatus(JSONObject object, String key)
            throws NativeGatewayException, JSONException {
        Object value = object.get(key);
        if (!(value instanceof Integer) && !(value instanceof Long)) {
            throw new NativeGatewayException("Native HTTP status must be an integer");
        }
        long status = ((Number) value).longValue();
        if (status < 200 || status > 599) {
            throw new NativeGatewayException("Native HTTP status is out of range");
        }
        return (int) status;
    }

    private static Set<String> setOf(String... values) {
        return Collections.unmodifiableSet(new HashSet<>(Arrays.asList(values)));
    }

    static String sha256Hex(byte[] body) throws GeneralSecurityException {
        byte[] digest = MessageDigest.getInstance("SHA-256").digest(body);
        StringBuilder output = new StringBuilder(digest.length * 2);
        for (byte value : digest) {
            output.append(String.format(Locale.ROOT, "%02x", value & 0xff));
        }
        String result = output.toString();
        if (!LOWER_HEX_SHA256.matcher(result).matches()) {
            throw new GeneralSecurityException("SHA-256 encoding failed");
        }
        return result;
    }

    public static final class EnrollmentResult {
        private final String deviceId;
        private final List<String> scopes;
        private final String enrolledAt;

        EnrollmentResult(String deviceId, List<String> scopes, String enrolledAt) {
            this.deviceId = deviceId;
            this.scopes = Collections.unmodifiableList(new ArrayList<>(scopes));
            this.enrolledAt = enrolledAt;
        }

        public String deviceId() {
            return deviceId;
        }

        public List<String> scopes() {
            return scopes;
        }

        public String enrolledAt() {
            return enrolledAt;
        }
    }

    public static final class SessionResult {
        private final String json;
        private final Set<String> actions;
        private final boolean smsSync;
        private final boolean smsSend;

        SessionResult(String json, Set<String> actions, boolean smsSync, boolean smsSend) {
            this.json = json;
            this.actions = Collections.unmodifiableSet(new HashSet<>(actions));
            this.smsSync = smsSync;
            this.smsSend = smsSend;
        }

        public String json() {
            return json;
        }

        public Set<String> actions() {
            return actions;
        }

        public boolean canSyncSms() {
            return smsSync && actions.contains("sms.read");
        }

        public boolean canSendSms() {
            return smsSend && actions.contains("sms.send");
        }
    }

    public enum SmsOutcome {
        SUBMITTED,
        NOT_SUBMITTED,
        UNKNOWN_LOCKED,
        NOT_FOUND_REQUIRES_CONFIRMATION
    }

    public static final class SmsSendResult {
        private final int httpStatus;
        private final String code;
        private final SmsOutcome outcome;

        SmsSendResult(int httpStatus, String code, SmsOutcome outcome) {
            this.httpStatus = httpStatus;
            this.code = code;
            this.outcome = outcome;
        }

        public int httpStatus() {
            return httpStatus;
        }

        public String code() {
            return code;
        }

        public SmsOutcome outcome() {
            return outcome;
        }
    }

    public static final class SmsOperationResult {
        private final String operationId;
        private final String state;
        private final int httpStatus;
        private final String code;
        private final SmsOutcome outcome;

        SmsOperationResult(
                String operationId,
                String state,
                int httpStatus,
                String code,
                SmsOutcome outcome) {
            this.operationId = operationId;
            this.state = state;
            this.httpStatus = httpStatus;
            this.code = code;
            this.outcome = outcome;
        }

        public String operationId() {
            return operationId;
        }

        public String state() {
            return state;
        }

        public int httpStatus() {
            return httpStatus;
        }

        public String code() {
            return code;
        }

        public SmsOutcome outcome() {
            return outcome;
        }
    }

    @FunctionalInterface
    public interface BeforeSmsPost {
        void markUnknownBeforePost() throws IOException;
    }

    public static final class SmsSyncPage {
        private final String json;
        private final String cursor;
        private final boolean bootstrap;
        private final boolean hasMore;
        private final int messageCount;

        SmsSyncPage(
                String json,
                String cursor,
                boolean bootstrap,
                boolean hasMore,
                int messageCount) {
            this.json = json;
            this.cursor = cursor;
            this.bootstrap = bootstrap;
            this.hasMore = hasMore;
            this.messageCount = messageCount;
        }

        public String json() {
            return json;
        }

        public String cursor() {
            return cursor;
        }

        public boolean bootstrap() {
            return bootstrap;
        }

        public boolean hasMore() {
            return hasMore;
        }

        public int messageCount() {
            return messageCount;
        }
    }
}
