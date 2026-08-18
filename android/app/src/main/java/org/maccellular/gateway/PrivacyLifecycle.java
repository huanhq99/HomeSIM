package org.maccellular.gateway;

/**
 * Tracks whether a sensitive WebView crossed an Activity pause boundary.
 * A resume after any pause must rebuild from the pinned gateway root rather
 * than reveal a possibly stale DOM behind the privacy shield.
 */
final class PrivacyLifecycle {
    private boolean foreground;

    void onPause() {
        foreground = false;
    }

    void onResume() {
        foreground = true;
    }

    boolean shouldCreateWebView(boolean hasWebView) {
        return foreground && !hasWebView;
    }

    boolean isForeground() {
        return foreground;
    }
}
