package org.maccellular.gateway.nativeclient;

import javax.net.ssl.HttpsURLConnection;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.InterruptedIOException;
import java.io.OutputStream;
import java.net.CookieHandler;
import java.net.HttpURLConnection;
import java.util.Map;

/** HTTPS-only transport with no redirect, cookie jar, custom CA, or hostname override. */
final class NativeHttpTransport implements NativeTransport {
    private static final int CONNECT_TIMEOUT_MILLIS = 10_000;
    private static final int READ_TIMEOUT_MILLIS = 15_000;
    // Ten maximally-sized SMS records plus JSON escaping and envelope overhead
    // fit comfortably while still keeping an explicit response ceiling.
    private static final int MAX_RESPONSE_BYTES = 4 * 1024 * 1024;

    private final NativeGatewayUrlPolicy urlPolicy;

    NativeHttpTransport(NativeGatewayUrlPolicy urlPolicy) {
        this.urlPolicy = urlPolicy;
    }

    @Override
    public NativeHttpResponse execute(NativeHttpRequest request) throws IOException {
        if (!urlPolicy.permits(request.uri)) {
            throw new SecurityException("Native request escaped the pinned gateway origin");
        }
        if (CookieHandler.getDefault() != null) {
            throw new SecurityException("A process-wide CookieHandler is not permitted");
        }
        if (Thread.currentThread().isInterrupted()) {
            throw new InterruptedIOException("Native request was cancelled");
        }

        HttpURLConnection raw = (HttpURLConnection) request.uri.toURL().openConnection();
        if (!(raw instanceof HttpsURLConnection)) {
            raw.disconnect();
            throw new SecurityException("Native request did not create an HTTPS connection");
        }
        HttpsURLConnection connection = (HttpsURLConnection) raw;
        try {
            connection.setInstanceFollowRedirects(false);
            connection.setUseCaches(false);
            connection.setConnectTimeout(CONNECT_TIMEOUT_MILLIS);
            connection.setReadTimeout(READ_TIMEOUT_MILLIS);
            connection.setRequestMethod(request.method);
            for (Map.Entry<String, String> header : request.headers.entrySet()) {
                connection.setRequestProperty(header.getKey(), header.getValue());
            }
            if (request.body.length != 0) {
                connection.setDoOutput(true);
                connection.setFixedLengthStreamingMode(request.body.length);
                try (OutputStream output = connection.getOutputStream()) {
                    output.write(request.body);
                }
            }

            int status = connection.getResponseCode();
            InputStream stream = status >= 400
                    ? connection.getErrorStream()
                    : connection.getInputStream();
            byte[] responseBody = stream == null ? new byte[0] : readBounded(stream);
            return new NativeHttpResponse(status, connection.getContentType(), responseBody);
        } finally {
            connection.disconnect();
        }
    }

    private static byte[] readBounded(InputStream stream) throws IOException {
        try (InputStream input = stream;
             ByteArrayOutputStream output = new ByteArrayOutputStream()) {
            byte[] buffer = new byte[8192];
            int total = 0;
            int read;
            while ((read = input.read(buffer)) != -1) {
                if (Thread.currentThread().isInterrupted()) {
                    throw new InterruptedIOException("Native request was cancelled");
                }
                total += read;
                if (total > MAX_RESPONSE_BYTES) {
                    throw new IOException("Native response exceeded the size limit");
                }
                output.write(buffer, 0, read);
            }
            return output.toByteArray();
        }
    }
}
