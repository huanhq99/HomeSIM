package org.maccellular.gateway.nativeclient;

public final class NativeGatewayException extends Exception {
    private final boolean enrollmentOutcomeUnknown;
    private final boolean smsOutcomeUnknown;

    NativeGatewayException(String message) {
        this(message, null, false, false);
    }

    NativeGatewayException(String message, Throwable cause) {
        this(message, cause, false, false);
    }

    NativeGatewayException(String message, Throwable cause, boolean enrollmentOutcomeUnknown) {
        this(message, cause, enrollmentOutcomeUnknown, false);
    }

    NativeGatewayException(
            String message,
            Throwable cause,
            boolean enrollmentOutcomeUnknown,
            boolean smsOutcomeUnknown) {
        super(message, cause);
        this.enrollmentOutcomeUnknown = enrollmentOutcomeUnknown;
        this.smsOutcomeUnknown = smsOutcomeUnknown;
    }

    public boolean isEnrollmentOutcomeUnknown() {
        return enrollmentOutcomeUnknown;
    }

    public boolean isSmsOutcomeUnknown() {
        return smsOutcomeUnknown;
    }
}
