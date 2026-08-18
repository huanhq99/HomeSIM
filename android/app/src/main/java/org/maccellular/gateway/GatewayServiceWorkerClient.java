package org.maccellular.gateway;

import android.webkit.ServiceWorkerClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceResponse;

/** Process-wide Service Worker boundary that never retains an Activity. */
final class GatewayServiceWorkerClient extends ServiceWorkerClient {
    private final GatewayUrlPolicy policy;

    GatewayServiceWorkerClient(GatewayUrlPolicy policy) {
        this.policy = policy;
    }

    @Override
    public WebResourceResponse shouldInterceptRequest(WebResourceRequest request) {
        return policy.isAllowedResource(request.getUrl().toString())
                ? null
                : GatewayWebViewClient.blockedResponse();
    }
}
