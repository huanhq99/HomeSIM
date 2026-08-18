package org.maccellular.gateway;

import static org.junit.Assert.assertEquals;

import android.webkit.PermissionRequest;

import org.junit.Test;

public final class ForegroundAudioPermissionPolicyTest {
    private final ForegroundAudioPermissionPolicy policy = new ForegroundAudioPermissionPolicy(
            new GatewayUrlPolicy("https://android-test.example-tailnet.ts.net/remote/"));

    @Test
    public void exactAudioRequestNeedsAndroidPermissionThenOneTimeConfirmation() {
        assertEquals(
                ForegroundAudioPermissionPolicy.Decision.REQUEST_ANDROID_PERMISSION,
                decide(true, true, false, false, exactOrigin(), audioOnly()));
        assertEquals(
                ForegroundAudioPermissionPolicy.Decision.REQUIRE_ONE_TIME_CONFIRMATION,
                decide(true, true, true, false, exactOrigin(), audioOnly()));
    }

    @Test
    public void viewerBackgroundAndConcurrentRequestsFailClosed() {
        assertDenied(false, true, true, false, exactOrigin(), audioOnly());
        assertDenied(true, false, true, false, exactOrigin(), audioOnly());
        assertDenied(true, true, true, true, exactOrigin(), audioOnly());
    }

    @Test
    public void wrongOriginAndExpandedResourcesFailClosed() {
        assertDenied(true, true, true, false,
                "https://other.example-tailnet.ts.net", audioOnly());
        assertDenied(true, true, true, false,
                "http://android-test.example-tailnet.ts.net", audioOnly());
        assertDenied(true, true, true, false,
                "https://android-test.example-tailnet.ts.net:8443", audioOnly());
        assertDenied(true, true, true, false, exactOrigin(), null);
        assertDenied(true, true, true, false, exactOrigin(), new String[0]);
        assertDenied(true, true, true, false, exactOrigin(), new String[]{
                PermissionRequest.RESOURCE_AUDIO_CAPTURE,
                PermissionRequest.RESOURCE_VIDEO_CAPTURE,
        });
        assertDenied(true, true, true, false, exactOrigin(), new String[]{
                PermissionRequest.RESOURCE_VIDEO_CAPTURE,
        });
    }

    private ForegroundAudioPermissionPolicy.Decision decide(
            boolean enabled,
            boolean trusted,
            boolean granted,
            boolean pending,
            String origin,
            String[] resources) {
        return policy.evaluate(enabled, trusted, granted, pending, origin, resources);
    }

    private void assertDenied(
            boolean enabled,
            boolean trusted,
            boolean granted,
            boolean pending,
            String origin,
            String[] resources) {
        assertEquals(
                ForegroundAudioPermissionPolicy.Decision.DENY,
                decide(enabled, trusted, granted, pending, origin, resources));
    }

    private static String exactOrigin() {
        return "https://android-test.example-tailnet.ts.net";
    }

    private static String[] audioOnly() {
        return new String[]{PermissionRequest.RESOURCE_AUDIO_CAPTURE};
    }
}
