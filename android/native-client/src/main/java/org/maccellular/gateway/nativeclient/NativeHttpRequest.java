package org.maccellular.gateway.nativeclient;

import java.net.URI;
import java.util.Collections;
import java.util.Arrays;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.Locale;
import java.util.Map;
import java.util.Objects;
import java.util.Set;

final class NativeHttpRequest {
    private static final Set<String> ALLOWED_HEADERS = Collections.unmodifiableSet(new HashSet<>(Arrays.asList(
            "accept",
            "content-type",
            "cache-control",
            "idempotency-key",
            "x-maccellular-device-id",
            "x-maccellular-challenge-id",
            "x-maccellular-signature")));

    final String method;
    final URI uri;
    final byte[] body;
    final Map<String, String> headers;

    NativeHttpRequest(String method, URI uri, byte[] body, Map<String, String> headers) {
        if (!"GET".equals(method) && !"POST".equals(method)) {
            throw new IllegalArgumentException("Native HTTP method is not permitted");
        }
        this.method = method;
        this.uri = Objects.requireNonNull(uri, "uri");
        this.body = Objects.requireNonNull(body, "body").clone();
        if ("GET".equals(method) && this.body.length != 0) {
            throw new IllegalArgumentException("Native GET requests cannot have a body");
        }
        LinkedHashMap<String, String> checked = new LinkedHashMap<>();
        HashSet<String> checkedNames = new HashSet<>();
        for (Map.Entry<String, String> entry : headers.entrySet()) {
            String name = Objects.requireNonNull(entry.getKey(), "header name");
            String value = Objects.requireNonNull(entry.getValue(), "header value");
            String lowerName = name.toLowerCase(Locale.ROOT);
            if (!ALLOWED_HEADERS.contains(lowerName) || lowerName.equals("cookie") || value.isEmpty()
                    || name.indexOf('\r') >= 0 || name.indexOf('\n') >= 0
                    || value.indexOf('\r') >= 0 || value.indexOf('\n') >= 0) {
                throw new IllegalArgumentException("Native HTTP header is not permitted");
            }
            if (lowerName.equals("idempotency-key")
                    && (!"POST".equals(method) || !NativeGatewayUrlPolicy.isOperationId(value))) {
                throw new IllegalArgumentException("Native idempotency header is invalid");
            }
            if (!checkedNames.add(lowerName)) {
                throw new IllegalArgumentException("Duplicate native HTTP header");
            }
            checked.put(name, value);
        }
        boolean smsSendTarget = NativeGatewayUrlPolicy.SMS_SEND_PATH.equals(uri.getRawPath())
                && uri.getRawQuery() == null;
        boolean hasIdempotencyKey = checkedNames.contains("idempotency-key");
        if (smsSendTarget != ("POST".equals(method) && hasIdempotencyKey)) {
            throw new IllegalArgumentException(
                    "Native SMS send requires POST with one idempotency header");
        }
        this.headers = Collections.unmodifiableMap(checked);
    }
}
