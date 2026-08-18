package org.maccellular.gateway.nativeclient;

import android.app.Activity;
import android.app.AlertDialog;
import android.os.Build;
import android.os.Bundle;
import android.view.View;
import android.view.WindowManager;
import android.widget.Button;
import android.widget.EditText;
import android.widget.TextView;

import java.io.IOException;
import java.security.GeneralSecurityException;
import java.time.Instant;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;

/** Foreground-only entry point for device trust, SMS sync, and confirmed SMS send. */
public final class EnrollmentActivity extends Activity {
    private final DeviceKeyStore deviceKeyStore = new DeviceKeyStore();
    private final ExecutorService executor = Executors.newSingleThreadExecutor();

    private NativeGatewayUrlPolicy urlPolicy;
    private SmsSendOperationStore smsOperationStore;
    private DeviceIdentity identity;
    private NativeGatewayClient.SessionResult sessionResult;
    private SmsSendOperation smsOperation;
    private AlertDialog activeDialog;
    private Future<?> activeTask;
    private EditText ticketId;
    private EditText ticketSecret;
    private EditText cursor;
    private EditText limit;
    private EditText smsPhone;
    private EditText smsMessage;
    private Button enroll;
    private Button loadSession;
    private Button syncSms;
    private Button sendSms;
    private Button checkSmsOperation;
    private Button closeSmsOperation;
    private TextView deviceIdentity;
    private TextView smsOperationGuard;
    private TextView status;
    private TextView result;
    private boolean journalHealthy = true;
    private boolean stopped;
    private long operationEpoch;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_SECURE);
        setContentView(R.layout.activity_enrollment);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            View root = findViewById(R.id.root);
            root.setImportantForContentCapture(
                    View.IMPORTANT_FOR_CONTENT_CAPTURE_NO_EXCLUDE_DESCENDANTS);
        }

        ticketId = findViewById(R.id.ticket_id);
        ticketSecret = findViewById(R.id.ticket_secret);
        cursor = findViewById(R.id.cursor);
        limit = findViewById(R.id.limit);
        smsPhone = findViewById(R.id.sms_phone);
        smsMessage = findViewById(R.id.sms_message);
        enroll = findViewById(R.id.enroll);
        loadSession = findViewById(R.id.load_session);
        syncSms = findViewById(R.id.sync_sms);
        sendSms = findViewById(R.id.send_sms);
        checkSmsOperation = findViewById(R.id.check_sms_operation);
        closeSmsOperation = findViewById(R.id.close_sms_operation);
        deviceIdentity = findViewById(R.id.device_identity);
        smsOperationGuard = findViewById(R.id.sms_operation_guard);
        status = findViewById(R.id.status);
        result = findViewById(R.id.result);
        smsOperationStore = new SmsSendOperationStore(this);
        protectSensitiveButtons(
                enroll,
                loadSession,
                syncSms,
                sendSms,
                checkSmsOperation,
                closeSmsOperation);

        try {
            urlPolicy = new NativeGatewayUrlPolicy(BuildConfig.GATEWAY_ORIGIN);
            ((TextView) findViewById(R.id.gateway)).setText(
                    getString(R.string.gateway_label, urlPolicy.origin()));
        } catch (IllegalArgumentException error) {
            status.setText(getString(R.string.request_failed, safeMessage(error)));
            setControlsEnabled(false);
            return;
        }

        enroll.setOnClickListener(ignored -> enrollDevice());
        loadSession.setOnClickListener(ignored -> loadSession());
        syncSms.setOnClickListener(ignored -> syncSms());
        sendSms.setOnClickListener(ignored -> confirmSmsSend());
        checkSmsOperation.setOnClickListener(ignored -> checkSmsOperation());
        closeSmsOperation.setOnClickListener(ignored -> confirmCloseSmsOperation());
        refreshExistingState();
    }

    @Override
    protected void onStart() {
        super.onStart();
        stopped = false;
        if (urlPolicy != null && (activeTask == null || activeTask.isDone())) {
            refreshExistingState();
        }
    }

    @Override
    protected void onStop() {
        stopped = true;
        sessionResult = null;
        dismissActiveDialog();
        cancelActiveTask();
        ticketSecret.setText(null);
        cursor.setText(null);
        smsPhone.setText(null);
        smsMessage.setText(null);
        result.setText(null);
        super.onStop();
    }

    @Override
    protected void onDestroy() {
        dismissActiveDialog();
        cancelActiveTask();
        executor.shutdownNow();
        super.onDestroy();
    }

    private void refreshExistingState() {
        submit(epoch -> {
            try {
                DeviceIdentity existing = deviceKeyStore.loadExisting();
                SmsSendOperation stored = smsOperationStore.load();
                if (stored != null
                        && (existing == null || !stored.deviceId().equals(existing.deviceId()))) {
                    throw new GeneralSecurityException(
                            "SMS operation belongs to a different device identity");
                }
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    identity = existing;
                    smsOperation = stored;
                    journalHealthy = true;
                    showIdentity();
                    finishWork(epoch, getString(R.string.idle), null);
                });
            } catch (GeneralSecurityException | IOException error) {
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    journalHealthy = false;
                    finishWork(epoch,
                            getString(R.string.sms_journal_failed, safeMessage(error)), null);
                });
            }
        });
    }

    private void enrollDevice() {
        String id = ticketId.getText().toString();
        char[] secret = ticketSecret.getText().toString().toCharArray();
        ticketSecret.setText(null);
        if (id.isEmpty() || secret.length == 0) {
            java.util.Arrays.fill(secret, '\0');
            status.setText(R.string.missing_ticket);
            return;
        }
        submit(epoch -> {
            DeviceIdentity candidate = null;
            try {
                candidate = deviceKeyStore.loadOrCreate();
                NativeGatewayClient.EnrollmentResult enrolled =
                        new NativeGatewayClient(urlPolicy, candidate).enroll(id, secret);
                DeviceIdentity enrolledIdentity = candidate;
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    identity = enrolledIdentity;
                    sessionResult = null;
                    ticketId.setText(null);
                    showIdentity();
                    finishWork(epoch, getString(
                            R.string.enrollment_success, enrolled.deviceId()), null);
                });
            } catch (Exception error) {
                java.util.Arrays.fill(secret, '\0');
                DeviceIdentity generatedIdentity = candidate;
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    if (generatedIdentity != null) {
                        identity = generatedIdentity;
                        showIdentity();
                    }
                    sessionResult = null;
                    finishWork(epoch,
                            getString(R.string.enrollment_failed, safeMessage(error)), null);
                });
            }
        });
    }

    private void loadSession() {
        DeviceIdentity current = identity;
        if (current == null) {
            status.setText(R.string.missing_identity);
            return;
        }
        sessionResult = null;
        submit(epoch -> {
            try {
                NativeGatewayClient.SessionResult response =
                        new NativeGatewayClient(urlPolicy, current).getSession();
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    sessionResult = response;
                    finishWork(epoch, getString(R.string.idle), response.json());
                });
            } catch (Exception error) {
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    sessionResult = null;
                    finishWork(epoch,
                            getString(R.string.request_failed, safeMessage(error)), null);
                });
            }
        });
    }

    private void syncSms() {
        DeviceIdentity current = identity;
        if (current == null) {
            status.setText(R.string.missing_identity);
            return;
        }
        int requestedLimit;
        try {
            requestedLimit = Integer.parseInt(limit.getText().toString());
            if (requestedLimit < 1
                    || requestedLimit > NativeGatewayUrlPolicy.SMS_SYNC_MAXIMUM_LIMIT) {
                throw new NumberFormatException("out of range");
            }
        } catch (NumberFormatException error) {
            status.setText(R.string.invalid_limit);
            return;
        }
        String requestedCursor = cursor.getText().toString();
        if (requestedCursor.isEmpty()) {
            requestedCursor = null;
        }
        String finalCursor = requestedCursor;
        submit(epoch -> {
            try {
                NativeGatewayClient.SmsSyncPage page = new NativeGatewayClient(urlPolicy, current)
                        .syncSms(finalCursor, requestedLimit);
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    cursor.setText(page.cursor());
                    finishWork(epoch, getString(R.string.idle), page.json());
                });
            } catch (Exception error) {
                runOnUiThread(() -> finishWork(epoch,
                        getString(R.string.request_failed, safeMessage(error)), null));
            }
        });
    }

    private void confirmSmsSend() {
        DeviceIdentity current = identity;
        NativeGatewayClient.SessionResult currentSession = sessionResult;
        if (current == null) {
            status.setText(R.string.missing_identity);
            return;
        }
        if (currentSession == null || !currentSession.canSendSms()) {
            status.setText(R.string.sms_session_required);
            return;
        }
        if (!journalHealthy || (smsOperation != null && smsOperation.blocksNewSend())) {
            renderSmsOperationGuard();
            return;
        }
        String phone = smsPhone.getText().toString().trim();
        String message = smsMessage.getText().toString().trim();
        if (phone.isEmpty() || message.isEmpty()) {
            status.setText(R.string.sms_missing_fields);
            return;
        }
        long confirmationEpoch = operationEpoch;
        AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle(R.string.sms_send_confirm_title)
                .setMessage(getString(R.string.sms_send_confirm_body, phone, message))
                .setNegativeButton(R.string.cancel, null)
                .setPositiveButton(R.string.sms_send_confirm,
                        (ignoredDialog, ignoredButton) -> {
                            if (canUseConfirmation(confirmationEpoch)
                                    && identity == current
                                    && sessionResult == currentSession
                                    && currentSession.canSendSms()
                                    && journalHealthy
                                    && (smsOperation == null
                                    || !smsOperation.blocksNewSend())) {
                                startSmsSend(current, phone, message);
                            }
                        })
                .create();
        showDialog(dialog);
    }

    private void startSmsSend(DeviceIdentity current, String phone, String message) {
        smsPhone.setText(null);
        smsMessage.setText(null);
        submit(epoch -> {
            SmsSendRequest request = null;
            SmsSendOperation[] durable = new SmsSendOperation[1];
            boolean[] journalFailure = new boolean[1];
            try {
                request = SmsSendRequest.create(phone, message);
                SmsSendOperation prepared = SmsSendOperation.prepared(
                        request.operationId(), current.deviceId(), Instant.now());
                writeSmsOperation(prepared, journalFailure);
                durable[0] = prepared;

                NativeGatewayClient.SmsSendResult sendResult =
                        new NativeGatewayClient(urlPolicy, current).sendSms(request, () -> {
                            SmsSendOperation unknown = durable[0].markUnknown(nowFor(durable[0]));
                            writeSmsOperation(unknown, journalFailure);
                            durable[0] = unknown;
                        });
                if (sendResult.outcome() == NativeGatewayClient.SmsOutcome.SUBMITTED
                        || sendResult.outcome() == NativeGatewayClient.SmsOutcome.NOT_SUBMITTED) {
                    SmsSendOperation terminal = durable[0].markTerminal(
                            sendResult.httpStatus(), sendResult.code(), nowFor(durable[0]));
                    writeSmsOperation(terminal, journalFailure);
                    durable[0] = terminal;
                } else if (sendResult.outcome()
                        == NativeGatewayClient.SmsOutcome.UNKNOWN_LOCKED) {
                    SmsSendOperation unknown = durable[0].markUnknown(
                            sendResult.httpStatus(), sendResult.code(), nowFor(durable[0]));
                    writeSmsOperation(unknown, journalFailure);
                    durable[0] = unknown;
                }
                SmsSendOperation completed = durable[0];
                String operationResult = operationResultText(
                        completed, sendResult.httpStatus(), sendResult.code());
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    smsOperation = completed;
                    String messageText;
                    if (sendResult.outcome() == NativeGatewayClient.SmsOutcome.SUBMITTED) {
                        messageText = getString(
                                R.string.sms_submitted, completed.operationId());
                    } else if (sendResult.outcome()
                            == NativeGatewayClient.SmsOutcome.NOT_SUBMITTED) {
                        messageText = getString(
                                R.string.sms_not_submitted,
                                completed.operationId(), sendResult.code());
                    } else {
                        messageText = getString(
                                R.string.sms_unknown, completed.operationId());
                    }
                    finishWork(epoch, messageText, operationResult);
                });
            } catch (Exception error) {
                SmsSendOperation recovered = durable[0];
                if (!journalFailure[0]) {
                    try {
                        SmsSendOperation fromDisk = smsOperationStore.load();
                        if (fromDisk != null) {
                            recovered = fromDisk;
                        }
                    } catch (IOException journalError) {
                        journalFailure[0] = true;
                    }
                }
                SmsSendOperation finalRecovered = recovered;
                boolean finalJournalFailure = journalFailure[0];
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    if (finalRecovered != null) {
                        smsOperation = finalRecovered;
                    }
                    if (finalJournalFailure) {
                        journalHealthy = false;
                        finishWork(epoch,
                                getString(R.string.sms_journal_failed, safeMessage(error)), null);
                    } else {
                        finishWork(epoch,
                                getString(R.string.request_failed, safeMessage(error)), null);
                    }
                });
            } finally {
                if (request != null) {
                    request.close();
                }
            }
        });
    }

    private void checkSmsOperation() {
        DeviceIdentity current = identity;
        SmsSendOperation pending = smsOperation;
        if (current == null || pending == null || !pending.blocksNewSend()) {
            return;
        }
        submit(epoch -> {
            try {
                NativeGatewayClient.SmsOperationResult lookup =
                        new NativeGatewayClient(urlPolicy, current)
                                .getSmsOperation(pending.operationId());
                SmsSendOperation next;
                switch (lookup.outcome()) {
                case SUBMITTED:
                case NOT_SUBMITTED:
                    next = pending.markTerminal(
                            lookup.httpStatus(), lookup.code(), nowFor(pending));
                    break;
                case NOT_FOUND_REQUIRES_CONFIRMATION:
                    next = pending.markNotFound(nowFor(pending));
                    break;
                case UNKNOWN_LOCKED:
                default:
                    next = lookup.httpStatus() == 0
                            ? pending.markUnknown(nowFor(pending))
                            : pending.markUnknown(
                                    lookup.httpStatus(), lookup.code(), nowFor(pending));
                    break;
                }
                smsOperationStore.write(next);
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    smsOperation = next;
                    String messageText;
                    if (lookup.outcome() == NativeGatewayClient.SmsOutcome.SUBMITTED) {
                        messageText = getString(R.string.sms_submitted, next.operationId());
                    } else if (lookup.outcome()
                            == NativeGatewayClient.SmsOutcome.NOT_SUBMITTED) {
                        messageText = getString(
                                R.string.sms_not_submitted, next.operationId(), lookup.code());
                    } else if (lookup.outcome()
                            == NativeGatewayClient.SmsOutcome.NOT_FOUND_REQUIRES_CONFIRMATION) {
                        messageText = getString(R.string.sms_not_found, next.operationId());
                    } else {
                        messageText = getString(R.string.sms_unknown, next.operationId());
                    }
                    finishWork(epoch, messageText,
                            operationResultText(next, lookup.httpStatus(), lookup.code()));
                });
            } catch (Exception error) {
                runOnUiThread(() -> finishWork(epoch,
                        getString(R.string.sms_query_failed, safeMessage(error)), null));
            }
        });
    }

    private void confirmCloseSmsOperation() {
        SmsSendOperation current = smsOperation;
        if (current == null || (current.state() != SmsSendOperation.State.PREPARED
                && current.state() != SmsSendOperation.State.NOT_FOUND)) {
            return;
        }
        boolean prepared = current.state() == SmsSendOperation.State.PREPARED;
        long confirmationEpoch = operationEpoch;
        AlertDialog dialog = new AlertDialog.Builder(this)
                .setTitle(prepared
                        ? R.string.sms_close_prepared_title
                        : R.string.sms_close_not_found_title)
                .setMessage(prepared
                        ? R.string.sms_close_prepared_body
                        : R.string.sms_close_not_found_body)
                .setNegativeButton(R.string.cancel, null)
                .setPositiveButton(R.string.sms_close_confirm,
                        (ignoredDialog, ignoredButton) -> {
                            if (canUseConfirmation(confirmationEpoch)
                                    && smsOperation == current
                                    && journalHealthy) {
                                closeSmsOperation(current, prepared);
                            }
                        })
                .create();
        showDialog(dialog);
    }

    private void closeSmsOperation(SmsSendOperation current, boolean prepared) {
        submit(epoch -> {
            try {
                smsOperationStore.clearExpected(current.operationId(), current.state());
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    smsOperation = null;
                    finishWork(epoch, getString(R.string.idle),
                            "operation_id=" + current.operationId()
                                    + "\nstate=" + (prepared
                                    ? "prepared_discarded" : "not_found_confirmed"));
                });
            } catch (Exception error) {
                runOnUiThread(() -> {
                    if (!isCurrent(epoch)) {
                        return;
                    }
                    journalHealthy = false;
                    finishWork(epoch,
                            getString(R.string.sms_journal_failed, safeMessage(error)), null);
                });
            }
        });
    }

    private void writeSmsOperation(SmsSendOperation operation, boolean[] journalFailure)
            throws IOException {
        try {
            smsOperationStore.write(operation);
        } catch (IOException error) {
            journalFailure[0] = true;
            throw error;
        }
    }

    private void submit(EpochOperation operation) {
        if (stopped || isFinishing() || isDestroyed()
                || (activeTask != null && !activeTask.isDone())) {
            return;
        }
        long epoch = ++operationEpoch;
        status.setText(R.string.working);
        result.setText(null);
        setControlsEnabled(false);
        activeTask = executor.submit(() -> operation.run(epoch));
    }

    private void finishWork(long epoch, String message, String response) {
        if (!isCurrent(epoch)) {
            return;
        }
        activeTask = null;
        status.setText(message);
        result.setText(response);
        renderSmsOperationGuard();
        setControlsEnabled(true);
    }

    private boolean isCurrent(long epoch) {
        return epoch == operationEpoch && !stopped && !isFinishing() && !isDestroyed();
    }

    private void showIdentity() {
        deviceIdentity.setText(identity == null
                ? getString(R.string.device_not_ready)
                : getString(R.string.device_ready, identity.deviceId()));
    }

    private void renderSmsOperationGuard() {
        if (!journalHealthy) {
            return;
        }
        SmsSendOperation current = smsOperation;
        if (current == null) {
            smsOperationGuard.setText(R.string.sms_guard_empty);
            return;
        }
        switch (current.state()) {
        case PREPARED:
            smsOperationGuard.setText(
                    getString(R.string.sms_prepared, current.operationId()));
            break;
        case UNKNOWN:
            smsOperationGuard.setText(
                    getString(R.string.sms_unknown, current.operationId()));
            break;
        case NOT_FOUND:
            smsOperationGuard.setText(
                    getString(R.string.sms_not_found, current.operationId()));
            break;
        case TERMINAL:
            smsOperationGuard.setText(getString(
                    R.string.sms_terminal,
                    current.operationId(),
                    current.httpStatus(),
                    current.resultCode()));
            break;
        }
    }

    private void setControlsEnabled(boolean enabled) {
        boolean hasIdentity = identity != null;
        boolean unresolved = smsOperation != null && smsOperation.blocksNewSend();
        boolean sendReady = enabled && journalHealthy && hasIdentity && !unresolved
                && sessionResult != null && sessionResult.canSendSms();
        enroll.setEnabled(enabled);
        loadSession.setEnabled(enabled && hasIdentity);
        syncSms.setEnabled(enabled && hasIdentity);
        ticketId.setEnabled(enabled);
        ticketSecret.setEnabled(enabled);
        cursor.setEnabled(enabled);
        limit.setEnabled(enabled);
        smsPhone.setEnabled(sendReady);
        smsMessage.setEnabled(sendReady);
        sendSms.setEnabled(sendReady);
        checkSmsOperation.setEnabled(enabled && journalHealthy && hasIdentity && unresolved);
        closeSmsOperation.setEnabled(enabled && journalHealthy && smsOperation != null
                && (smsOperation.state() == SmsSendOperation.State.PREPARED
                || smsOperation.state() == SmsSendOperation.State.NOT_FOUND));
    }

    private void cancelActiveTask() {
        operationEpoch++;
        Future<?> current = activeTask;
        activeTask = null;
        if (current != null) {
            current.cancel(true);
        }
    }

    private boolean canUseConfirmation(long confirmationEpoch) {
        return !stopped && !isFinishing() && !isDestroyed()
                && confirmationEpoch == operationEpoch
                && (activeTask == null || activeTask.isDone());
    }

    private void showDialog(AlertDialog dialog) {
        dismissActiveDialog();
        activeDialog = dialog;
        dialog.setOnShowListener(ignored -> {
            if (dialog.getWindow() != null) {
                dialog.getWindow().addFlags(WindowManager.LayoutParams.FLAG_SECURE);
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    dialog.getWindow().getDecorView().setImportantForContentCapture(
                            View.IMPORTANT_FOR_CONTENT_CAPTURE_NO_EXCLUDE_DESCENDANTS);
                }
            }
            Button positive = dialog.getButton(AlertDialog.BUTTON_POSITIVE);
            Button negative = dialog.getButton(AlertDialog.BUTTON_NEGATIVE);
            if (positive != null) {
                positive.setFilterTouchesWhenObscured(true);
            }
            if (negative != null) {
                negative.setFilterTouchesWhenObscured(true);
            }
        });
        dialog.setOnDismissListener(ignored -> {
            if (activeDialog == dialog) {
                activeDialog = null;
            }
        });
        dialog.show();
    }

    private static void protectSensitiveButtons(Button... buttons) {
        for (Button button : buttons) {
            button.setFilterTouchesWhenObscured(true);
        }
    }

    private void dismissActiveDialog() {
        AlertDialog dialog = activeDialog;
        activeDialog = null;
        if (dialog != null && dialog.isShowing()) {
            dialog.dismiss();
        }
    }

    private static Instant nowFor(SmsSendOperation operation) {
        Instant now = Instant.now();
        return now.isBefore(operation.updatedAt()) ? operation.updatedAt() : now;
    }

    private static String operationResultText(
            SmsSendOperation operation,
            int httpStatus,
            String code) {
        return "operation_id=" + operation.operationId()
                + "\nstate=" + operation.state().name().toLowerCase(java.util.Locale.ROOT)
                + "\nhttp_status=" + httpStatus
                + "\ncode=" + code;
    }

    private static String safeMessage(Throwable error) {
        String message = error.getMessage();
        return message == null || message.isEmpty()
                ? error.getClass().getSimpleName()
                : message;
    }

    private interface EpochOperation {
        void run(long epoch);
    }
}
