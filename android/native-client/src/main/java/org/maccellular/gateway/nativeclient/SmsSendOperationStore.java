package org.maccellular.gateway.nativeclient;

import android.content.Context;
import android.util.AtomicFile;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;

/** Atomic no-backup storage for one SMS guard without recipient, message, or body hash. */
final class SmsSendOperationStore {
    private static final int MAX_RECORD_BYTES = 16 << 10;

    private final AtomicFile file;

    SmsSendOperationStore(Context context) {
        File root = context.getNoBackupFilesDir();
        file = new AtomicFile(new File(root, "native-sms-operation-v1.json"));
    }

    synchronized SmsSendOperation load() throws IOException {
        if (!file.getBaseFile().exists()) {
            return null;
        }
        try (FileInputStream input = file.openRead();
             ByteArrayOutputStream output = new ByteArrayOutputStream()) {
            byte[] buffer = new byte[1024];
            int total = 0;
            int read;
            while ((read = input.read(buffer)) != -1) {
                total += read;
                if (total > MAX_RECORD_BYTES) {
                    throw new IOException("SMS operation record is oversized");
                }
                output.write(buffer, 0, read);
            }
            try {
                return SmsSendOperation.decode(output.toByteArray());
            } catch (IllegalArgumentException error) {
                throw new IOException("SMS operation record is corrupt", error);
            }
        }
    }

    synchronized void write(SmsSendOperation operation) throws IOException {
        byte[] encoded = operation.encode();
        if (encoded.length == 0 || encoded.length > MAX_RECORD_BYTES) {
            throw new IOException("SMS operation record is oversized");
        }
        FileOutputStream output = null;
        try {
            output = file.startWrite();
            output.write(encoded);
            file.finishWrite(output);
            output = null;
        } catch (IOException | RuntimeException error) {
            if (output != null) {
                file.failWrite(output);
            }
            if (error instanceof IOException) {
                throw (IOException) error;
            }
            throw new IOException("Unable to persist SMS operation", error);
        }
    }

    synchronized void clearExpected(
            String operationId,
            SmsSendOperation.State expectedState) throws IOException {
        if (expectedState != SmsSendOperation.State.PREPARED
                && expectedState != SmsSendOperation.State.NOT_FOUND) {
            throw new IOException("SMS operation state cannot be explicitly closed");
        }
        SmsSendOperation current = load();
        if (current == null || !current.operationId().equals(operationId)
                || current.state() != expectedState) {
            throw new IOException("SMS operation changed before explicit close");
        }
        file.delete();
        if (file.getBaseFile().exists()) {
            throw new IOException("Unable to remove the explicitly closed SMS operation");
        }
    }
}
