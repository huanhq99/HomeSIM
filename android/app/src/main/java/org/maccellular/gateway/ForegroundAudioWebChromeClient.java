package org.maccellular.gateway;

import android.net.Uri;
import android.os.Handler;
import android.os.Looper;
import android.os.Message;
import android.webkit.GeolocationPermissions;
import android.webkit.PermissionRequest;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebView;

/** Grants exactly one foreground microphone resource after a one-time native confirmation. */
final class ForegroundAudioWebChromeClient extends WebChromeClient {
    private static final long CONFIRMATION_TIMEOUT_MILLISECONDS = 15_000L;

    interface Confirmation {
        void complete(boolean allow);
    }

    interface Host {
        boolean isAudioContextCurrent(WebView source);

        boolean hasRecordAudioPermission();

        void requestRecordAudioPermission();

        void confirmOneTimeMicrophoneAccess(Confirmation confirmation);

        void cancelMicrophoneConfirmation();
    }

    private final WebView source;
    private final Host host;
    private final ForegroundAudioPermissionPolicy policy;
    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private final Runnable confirmationTimeout;
    private PermissionRequest pending;
    private boolean closed;

    ForegroundAudioWebChromeClient(WebView source, GatewayUrlPolicy gatewayPolicy, Host host) {
        this.source = source;
        this.host = host;
        this.policy = new ForegroundAudioPermissionPolicy(gatewayPolicy);
        this.confirmationTimeout = () -> {
            PermissionRequest request = pending;
            if (request != null) finishPending(request, false);
            this.host.cancelMicrophoneConfirmation();
        };
    }

    @Override
    public void onPermissionRequest(PermissionRequest request) {
        if (request == null || closed) {
            if (request != null) request.deny();
            return;
        }
        ForegroundAudioPermissionPolicy.Decision decision = policy.evaluate(
                BuildConfig.FOREGROUND_CALLS,
                host.isAudioContextCurrent(source),
                host.hasRecordAudioPermission(),
                pending != null,
                request.getOrigin() == null ? "" : request.getOrigin().toString(),
                request.getResources());
        if (decision == ForegroundAudioPermissionPolicy.Decision.DENY) {
            request.deny();
            return;
        }
        if (decision == ForegroundAudioPermissionPolicy.Decision.REQUEST_ANDROID_PERMISSION) {
            request.deny();
            host.requestRecordAudioPermission();
            return;
        }

        pending = request;
        mainHandler.postDelayed(confirmationTimeout, CONFIRMATION_TIMEOUT_MILLISECONDS);
        host.confirmOneTimeMicrophoneAccess(allow -> finishPending(request, allow));
    }

    @Override
    public void onPermissionRequestCanceled(PermissionRequest request) {
        if (request == pending) {
            pending = null;
            mainHandler.removeCallbacks(confirmationTimeout);
            host.cancelMicrophoneConfirmation();
        }
    }

    void close() {
        closed = true;
        PermissionRequest request = pending;
        pending = null;
        mainHandler.removeCallbacks(confirmationTimeout);
        host.cancelMicrophoneConfirmation();
        if (request != null) request.deny();
    }

    private void finishPending(PermissionRequest expected, boolean allow) {
        if (closed || pending != expected) {
            return;
        }
        pending = null;
        mainHandler.removeCallbacks(confirmationTimeout);
        ForegroundAudioPermissionPolicy.Decision current = policy.evaluate(
                BuildConfig.FOREGROUND_CALLS,
                host.isAudioContextCurrent(source),
                host.hasRecordAudioPermission(),
                false,
                expected.getOrigin() == null ? "" : expected.getOrigin().toString(),
                expected.getResources());
        if (allow && current == ForegroundAudioPermissionPolicy.Decision.REQUIRE_ONE_TIME_CONFIRMATION) {
            expected.grant(new String[]{PermissionRequest.RESOURCE_AUDIO_CAPTURE});
        } else {
            expected.deny();
        }
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
