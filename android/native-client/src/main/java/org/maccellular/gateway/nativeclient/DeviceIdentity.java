package org.maccellular.gateway.nativeclient;

import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.AlgorithmParameters;
import java.security.MessageDigest;
import java.security.PrivateKey;
import java.security.PublicKey;
import java.security.Signature;
import java.security.interfaces.ECPublicKey;
import java.security.spec.ECFieldFp;
import java.security.spec.ECGenParameterSpec;
import java.security.spec.ECParameterSpec;
import java.util.Base64;
import java.util.Objects;

/** Public device identity plus signing operations; the private key is never exposed. */
public final class DeviceIdentity {
    public static final String ALGORITHM = "ES256";

    private static final int MAX_SIGNING_INPUT_BYTES = 16 * 1024;

    private final PrivateKey privateKey;
    private final ECPublicKey publicKey;
    private final String publicKeySpki;
    private final String deviceId;

    DeviceIdentity(PrivateKey privateKey, PublicKey publicKey) throws GeneralSecurityException {
        this.privateKey = Objects.requireNonNull(privateKey, "privateKey");
        Objects.requireNonNull(publicKey, "publicKey");
        if (!(publicKey instanceof ECPublicKey)) {
            throw new GeneralSecurityException("Device public key must be EC");
        }
        ECPublicKey ecPublicKey = (ECPublicKey) publicKey;
        if (!isP256(ecPublicKey.getParams())) {
            throw new GeneralSecurityException("Device public key must use a P-256 curve");
        }
        byte[] spki = ecPublicKey.getEncoded();
        if (spki == null || spki.length == 0) {
            throw new GeneralSecurityException("Device public key is not encodable as SPKI");
        }
        this.publicKey = ecPublicKey;
        publicKeySpki = base64Url(spki);
        deviceId = "dev_" + base64Url(MessageDigest.getInstance("SHA-256").digest(spki));
    }

    public String deviceId() {
        return deviceId;
    }

    public String publicKeySpki() {
        return publicKeySpki;
    }

    public PublicKey publicKey() {
        return publicKey;
    }

    public String signServerInput(String signingInput) throws GeneralSecurityException {
        Objects.requireNonNull(signingInput, "signingInput");
        byte[] bytes = signingInput.getBytes(StandardCharsets.UTF_8);
        if (bytes.length == 0 || bytes.length > MAX_SIGNING_INPUT_BYTES) {
            throw new GeneralSecurityException("Server signing input length is invalid");
        }
        Signature signer = Signature.getInstance("SHA256withECDSA");
        signer.initSign(privateKey);
        signer.update(bytes);
        return base64Url(signer.sign());
    }

    static String base64Url(byte[] value) {
        return Base64.getUrlEncoder().withoutPadding().encodeToString(value);
    }

    private static boolean isP256(ECParameterSpec candidate) throws GeneralSecurityException {
        if (candidate == null || !(candidate.getCurve().getField() instanceof ECFieldFp)) {
            return false;
        }
        AlgorithmParameters parameters = AlgorithmParameters.getInstance("EC");
        parameters.init(new ECGenParameterSpec("secp256r1"));
        ECParameterSpec expected = parameters.getParameterSpec(ECParameterSpec.class);
        ECFieldFp candidateField = (ECFieldFp) candidate.getCurve().getField();
        ECFieldFp expectedField = (ECFieldFp) expected.getCurve().getField();
        return candidateField.getP().equals(expectedField.getP())
                && candidate.getCurve().getA().equals(expected.getCurve().getA())
                && candidate.getCurve().getB().equals(expected.getCurve().getB())
                && candidate.getGenerator().equals(expected.getGenerator())
                && candidate.getOrder().equals(expected.getOrder())
                && candidate.getCofactor() == expected.getCofactor();
    }
}
