package org.maccellular.gateway.nativeclient;

import org.junit.Test;

import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.MessageDigest;
import java.security.PrivateKey;
import java.security.Signature;
import java.security.spec.ECGenParameterSpec;
import java.util.Base64;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertThrows;
import static org.junit.Assert.assertTrue;

public final class DeviceIdentityTest {
    @Test
    public void derivesStableIdFromSpkiAndSignsExactServerUtf8Bytes() throws Exception {
        KeyPair pair = newP256KeyPair();
        DeviceIdentity identity = new DeviceIdentity(pair.getPrivate(), pair.getPublic());
        byte[] spki = pair.getPublic().getEncoded();
        String expectedId = "dev_" + Base64.getUrlEncoder().withoutPadding().encodeToString(
                MessageDigest.getInstance("SHA-256").digest(spki));

        assertEquals(expectedId, identity.deviceId());
        assertEquals(Base64.getUrlEncoder().withoutPadding().encodeToString(spki),
                identity.publicKeySpki());

        String serverInput = "server-issued\n签名原文" + (char) 0 + "tail";
        byte[] signatureBytes = Base64.getUrlDecoder().decode(
                identity.signServerInput(serverInput));
        Signature verifier = Signature.getInstance("SHA256withECDSA");
        verifier.initVerify(pair.getPublic());
        verifier.update(serverInput.getBytes(StandardCharsets.UTF_8));
        assertTrue(verifier.verify(signatureBytes));

        verifier.initVerify(pair.getPublic());
        verifier.update((serverInput + "\n").getBytes(StandardCharsets.UTF_8));
        assertFalse(verifier.verify(signatureBytes));
    }

    @Test
    public void publicApiNeverReturnsPrivateKey() {
        for (Method method : DeviceIdentity.class.getMethods()) {
            assertFalse("Private key leaked by " + method,
                    PrivateKey.class.isAssignableFrom(method.getReturnType()));
        }
    }

    @Test
    public void rejectsWrongCurveAndEmptySigningInput() throws Exception {
        KeyPairGenerator generator = KeyPairGenerator.getInstance("EC");
        generator.initialize(new ECGenParameterSpec("secp384r1"));
        KeyPair p384 = generator.generateKeyPair();
        assertThrows(GeneralSecurityException.class,
                () -> new DeviceIdentity(p384.getPrivate(), p384.getPublic()));

        KeyPair p256 = newP256KeyPair();
        DeviceIdentity identity = new DeviceIdentity(p256.getPrivate(), p256.getPublic());
        assertThrows(GeneralSecurityException.class, () -> identity.signServerInput(""));
    }

    private static KeyPair newP256KeyPair() throws GeneralSecurityException {
        KeyPairGenerator generator = KeyPairGenerator.getInstance("EC");
        generator.initialize(new ECGenParameterSpec("secp256r1"));
        return generator.generateKeyPair();
    }
}
