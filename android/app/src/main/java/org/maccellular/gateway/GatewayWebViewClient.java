package org.maccellular.gateway;

import android.graphics.Bitmap;
import android.net.http.SslError;
import android.webkit.SafeBrowsingResponse;
import android.webkit.SslErrorHandler;
import android.webkit.WebResourceError;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceResponse;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import java.io.ByteArrayInputStream;
import java.nio.charset.StandardCharsets;
import java.util.Collections;

final class GatewayWebViewClient extends WebViewClient {
    interface Events {
        void onTrustedPageVisible(WebView source);

        void onGatewayUnavailable(WebView source, String reason);
    }

    private final GatewayUrlPolicy policy;
    private final Events events;

    GatewayWebViewClient(GatewayUrlPolicy policy, Events events) {
        this.policy = policy;
        this.events = events;
    }

    @Override
    public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
        String target = request.getUrl().toString();
        if (request.isForMainFrame() && !policy.isAllowedNavigation(target)) {
            events.onGatewayUnavailable(view, "已阻止非网关页面");
            return true;
        }
        return false;
    }

    @Override
    public WebResourceResponse shouldInterceptRequest(WebView view, WebResourceRequest request) {
        if (policy.isAllowedResource(request.getUrl().toString())) {
            return null;
        }
        return blockedResponse();
    }

    static WebResourceResponse blockedResponse() {
        byte[] body = "blocked".getBytes(StandardCharsets.UTF_8);
        return new WebResourceResponse(
                "text/plain",
                "UTF-8",
                403,
                "Blocked by exact-origin policy",
                Collections.singletonMap("Cache-Control", "no-store"),
                new ByteArrayInputStream(body));
    }

    @Override
    public void onPageStarted(WebView view, String url, Bitmap favicon) {
        if (!policy.isAllowedNavigation(url)) {
            view.stopLoading();
            events.onGatewayUnavailable(view, "已阻止非网关导航");
        }
    }

    @Override
    public void onPageCommitVisible(WebView view, String url) {
        if (policy.isAllowedNavigation(url)) {
            events.onTrustedPageVisible(view);
        } else {
            events.onGatewayUnavailable(view, "网关地址校验失败");
        }
    }

    @Override
    public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
        handler.cancel();
        events.onGatewayUnavailable(view, "TLS 证书校验失败");
    }

    @Override
    public void onSafeBrowsingHit(
            WebView view,
            WebResourceRequest request,
            int threatType,
            SafeBrowsingResponse callback) {
        callback.backToSafety(false);
        events.onGatewayUnavailable(view, "安全浏览已阻止该页面");
    }

    @Override
    public void onReceivedError(WebView view, WebResourceRequest request, WebResourceError error) {
        if (request.isForMainFrame()) {
            events.onGatewayUnavailable(view, "无法连接家中 Mac；请检查 Tailscale 与 Serve");
        }
    }

    @Override
    public void onReceivedHttpError(
            WebView view,
            WebResourceRequest request,
            WebResourceResponse errorResponse) {
        if (request.isForMainFrame() && errorResponse.getStatusCode() >= 400) {
            events.onGatewayUnavailable(view, "网关拒绝访问；请检查身份与 App Cap");
        }
    }
}
