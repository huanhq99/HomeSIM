package org.maccellular.gateway;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

public final class PrivacyLifecycleTest {
    @Test
    public void initialResumeCreatesTheFirstTrustedPage() {
        PrivacyLifecycle lifecycle = new PrivacyLifecycle();

        assertFalse(lifecycle.isForeground());
        lifecycle.onResume();
        assertTrue(lifecycle.shouldCreateWebView(false));
        assertFalse(lifecycle.shouldCreateWebView(true));
    }

    @Test
    public void pauseImmediatelyRevokesForegroundCallbacks() {
        PrivacyLifecycle lifecycle = new PrivacyLifecycle();
        lifecycle.onResume();
        lifecycle.onPause();

        assertFalse(lifecycle.isForeground());
        assertFalse(lifecycle.shouldCreateWebView(false));
    }

    @Test
    public void pauseStopResumeCreatesOneNewViewAtResume() {
        PrivacyLifecycle lifecycle = new PrivacyLifecycle();
        lifecycle.onResume();
        lifecycle.onPause();

        lifecycle.onResume();
        assertTrue(lifecycle.shouldCreateWebView(false));
        assertFalse(lifecycle.shouldCreateWebView(true));
    }
}
