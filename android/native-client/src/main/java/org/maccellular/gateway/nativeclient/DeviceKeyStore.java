package org.maccellular.gateway.nativeclient;

import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;

import java.security.GeneralSecurityException;
import java.security.KeyPairGenerator;
import java.security.KeyStore;
import java.security.PrivateKey;
import java.security.cert.Certificate;
import java.security.spec.ECGenParameterSpec;

/** Owns one non-exportable AndroidKeyStore P-256 signing key. */
public final class DeviceKeyStore {
    private static final String PROVIDER = "AndroidKeyStore";
    private static final String ALIAS = "maccellular_native_device_identity_v1";

    public DeviceIdentity loadExisting() throws GeneralSecurityException {
        KeyStore store = loadStore();
        if (!store.containsAlias(ALIAS)) {
            return null;
        }
        return identityFrom(store);
    }

    public DeviceIdentity loadOrCreate() throws GeneralSecurityException {
        KeyStore store = loadStore();
        if (!store.containsAlias(ALIAS)) {
            KeyPairGenerator generator = KeyPairGenerator.getInstance(
                    KeyProperties.KEY_ALGORITHM_EC, PROVIDER);
            generator.initialize(new KeyGenParameterSpec.Builder(
                            ALIAS, KeyProperties.PURPOSE_SIGN)
                    .setAlgorithmParameterSpec(new ECGenParameterSpec("secp256r1"))
                    .setDigests(KeyProperties.DIGEST_SHA256)
                    .setUserAuthenticationRequired(false)
                    .build());
            generator.generateKeyPair();
            store = loadStore();
        }
        return identityFrom(store);
    }

    private static KeyStore loadStore() throws GeneralSecurityException {
        KeyStore store = KeyStore.getInstance(PROVIDER);
        try {
            store.load(null);
        } catch (java.io.IOException impossible) {
            throw new GeneralSecurityException("Unable to load AndroidKeyStore", impossible);
        }
        return store;
    }

    private static DeviceIdentity identityFrom(KeyStore store) throws GeneralSecurityException {
        java.security.Key key = store.getKey(ALIAS, null);
        Certificate certificate = store.getCertificate(ALIAS);
        if (!(key instanceof PrivateKey) || certificate == null) {
            throw new GeneralSecurityException("AndroidKeyStore identity is incomplete");
        }
        PrivateKey privateKey = (PrivateKey) key;
        if (privateKey.getEncoded() != null || privateKey.getFormat() != null) {
            throw new GeneralSecurityException("AndroidKeyStore private key is unexpectedly exportable");
        }
        return new DeviceIdentity(privateKey, certificate.getPublicKey());
    }
}
