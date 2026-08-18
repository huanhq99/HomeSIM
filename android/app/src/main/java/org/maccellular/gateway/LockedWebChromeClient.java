package org.maccellular.gateway;

import android.net.Uri;
import android.os.Message;
import android.webkit.GeolocationPermissions;
import android.webkit.PermissionRequest;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebView;

/** Phase 1 deliberately grants no native permission to web content. */
final class LockedWebChromeClient extends WebChromeClient {
    @Override
    public void onPermissionRequest(PermissionRequest request) {
        request.deny();
    }

    @Override
    public void onGeolocationPermissionsShowPrompt(
            String origin,
            GeolocationPermissions.Callback callback) {
        callback.invoke(origin, false, false);
    }

    @Override
    public boolean onShowFileChooser(
            WebView webView,
            ValueCallback<Uri[]> filePathCallback,
            FileChooserParams fileChooserParams) {
        filePathCallback.onReceiveValue(null);
        return true;
    }

    @Override
    public boolean onCreateWindow(
            WebView view,
            boolean isDialog,
            boolean isUserGesture,
            Message resultMsg) {
        return false;
    }
}
