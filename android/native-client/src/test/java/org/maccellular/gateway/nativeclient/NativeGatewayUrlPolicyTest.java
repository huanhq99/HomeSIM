package org.maccellular.gateway.nativeclient;

import org.junit.Test;

import java.net.URI;
import java.util.Arrays;
import java.util.List;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertThrows;
import static org.junit.Assert.assertTrue;

public final class NativeGatewayUrlPolicyTest {
    private static final String ORIGIN = "https://maccellular.tail123.ts.net";

    @Test
    public void acceptsOnlyCanonicalTailscaleHttpsOrigin() {
        NativeGatewayUrlPolicy policy = new NativeGatewayUrlPolicy(ORIGIN);
        assertEquals(ORIGIN, policy.origin());

        List<String> rejected = Arrays.asList(
                "http://maccellular.tail123.ts.net",
                "https://maccellular.tail123.ts.net/",
                "https://maccellular.tail123.ts.net/remote/",
                "https://maccellular.tail123.ts.net:443",
                "https://user@maccellular.tail123.ts.net",
                "https://OTHER-phone.tail123.ts.net",
                "https://maccellular.tail123.ts.net?x=1",
                "https://maccellular.tail123.ts.net#fragment",
                "https://ts.net",
                "https://maccellular.example.com",
                " https://maccellular.tail123.ts.net");
        for (String value : rejected) {
            assertThrows(value, IllegalArgumentException.class,
                    () -> new NativeGatewayUrlPolicy(value));
        }
    }

    @Test
    public void buildsOnlyTheCanonicalNativeApiTargets() {
        NativeGatewayUrlPolicy policy = new NativeGatewayUrlPolicy(ORIGIN);
        String operationId = "nsm_AAAAAAAAAAAAAAAAAAAAAA";

        assertEquals(ORIGIN + NativeGatewayUrlPolicy.ENROLLMENTS_PATH,
                policy.enrollmentsUri().toString());
        assertEquals(ORIGIN + NativeGatewayUrlPolicy.CHALLENGES_PATH,
                policy.challengesUri().toString());
        assertEquals(ORIGIN + NativeGatewayUrlPolicy.SESSION_PATH,
                policy.sessionUri().toString());
        assertEquals("/api/native/v1/sms/sync?cursor=a%2Bb%2F%3D%3F&limit=10",
                policy.smsSyncTarget("a+b/=?", 10));
        assertEquals("/api/native/v1/sms/sync?limit=1",
                policy.smsSyncTarget(null, 1));
        assertEquals(ORIGIN + "/api/native/v1/sms/send",
                policy.smsSendUri().toString());
        assertEquals("/api/native/v1/sms/operations?operation_id=" + operationId,
                policy.smsOperationTarget(operationId));
        assertEquals(ORIGIN + "/api/native/v1/sms/operations?operation_id=" + operationId,
                policy.smsOperationUri(operationId).toString());
    }

    @Test
    public void transportPolicyRejectsOtherOriginsPathsAndQueries() {
        NativeGatewayUrlPolicy policy = new NativeGatewayUrlPolicy(ORIGIN);

        assertTrue(policy.permits(policy.sessionUri()));
        assertTrue(policy.permits(policy.smsSyncUri("cursor_value", 10)));
        assertTrue(policy.permits(policy.smsSendUri()));
        assertTrue(policy.permits(policy.smsOperationUri(
                "nsm_AAAAAAAAAAAAAAAAAAAAAA")));
        assertFalse(policy.permits(URI.create(
                "https://other.tail123.ts.net/api/native/v1/session")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/remote/v1/session")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/native/v1/session?x=1")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/native/v1/sms/sync?limit=10&cursor=x")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/native/v1/sms/sync?cursor=x&limit=10&extra=1")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/native/v1/sms/send?x=1")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/native/v1/sms/operations?x=1&operation_id=nsm_AAAAAAAAAAAAAAAAAAAAAA")));
        assertFalse(policy.permits(URI.create(
                ORIGIN + "/api/native/v1/sms/operations?operation_id=nsm_AAAAAAAAAAAAAAAAAAAAAA&x=1")));
    }

    @Test
    public void rejectsInvalidSmsCursorAndLimitBeforeNetwork() {
        NativeGatewayUrlPolicy policy = new NativeGatewayUrlPolicy(ORIGIN);

        assertThrows(IllegalArgumentException.class, () -> policy.smsSyncTarget(" x", 10));
        assertThrows(IllegalArgumentException.class, () -> policy.smsSyncTarget("x ", 10));
        assertThrows(IllegalArgumentException.class, () -> policy.smsSyncTarget("x".repeat(257), 10));
        assertThrows(IllegalArgumentException.class, () -> policy.smsSyncTarget(null, 0));
        assertThrows(IllegalArgumentException.class, () -> policy.smsSyncTarget(null, 11));
        assertThrows(IllegalArgumentException.class,
                () -> policy.smsOperationTarget("nsm_too-short"));
        assertThrows(IllegalArgumentException.class,
                () -> policy.smsOperationTarget(" nsm_AAAAAAAAAAAAAAAAAAAAAA"));
    }
}
