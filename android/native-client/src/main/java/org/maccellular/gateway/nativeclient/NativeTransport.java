package org.maccellular.gateway.nativeclient;

import java.io.IOException;

interface NativeTransport {
    NativeHttpResponse execute(NativeHttpRequest request) throws IOException;
}
