package org.maccellular.gateway.nativeclient;

final class NativeHttpResponse {
    final int statusCode;
    final String contentType;
    final byte[] body;

    NativeHttpResponse(int statusCode, String contentType, byte[] body) {
        this.statusCode = statusCode;
        this.contentType = contentType == null ? "" : contentType;
        this.body = body.clone();
    }
}
