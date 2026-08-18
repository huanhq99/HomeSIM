package org.maccellular.gateway;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.annotation.SuppressLint;
import android.content.pm.PackageManager;
import android.os.Bundle;
import android.view.View;
import android.view.ViewGroup;
import android.view.WindowManager;
import android.webkit.CookieManager;
import android.webkit.ServiceWorkerController;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.FrameLayout;
import android.widget.TextView;
import android.widget.Toast;

public final class MainActivity extends Activity implements
        GatewayWebViewClient.Events,
        ForegroundAudioWebChromeClient.Host {
    private static final int RECORD_AUDIO_REQUEST = 1701;

    private FrameLayout webContainer;
    private View privacyShield;
    private TextView privacyMessage;
    private Button retryButton;
    private GatewayUrlPolicy gatewayPolicy;
    private WebView webView;
    private WebView trustedPage;
    private ForegroundAudioWebChromeClient foregroundAudioClient;
    private AlertDialog microphoneDialog;
    private boolean microphonePermissionRequestInFlight;
    private final PrivacyLifecycle privacyLifecycle = new PrivacyLifecycle();

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_SECURE);
        setContentView(R.layout.activity_main);

        webContainer = findViewById(R.id.web_container);
        privacyShield = findViewById(R.id.privacy_shield);
        privacyMessage = findViewById(R.id.privacy_message);
        retryButton = findViewById(R.id.retry_button);
        retryButton.setOnClickListener(view -> restartGateway());

        try {
            gatewayPolicy = new GatewayUrlPolicy(BuildConfig.GATEWAY_URL);
        } catch (IllegalArgumentException error) {
            showShield("此安装包没有有效的私有网关地址，请重新构建。", false);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        privacyLifecycle.onResume();
        if (privacyLifecycle.shouldCreateWebView(webView != null)) {
            startGateway();
        } else if (webView != null) {
            webView.onResume();
        }
    }

    @Override
    protected void onPause() {
        privacyLifecycle.onPause();
        showShield("内容已隐藏；返回前台后会重新同步。", false);
        disposeWebView();
        super.onPause();
    }

    @Override
    protected void onStop() {
        disposeWebView();
        super.onStop();
    }

    @Override
    protected void onDestroy() {
        disposeWebView();
        cancelMicrophoneConfirmation();
        super.onDestroy();
    }

    @Override
    public void onTrustedPageVisible(WebView source) {
        runOnUiThread(() -> {
            if (privacyLifecycle.isForeground() && source == webView) {
                trustedPage = source;
                privacyShield.setVisibility(View.GONE);
            }
        });
    }

    @Override
    public void onGatewayUnavailable(WebView source, String reason) {
        runOnUiThread(() -> {
            if (!privacyLifecycle.isForeground() || source != webView) {
                return;
            }
            showShield(reason, true);
            disposeWebView();
        });
    }

    private void startGateway() {
        if (gatewayPolicy == null || isFinishing() || webView != null) {
            return;
        }
        showShield("正在通过 Tailscale 连接家中 Mac…", false);
        WebView.setWebContentsDebuggingEnabled(false);

        WebView candidate = new WebView(this);
        candidate.setFilterTouchesWhenObscured(true);
        candidate.setImportantForAutofill(View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS);
        candidate.setLayoutParams(new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.MATCH_PARENT));
        harden(candidate);
        ServiceWorkerController workers = ServiceWorkerController.getInstance();
        workers.getServiceWorkerWebSettings().setAllowContentAccess(false);
        workers.getServiceWorkerWebSettings().setAllowFileAccess(false);
        workers.getServiceWorkerWebSettings().setCacheMode(WebSettings.LOAD_NO_CACHE);
        workers.setServiceWorkerClient(new GatewayServiceWorkerClient(gatewayPolicy));
        candidate.setWebViewClient(new GatewayWebViewClient(gatewayPolicy, this));
        if (BuildConfig.FOREGROUND_CALLS) {
            foregroundAudioClient = new ForegroundAudioWebChromeClient(candidate, gatewayPolicy, this);
            candidate.setWebChromeClient(foregroundAudioClient);
        } else {
            candidate.setWebChromeClient(new LockedWebChromeClient());
        }
        candidate.setDownloadListener((url, userAgent, contentDisposition, mimeType, contentLength) ->
                onGatewayUnavailable(candidate, "文件下载已禁用"));

        webView = candidate;
        webContainer.addView(candidate);
        candidate.loadUrl(gatewayPolicy.rootUrl());
    }

    private void restartGateway() {
        disposeWebView();
        startGateway();
    }

    @SuppressLint("SetJavaScriptEnabled")
    @SuppressWarnings("deprecation")
    private static void harden(WebView view) {
        // The existing PWA requires JavaScript. The surrounding WebView client
        // enforces one build-pinned HTTPS origin and exposes no Java bridge.
        WebSettings settings = view.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setDatabaseEnabled(false);
        settings.setAllowFileAccess(false);
        settings.setAllowContentAccess(false);
        settings.setAllowFileAccessFromFileURLs(false);
        settings.setAllowUniversalAccessFromFileURLs(false);
        settings.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        settings.setSafeBrowsingEnabled(true);
        settings.setJavaScriptCanOpenWindowsAutomatically(false);
        settings.setSupportMultipleWindows(false);
        settings.setGeolocationEnabled(false);
        settings.setMediaPlaybackRequiresUserGesture(true);
        settings.setCacheMode(WebSettings.LOAD_NO_CACHE);
        settings.setSaveFormData(false);
        settings.setBuiltInZoomControls(false);
        settings.setDisplayZoomControls(false);

        view.removeJavascriptInterface("searchBoxJavaBridge_");
        view.removeJavascriptInterface("accessibility");
        view.removeJavascriptInterface("accessibilityTraversal");

        CookieManager cookies = CookieManager.getInstance();
        cookies.setAcceptCookie(false);
        cookies.setAcceptThirdPartyCookies(view, false);
    }

    private void showShield(String message, boolean retry) {
        privacyMessage.setText(message);
        retryButton.setVisibility(retry ? View.VISIBLE : View.GONE);
        privacyShield.setVisibility(View.VISIBLE);
    }

    private void disposeWebView() {
        WebView current = webView;
        webView = null;
        trustedPage = null;
        ForegroundAudioWebChromeClient audioClient = foregroundAudioClient;
        foregroundAudioClient = null;
        if (audioClient != null) audioClient.close();
        if (current == null) {
            return;
        }
        webContainer.removeView(current);
        current.stopLoading();
        current.setWebChromeClient(null);
        current.setWebViewClient(new WebViewClient());
        current.clearHistory();
        current.clearFormData();
        current.removeAllViews();
        current.destroy();
    }

    @Override
    public boolean isAudioContextCurrent(WebView source) {
        return BuildConfig.FOREGROUND_CALLS && privacyLifecycle.isForeground() &&
                !microphonePermissionRequestInFlight &&
                source != null && source == webView && source == trustedPage &&
                gatewayPolicy != null && gatewayPolicy.isAllowedNavigation(source.getUrl());
    }

    @Override
    public boolean hasRecordAudioPermission() {
        return checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED;
    }

    @Override
    public void requestRecordAudioPermission() {
        if (!BuildConfig.FOREGROUND_CALLS || microphonePermissionRequestInFlight ||
                !privacyLifecycle.isForeground()) {
            return;
        }
        microphonePermissionRequestInFlight = true;
        requestPermissions(new String[]{Manifest.permission.RECORD_AUDIO}, RECORD_AUDIO_REQUEST);
    }

    @Override
    public void confirmOneTimeMicrophoneAccess(
            ForegroundAudioWebChromeClient.Confirmation confirmation) {
        cancelMicrophoneConfirmation();
        if (!BuildConfig.FOREGROUND_CALLS || !privacyLifecycle.isForeground() || isFinishing()) {
            confirmation.complete(false);
            return;
        }
        AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle(R.string.microphone_confirmation_title)
                .setMessage(R.string.microphone_confirmation_message)
                .setPositiveButton(R.string.microphone_allow_once, (ignored, which) -> {
                    microphoneDialog = null;
                    confirmation.complete(true);
                })
                .setNegativeButton(android.R.string.cancel, (ignored, which) -> {
                    microphoneDialog = null;
                    confirmation.complete(false);
                })
                .create();
        dialog.setOnCancelListener(ignored -> {
            microphoneDialog = null;
            confirmation.complete(false);
        });
        microphoneDialog = dialog;
        dialog.show();
        if (dialog.getWindow() != null) {
            dialog.getWindow().getDecorView().setFilterTouchesWhenObscured(true);
        }
    }

    @Override
    public void cancelMicrophoneConfirmation() {
        AlertDialog dialog = microphoneDialog;
        microphoneDialog = null;
        if (dialog != null) dialog.cancel();
    }

    @Override
    public void onRequestPermissionsResult(
            int requestCode,
            String[] permissions,
            int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode != RECORD_AUDIO_REQUEST) {
            return;
        }
        microphonePermissionRequestInFlight = false;
        boolean granted = permissions.length == 1 &&
                Manifest.permission.RECORD_AUDIO.equals(permissions[0]) &&
                grantResults.length == 1 &&
                grantResults[0] == PackageManager.PERMISSION_GRANTED;
        Toast.makeText(
                this,
                granted ? R.string.microphone_granted_retry : R.string.microphone_denied,
                Toast.LENGTH_LONG).show();
    }
}
