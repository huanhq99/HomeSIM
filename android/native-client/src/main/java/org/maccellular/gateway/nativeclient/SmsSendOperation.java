package org.maccellular.gateway.nativeclient;

import org.json.JSONException;
import org.json.JSONObject;
import org.json.JSONTokener;

import java.nio.ByteBuffer;
import java.nio.charset.CharacterCodingException;
import java.nio.charset.CodingErrorAction;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.time.format.DateTimeParseException;
import java.util.Base64;
import java.util.HashSet;
import java.util.Set;
import java.util.regex.Pattern;

/** Durable guard for one foreground SMS attempt; it excludes recipient, message, and body hash. */
public final class SmsSendOperation {
    private static final int VERSION = 1;
    private static final Pattern RESULT_CODE = Pattern.compile("[a-z][a-z0-9_]{0,47}");

    public enum State {
        PREPARED("prepared"),
        UNKNOWN("unknown"),
        NOT_FOUND("not_found"),
        TERMINAL("terminal");

        private final String wireValue;

        State(String wireValue) {
            this.wireValue = wireValue;
        }

        String wireValue() {
            return wireValue;
        }

        static State parse(String value) {
            for (State state : values()) {
                if (state.wireValue.equals(value)) {
                    return state;
                }
            }
            throw new IllegalArgumentException("Unknown SMS operation state");
        }
    }

    private final String operationId;
    private final String deviceId;
    private final State state;
    private final Instant createdAt;
    private final Instant updatedAt;
    private final int httpStatus;
    private final String resultCode;

    public static SmsSendOperation prepared(
            String operationId,
            String deviceId,
            Instant now) {
        return new SmsSendOperation(
                operationId, deviceId, State.PREPARED,
                now, now, 0, "");
    }

    private SmsSendOperation(
            String operationId,
            String deviceId,
            State state,
            Instant createdAt,
            Instant updatedAt,
            int httpStatus,
            String resultCode) {
        if (!NativeGatewayUrlPolicy.isOperationId(operationId)
                || !isDeviceId(deviceId)
                || state == null
                || createdAt == null
                || updatedAt == null
                || updatedAt.isBefore(createdAt)) {
            throw new IllegalArgumentException("SMS operation record is invalid");
        }
        if (state == State.TERMINAL) {
            if (resultCode == null || !RESULT_CODE.matcher(resultCode).matches()
                    || !isSafeTerminalResult(httpStatus, resultCode)) {
                throw new IllegalArgumentException("SMS terminal result is invalid");
            }
        } else if (state == State.PREPARED
                && (httpStatus != 0 || resultCode == null || !resultCode.isEmpty())) {
            throw new IllegalArgumentException("Prepared SMS operation has a result");
        } else if (state == State.NOT_FOUND
                && (httpStatus != 200 || !"not_found".equals(resultCode))) {
            throw new IllegalArgumentException("Not-found SMS operation result is invalid");
        } else if (state == State.UNKNOWN
                && !validOptionalUnresolvedResult(httpStatus, resultCode)) {
            throw new IllegalArgumentException("Unknown SMS operation result is invalid");
        }
        this.operationId = operationId;
        this.deviceId = deviceId;
        this.state = state;
        this.createdAt = createdAt;
        this.updatedAt = updatedAt;
        this.httpStatus = httpStatus;
        this.resultCode = resultCode;
    }

    public SmsSendOperation markUnknown(Instant now) {
        if (state != State.PREPARED && state != State.UNKNOWN && state != State.NOT_FOUND) {
            throw new IllegalStateException("Terminal SMS operation cannot become unknown");
        }
        return unresolved(State.UNKNOWN, 0, "", now);
    }

    public SmsSendOperation markUnknown(int status, String code, Instant now) {
        if (state == State.TERMINAL) {
            throw new IllegalStateException("Terminal SMS operation cannot become unknown");
        }
        return unresolved(State.UNKNOWN, status, code, now);
    }

    public SmsSendOperation markNotFound(Instant now) {
        if (state == State.TERMINAL) {
            throw new IllegalStateException("Terminal SMS operation cannot become not-found");
        }
        return unresolved(State.NOT_FOUND, 200, "not_found", now);
    }

    public SmsSendOperation markTerminal(int status, String code, Instant now) {
        if (state == State.TERMINAL) {
            throw new IllegalStateException("SMS operation is already terminal");
        }
        return new SmsSendOperation(
                operationId, deviceId, State.TERMINAL,
                createdAt, requireMonotonic(now), status, code);
    }

    private SmsSendOperation unresolved(State next, int status, String code, Instant now) {
        return new SmsSendOperation(
                operationId, deviceId, next,
                createdAt, requireMonotonic(now), status, code);
    }

    private Instant requireMonotonic(Instant now) {
        if (now == null || now.isBefore(updatedAt)) {
            throw new IllegalArgumentException("SMS operation time moved backwards");
        }
        return now;
    }

    public String operationId() {
        return operationId;
    }

    public String deviceId() {
        return deviceId;
    }

    public State state() {
        return state;
    }

    public Instant createdAt() {
        return createdAt;
    }

    public Instant updatedAt() {
        return updatedAt;
    }

    public int httpStatus() {
        return httpStatus;
    }

    public String resultCode() {
        return resultCode;
    }

    public boolean blocksNewSend() {
        return state != State.TERMINAL;
    }

    byte[] encode() {
        try {
            JSONObject object = new JSONObject();
            object.put("version", VERSION);
            object.put("operation_id", operationId);
            object.put("device_id", deviceId);
            object.put("state", state.wireValue());
            object.put("created_at", createdAt.toString());
            object.put("updated_at", updatedAt.toString());
            if (state == State.TERMINAL || httpStatus != 0 || !resultCode.isEmpty()) {
                object.put("http_status", httpStatus);
                object.put("code", resultCode);
            }
            return object.toString().getBytes(StandardCharsets.UTF_8);
        } catch (JSONException impossible) {
            throw new IllegalStateException("Unable to encode SMS operation", impossible);
        }
    }

    static SmsSendOperation decode(byte[] encoded) {
        if (encoded == null || encoded.length == 0 || encoded.length > 16 << 10) {
            throw new IllegalArgumentException("SMS operation record size is invalid");
        }
        try {
            String text;
            try {
                text = StandardCharsets.UTF_8.newDecoder()
                        .onMalformedInput(CodingErrorAction.REPORT)
                        .onUnmappableCharacter(CodingErrorAction.REPORT)
                        .decode(ByteBuffer.wrap(encoded))
                        .toString();
            } catch (CharacterCodingException error) {
                throw new IllegalArgumentException("SMS operation record is not UTF-8", error);
            }
            JSONTokener tokener = new JSONTokener(text);
            Object value = tokener.nextValue();
            if (!(value instanceof JSONObject) || tokener.nextClean() != 0) {
                throw new IllegalArgumentException("SMS operation JSON framing is invalid");
            }
            JSONObject object = (JSONObject) value;
            Object version = object.get("version");
            if (!(version instanceof Integer) && !(version instanceof Long)) {
                throw new IllegalArgumentException("SMS operation version is invalid");
            }
            if (((Number) version).longValue() != VERSION) {
                throw new IllegalArgumentException("SMS operation version is unsupported");
            }
            State state = State.parse(requireString(object, "state"));
            Set<String> expected = new HashSet<>();
            expected.add("version");
            expected.add("operation_id");
            expected.add("device_id");
            expected.add("state");
            expected.add("created_at");
            expected.add("updated_at");
            boolean hasStatus = object.has("http_status");
            boolean hasCode = object.has("code");
            if (hasStatus != hasCode) {
                throw new IllegalArgumentException("SMS operation result fields are incomplete");
            }
            if ((state == State.PREPARED && hasStatus)
                    || ((state == State.NOT_FOUND || state == State.TERMINAL) && !hasStatus)) {
                throw new IllegalArgumentException("SMS operation result fields do not match state");
            }
            if (state == State.TERMINAL || hasStatus) {
                expected.add("http_status");
                expected.add("code");
            }
            Set<String> actual = new HashSet<>();
            object.keys().forEachRemaining(actual::add);
            if (!actual.equals(expected)) {
                throw new IllegalArgumentException("SMS operation record fields are invalid");
            }
            int status = 0;
            String code = "";
            if (state == State.TERMINAL || hasStatus) {
                Object rawStatus = object.get("http_status");
                if (!(rawStatus instanceof Integer) && !(rawStatus instanceof Long)) {
                    throw new IllegalArgumentException("SMS operation status is invalid");
                }
                long statusValue = ((Number) rawStatus).longValue();
                if (statusValue < 0 || statusValue > Integer.MAX_VALUE) {
                    throw new IllegalArgumentException("SMS operation status is invalid");
                }
                status = (int) statusValue;
                code = requireString(object, "code");
            }
            return new SmsSendOperation(
                    requireString(object, "operation_id"),
                    requireString(object, "device_id"),
                    state,
                    parseInstant(requireString(object, "created_at")),
                    parseInstant(requireString(object, "updated_at")),
                    status,
                    code);
        } catch (JSONException | DateTimeParseException error) {
            throw new IllegalArgumentException("SMS operation record is invalid", error);
        }
    }

    private static String requireString(JSONObject object, String name) throws JSONException {
        Object value = object.get(name);
        if (!(value instanceof String)) {
            throw new IllegalArgumentException("SMS operation field is not a string");
        }
        return (String) value;
    }

    private static Instant parseInstant(String value) {
        return Instant.parse(value);
    }

    private static boolean isDeviceId(String value) {
        if (value == null || !value.startsWith("dev_") || value.indexOf('=') >= 0) {
            return false;
        }
        try {
            byte[] decoded = Base64.getUrlDecoder().decode(value.substring(4));
            return decoded.length == 32
                    && DeviceIdentity.base64Url(decoded).equals(value.substring(4));
        } catch (IllegalArgumentException error) {
            return false;
        }
    }

    static boolean isSafeTerminalResult(int status, String code) {
        if (status == 200) {
            return "completed".equals(code);
        }
        if (status == 400) {
            return "invalid_request".equals(code);
        }
        // Only results which can be replayed from the authoritative operation
        // ledger may unlock another send. Native transport/auth failures occur
        // before that ledger and cannot disprove an earlier transparently
        // replayed POST, even when their individual response is unambiguous.
        return status == 503 && "service_unavailable".equals(code);
    }

    private static boolean validOptionalUnresolvedResult(int status, String code) {
        if (status == 0) {
            return code != null && code.isEmpty();
        }
        return status >= 200 && status <= 599
                && code != null && RESULT_CODE.matcher(code).matches();
    }
}
