package org.maccellular.gateway.nativeclient;

import java.io.UnsupportedEncodingException;
import java.net.URI;
import java.net.URLDecoder;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.Locale;
import java.util.Objects;
import java.util.regex.Pattern;

/** Builds every native API URL from one build-pinned Tailscale HTTPS origin. */
public final class NativeGatewayUrlPolicy {
    static final String ENROLLMENTS_PATH = "/api/native/v1/enrollments";
    static final String CHALLENGES_PATH = "/api/native/v1/auth/challenges";
    static final String SESSION_PATH = "/api/native/v1/session";
    static final String SMS_SYNC_PATH = "/api/native/v1/sms/sync";
    static final String SMS_OPERATIONS_PATH = "/api/native/v1/sms/operations";
    static final String SMS_SEND_PATH = "/api/native/v1/sms/send";
    static final int SMS_SYNC_MAXIMUM_LIMIT = 10;

    private static final Pattern TAILSCALE_HOST = Pattern.compile(
            "[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?" +
                    "(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\\.ts\\.net");

    private final String origin;
    private final URI originUri;

    public NativeGatewayUrlPolicy(String configuredOrigin) {
        Objects.requireNonNull(configuredOrigin, "configuredOrigin");
        if (!configuredOrigin.equals(configuredOrigin.trim())) {
            throw new IllegalArgumentException("Gateway origin must not contain surrounding whitespace");
        }
        URI parsed;
        try {
            parsed = URI.create(configuredOrigin);
        } catch (IllegalArgumentException error) {
            throw new IllegalArgumentException("Gateway origin is not a valid URI", error);
        }
        String host = parsed.getHost() == null
                ? ""
                : parsed.getHost().toLowerCase(Locale.ROOT);
        String canonical = "https://" + host;
        if (!"https".equals(parsed.getScheme()) || !TAILSCALE_HOST.matcher(host).matches()
                || parsed.getPort() != -1 || parsed.getRawUserInfo() != null
                || !(parsed.getRawPath() == null || parsed.getRawPath().isEmpty())
                || parsed.getRawQuery() != null || parsed.getRawFragment() != null
                || !configuredOrigin.equals(canonical)) {
            throw new IllegalArgumentException(
                    "Gateway origin must be exactly https://<host>.ts.net with no trailing slash");
        }
        origin = canonical;
        originUri = parsed;
    }

    public String origin() {
        return origin;
    }

    URI enrollmentsUri() {
        return endpoint(ENROLLMENTS_PATH);
    }

    URI challengesUri() {
        return endpoint(CHALLENGES_PATH);
    }

    URI sessionUri() {
        return endpoint(SESSION_PATH);
    }

    URI smsSyncUri(String cursor, int limit) {
        return endpoint(smsSyncTarget(cursor, limit));
    }

    URI smsSendUri() {
        return endpoint(SMS_SEND_PATH);
    }

    URI smsOperationUri(String operationId) {
        return endpoint(smsOperationTarget(operationId));
    }

    String smsOperationTarget(String operationId) {
        if (!isOperationId(operationId)) {
            throw new IllegalArgumentException("SMS operation id is invalid");
        }
        return SMS_OPERATIONS_PATH + "?operation_id=" + operationId;
    }

    String smsSyncTarget(String cursor, int limit) {
        if (limit < 1 || limit > SMS_SYNC_MAXIMUM_LIMIT) {
            throw new IllegalArgumentException("SMS sync limit must be between 1 and 10");
        }
        StringBuilder target = new StringBuilder(SMS_SYNC_PATH).append('?');
        if (cursor != null && !cursor.isEmpty()) {
            if (!cursor.equals(cursor.trim()) || cursor.length() > 256) {
                throw new IllegalArgumentException("SMS cursor is invalid");
            }
            target.append("cursor=").append(urlEncode(cursor)).append('&');
        }
        return target.append("limit=").append(limit).toString();
    }

    boolean permits(URI uri) {
        if (uri == null || !"https".equals(uri.getScheme()) || uri.getPort() != -1
                || uri.getRawUserInfo() != null || uri.getRawFragment() != null
                || !Objects.equals(originUri.getHost(), uri.getHost())) {
            return false;
        }
        String path = uri.getRawPath();
        if (ENROLLMENTS_PATH.equals(path) || CHALLENGES_PATH.equals(path)
                || SESSION_PATH.equals(path) || SMS_SEND_PATH.equals(path)) {
            return uri.getRawQuery() == null;
        }
        if (SMS_OPERATIONS_PATH.equals(path)) {
            String rawQuery = uri.getRawQuery();
            if (rawQuery == null || !rawQuery.startsWith("operation_id=")
                    || rawQuery.indexOf('&') >= 0) {
                return false;
            }
            String operationId = rawQuery.substring("operation_id=".length());
            return isOperationId(operationId)
                    && requestTarget(uri).equals(smsOperationTarget(operationId));
        }
        if (!SMS_SYNC_PATH.equals(path) || uri.getRawQuery() == null) {
            return false;
        }
        try {
            String rawQuery = uri.getRawQuery();
            String cursor = null;
            String rawLimit;
            if (rawQuery.startsWith("cursor=")) {
                int separator = rawQuery.indexOf("&limit=");
                if (separator < 8 || rawQuery.indexOf('&', separator + 1) >= 0) {
                    return false;
                }
                cursor = URLDecoder.decode(
                        rawQuery.substring("cursor=".length(), separator),
                        StandardCharsets.UTF_8.name());
                rawLimit = rawQuery.substring(separator + "&limit=".length());
            } else if (rawQuery.startsWith("limit=") && rawQuery.indexOf('&') < 0) {
                rawLimit = rawQuery.substring("limit=".length());
            } else {
                return false;
            }
            if (rawLimit.isEmpty() || !rawLimit.chars().allMatch(Character::isDigit)) {
                return false;
            }
            int limit = Integer.parseInt(rawLimit);
            return requestTarget(uri).equals(smsSyncTarget(cursor, limit));
        } catch (IllegalArgumentException | UnsupportedEncodingException error) {
            return false;
        }
    }

    static String requestTarget(URI uri) {
        String query = uri.getRawQuery();
        return uri.getRawPath() + (query == null ? "" : "?" + query);
    }

    private URI endpoint(String requestTarget) {
        URI result = URI.create(origin + requestTarget);
        if (!permits(result)) {
            throw new IllegalArgumentException("Native API target is not permitted");
        }
        return result;
    }

    static boolean isOperationId(String value) {
        if (value == null || !value.startsWith("nsm_") || value.indexOf('=') >= 0) {
            return false;
        }
        try {
            byte[] decoded = java.util.Base64.getUrlDecoder().decode(value.substring(4));
            return decoded.length == 16
                    && DeviceIdentity.base64Url(decoded).equals(value.substring(4));
        } catch (IllegalArgumentException error) {
            return false;
        }
    }

    private static String urlEncode(String value) {
        try {
            return URLEncoder.encode(value, StandardCharsets.UTF_8.name());
        } catch (UnsupportedEncodingException impossible) {
            throw new AssertionError(impossible);
        }
    }
}
