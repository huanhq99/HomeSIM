package org.maccellular.gateway.nativeclient;

import org.json.JSONObject;

import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.SecureRandom;
import java.util.Arrays;

/** One immutable, foreground-only SMS request. Its body is never persisted. */
public final class SmsSendRequest implements AutoCloseable {
    private static final int MAX_PHONE_BYTES = 256;
    private static final int MAX_MESSAGE_BYTES = 8 << 10;

    private final String operationId;
    private final String bodySha256;
    private byte[] body;

    public static SmsSendRequest create(String phone, String message)
            throws NativeGatewayException {
        byte[] random = new byte[16];
        new SecureRandom().nextBytes(random);
        return createForOperation(
                "nsm_" + DeviceIdentity.base64Url(random), phone, message);
    }

    static SmsSendRequest createForOperation(
            String operationId,
            String phone,
            String message) throws NativeGatewayException {
        if (!NativeGatewayUrlPolicy.isOperationId(operationId)) {
            throw new NativeGatewayException("SMS operation id is invalid");
        }
        validateText(phone, "phone", MAX_PHONE_BYTES);
        validateText(message, "message", MAX_MESSAGE_BYTES);
        String json = "{\"version\":1,\"operation_id\":" + JSONObject.quote(operationId)
                + ",\"phone\":" + JSONObject.quote(phone)
                + ",\"message\":" + JSONObject.quote(message) + "}";
        byte[] exactBody = json.getBytes(StandardCharsets.UTF_8);
        if (exactBody.length == 0 || exactBody.length > 16 << 10) {
            throw new NativeGatewayException("Native SMS request exceeds the body limit");
        }
        try {
            return new SmsSendRequest(
                    operationId, exactBody, NativeGatewayClient.sha256Hex(exactBody));
        } catch (GeneralSecurityException error) {
            throw new NativeGatewayException("Unable to hash the SMS request", error);
        } finally {
            Arrays.fill(exactBody, (byte) 0);
        }
    }

    private SmsSendRequest(String operationId, byte[] body, String bodySha256) {
        this.operationId = operationId;
        this.body = body.clone();
        this.bodySha256 = bodySha256;
    }

    public String operationId() {
        return operationId;
    }

    public String bodySha256() {
        return bodySha256;
    }

    synchronized byte[] exactBody() {
        if (body == null) {
            throw new IllegalStateException("SMS request body was already cleared");
        }
        return body.clone();
    }

    @Override
    public synchronized void close() {
        if (body != null) {
            Arrays.fill(body, (byte) 0);
            body = null;
        }
    }

    private static void validateText(String value, String name, int maximumBytes)
            throws NativeGatewayException {
        if (value == null) {
            throw new NativeGatewayException("SMS " + name + " is required");
        }
        int bytes = value.getBytes(StandardCharsets.UTF_8).length;
        if (value.isEmpty() || !value.equals(value.trim()) || value.indexOf('\0') >= 0
                || bytes > maximumBytes) {
            throw new NativeGatewayException("SMS " + name + " is invalid");
        }
    }
}
