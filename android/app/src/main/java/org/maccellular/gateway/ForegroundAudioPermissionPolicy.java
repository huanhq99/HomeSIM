package org.maccellular.gateway;

import android.webkit.PermissionRequest;

/** Pure fail-closed decision policy for the opt-in foreground call flavor. */
final class ForegroundAudioPermissionPolicy {
    enum Decision {
        DENY,
        REQUEST_ANDROID_PERMISSION,
        REQUIRE_ONE_TIME_CONFIRMATION
    }

    private final GatewayUrlPolicy gatewayPolicy;

    ForegroundAudioPermissionPolicy(GatewayUrlPolicy gatewayPolicy) {
        this.gatewayPolicy = gatewayPolicy;
    }

    Decision evaluate(
            boolean capabilityEnabled,
            boolean currentTrustedPage,
            boolean androidPermissionGranted,
            boolean anotherRequestPending,
            String origin,
            String[] resources) {
        if (!capabilityEnabled || !currentTrustedPage || anotherRequestPending ||
                !gatewayPolicy.isAllowedPermissionOrigin(origin) ||
                resources == null || resources.length != 1 ||
                !PermissionRequest.RESOURCE_AUDIO_CAPTURE.equals(resources[0])) {
            return Decision.DENY;
        }
        return androidPermissionGranted
                ? Decision.REQUIRE_ONE_TIME_CONFIRMATION
                : Decision.REQUEST_ANDROID_PERMISSION;
    }
}
