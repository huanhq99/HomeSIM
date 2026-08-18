package org.maccellular.gateway;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertThrows;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

public final class GatewayUrlPolicyTest {
    private final GatewayUrlPolicy policy =
            new GatewayUrlPolicy("https://android-test.example-tailnet.ts.net/remote/");

    @Test
    public void acceptsOnlyExactRemoteNavigation() {
        assertTrue(policy.isAllowedNavigation("https://android-test.example-tailnet.ts.net/remote/"));
        assertFalse(policy.isAllowedNavigation("https://android-test.example-tailnet.ts.net/remote/app.js"));
        assertFalse(policy.isAllowedNavigation("https://android-test.example-tailnet.ts.net/remote/?next=1"));
        assertFalse(policy.isAllowedNavigation("https://other.example-tailnet.ts.net/remote/"));
    }

    @Test
    public void permitsOnlySameOriginRemoteAssetsAndMinimalApi() {
        assertTrue(policy.isAllowedResource("https://android-test.example-tailnet.ts.net/remote/app.js?v=1"));
        assertTrue(policy.isAllowedResource("https://android-test.example-tailnet.ts.net/api/remote/v1/session"));
        assertTrue(policy.isAllowedResource(
                "https://android-test.example-tailnet.ts.net/api/remote/v2/voice/snapshot"));
        assertTrue(policy.isAllowedResource(
                "https://android-test.example-tailnet.ts.net/api/remote/v2/voice/media/offers"));
        assertTrue(policy.isAllowedResource(
                "https://android-test.example-tailnet.ts.net/api/remote/v2/voice/calls/answer"));
        assertFalse(policy.isAllowedResource(
                "https://android-test.example-tailnet.ts.net/api/remote/v2/session"));
        assertFalse(policy.isAllowedResource(
                "https://android-test.example-tailnet.ts.net/api/remote/v2/voice"));
        assertFalse(policy.isAllowedResource(
                "https://android-test.example-tailnet.ts.net/api/remote/v2/voice-legacy/snapshot"));
        assertFalse(policy.isAllowedResource("https://android-test.example-tailnet.ts.net/api/status"));
        assertFalse(policy.isAllowedResource("https://android-test.example-tailnet.ts.net/remote/%2e%2e/api/status"));
        assertFalse(policy.isAllowedResource("https://android-test.example-tailnet.ts.net/remote/../api/status"));
        assertFalse(policy.isAllowedResource("data:text/html,unsafe"));
        assertFalse(policy.isAllowedResource("file:///etc/passwd"));
        assertFalse(policy.isAllowedResource("http://android-test.example-tailnet.ts.net/remote/"));
    }

    @Test
    public void permissionOriginMustBeTheExactPinnedHttpsOrigin() {
        assertTrue(policy.isAllowedPermissionOrigin(
                "https://android-test.example-tailnet.ts.net"));
        assertTrue(policy.isAllowedPermissionOrigin(
                "https://android-test.example-tailnet.ts.net/"));
        assertFalse(policy.isAllowedPermissionOrigin(
                "https://android-test.example-tailnet.ts.net/remote/"));
        assertFalse(policy.isAllowedPermissionOrigin(
                "https://other.example-tailnet.ts.net"));
        assertFalse(policy.isAllowedPermissionOrigin(
                "http://android-test.example-tailnet.ts.net"));
        assertFalse(policy.isAllowedPermissionOrigin(
                "https://android-test.example-tailnet.ts.net:8443"));
        assertFalse(policy.isAllowedPermissionOrigin(
                "https://user@android-test.example-tailnet.ts.net"));
    }

    @Test
    public void rejectsHostSuffixAndUrlConfusion() {
        assertThrows(IllegalArgumentException.class,
                () -> new GatewayUrlPolicy("https://example.ts.net.evil.test/remote/"));
        assertThrows(IllegalArgumentException.class,
                () -> new GatewayUrlPolicy("https://user@example.ts.net/remote/"));
        assertThrows(IllegalArgumentException.class,
                () -> new GatewayUrlPolicy("https://example.ts.net:8443/remote/"));
        assertThrows(IllegalArgumentException.class,
                () -> new GatewayUrlPolicy("https://example.ts.net/remote/?host=other"));
        assertThrows(IllegalArgumentException.class,
                () -> new GatewayUrlPolicy("http://example.ts.net/remote/"));
    }
}
