package org.maccellular.gateway.nativeclient;

import org.json.JSONObject;
import org.junit.Test;

import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.Arrays;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertThrows;
import static org.junit.Assert.assertTrue;

public final class SmsSendOperationTest {
    private static final String OPERATION_ID = "nsm_AAAAAAAAAAAAAAAAAAAAAA";
    private static final String DEVICE_ID = "dev_" + DeviceIdentity.base64Url(new byte[32]);
    private static final Instant NOW = Instant.parse("2026-08-14T08:00:00Z");
    private static final Instant LATER = Instant.parse("2026-08-14T08:01:00Z");

    @Test
    public void unresolvedStatesAlwaysBlockAndRoundTripWithoutPii() throws Exception {
        SmsSendOperation prepared = SmsSendOperation.prepared(
                OPERATION_ID, DEVICE_ID, NOW);
        assertTrue(prepared.blocksNewSend());
        assertEquals(SmsSendOperation.State.PREPARED, prepared.state());

        SmsSendOperation unknown = prepared.markUnknown(
                409, "unknown_outcome", LATER);
        assertTrue(unknown.blocksNewSend());
        assertEquals(SmsSendOperation.State.UNKNOWN, unknown.state());
        assertEquals(409, unknown.httpStatus());
        assertEquals("unknown_outcome", unknown.resultCode());

        byte[] encoded = unknown.encode();
        String json = new String(encoded, StandardCharsets.UTF_8);
        assertFalse(json.contains("synthetic-recipient"));
        assertFalse(json.contains("synthetic message"));
        assertFalse(json.contains("\"phone\""));
        assertFalse(json.contains("\"message\""));
        assertFalse(json.contains("\"signature\""));
        assertFalse(json.contains("\"challenge"));
        assertFalse(json.contains("body_sha256"));
        SmsSendOperation decoded = SmsSendOperation.decode(encoded);
        assertEquals(unknown.operationId(), decoded.operationId());
        assertEquals(unknown.deviceId(), decoded.deviceId());
        assertEquals(unknown.state(), decoded.state());
        assertEquals(unknown.httpStatus(), decoded.httpStatus());
        assertEquals(unknown.resultCode(), decoded.resultCode());

        SmsSendOperation notFound = unknown.markNotFound(LATER);
        assertTrue(notFound.blocksNewSend());
        assertEquals(SmsSendOperation.State.NOT_FOUND, notFound.state());
        assertEquals("not_found", notFound.resultCode());
        assertEquals(SmsSendOperation.State.NOT_FOUND,
                SmsSendOperation.decode(notFound.encode()).state());
    }

    @Test
    public void onlyExplicitlySafeStatusCodePairsCanBecomeTerminal() {
        SmsSendOperation prepared = SmsSendOperation.prepared(
                OPERATION_ID, DEVICE_ID, NOW);
        SmsSendOperation submitted = prepared.markTerminal(200, "completed", LATER);
        assertFalse(submitted.blocksNewSend());
        assertEquals(SmsSendOperation.State.TERMINAL, submitted.state());

        assertFalse(SmsSendOperation.prepared(
                OPERATION_ID, DEVICE_ID, NOW)
                .markTerminal(400, "invalid_request", LATER)
                .blocksNewSend());
        assertFalse(SmsSendOperation.prepared(
                OPERATION_ID, DEVICE_ID, NOW)
                .markTerminal(503, "service_unavailable", LATER)
                .blocksNewSend());
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(0, "not_found_confirmed", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(403, "device_proof_invalid", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(415, "json_required", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(503, "operation_ledger_unavailable", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(202, "completed_audit_unavailable", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(409, "unknown_outcome", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(409, "conflict", LATER));
        assertThrows(IllegalArgumentException.class,
                () -> prepared.markTerminal(200, "arbitrary_code", LATER));
        assertThrows(IllegalStateException.class,
                () -> submitted.markUnknown(LATER));
    }

    @Test
    public void corruptMissingUnknownAndTrailingJournalDataFailClosed() throws Exception {
        SmsSendOperation prepared = SmsSendOperation.prepared(
                OPERATION_ID, DEVICE_ID, NOW);
        JSONObject valid = new JSONObject(new String(
                prepared.encode(), StandardCharsets.UTF_8));

        JSONObject missing = new JSONObject(valid.toString());
        missing.remove("device_id");
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(
                        missing.toString().getBytes(StandardCharsets.UTF_8)));

        JSONObject legacyBodyHash = new JSONObject(valid.toString())
                .put("body_sha256", "0".repeat(64));
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(
                        legacyBodyHash.toString().getBytes(StandardCharsets.UTF_8)));

        JSONObject unknown = new JSONObject(valid.toString()).put("phone", "secret");
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(
                        unknown.toString().getBytes(StandardCharsets.UTF_8)));

        JSONObject preparedWithResult = new JSONObject(valid.toString())
                .put("http_status", 0)
                .put("code", "");
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(
                        preparedWithResult.toString().getBytes(StandardCharsets.UTF_8)));

        byte[] trailing = (valid + " {}").getBytes(StandardCharsets.UTF_8);
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(trailing));

        byte[] malformedUtf8 = {(byte) 0xc3, (byte) 0x28};
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(malformedUtf8));

        byte[] oversized = new byte[(16 << 10) + 1];
        Arrays.fill(oversized, (byte) 'x');
        assertThrows(IllegalArgumentException.class,
                () -> SmsSendOperation.decode(oversized));
    }
}
