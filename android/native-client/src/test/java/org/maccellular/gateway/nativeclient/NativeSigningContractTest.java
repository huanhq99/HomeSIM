package org.maccellular.gateway.nativeclient;

import org.json.JSONArray;
import org.json.JSONObject;
import org.junit.Test;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.Signature;
import java.security.spec.ECGenParameterSpec;
import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.Collections;
import java.util.Deque;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertThrows;
import static org.junit.Assert.assertTrue;

public final class NativeSigningContractTest {
    private static final String ORIGIN = "https://unit-test.tail123.ts.net";
    private static final String NOW = "2026-08-14T08:00:00Z";
    private static final String LATER = "2026-08-14T08:01:00Z";
    private static final String EMPTY_SHA256 =
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

    @Test
    public void enrollmentUsesTwoExactPostsAndSignsServerInputOnce() throws Exception {
        Fixture fixture = new Fixture();
        String signingInput = "maccellular-enrollment-v1|server-owned|opaque";
        fixture.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("enrollment_id", "npe_AAAAAAAAAAAAAAAAAAAAAA")
                .put("expires_at", LATER)
                .put("signing_input", signingInput));
        fixture.transport.addJson(201, new JSONObject()
                .put("version", 1)
                .put("device_id", fixture.identity.deviceId())
                .put("algorithm", "ES256")
                .put("scopes", new JSONArray(Arrays.asList("sms.read", "status.read")))
                .put("enrolled_at", NOW));
        char[] secret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA".toCharArray();

        NativeGatewayClient.EnrollmentResult result = fixture.client.enroll(
                "enr_AAAAAAAAAAAAAAAAAAAAAA", secret);

        assertEquals(fixture.identity.deviceId(), result.deviceId());
        assertEquals(Arrays.asList("sms.read", "status.read"), result.scopes());
        assertArrayEquals(new char[43], secret);
        assertEquals(2, fixture.transport.requests.size());

        NativeHttpRequest prepare = fixture.transport.requests.get(0);
        assertEquals("POST", prepare.method);
        assertEquals(NativeGatewayUrlPolicy.ENROLLMENTS_PATH, prepare.uri.getPath());
        JSONObject prepareJson = new JSONObject(new String(prepare.body, StandardCharsets.UTF_8));
        assertEquals(5, prepareJson.length());
        assertEquals("prepare", prepareJson.getString("step"));
        assertEquals("enr_AAAAAAAAAAAAAAAAAAAAAA", prepareJson.getString("ticket_id"));
        assertEquals("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
                prepareJson.getString("ticket_secret"));
        assertEquals("ES256", prepareJson.getString("algorithm"));
        assertEquals(fixture.identity.publicKeySpki(), prepareJson.getString("public_key_spki"));
        assertFalse(prepare.headers.containsKey("Cookie"));

        NativeHttpRequest complete = fixture.transport.requests.get(1);
        JSONObject completeJson = new JSONObject(new String(complete.body, StandardCharsets.UTF_8));
        assertEquals(3, completeJson.length());
        assertEquals("complete", completeJson.getString("step"));
        assertEquals("npe_AAAAAAAAAAAAAAAAAAAAAA",
                completeJson.getString("enrollment_id"));
        verifySignature(fixture.pair, signingInput, completeJson.getString("signature"));
    }

    @Test
    public void enrollmentAcceptsAReadOnlyScopeSubset() throws Exception {
        Fixture fixture = new Fixture();
        fixture.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("enrollment_id", "npe_AAAAAAAAAAAAAAAAAAAAAA")
                .put("expires_at", LATER)
                .put("signing_input", "single-scope enrollment proof"));
        fixture.transport.addJson(201, new JSONObject()
                .put("version", 1)
                .put("device_id", fixture.identity.deviceId())
                .put("algorithm", "ES256")
                .put("scopes", new JSONArray(Collections.singletonList("sms.read")))
                .put("enrolled_at", NOW));

        NativeGatewayClient.EnrollmentResult result = fixture.client.enroll(
                "enr_AAAAAAAAAAAAAAAAAAAAAA",
                "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA".toCharArray());

        assertEquals(Collections.singletonList("sms.read"), result.scopes());
    }

    @Test
    public void enrollmentAcceptsExplicitSmsSendScopeInSortedOrder() throws Exception {
        Fixture fixture = new Fixture();
        fixture.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("enrollment_id", "npe_AAAAAAAAAAAAAAAAAAAAAA")
                .put("expires_at", LATER)
                .put("signing_input", "send enrollment proof"));
        fixture.transport.addJson(201, new JSONObject()
                .put("version", 1)
                .put("device_id", fixture.identity.deviceId())
                .put("algorithm", "ES256")
                .put("scopes", new JSONArray(
                        Arrays.asList("sms.read", "sms.send", "status.read")))
                .put("enrolled_at", NOW));

        NativeGatewayClient.EnrollmentResult result = fixture.client.enroll(
                "enr_AAAAAAAAAAAAAAAAAAAAAA",
                "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA".toCharArray());

        assertEquals(Arrays.asList("sms.read", "sms.send", "status.read"), result.scopes());
    }

    @Test
    public void enrollmentTransportLossIsUnknownAndNeverRetried() throws Exception {
        Fixture fixture = new Fixture();
        fixture.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("enrollment_id", "npe_AAAAAAAAAAAAAAAAAAAAAA")
                .put("expires_at", LATER)
                .put("signing_input", "one-use enrollment proof"));
        fixture.transport.failNext(new IOException("connection lost after send"));
        char[] secret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA".toCharArray();

        NativeGatewayException error = assertThrows(NativeGatewayException.class,
                () -> fixture.client.enroll("enr_AAAAAAAAAAAAAAAAAAAAAA", secret));

        assertTrue(error.isEnrollmentOutcomeUnknown());
        assertEquals(2, fixture.transport.requests.size());
        assertEquals(0, fixture.transport.remainingOutcomes());
        assertArrayEquals(new char[43], secret);
    }

    @Test
    public void signedGetBindsExactTargetAndSignsReturnedAsciiWithoutReconstruction()
            throws Exception {
        Fixture fixture = new Fixture();
        String signingInput = "opaque server bytes\nnot-a-client-template";
        fixture.transport.addJson(200, challenge("nch_AAAAAAAAAAAAAAAAAAAAAA", signingInput));
        fixture.transport.addJson(200, session(fixture.identity));

        assertEquals(session(fixture.identity).toString(), fixture.client.getSessionJson());
        assertEquals(2, fixture.transport.requests.size());

        NativeHttpRequest challengeRequest = fixture.transport.requests.get(0);
        JSONObject challengeJson = new JSONObject(
                new String(challengeRequest.body, StandardCharsets.UTF_8));
        assertEquals(5, challengeJson.length());
        assertEquals(1, challengeJson.getInt("version"));
        assertEquals(fixture.identity.deviceId(), challengeJson.getString("device_id"));
        assertEquals("GET", challengeJson.getString("method"));
        assertEquals(NativeGatewayUrlPolicy.SESSION_PATH,
                challengeJson.getString("request_target"));
        assertEquals(EMPTY_SHA256, challengeJson.getString("body_sha256"));

        NativeHttpRequest signedGet = fixture.transport.requests.get(1);
        assertEquals("GET", signedGet.method);
        assertArrayEquals(new byte[0], signedGet.body);
        assertEquals(fixture.identity.deviceId(),
                signedGet.headers.get(NativeGatewayClient.DEVICE_ID_HEADER));
        assertEquals("nch_AAAAAAAAAAAAAAAAAAAAAA",
                signedGet.headers.get(NativeGatewayClient.CHALLENGE_ID_HEADER));
        assertFalse(signedGet.headers.containsKey("Cookie"));
        assertFalse(signedGet.headers.containsKey("Origin"));
        assertFalse(signedGet.headers.containsKey("Sec-Fetch-Site"));
        assertFalse(signedGet.headers.containsKey("X-MacCellular-CSRF"));
        verifySignature(fixture.pair, signingInput,
                signedGet.headers.get(NativeGatewayClient.SIGNATURE_HEADER));
    }

    @Test
    public void smsSyncUsesCanonicalQueryAndRejectsUnknownResponseFields() throws Exception {
        Fixture fixture = new Fixture();
        fixture.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "sms server signing input"));
        fixture.transport.addJson(200, new JSONObject()
                .put("messages", new JSONArray())
                .put("cursor", "cursor_abcdefghijklmnop")
                .put("bootstrap", true)
                .put("has_more", false));

        fixture.client.syncSmsJson("cursor+/=?", 10);

        JSONObject challengeJson = new JSONObject(new String(
                fixture.transport.requests.get(0).body, StandardCharsets.UTF_8));
        String expectedTarget =
                "/api/native/v1/sms/sync?cursor=cursor%2B%2F%3D%3F&limit=10";
        assertEquals(expectedTarget, challengeJson.getString("request_target"));
        assertEquals(expectedTarget,
                NativeGatewayUrlPolicy.requestTarget(fixture.transport.requests.get(1).uri));

        Fixture rejected = new Fixture();
        rejected.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "another signing input"));
        rejected.transport.addJson(200, new JSONObject()
                .put("messages", new JSONArray())
                .put("cursor", "cursor_abcdefghijklmnop")
                .put("bootstrap", true)
                .put("has_more", false)
                .put("unexpected", true));
        assertThrows(NativeGatewayException.class,
                () -> rejected.client.syncSmsJson(null, 10));
    }

    @Test
    public void signedSmsPostHashesAndSendsTheSameBytesWithExactHeaders() throws Exception {
        Fixture fixture = new Fixture();
        String operationId = "nsm_AAAAAAAAAAAAAAAAAAAAAA";
        SmsSendRequest request = SmsSendRequest.createForOperation(
                operationId, "synthetic-recipient", "synthetic message");
        byte[] expectedBody = request.exactBody();
        String signingInput = "opaque native SMS signing input";
        fixture.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", signingInput));
        fixture.transport.addJson(200, new JSONObject()
                .put("ok", true)
                .put("code", "completed"));
        AtomicInteger gateCalls = new AtomicInteger();

        NativeGatewayClient.SmsSendResult result = fixture.client.sendSms(
                request, gateCalls::incrementAndGet);

        assertEquals(NativeGatewayClient.SmsOutcome.SUBMITTED, result.outcome());
        assertEquals(1, gateCalls.get());
        assertEquals(2, fixture.transport.requests.size());
        NativeHttpRequest challengeRequest = fixture.transport.requests.get(0);
        JSONObject challengeJson = new JSONObject(new String(
                challengeRequest.body, StandardCharsets.UTF_8));
        assertEquals(5, challengeJson.length());
        assertEquals("POST", challengeJson.getString("method"));
        assertEquals(NativeGatewayUrlPolicy.SMS_SEND_PATH,
                challengeJson.getString("request_target"));
        assertEquals(NativeGatewayClient.sha256Hex(expectedBody),
                challengeJson.getString("body_sha256"));

        NativeHttpRequest send = fixture.transport.requests.get(1);
        assertEquals("POST", send.method);
        assertArrayEquals(expectedBody, send.body);
        JSONObject sendJson = new JSONObject(new String(send.body, StandardCharsets.UTF_8));
        assertEquals(4, sendJson.length());
        assertEquals(1, sendJson.getInt("version"));
        assertEquals(operationId, sendJson.getString("operation_id"));
        assertEquals("synthetic-recipient", sendJson.getString("phone"));
        assertEquals("synthetic message", sendJson.getString("message"));
        assertEquals(operationId,
                send.headers.get(NativeGatewayClient.IDEMPOTENCY_HEADER));
        assertEquals(sendJson.getString("operation_id"),
                send.headers.get(NativeGatewayClient.IDEMPOTENCY_HEADER));
        assertEquals(fixture.identity.deviceId(),
                send.headers.get(NativeGatewayClient.DEVICE_ID_HEADER));
        assertFalse(send.headers.containsKey("Cookie"));
        assertFalse(send.headers.containsKey("Origin"));
        assertFalse(send.headers.containsKey("Sec-Fetch-Site"));
        assertFalse(send.headers.containsKey("X-MacCellular-CSRF"));
        verifySignature(fixture.pair, signingInput,
                send.headers.get(NativeGatewayClient.SIGNATURE_HEADER));
        request.close();
        assertThrows(IllegalStateException.class, request::exactBody);
    }

    @Test
    public void smsTransportLossAfterPostIsUnknownAndNeverRetried() throws Exception {
        Fixture fixture = new Fixture();
        SmsSendRequest request = SmsSendRequest.createForOperation(
                "nsm_AAAAAAAAAAAAAAAAAAAAAA", "synthetic-recipient", "one attempt");
        fixture.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "lost-response signing input"));
        fixture.transport.failNext(new IOException("lost after write"));
        AtomicInteger gateCalls = new AtomicInteger();

        NativeGatewayException error = assertThrows(
                NativeGatewayException.class,
                () -> fixture.client.sendSms(request, gateCalls::incrementAndGet));

        assertTrue(error.isSmsOutcomeUnknown());
        assertEquals(1, gateCalls.get());
        assertEquals(2, fixture.transport.requests.size());
        assertEquals(0, fixture.transport.remainingOutcomes());
    }

    @Test
    public void smsChallengeFailureNeverStartsTheFinalPost() throws Exception {
        Fixture fixture = new Fixture();
        SmsSendRequest request = SmsSendRequest.createForOperation(
                "nsm_AAAAAAAAAAAAAAAAAAAAAA", "synthetic-recipient", "preflight only");
        fixture.transport.addJson(403, new JSONObject()
                .put("error", "native request refused")
                .put("code", "challenge_denied"));
        AtomicInteger gateCalls = new AtomicInteger();

        NativeGatewayException error = assertThrows(
                NativeGatewayException.class,
                () -> fixture.client.sendSms(request, gateCalls::incrementAndGet));

        assertFalse(error.isSmsOutcomeUnknown());
        assertEquals(0, gateCalls.get());
        assertEquals(1, fixture.transport.requests.size());
    }

    @Test
    public void smsResponseMatrixFailsClosed() throws Exception {
        assertEquals(NativeGatewayClient.SmsOutcome.SUBMITTED,
                sendOutcome(200, new JSONObject().put("ok", true).put("code", "completed")));
        assertEquals(NativeGatewayClient.SmsOutcome.NOT_SUBMITTED,
                sendOutcome(400, new JSONObject().put("ok", false).put("code", "invalid_request")));
        assertEquals(NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED,
                sendOutcome(503, new JSONObject().put("ok", false).put("code", "service_unavailable")));
        assertEquals(NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED,
                sendOutcome(409, new JSONObject().put("ok", false).put("code", "unknown_outcome")));
        assertEquals(NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED,
                sendOutcome(409, new JSONObject().put("ok", false).put("code", "conflict")));
        assertEquals(NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED,
                sendOutcome(202, new JSONObject()
                        .put("ok", true)
                        .put("code", "completed_audit_unavailable")));
        assertEquals(NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED,
                sendOutcome(403, new JSONObject()
                        .put("error", "native request refused")
                        .put("code", "device_proof_invalid")));

        Fixture unsafe = preparedSendFixture();
        unsafe.transport.addJson(200, new JSONObject()
                .put("ok", true)
                .put("code", "unknown_outcome"));
        NativeGatewayException error = assertThrows(NativeGatewayException.class,
                () -> unsafe.client.sendSms(
                        SmsSendRequest.createForOperation(
                                "nsm_AAAAAAAAAAAAAAAAAAAAAA",
                                "synthetic-recipient", "unsafe response"),
                        () -> { }));
        assertTrue(error.isSmsOutcomeUnknown());
        assertEquals(2, unsafe.transport.requests.size());
    }

    @Test
    public void operationCompletedWithUnknownOutcomeStaysLockedAndGetHasNoIdempotencyHeader()
            throws Exception {
        Fixture fixture = new Fixture();
        String operationId = "nsm_AAAAAAAAAAAAAAAAAAAAAA";
        fixture.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "operation lookup signing input"));
        fixture.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("operation_id", operationId)
                .put("state", "completed")
                .put("http_status", 409)
                .put("code", "unknown_outcome"));

        NativeGatewayClient.SmsOperationResult result =
                fixture.client.getSmsOperation(operationId);

        assertEquals(NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED, result.outcome());
        assertEquals(409, result.httpStatus());
        assertEquals(2, fixture.transport.requests.size());
        NativeHttpRequest lookup = fixture.transport.requests.get(1);
        assertEquals("GET", lookup.method);
        assertArrayEquals(new byte[0], lookup.body);
        assertFalse(lookup.headers.containsKey(NativeGatewayClient.IDEMPOTENCY_HEADER));
        JSONObject challengeJson = new JSONObject(new String(
                fixture.transport.requests.get(0).body, StandardCharsets.UTF_8));
        assertEquals("/api/native/v1/sms/operations?operation_id=" + operationId,
                challengeJson.getString("request_target"));
        assertEquals(EMPTY_SHA256, challengeJson.getString("body_sha256"));
    }

    @Test
    public void operationLookupRequiresExactStateDependentFields() throws Exception {
        String operationId = "nsm_AAAAAAAAAAAAAAAAAAAAAA";
        Fixture notFound = new Fixture();
        notFound.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "not-found lookup"));
        notFound.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("operation_id", operationId)
                .put("state", "not_found"));
        assertEquals(NativeGatewayClient.SmsOutcome.NOT_FOUND_REQUIRES_CONFIRMATION,
                notFound.client.getSmsOperation(operationId).outcome());

        Fixture extra = new Fixture();
        extra.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "extra-field lookup"));
        extra.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("operation_id", operationId)
                .put("state", "not_found")
                .put("http_status", 404));
        assertThrows(NativeGatewayException.class,
                () -> extra.client.getSmsOperation(operationId));
    }

    @Test
    public void sessionSendReadinessRequiresExplicitActionAndBoolean() throws Exception {
        Fixture allowed = new Fixture();
        allowed.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "send session"));
        JSONObject allowedSession = session(allowed.identity)
                .put("actions", new JSONArray(
                        Arrays.asList("sms.read", "sms.send", "status.read")))
                .put("sms_send", true);
        allowed.transport.addJson(200, allowedSession);
        assertTrue(allowed.client.getSession().canSendSms());

        Fixture unavailable = new Fixture();
        unavailable.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "disabled send session"));
        JSONObject unavailableSession = session(unavailable.identity)
                .put("actions", new JSONArray(
                        Arrays.asList("sms.read", "sms.send", "status.read")))
                .put("sms_send", false);
        unavailable.transport.addJson(200, unavailableSession);
        assertFalse(unavailable.client.getSession().canSendSms());

        Fixture exceeds = new Fixture();
        exceeds.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "excess send session"));
        exceeds.transport.addJson(200, session(exceeds.identity).put("sms_send", true));
        assertThrows(NativeGatewayException.class, exceeds.client::getSession);
    }

    @Test
    public void redirectAndUnknownChallengeFieldsFailClosedBeforeAnyFollowUp() throws Exception {
        Fixture redirect = new Fixture();
        redirect.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "redirect signing input"));
        redirect.transport.add(new NativeHttpResponse(
                302, "text/html", "redirect".getBytes(StandardCharsets.UTF_8)));
        assertThrows(NativeGatewayException.class, redirect.client::getSessionJson);
        assertEquals(2, redirect.transport.requests.size());

        Fixture unknown = new Fixture();
        unknown.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "unknown-field signing input")
                .put("extra", true));
        assertThrows(NativeGatewayException.class, unknown.client::getSessionJson);
        assertEquals(1, unknown.transport.requests.size());
    }

    @Test
    public void requestObjectRejectsCookiesAndGetBodies() {
        NativeGatewayUrlPolicy policy = new NativeGatewayUrlPolicy(ORIGIN);
        assertThrows(IllegalArgumentException.class, () -> new NativeHttpRequest(
                "GET",
                policy.sessionUri(),
                new byte[0],
                Collections.singletonMap("Cookie", "secret=value")));
        assertThrows(IllegalArgumentException.class, () -> new NativeHttpRequest(
                "GET",
                policy.sessionUri(),
                new byte[]{1},
                Collections.emptyMap()));
        assertThrows(IllegalArgumentException.class, () -> new NativeHttpRequest(
                "GET",
                policy.smsOperationUri("nsm_AAAAAAAAAAAAAAAAAAAAAA"),
                new byte[0],
                Collections.singletonMap(
                        NativeGatewayClient.IDEMPOTENCY_HEADER,
                        "nsm_AAAAAAAAAAAAAAAAAAAAAA")));
        assertThrows(IllegalArgumentException.class, () -> new NativeHttpRequest(
                "POST",
                policy.smsSendUri(),
                new byte[]{1},
                Collections.singletonMap(
                        NativeGatewayClient.IDEMPOTENCY_HEADER,
                        "not-an-operation")));
        assertThrows(IllegalArgumentException.class, () -> new NativeHttpRequest(
                "POST",
                policy.smsSendUri(),
                new byte[]{1},
                Collections.emptyMap()));
        assertThrows(IllegalArgumentException.class, () -> new NativeHttpRequest(
                "POST",
                policy.challengesUri(),
                new byte[]{1},
                Collections.singletonMap(
                        NativeGatewayClient.IDEMPOTENCY_HEADER,
                        "nsm_AAAAAAAAAAAAAAAAAAAAAA")));
    }

    @Test
    public void strictAuthResponsesRejectMissingUnknownAndNonAsciiFields() throws Exception {
        Fixture unknownEnrollment = new Fixture();
        unknownEnrollment.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("enrollment_id", "npe_AAAAAAAAAAAAAAAAAAAAAA")
                .put("expires_at", LATER)
                .put("signing_input", "valid input")
                .put("unknown", true));
        assertThrows(NativeGatewayException.class, () -> unknownEnrollment.client.enroll(
                "enr_AAAAAAAAAAAAAAAAAAAAAA",
                "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA".toCharArray()));
        assertEquals(1, unknownEnrollment.transport.requests.size());

        Fixture missingChallenge = new Fixture();
        missingChallenge.transport.addJson(200, new JSONObject()
                .put("version", 1)
                .put("challenge_id", "nch_AAAAAAAAAAAAAAAAAAAAAA")
                .put("signing_input", "missing expiration"));
        assertThrows(NativeGatewayException.class, missingChallenge.client::getSessionJson);
        assertEquals(1, missingChallenge.transport.requests.size());

        Fixture nonAsciiChallenge = new Fixture();
        nonAsciiChallenge.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "server input 不允许"));
        assertThrows(NativeGatewayException.class, nonAsciiChallenge.client::getSessionJson);
        assertEquals(1, nonAsciiChallenge.transport.requests.size());

        Fixture missingSession = new Fixture();
        missingSession.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "session input"));
        JSONObject session = session(missingSession.identity);
        session.remove("sms_sync");
        missingSession.transport.addJson(200, session);
        assertThrows(NativeGatewayException.class, missingSession.client::getSessionJson);
        assertEquals(2, missingSession.transport.requests.size());
    }

    @Test
    public void lowercaseBodyHashMatchesTheLockedEmptyGetContract() throws Exception {
        assertEquals(EMPTY_SHA256, NativeGatewayClient.sha256Hex(new byte[0]));
    }

    private static JSONObject challenge(String challengeId, String signingInput) throws Exception {
        return new JSONObject()
                .put("version", 1)
                .put("challenge_id", challengeId)
                .put("expires_at", LATER)
                .put("signing_input", signingInput);
    }

    private static Fixture preparedSendFixture() throws Exception {
        Fixture fixture = new Fixture();
        fixture.transport.addJson(200, challenge(
                "nch_AAAAAAAAAAAAAAAAAAAAAA", "matrix signing input"));
        return fixture;
    }

    private static NativeGatewayClient.SmsOutcome sendOutcome(int status, JSONObject response)
            throws Exception {
        Fixture fixture = preparedSendFixture();
        fixture.transport.addJson(status, response);
        SmsSendRequest request = SmsSendRequest.createForOperation(
                "nsm_AAAAAAAAAAAAAAAAAAAAAA",
                "synthetic-recipient", "matrix message");
        return fixture.client.sendSms(request, () -> { }).outcome();
    }

    private static JSONObject session(DeviceIdentity identity) throws Exception {
        return new JSONObject()
                .put("version", 1)
                .put("device_id", identity.deviceId())
                .put("algorithm", "ES256")
                .put("actions", new JSONArray(Arrays.asList("sms.read", "status.read")))
                .put("transport", "tailscale-serve-device-proof")
                .put("sms_sync", true)
                .put("sms_send", false);
    }

    private static void verifySignature(KeyPair pair, String input, String encoded)
            throws Exception {
        Signature verifier = Signature.getInstance("SHA256withECDSA");
        verifier.initVerify(pair.getPublic());
        verifier.update(input.getBytes(StandardCharsets.UTF_8));
        assertTrue(verifier.verify(Base64.getUrlDecoder().decode(encoded)));
    }

    private static final class Fixture {
        final KeyPair pair;
        final DeviceIdentity identity;
        final FakeTransport transport = new FakeTransport();
        final NativeGatewayClient client;

        Fixture() throws Exception {
            KeyPairGenerator generator = KeyPairGenerator.getInstance("EC");
            generator.initialize(new ECGenParameterSpec("secp256r1"));
            pair = generator.generateKeyPair();
            identity = new DeviceIdentity(pair.getPrivate(), pair.getPublic());
            NativeGatewayUrlPolicy policy = new NativeGatewayUrlPolicy(ORIGIN);
            client = new NativeGatewayClient(policy, identity, transport);
        }
    }

    private static final class FakeTransport implements NativeTransport {
        final List<NativeHttpRequest> requests = new ArrayList<>();
        final Deque<Object> outcomes = new ArrayDeque<>();

        void addJson(int status, JSONObject body) {
            add(new NativeHttpResponse(
                    status,
                    "application/json; charset=utf-8",
                    body.toString().getBytes(StandardCharsets.UTF_8)));
        }

        void add(NativeHttpResponse response) {
            outcomes.addLast(response);
        }

        void failNext(IOException error) {
            outcomes.addLast(error);
        }

        int remainingOutcomes() {
            return outcomes.size();
        }

        @Override
        public NativeHttpResponse execute(NativeHttpRequest request) throws IOException {
            requests.add(request);
            Object outcome = outcomes.removeFirst();
            if (outcome instanceof IOException) {
                throw (IOException) outcome;
            }
            return (NativeHttpResponse) outcome;
        }
    }
}
