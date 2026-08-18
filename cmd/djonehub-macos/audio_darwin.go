//go:build darwin && cgo

package main

/*
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework CoreFoundation -framework Foundation -framework AVFoundation -framework IOKit
#include <AudioToolbox/AudioToolbox.h>
#include <mach/mach_time.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include <unistd.h>

void *dj_mavo_uac_bridge_start(void *context, uint16_t vendor, uint16_t product,
                               uint32_t location, char *error, size_t error_capacity);
void dj_mavo_uac_bridge_stop(void *bridge);
void dj_mavo_uac_bridge_set_muted(void *bridge, int muted);
int dj_mavo_uac_bridge_running(void *bridge);
void dj_mavo_uac_bridge_stats(void *bridge, uint64_t *input_callbacks,
                              uint64_t *output_callbacks, uint64_t *input_frames,
                              uint64_t *output_frames);
const char *dj_mavo_uac_bridge_name(void *bridge);

static int make_conv(const AudioStreamBasicDescription *src,
                     const AudioStreamBasicDescription *dst,
                     AudioConverterRef *out);

// ---- byte ring buffer (bounded, lock-free SPSC) ----
//
// Each live call path has one producer and one consumer. Using a mutex in an
// AudioDeviceIOProc means an unrelated recorder/playback callback can block a
// hardware callback long enough to turn into an audible burst. Keep ownership
// strict and drop newest data on producer overflow; silence is preferable to
// waiting in a real-time callback.
typedef struct {
	uint8_t *buf;
	size_t   cap;
	_Atomic uint64_t read_pos;
	_Atomic uint64_t write_pos;
	_Atomic uint64_t dropped;
} ringbuf;

static void rb_init(ringbuf *r, size_t cap) {
	r->buf = (uint8_t *)malloc(cap);
	r->cap = cap;
	atomic_init(&r->read_pos, 0);
	atomic_init(&r->write_pos, 0);
	atomic_init(&r->dropped, 0);
}

static size_t rb_used(ringbuf *r) {
	if (!r || r->cap == 0) return 0;
	uint64_t write_pos = atomic_load_explicit(&r->write_pos, memory_order_acquire);
	uint64_t read_pos = atomic_load_explicit(&r->read_pos, memory_order_acquire);
	uint64_t used = write_pos - read_pos;
	return used > r->cap ? r->cap : (size_t)used;
}

static size_t rb_write(ringbuf *r, const uint8_t *data, size_t n) {
	if (!r || !r->buf || !data || r->cap == 0 || n == 0) return 0;
	uint64_t write_pos = atomic_load_explicit(&r->write_pos, memory_order_relaxed);
	uint64_t read_pos = atomic_load_explicit(&r->read_pos, memory_order_acquire);
	uint64_t used = write_pos - read_pos;
	if (used > r->cap) used = r->cap;
	size_t accepted = n < (r->cap - (size_t)used) ? n : r->cap - (size_t)used;
	if (accepted == 0) {
		atomic_fetch_add_explicit(&r->dropped, n, memory_order_relaxed);
		return 0;
	}
	size_t offset = (size_t)(write_pos % r->cap);
	size_t first = accepted < r->cap - offset ? accepted : r->cap - offset;
	memcpy(r->buf + offset, data, first);
	if (accepted > first) memcpy(r->buf, data + first, accepted - first);
	atomic_store_explicit(&r->write_pos, write_pos + accepted, memory_order_release);
	if (accepted < n) atomic_fetch_add_explicit(&r->dropped, n - accepted, memory_order_relaxed);
	return accepted;
}

static size_t rb_read(ringbuf *r, uint8_t *out, size_t n) {
	if (!r || !r->buf || !out || r->cap == 0 || n == 0) return 0;
	uint64_t read_pos = atomic_load_explicit(&r->read_pos, memory_order_relaxed);
	uint64_t write_pos = atomic_load_explicit(&r->write_pos, memory_order_acquire);
	uint64_t available64 = write_pos - read_pos;
	if (available64 > r->cap) available64 = r->cap;
	size_t copied = n < (size_t)available64 ? n : (size_t)available64;
	size_t offset = (size_t)(read_pos % r->cap);
	size_t first = copied < r->cap - offset ? copied : r->cap - offset;
	memcpy(out, r->buf + offset, first);
	if (copied > first) memcpy(out + first, r->buf, copied - first);
	atomic_store_explicit(&r->read_pos, read_pos + copied, memory_order_release);
	return copied;
}

// Only call while the producer is stopped (recording/session boundary).
static void rb_clear(ringbuf *r) {
	if (!r) return;
	uint64_t write_pos = atomic_load_explicit(&r->write_pos, memory_order_acquire);
	atomic_store_explicit(&r->read_pos, write_pos, memory_order_release);
	atomic_store_explicit(&r->dropped, 0, memory_order_relaxed);
}

// 4th-order Butterworth LPF state (two biquads in series), used on the far
// path to remove the module's out-of-band 3.5-4k noise. Defined here, before
// router, so the router struct can hold an instance.
typedef struct { float z1, z2; } bq_state;
typedef struct { bq_state st[2]; float b0, b1, b2, a1, a2; } lpf4_state;

// ---- call recorder ----
typedef struct {
	_Atomic int on;
	_Atomic int threadRun;
	int err;
	UInt32 nearCorruptBlocks;
	FILE *file;
	UInt32 wavBytes;
	ringbuf farRec;
	ringbuf nearRec;
	pthread_t thread;
} recorder;

// A Studio Display capture failure observed in real calls is unambiguous: the
// near channel jumps from normal speech/noise to sustained full-scale broadband
// samples. Keep this guard recording-only so a false positive can never mute
// the live microphone path. Normal loud speech may peak at 1.0, but it does not
// combine >0.30 RMS with >10% clipped samples across a complete recorder block.
static int rec_near_block_is_corrupt(const float *p, size_t n) {
	if (!p || n < 80) return 0;
	double energy = 0.0;
	size_t clipped = 0;
	float peak = 0.0f;
	for (size_t i = 0; i < n; i++) {
		float x = p[i];
		if (!isfinite(x)) return 1;
		float a = fabsf(x);
		if (a > peak) peak = a;
		if (a >= 0.98f) clipped++;
		energy += (double)x * (double)x;
	}
	float rms = (float)sqrt(energy / (double)n);
	return peak >= 0.98f && rms >= 0.30f && clipped * 10 >= n;
}

static void rec_wav_u16(FILE *f, UInt32 v) {
	fputc((int)(v & 0xff), f); fputc((int)((v >> 8) & 0xff), f);
}

static void rec_wav_u32(FILE *f, UInt32 v) {
	fputc((int)(v & 0xff), f); fputc((int)((v >> 8) & 0xff), f);
	fputc((int)((v >> 16) & 0xff), f); fputc((int)((v >> 24) & 0xff), f);
}

static int rec_wav_header(FILE *f, UInt32 dataBytes) {
	if (!f) return 1;
	if (fseek(f, 0, SEEK_SET) != 0) return 1;
	fwrite("RIFF", 1, 4, f); rec_wav_u32(f, 36 + dataBytes); fwrite("WAVE", 1, 4, f);
	fwrite("fmt ", 1, 4, f); rec_wav_u32(f, 16); rec_wav_u16(f, 1); rec_wav_u16(f, 2);
	rec_wav_u32(f, 8000); rec_wav_u32(f, 32000); rec_wav_u16(f, 4); rec_wav_u16(f, 16);
	fwrite("data", 1, 4, f); rec_wav_u32(f, dataBytes);
	return ferror(f) ? 1 : 0;
}

static void *rec_thread(void *arg) {
	recorder *rc = (recorder *)arg;
	// Both far and near are 8 kHz float32 mono, so they are interleaved 1:1 into
	// a single 8 kHz stereo file. The capture callbacks are independent: if the
	// near ring lags, write silence rather than repeating an old sample. Repeating
	// an uninitialised/stale sample here can look exactly like a continuous full-
	// scale electrical buzz in an otherwise clean call recording.
	float farBuf[800];
	float nearBuf[800];
	int16_t stereo[2 * 800];
	while (atomic_load_explicit(&rc->threadRun, memory_order_acquire)) {
		usleep(20000);
		if (!atomic_load_explicit(&rc->on, memory_order_acquire)) continue;
		size_t nFar = rb_read(&rc->farRec, (uint8_t *)farBuf, sizeof(farBuf)) / sizeof(float);
		if (nFar < 80) continue;
		memset(nearBuf, 0, sizeof(nearBuf));
		size_t nNear = rb_read(&rc->nearRec, (uint8_t *)nearBuf, nFar * sizeof(float)) / sizeof(float);
		if (rec_near_block_is_corrupt(nearBuf, nNear)) {
			memset(nearBuf, 0, nNear * sizeof(float));
			rc->nearCorruptBlocks++;
			if (rc->nearCorruptBlocks == 1 || (rc->nearCorruptBlocks % 100) == 0) {
				fprintf(stderr, "voice recorder: isolated corrupt near block (count=%u)\n",
				        (unsigned int)rc->nearCorruptBlocks);
			}
		}
		for (size_t i = 0; i < nFar; i++) {
			float far = fmaxf(-1.0f, fminf(1.0f, farBuf[i]));
			float near = i < nNear ? fmaxf(-1.0f, fminf(1.0f, nearBuf[i])) : 0.0f;
			stereo[2 * i] = (int16_t)lrintf(far * 32767.0f);
			stereo[2 * i + 1] = (int16_t)lrintf(near * 32767.0f);
		}
		size_t bytes = nFar * 2 * sizeof(int16_t);
		if (fwrite(stereo, 1, bytes, rc->file) != bytes) { rc->err = 1; atomic_store_explicit(&rc->on, 0, memory_order_release); break; }
		rc->wavBytes += (UInt32)bytes;
	}
	return NULL;
}


// ---- router ----
typedef struct {
	AudioDeviceID modIn;
	AudioDeviceID modOut;
	AudioDeviceID macIn;
	AudioDeviceID macOut;
	Float64 modInOriginalRate;
	Float64 modOutOriginalRate;
	int modInRateOverridden;
	int modOutRateOverridden;

	AudioStreamBasicDescription modInFmt;
	AudioStreamBasicDescription modOutFmt;
	AudioStreamBasicDescription macInFmt;
	AudioStreamBasicDescription macOutFmt;

	// far canonical: float32 mono 8kHz (module input -> Mac output; the system
	// mixer SRCs this to the device's real rate, exactly like CellDock).
	// near canonical: float32 mono 8kHz (Mac input -> module output)
	AudioStreamBasicDescription farFmt;
	AudioStreamBasicDescription nearFmt;

	AudioConverterRef convToFar;    // modInFmt -> farFmt (fixed 8k, no rate SRC)
	AudioConverterRef convToFar16;  // int16 modInFmt -> farFmt (auto-detected)
	AudioConverterRef convToNear;   // macInFmt -> nearFmt
	AudioConverterRef convModOut;   // nearFmt -> modOutFmt
	// The module exposes native 8 kHz float mono on healthy firmware. Keep that
	// path converter-free: a same-rate converter adds no value but still runs in
	// the real-time callback and can add scheduling noise on USB UAC devices.
	int modInDirect;
	int modOutDirect;
	// Scratch buffers are allocated once during startup. Audio callbacks must
	// never malloc/free: the allocator can block behind unrelated work and shows
	// up as intermittent buzz or bursts on the narrowband UAC stream.
	uint8_t *farCaptureWork;
	uint8_t *nearCaptureWork;
	uint8_t *nearOutWork;
	size_t captureWorkCap;
	size_t nearOutWorkCap;
	int modInIs16;                  // module input currently decoded as 16-bit PCM
	long long modInLastHost;        // last modIn callback host time (actual-rate probe)
	double   modInMeasured;         // smoothed measured input rate (Hz)
	int      modInMeasCount;        // probe samples; -1 once calibrated for this call

	// macOut plays the 8 kHz far chain to the Mac's output device at a fixed
	// 8k->48k (6:1 integer-ratio) SRC. CellDock hands 8 kHz to coreaudiod and
	// lets the system mixer SRC + clock-sync; the AudioQueue path we tried for
	// exactly that failed to schedule on this device (0-frame callbacks, silent
	// output — "voice macqueue diag: frames=0"), so we drive the device IOProc
	// directly with a fixed integer-ratio converter. No servo, no filter chain:
	// 8 kHz in, device rate out, drift absorbed by the ring + saturation resync.
	int      macOutUnderruns;       // converter input underruns since the last resync (diagnostic)
	float    farWmLvl;              // low-passed far ring watermark (bytes) — diagnostic only

	ringbuf farRing;
	ringbuf nearRing;

	AudioDeviceIOProcID modInProc;
	AudioDeviceIOProcID modOutProc;
	AudioDeviceIOProcID macInProc;
	AudioDeviceIOProcID macOutProc;
	AudioConverterRef   convMacOut;    // legacy device-rate path; normally unused
	AudioQueueRef        macQueue;      // native 8 kHz stream to the system mixer
	void                *mavoBridge;    // MaVo-compatible UAC + AVAudioEngine route

	float farPeak;
	float nearPeak;
	// DSP state: envelope trackers for the far-talk detector (near-path echo
	// suppression) and the near noise gate. The far path is otherwise a pure
	// passthrough — CellDock's "original" voice effect is no filter, and its
	// cleanliness comes from the system mixer doing SRC + clock sync, not from
	// any DSP on our side. (The old far chain — HP/LP pair/notch/Butterworth +
	// a servoed rate — was the residual 滋滋 source; it is gone.)
	float farEnv;
	float farHold;
	lpf4_state farLpf;           // linear 4th-order LPF @3.4k (kills module 3.5-4k 嘶嘶)
	float nearHPx1, nearHPy1, nearEnv;
	float nearLPx1, nearLPy1;
	float farGain;
	float nearGain;
	float farGateThresh;
	float nearGateThresh;
	long modInCalls;
	long modOutCalls;
	long macInCalls;
	long macOutCalls;
	int   muted;
	int   running;
	char  err[256];
	char  modInName[64];
	char  modOutName[64];
	char  macInName[64];
	char  macOutName[64];
	float farLive;
	float nearLive;
	float farOutLive;
	float nearOutLive;
	char  fmtInfo[512];
	recorder rec;
} router;

static void router_set_err(router *r, const char *msg) {
	strncpy(r->err, msg, sizeof(r->err) - 1);
	r->err[sizeof(r->err) - 1] = 0;
}

static void router_stop(router *r);

static void fmt_float(AudioStreamBasicDescription *f, Float64 rate, UInt32 ch) {
	memset(f, 0, sizeof(*f));
	f->mSampleRate = rate;
	f->mFormatID = kAudioFormatLinearPCM;
	f->mFormatFlags = kAudioFormatFlagsNativeFloatPacked;
	f->mChannelsPerFrame = ch;
	f->mBitsPerChannel = 32;
	f->mBytesPerFrame = 4 * ch;
	f->mFramesPerPacket = 1;
	f->mBytesPerPacket = 4 * ch;
}

static void fmt_int16(AudioStreamBasicDescription *f, Float64 rate) {
	memset(f, 0, sizeof(*f));
	f->mSampleRate = rate;
	f->mFormatID = kAudioFormatLinearPCM;
	f->mFormatFlags = kAudioFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked;
	f->mChannelsPerFrame = 1;
	f->mBitsPerChannel = 16;
	f->mBytesPerFrame = 2;
	f->mFramesPerPacket = 1;
	f->mBytesPerPacket = 2;
}

static int fmt_is_native_8k_float_mono(const AudioStreamBasicDescription *f) {
	return f && f->mSampleRate == 8000.0 &&
		f->mFormatID == kAudioFormatLinearPCM &&
		(f->mFormatFlags & kAudioFormatFlagIsFloat) &&
		f->mChannelsPerFrame == 1 && f->mBitsPerChannel == 32 &&
		f->mBytesPerFrame == sizeof(float) && f->mBytesPerPacket == sizeof(float);
}

// Converter input proc: pull from a fixed source buffer.
typedef struct {
	const uint8_t *data;
	size_t len;
	size_t off;
	AudioStreamBasicDescription fmt;
} src_pull_ctx;

static OSStatus src_pull_proc(AudioConverterRef inConverter,
                              UInt32 *ioNumberDataPackets,
                              AudioBufferList *ioData,
                              AudioStreamPacketDescription **outDataPacketDescription,
                              void *inUserData) {
	(void)inConverter;
	(void)outDataPacketDescription;
	src_pull_ctx *ctx = (src_pull_ctx *)inUserData;
	size_t bytesPerFrame = ctx->fmt.mBytesPerFrame;
	size_t remaining = ctx->len - ctx->off;
	UInt32 wantFrames = *ioNumberDataPackets;
	size_t canFrames = remaining / bytesPerFrame;
	if ((size_t)wantFrames > canFrames) wantFrames = (UInt32)canFrames;
	ioData->mNumberBuffers = 1;
	ioData->mBuffers[0].mData = (void *)(ctx->data + ctx->off);
	ioData->mBuffers[0].mDataByteSize = wantFrames * (UInt32)bytesPerFrame;
	ioData->mBuffers[0].mNumberChannels = ctx->fmt.mChannelsPerFrame;
	ctx->off += (size_t)wantFrames * bytesPerFrame;
	*ioNumberDataPackets = wantFrames;
	return noErr;
}

static float peak16(const int16_t *p, size_t n) {
	float peak = 0;
	for (size_t i = 0; i < n; i++) {
		float v = p[i] / 32768.0f;
		if (fabsf(v) > peak) peak = fabsf(v);
	}
	return peak;
}

static float peak32f(const float *p, size_t n) {
	float peak = 0;
	for (size_t i = 0; i < n; i++) {
		if (fabsf(p[i]) > peak) peak = fabsf(p[i]);
	}
	return peak;
}

// One-pole high-pass filter (removes DC offset and mains hum).
static float hp1(float x, float *x1, float *y1, float a) {
	float y = a * (*y1 + x - *x1);
	*x1 = x;
	*y1 = y;
	return y;
}

// One-pole low-pass filter.
static float lp1(float x, float *x1, float *y1, float a) {
	(void)x1;
	*y1 += a * (x - *y1);
	return *y1;
}

// Two cascaded one-pole LPFs: lp1(a) with a = 1 - exp(-2*pi*fc/fs). This is
// the far-path hiss killer. A single one-pole was too gentle to roll off the
// module vocoder's 3-4 kHz noise band, and a 2nd-order Butterworth at 2.5 kHz
// was worse: it cut 2-3 kHz speech (dulled the voice) and its phase skirt
// around cutoff left MORE energy in 3-4 kHz than the gentler pair. Cascaded
// one-poles have real poles only — zero ringing — so 3-4 kHz rolls off cleanly
// while 1-2 kHz speech stays untouched.

// The legacy far filter remains an experiment and is not treated as the cause
// of call noise. The active migration first removes real-time lock contention
// and multi-second buffering; any DSP adjustment requires a matched A/B test.

// Voice-band (300-3400 Hz) filter chain: HPF then LPF. This removes mains
// hum, DC offset and broadband hiss from the module's noisy USB audio path so
// the noise gate and echo detector only see real speech.
static float bandpass(float x, float *hpx1, float *hpy1, float *lpx1, float *lpy1,
                      float hpA, float lpA) {
	float hp = hp1(x, hpx1, hpy1, hpA);
	return lp1(hp, lpx1, lpy1, lpA);
}

// Soft limiter: tanh saturation. A plain hard clamp (tried once) turned the
// module's occasional loud bursts into square waves — the squared corners
// flattened the whole spectrum and read as a much worse "滋滋". tanh saturates
// smoothly: tanh(2)=0.96, tanh(4)=0.999, so no output ever square-waves and
// the small harmonic cost at loud peaks is far better than hard clipping.
static float soft_limiter(float x) {
	float y = tanhf(x) * 1.2f;
	if (y > 1.0f) y = 1.0f; else if (y < -1.0f) y = -1.0f;
	return y;
}

// 4th-order Butterworth low-pass (state + coefficients live in lpf4_state,
// defined above near the ring buffer). Purely linear — the far path's only
// stage besides gain. Rationale (verified on real recordings): the module's
// UAC audio carries strong out-of-band noise at 3.5-4 kHz (8 kHz fs: just under
// Nyquist) that is amplified during speech — speech segments sit ~19 dB higher
// there than silence, an isolated peak above the natural rolloff. Voice-band
// tops out at 3.4 kHz, so a linear LPF at 3.4 kHz removes that 嘶嘶 with zero
// loss below 3 kHz (measured: 1 kHz unchanged, 3.5-4k -17 dB -> -32 dB). No
// tanh here — non-linearity was the previous 滋滋 (memory
// djonehub-far-filter-tanh-intermod).
static void lpf4_init(lpf4_state *f, float fc, float fs) {
	float w0 = 2.0f * 3.14159265358979f * fc / fs;
	float cw = cosf(w0), sw = sinf(w0);
	float alpha = sw * 0.70710678f;   // Q = 1/sqrt(2) (Butterworth)
	float b0 = (1.0f - cw) / 2.0f, b1 = 1.0f - cw, b2 = (1.0f - cw) / 2.0f;
	float a0 = 1.0f + alpha, a1 = -2.0f * cw, a2 = 1.0f - alpha;
	f->b0 = b0 / a0; f->b1 = b1 / a0; f->b2 = b2 / a0;
	f->a1 = a1 / a0; f->a2 = a2 / a0;
	f->st[0].z1 = f->st[0].z2 = 0.0f;
	f->st[1].z1 = f->st[1].z2 = 0.0f;
}

static float lpf4_run(lpf4_state *f, float x) {
	for (int i = 0; i < 2; i++) {
		float o = f->b0 * x + f->st[i].z1;
		f->st[i].z1 = f->b1 * x - f->a1 * o + f->st[i].z2;
		f->st[i].z2 = f->b2 * x - f->a2 * o;
		x = o;
	}
	return x;
}

// Far path: linear 4th-order LPF @3.0 kHz -> gain. The LPF is the one DSP the
// far chain needs: it removes the module's out-of-band 3.4-4k noise (strongest
// during speech) while preserving the telephone voice band. Everything else from the old
// chain (HP/LP pair, notch, Butterworth, tanh limiter, rate servo) is gone —
// those were chasing the saturating ring / tanh intermod, not this noise.
// farEnv keeps feeding the near-path echo suppressor's far-talk detector.
static void process_far_block(float *p, size_t n, router *r) {
	for (size_t i = 0; i < n; i++) {
		float x = lpf4_run(&r->farLpf, p[i]);
		float a = fabsf(x);
		if (a > r->farEnv) r->farEnv = a;
		else r->farEnv *= 0.9995f;
		p[i] = x * r->farGain;
		a = fabsf(p[i]);
		if (a > r->farOutLive) r->farOutLive = a;
		else r->farOutLive *= 0.9995f;
	}
}

// Near path: HPF -> noise gate -> gain, with half-duplex echo suppression:
// while the far side is speaking we attenuate the mic path so the Mac speaker
// audio is not picked up by the Mac microphone and echoed back to the caller.
static void process_near_block(float *p, size_t n, router *r) {
	float gain = r->nearGain;
	// Studio Display's measured speaker-to-mic path is about 350 ms. Keep the
	// far-side detector alive for that window and strongly duck the mic while
	// remote speech is present, so playback is not recorded as near audio.
	if (r->farEnv > 0.04f) r->farHold = 1.0f;
	else r->farHold *= 0.9997f;
	// The Studio Display microphone can emit a full-scale broadband burst while
	// its own speakers are active. At that point the useful near speech is
	// indistinguishable from speaker leakage, so prefer clean half-duplex voice.
	if (r->farHold > 0.5f) gain *= 0.04f;
	for (size_t i = 0; i < n; i++) {
		float x = bandpass(p[i], &r->nearHPx1, &r->nearHPy1, &r->nearLPx1, &r->nearLPy1, 0.809f, 0.727f);
		float a = fabsf(x);
		if (a > r->nearEnv) r->nearEnv = a;
		else r->nearEnv *= 0.9995f;
		float gate = r->nearEnv > r->nearGateThresh ? 1.0f : r->nearEnv / r->nearGateThresh;
		p[i] = soft_limiter(x * gain * gate);
		a = fabsf(p[i]);
		if (a > r->nearOutLive) r->nearOutLive = a;
		else r->nearOutLive *= 0.9995f;
	}
}

// Number of frames in an output AudioBuffer. Non-interleaved devices expose
// one buffer per channel with all frames; interleaved devices expose one
// buffer containing all channels.
static UInt32 buffer_frames(const AudioBuffer *b, const AudioStreamBasicDescription *fmt) {
	if (fmt->mFormatFlags & kAudioFormatFlagIsNonInterleaved) {
		return b->mDataByteSize / (fmt->mBitsPerChannel / 8);
	}
	return b->mDataByteSize / fmt->mBytesPerFrame;
}

static void zero_output(AudioBufferList *outData) {
	for (UInt32 i = 0; i < outData->mNumberBuffers; i++) {
		if (outData->mBuffers[i].mData) memset(outData->mBuffers[i].mData, 0, outData->mBuffers[i].mDataByteSize);
	}
}

// Convert one capture buffer (srcFmt) into a canonical float32 mono ring.
// The module's USB gadget sometimes declares float32 but actually streams
// 16-bit PCM (narrowband 8 kHz / wideband 16 kHz); when the float
// interpretation is mostly out-of-range/NaN, the buffer is decoded as 16-bit
// with conv16 and the correct frame count, keeping pitch and rate intact.
static void capture_to_ring(AudioConverterRef conv, AudioConverterRef conv16,
                            const void *srcData, size_t srcBytes,
                            AudioStreamBasicDescription srcFmt,
	                            ringbuf *ring, float *peak, int *is16,
	                            int directFloat, uint8_t *work, size_t workCap) {
	if (srcFmt.mBitsPerChannel == 32 && (srcFmt.mFormatFlags & kAudioFormatFlagIsFloat) && conv16 != NULL) {
		const float *f = (const float *)srcData;
		size_t nf = srcBytes / 4;
		size_t bad = 0;
		float floatPeak = 0.0f;
		for (size_t i = 0; i < nf; i++) {
			float v = f[i];
			if (!(v > -2.0f && v < 2.0f)) {
				if (++bad > nf / 4) break;
				continue;
			}
			float a = v < 0.0f ? -v : v;
			if (a > floatPeak) floatPeak = a;
		}
		// The module's USB gadget sometimes declares float32 but streams 16-bit
		// PCM. Two signatures: (a) mostly out-of-range/NaN when read as float;
		// (b) the float interpretation is subnormal (~0, digital silence) while
		// the 16-bit interpretation carries real signal. Either way the buffer
		// must be decoded as int16 PCM or quiet speech is lost as silence.
		float intPeak = peak16((const int16_t *)srcData, srcBytes / 2);
		int use16 = bad > nf / 4 || (floatPeak < 1e-6f && intPeak > 1e-4f);
		if (use16) {
			fmt_int16(&srcFmt, srcFmt.mSampleRate);
			conv = conv16;
			if (is16) *is16 = 1;
		} else if (is16) {
			*is16 = 0;
		}
	}
	if (srcFmt.mBitsPerChannel == 16) {
		float p = peak16((const int16_t *)srcData, srcBytes / 2);
		if (p > *peak) *peak = p;
	} else if (srcFmt.mBitsPerChannel == 32 && (srcFmt.mFormatFlags & kAudioFormatFlagIsFloat)) {
		float p = peak32f((const float *)srcData, srcBytes / 4);
		if (p > *peak) *peak = p;
	}
	// Native QDC507 Float32/8 kHz/mono needs no conversion at all. This is the
	// same PCM representation used by the far ring, so copy it directly.
	if (directFloat && fmt_is_native_8k_float_mono(&srcFmt)) {
		rb_write(ring, (const uint8_t *)srcData, srcBytes);
		return;
	}
	if (!conv || !work || workCap < sizeof(float)) return;
	UInt32 packets = (UInt32)(srcBytes / srcFmt.mBytesPerPacket);
	if (packets == 0) return;
	src_pull_ctx ctx;
	ctx.data = (const uint8_t *)srcData;
	ctx.len = srcBytes;
	ctx.off = 0;
	ctx.fmt = srcFmt;
	UInt32 framesPerChunk = (UInt32)(workCap / sizeof(float));
	if (framesPerChunk > 2048) framesPerChunk = 2048;
	if (framesPerChunk == 0) return;
	size_t dstBytes = (size_t)framesPerChunk * sizeof(float);
	for (;;) {
		AudioBufferList dstBuf;
		dstBuf.mNumberBuffers = 1;
		dstBuf.mBuffers[0].mData = work;
		dstBuf.mBuffers[0].mDataByteSize = (UInt32)dstBytes;
		dstBuf.mBuffers[0].mNumberChannels = 1;
		UInt32 framesThis = framesPerChunk;
		OSStatus st = AudioConverterFillComplexBuffer(conv, src_pull_proc, &ctx, &framesThis, &dstBuf, NULL);
		(void)st;
		rb_write(ring, work, (size_t)framesThis * sizeof(float));
		if (framesThis < framesPerChunk) break;
	}
}

static OSStatus modIn_proc(AudioDeviceID dev, const AudioTimeStamp *now,
                           const AudioBufferList *inData, const AudioTimeStamp *inTime,
                           AudioBufferList *outData, const AudioTimeStamp *outTime,
                           void *clientData) {
	(void)dev; (void)now; (void)inTime; (void)outData; (void)outTime;
	router *r = (router *)clientData;
	r->modInCalls++;
	if (!inData || inData->mNumberBuffers == 0 || !inData->mBuffers[0].mData) return noErr;
	const AudioBuffer *b = &inData->mBuffers[0];
	// Bounded SPSC ring: producer overflow drops only the newest frames. This
	// avoids both lock waits and the old multi-second backlog-resync behaviour.
	r->farWmLvl = r->farWmLvl * 0.92f + (float)rb_used(&r->farRing) * 0.08f;
	capture_to_ring(r->convToFar, r->convToFar16, b->mData, b->mDataByteSize, r->modInFmt,
	                &r->farRing, &r->farPeak, &r->modInIs16, r->modInDirect,
	                r->farCaptureWork, r->captureWorkCap);
	{
		float p = r->modInIs16 ? peak16((const int16_t *)b->mData, b->mDataByteSize / 2)
		                       : peak32f((const float *)b->mData, b->mDataByteSize / 4);
		if (p > r->farLive) r->farLive = p;
		else r->farLive *= 0.92f;
	}
	if ((r->modInCalls & 255) == 1) {
		const uint8_t *raw = (const uint8_t *)b->mData;
		size_t nb = b->mDataByteSize < 16 ? b->mDataByteSize : 16;
		char hex[40];
		size_t o = 0;
		for (size_t i = 0; i < nb && o + 2 < sizeof(hex); i++) {
			o += snprintf(hex + o, sizeof(hex) - o, "%02x", raw[i]);
		}
		hex[o] = 0;
		float fp = peak32f((const float *)b->mData, b->mDataByteSize / 4);
		float ip = peak16((const int16_t *)b->mData, b->mDataByteSize / 2);
		fprintf(stderr, "voice raw diag: mode=%s floatPeak=%.6f intPeak=%.4f bytes=%u hex=%s\n",
		        r->modInIs16 ? "int16" : "float32", fp, ip, (unsigned int)b->mDataByteSize, hex);
	}
	return noErr;
}

static OSStatus macIn_proc(AudioDeviceID dev, const AudioTimeStamp *now,
                           const AudioBufferList *inData, const AudioTimeStamp *inTime,
                           AudioBufferList *outData, const AudioTimeStamp *outTime,
                           void *clientData) {
	(void)dev; (void)now; (void)inTime; (void)outData; (void)outTime;
	router *r = (router *)clientData;
	r->macInCalls++;
	if (!inData || inData->mNumberBuffers == 0 || !inData->mBuffers[0].mData) return noErr;
	const AudioBuffer *b = &inData->mBuffers[0];
	{
		float p = peak32f((const float *)b->mData, b->mDataByteSize / 4);
		if (p > r->nearLive) r->nearLive = p;
		else r->nearLive *= 0.92f;
	}
	capture_to_ring(r->convToNear, NULL, b->mData, b->mDataByteSize, r->macInFmt,
	                &r->nearRing, &r->nearPeak, NULL, 0,
	                r->nearCaptureWork, r->captureWorkCap);
	return noErr;
}

// macOut converter input: pull 8 kHz float32 mono far audio from the ring.
// This is where the far stream gets its gain/envelope tracking (process_far_block)
// and recording, before the fixed SRC to the device format — the 8k side of the
// path, matching the old pre-filter dump semantics (raw far, no DSP).
typedef struct {
	ringbuf *ring;
	router *r;
} far_pull_ctx;

static OSStatus macOut_pull_proc(AudioConverterRef inConverter, UInt32 *ioNumberDataPackets,
                                 AudioBufferList *ioData,
                                 AudioStreamPacketDescription **outDataPacketDescription,
                                 void *inUserData) {
	(void)inConverter;
	(void)outDataPacketDescription;
	far_pull_ctx *ctx = (far_pull_ctx *)inUserData;
	UInt32 frames = *ioNumberDataPackets;
	size_t want = (size_t)frames * sizeof(float);
	uint8_t *dst = (uint8_t *)ioData->mBuffers[0].mData;
	size_t got = rb_read(ctx->ring, dst, want);
	if (got < want) {
		memset(dst + got, 0, want - got);
		ctx->r->macOutUnderruns++;
	}
	// Debug PCM export is intentionally disabled here: disk I/O in this callback
	// can block the shared mixer and create the very burst we are measuring.
	process_far_block((float *)dst, frames, ctx->r);
	if (atomic_load_explicit(&ctx->r->rec.on, memory_order_acquire)) rb_write(&ctx->r->rec.farRec, dst, want);
	ioData->mBuffers[0].mDataByteSize = (UInt32)want;
	ioData->mBuffers[0].mNumberChannels = 1;
	*ioNumberDataPackets = frames;
	return noErr;
}

// macOut IOProc: the device calls us at its real rate; we ask the fixed
// convMacOut (8 kHz -> device format, 6:1 integer ratio) for the output frames.
// Because the ratio is fixed and integer, the ring drains at exactly the device's
// consumption divided by 6 — the supply matches consumption, so no saturation
// resync and no dropouts. This is CellDock's architecture (8 kHz out, clean SRC,
// no servo/filter) implemented without AudioQueue.
static OSStatus macOut_proc(AudioDeviceID dev, const AudioTimeStamp *now,
                            const AudioBufferList *inData, const AudioTimeStamp *inTime,
                            AudioBufferList *outData, const AudioTimeStamp *outTime,
                            void *clientData) {
	(void)dev; (void)now; (void)inData; (void)inTime; (void)outTime;
	router *r = (router *)clientData;
	r->macOutCalls++;
	if (!outData || outData->mNumberBuffers == 0) return noErr;
	if (r->muted) { zero_output(outData); return noErr; }
	UInt32 outFrames = buffer_frames(&outData->mBuffers[0], &r->macOutFmt);
	zero_output(outData);
	far_pull_ctx ctx;
	ctx.ring = &r->farRing;
	ctx.r = r;
	UInt32 framesThis = outFrames;
	OSStatus st = AudioConverterFillComplexBuffer(r->convMacOut, macOut_pull_proc, &ctx, &framesThis, outData, NULL);
	// macOut_pull_proc always zero-fills, so a non-noErr return is unexpected;
	// log it rarely to avoid flooding the log on a pathological device.
	if (st != noErr && (r->macOutCalls & 1023) == 0) {
		fprintf(stderr, "voice macOut conv: st=%d frames=%u\n", (int)st, (unsigned)framesThis);
	}
	return noErr;
}

// Hand the original 8 kHz mono stream to the macOS shared mixer. This lets
// CoreAudio own the output-device clock and SRC, rather than running a second
// app-side resampler before AudioQueue (the remaining mismatch with CellDock).
static void mac_queue_fill(AudioQueueRef queue, AudioQueueBufferRef buffer, router *r) {
	UInt32 outFrames = buffer->mAudioDataBytesCapacity / r->farFmt.mBytesPerFrame;
	memset(buffer->mAudioData, 0, buffer->mAudioDataBytesCapacity);
	far_pull_ctx ctx;
	ctx.ring = &r->farRing;
	ctx.r = r;
	UInt32 framesThis = outFrames;
	AudioBufferList out;
	out.mNumberBuffers = 1;
	out.mBuffers[0].mNumberChannels = 1;
	out.mBuffers[0].mDataByteSize = buffer->mAudioDataBytesCapacity;
	out.mBuffers[0].mData = buffer->mAudioData;
	macOut_pull_proc(NULL, &framesThis, &out, NULL, &ctx);
	buffer->mAudioDataByteSize = (UInt32)((size_t)framesThis * sizeof(float));
	r->macOutCalls++;
	AudioQueueEnqueueBuffer(queue, buffer, 0, NULL);
}

static void mac_queue_proc(void *userData, AudioQueueRef queue, AudioQueueBufferRef buffer) {
	router *r = (router *)userData;
	if (!r || r->muted) {
		memset(buffer->mAudioData, 0, buffer->mAudioDataBytesCapacity);
		buffer->mAudioDataByteSize = buffer->mAudioDataBytesCapacity;
		AudioQueueEnqueueBuffer(queue, buffer, 0, NULL);
		return;
	}
	mac_queue_fill(queue, buffer, r);
}

// Create a native 8 kHz shared-mixer path. CoreAudio adapts this to whatever
// sample rate the selected output device currently uses.
static int mac_out_start(router *r) {
	OSStatus st = AudioQueueNewOutput(&r->farFmt, mac_queue_proc, r, NULL, NULL, 0, &r->macQueue);
	if (st != noErr) { fprintf(stderr, "mac_out_start: AudioQueueNewOutput failed: %d\n", (int)st); return (int)st; }
	// Three 20 ms 8 kHz buffers keep latency low while giving the shared mixer
	// enough runway for normal callback jitter.
	for (int i = 0; i < 3; i++) {
		AudioQueueBufferRef buffer = NULL;
		st = AudioQueueAllocateBuffer(r->macQueue, 160 * r->farFmt.mBytesPerFrame, &buffer);
		if (st != noErr || !buffer) { fprintf(stderr, "mac_out_start: AudioQueueAllocateBuffer failed: %d\n", (int)st); AudioQueueDispose(r->macQueue, true); r->macQueue = NULL; return (int)st; }
		mac_queue_fill(r->macQueue, buffer, r);
	}
	st = AudioQueueStart(r->macQueue, NULL);
	if (st != noErr) { fprintf(stderr, "mac_out_start: AudioQueueStart failed: %d\n", (int)st); AudioQueueDispose(r->macQueue, true); r->macQueue = NULL; return (int)st; }
	return 0;
}

static void mac_out_stop(router *r) {
	if (r->macQueue) {
		AudioQueueStop(r->macQueue, true);
		AudioQueueDispose(r->macQueue, true);
		r->macQueue = NULL;
	}
	if (r->macOutProc) {
		AudioDeviceStop(r->macOut, r->macOutProc);
		AudioDeviceDestroyIOProcID(r->macOut, r->macOutProc);
		r->macOutProc = NULL;
	}
	if (r->convMacOut) { AudioConverterDispose(r->convMacOut); r->convMacOut = NULL; }
}

static OSStatus modOut_proc(AudioDeviceID dev, const AudioTimeStamp *now,
                            const AudioBufferList *inData, const AudioTimeStamp *inTime,
                            AudioBufferList *outData, const AudioTimeStamp *outTime,
                            void *clientData) {
	(void)dev; (void)now; (void)inData; (void)inTime; (void)outTime;
	router *r = (router *)clientData;
	r->modOutCalls++;
	if (!outData || outData->mNumberBuffers == 0) return noErr;
	if (r->muted) {
		zero_output(outData);
		return noErr;
	}
	UInt32 frames = buffer_frames(&outData->mBuffers[0], &r->modOutFmt);
	size_t wantBytes = (size_t)frames * sizeof(float);
	if (!r->nearOutWork || wantBytes > r->nearOutWorkCap) {
		zero_output(outData);
		return noErr;
	}
	uint8_t *canon = r->nearOutWork;
	size_t got = rb_read(&r->nearRing, canon, wantBytes);
	if (got < wantBytes) memset(canon + got, 0, wantBytes - got);
	process_near_block((float *)canon, frames, r);
	if (atomic_load_explicit(&r->rec.on, memory_order_acquire)) rb_write(&r->rec.nearRec, canon, wantBytes);
	zero_output(outData);
	if (r->modOutDirect) {
		AudioBuffer *dst = &outData->mBuffers[0];
		size_t copy = wantBytes < dst->mDataByteSize ? wantBytes : dst->mDataByteSize;
		memcpy(dst->mData, canon, copy);
		return noErr;
	}
	src_pull_ctx ctx;
	ctx.data = canon;
	ctx.len = wantBytes;
	ctx.off = 0;
	ctx.fmt = r->nearFmt;
	UInt32 framesThis = frames;
	OSStatus st = AudioConverterFillComplexBuffer(r->convModOut, src_pull_proc, &ctx, &framesThis, outData, NULL);
	(void)st;
	return noErr;
}

// ---- device discovery ----
static UInt32 device_channel_count(AudioDeviceID dev, AudioObjectPropertyScope scope) {
	UInt32 size = 0;
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyStreamConfiguration, scope, 0};
	if (AudioObjectGetPropertyDataSize(dev, &addr, 0, NULL, &size) != noErr || size < sizeof(AudioBufferList)) return 0;
	AudioBufferList *buffers = (AudioBufferList *)malloc(size);
	if (!buffers) return 0;
	UInt32 channels = 0;
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, buffers) == noErr) {
		for (UInt32 i = 0; i < buffers->mNumberBuffers; i++) channels += buffers->mBuffers[i].mNumberChannels;
	}
	free(buffers);
	return channels;
}

static int device_is_baiwang_usb_uac(AudioDeviceID dev) {
	UInt32 transport = 0;
	UInt32 size = sizeof(transport);
	AudioObjectPropertyAddress transportAddr = {kAudioDevicePropertyTransportType,
		kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	if (AudioObjectGetPropertyData(dev, &transportAddr, 0, NULL, &size, &transport) != noErr ||
		transport != kAudioDeviceTransportTypeUSB) return 0;
	char manufacturer[128] = {0};
	size = sizeof(manufacturer);
	AudioObjectPropertyAddress manufacturerAddr = {kAudioDevicePropertyDeviceManufacturer,
		kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	return AudioObjectGetPropertyData(dev, &manufacturerAddr, 0, NULL, &size, manufacturer) == noErr &&
		strcmp(manufacturer, "BAIWANG") == 0;
}

static int device_supports_nominal_rate(AudioDeviceID dev, Float64 rate) {
	UInt32 size = 0;
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyAvailableNominalSampleRates,
		kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	if (AudioObjectGetPropertyDataSize(dev, &addr, 0, NULL, &size) != noErr || size < sizeof(AudioValueRange)) return 0;
	AudioValueRange *ranges = (AudioValueRange *)malloc(size);
	if (!ranges) return 0;
	int supported = 0;
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, ranges) == noErr) {
		for (UInt32 i = 0; i < size / sizeof(AudioValueRange); i++) {
			if (ranges[i].mMinimum <= rate && rate <= ranges[i].mMaximum) { supported = 1; break; }
		}
	}
	free(ranges);
	return supported;
}

static int module_device_supports_8k_mono(AudioDeviceID dev, AudioObjectPropertyScope scope) {
	return device_is_baiwang_usb_uac(dev) &&
		device_channel_count(dev, scope) == 1 &&
		device_supports_nominal_rate(dev, 8000.0);
}

static AudioDeviceID find_module_device(int wantInput) {
	UInt32 size = 0;
	AudioObjectPropertyAddress addr = {kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &addr, 0, NULL, &size) != noErr) return 0;
	int n = size / sizeof(AudioDeviceID);
	AudioDeviceID *devs = (AudioDeviceID *)malloc(size);
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, devs) != noErr) {
		free(devs);
		return 0;
	}
	AudioObjectPropertyScope scope = wantInput ? kAudioDevicePropertyScopeInput : kAudioDevicePropertyScopeOutput;
	AudioDeviceID found = 0;
	for (int i = 0; i < n; i++) {
		if (module_device_supports_8k_mono(devs[i], scope)) { found = devs[i]; break; }
	}
	free(devs);
	return found;
}

static AudioDeviceID get_default_device(AudioObjectPropertySelector sel) {
	AudioDeviceID dev = kAudioObjectUnknown;
	UInt32 size = sizeof(dev);
	AudioObjectPropertyAddress addr = {sel, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, &dev) != noErr) return 0;
	return dev;
}

static void get_device_name(AudioDeviceID dev, char *buf, size_t n) {
	UInt32 size = (UInt32)n;
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyDeviceName,
		kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, buf) != noErr) {
		strncpy(buf, "?", n - 1);
		buf[n - 1] = 0;
		return;
	}
	buf[n - 1] = 0;
}

static int get_stream_format(AudioDeviceID dev, AudioObjectPropertyScope scope, AudioStreamBasicDescription *out) {
	UInt32 size = sizeof(*out);
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyStreamFormat, scope, kAudioObjectPropertyElementMain};
	return AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, out);
}

static int module_format_is_8k_mono_pcm(const AudioStreamBasicDescription *fmt) {
	return fmt && fmt->mFormatID == kAudioFormatLinearPCM &&
		fabs(fmt->mSampleRate - 8000.0) < 0.5 && fmt->mChannelsPerFrame == 1;
}

static int get_nominal_rate(AudioDeviceID dev, Float64 *out) {
	UInt32 size = sizeof(*out);
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyNominalSampleRate,
		kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	return AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, out) == noErr;
}

static int set_nominal_rate(AudioDeviceID dev, Float64 rate) {
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyNominalSampleRate,
		kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	Boolean settable = false;
	return AudioObjectIsPropertySettable(dev, &addr, &settable) == noErr && settable &&
		AudioObjectSetPropertyData(dev, &addr, 0, NULL, sizeof(rate), &rate) == noErr;
}

static int lock_module_nominal_rate(AudioDeviceID dev, Float64 *original, int *overridden) {
	if (!device_supports_nominal_rate(dev, 8000.0) || !get_nominal_rate(dev, original)) return 0;
	*overridden = 0;
	if (fabs(*original - 8000.0) < 0.5) return 1;
	if (!set_nominal_rate(dev, 8000.0)) return 0;
	*overridden = 1;
	Float64 applied = 0;
	return get_nominal_rate(dev, &applied) && fabs(applied - 8000.0) < 0.5;
}

static void restore_module_nominal_rates(router *r) {
	if (!r) return;
	if (r->modInRateOverridden) set_nominal_rate(r->modIn, r->modInOriginalRate);
	if (r->modOutRateOverridden && r->modOut != r->modIn) set_nominal_rate(r->modOut, r->modOutOriginalRate);
	r->modInRateOverridden = 0;
	r->modOutRateOverridden = 0;
}

// Actual sample rate is diagnostic only. Module-side routing remains at the
// verified 8 kHz nominal rate and must never substitute this value into a
// converter stream description.
static Float64 get_device_actual_rate(AudioDeviceID dev, AudioObjectPropertyScope scope) {
	AudioObjectPropertyAddress addr = {kAudioDevicePropertyActualSampleRate, scope, kAudioObjectPropertyElementMain};
	Float64 rate = 0;
	UInt32 size = sizeof(rate);
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, &rate) != noErr) return 0;
	if (!(rate > 0)) return 0;
	return rate;
}

// True when the device is the DJI/BAIWANG module's own USB audio device.
static int is_module_audio_device(AudioDeviceID dev) {
	return device_is_baiwang_usb_uac(dev);
}

static int device_has_channels(AudioDeviceID dev, AudioObjectPropertyScope scope) {
	UInt32 sz = 0;
	AudioObjectPropertyAddress ca = {kAudioDevicePropertyStreamConfiguration, scope, 0};
	if (AudioObjectGetPropertyDataSize(dev, &ca, 0, NULL, &sz) != noErr) return 0;
	AudioBufferList *bl = (AudioBufferList *)malloc(sz);
	UInt32 ch = 0;
	if (AudioObjectGetPropertyData(dev, &ca, 0, NULL, &sz, bl) == noErr) {
		for (UInt32 k = 0; k < bl->mNumberBuffers; k++) ch += bl->mBuffers[k].mNumberChannels;
	}
	free(bl);
	return ch > 0;
}

// The Mac's default input/output device, but never the module's own USB
// audio device. macOS can make the dongle the default output (or input) when
// it is plugged in; routing the call to the dongle would silence the Mac
// speakers and use the dongle's microphone instead of the Mac's.
static AudioDeviceID get_mac_default_device(AudioObjectPropertySelector sel,
                                            AudioObjectPropertyScope scope) {
	AudioDeviceID dev = get_default_device(sel);
	if (dev != 0 && !is_module_audio_device(dev)) return dev;
	UInt32 size = 0;
	AudioObjectPropertyAddress addr = {kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
	if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &addr, 0, NULL, &size) != noErr) return 0;
	int n = size / sizeof(AudioDeviceID);
	AudioDeviceID *devs = (AudioDeviceID *)malloc(size);
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, devs) != noErr) {
		free(devs);
		return 0;
	}
	AudioDeviceID found = 0;
	for (int i = 0; i < n; i++) {
		if (is_module_audio_device(devs[i])) continue;
		if (!device_has_channels(devs[i], scope)) continue;
		found = devs[i];
		break;
	}
	free(devs);
	return found;
}

static int make_conv(const AudioStreamBasicDescription *src, const AudioStreamBasicDescription *dst, AudioConverterRef *out) {
	AudioConverterRef conv = NULL;
	OSStatus st = AudioConverterNew(src, dst, &conv);
	if (st != noErr) return (int)st;
	// MUST set the converter complexity explicitly. Without it AudioConverter
	// defaults to kAudioConverterSampleRateConverterComplexity_Linear, whose
	// 8 kHz -> ~44.7 kHz upsampling leaves aliasing images only ~24 dB down —
	// that broadband skirt above 8 kHz is the residual "滋滋". CellDock sidesteps
	// this by handing 8 kHz PCM16 to the system mixer, whose SRC is the same
	// Normal/Mastering class. (CellDock measured clean in A/B; this was the gap.)
	UInt32 complexity = kAudioConverterSampleRateConverterComplexity_Normal;
	OSStatus ps = AudioConverterSetProperty(conv, kAudioConverterSampleRateConverterComplexity, sizeof(complexity), &complexity);
	UInt32 quality = kAudioConverterQuality_Max;
	OSStatus pq = AudioConverterSetProperty(conv, kAudioConverterSampleRateConverterQuality, sizeof(quality), &quality);
	UInt32 got = 0; UInt32 gsz = sizeof(got);
	OSStatus pg = AudioConverterGetProperty(conv, kAudioConverterSampleRateConverterComplexity, &gsz, &got);
	fprintf(stderr, "make_conv %.0f->%.0f ch%d/%d: set=%s/%s readback=%u (%s)\n",
	        src->mSampleRate, dst->mSampleRate, (int)src->mChannelsPerFrame, (int)dst->mChannelsPerFrame,
	        ps == noErr ? "OK" : "FAIL", pq == noErr ? "OK" : "FAIL", (unsigned)got,
	        got == kAudioConverterSampleRateConverterComplexity_Normal ? "Normal" :
	        (got == kAudioConverterSampleRateConverterComplexity_Linear ? "Linear" : "?"));
	(void)pg;
	*out = conv;
	return 0;
}

// ---- lifecycle ----
static int router_rec_start(router *r, const char *path) {
	if (atomic_load_explicit(&r->rec.on, memory_order_acquire)) return 1;
	const char *preDump = getenv("DJONEHUB_FAR_PRE_DUMP");
	if (preDump && *preDump) fprintf(stderr, "far pre-filter dump disabled during real-time routing\n");
	FILE *file = fopen(path, "wb");
	if (!file || rec_wav_header(file, 0) != 0) {
		if (file) fclose(file);
		return 1;
	}
	rb_clear(&r->rec.farRec);
	rb_clear(&r->rec.nearRec);
	r->rec.file = file;
	r->rec.wavBytes = 0;
	r->rec.err = 0;
	r->rec.nearCorruptBlocks = 0;
	atomic_store_explicit(&r->rec.on, 1, memory_order_release);
	if (!atomic_load_explicit(&r->rec.threadRun, memory_order_acquire)) {
		atomic_store_explicit(&r->rec.threadRun, 1, memory_order_release);
		pthread_create(&r->rec.thread, NULL, rec_thread, &r->rec);
	}
	return 0;
}

static void router_rec_stop(router *r) {
	if (!r) return;
	atomic_store_explicit(&r->rec.on, 0, memory_order_release);
	if (atomic_load_explicit(&r->rec.threadRun, memory_order_acquire)) {
		atomic_store_explicit(&r->rec.threadRun, 0, memory_order_release);
		pthread_join(r->rec.thread, NULL);
	}
	if (r->rec.file) {
		rec_wav_header(r->rec.file, r->rec.wavBytes);
		fclose(r->rec.file);
		r->rec.file = NULL;
	}
}

static int router_rec_on(router *r) {
	if (!r) return 0;
	return atomic_load_explicit(&r->rec.on, memory_order_acquire);
}

// Called from the non-realtime MaVo bridge pump/capture queues. The UAC
// callbacks themselves only touch the bounded PCM16 SPSC rings in
// mavo_uac_probe.c, matching the verified reference architecture.
void dj_router_record_far_pcm16(void *context, const int16_t *samples, size_t frames) {
	router *r = (router *)context;
	if (!r || !samples || frames == 0) return;
	float converted[512];
	while (frames > 0) {
		size_t count = frames > 512 ? 512 : frames;
		float peak = 0.0f;
		for (size_t i = 0; i < count; i++) {
			converted[i] = (float)samples[i] / 32768.0f;
			float level = fabsf(converted[i]);
			if (level > peak) peak = level;
		}
		if (atomic_load_explicit(&r->rec.on, memory_order_acquire)) {
			rb_write(&r->rec.farRec, (const uint8_t *)converted, count * sizeof(float));
		}
		if (peak > r->farPeak) r->farPeak = peak;
		r->farLive = peak;
		r->farOutLive = peak;
		r->modInCalls++;
		r->macOutCalls++;
		samples += count;
		frames -= count;
	}
}

int dj_router_recording_active(void *context) {
	router *r = (router *)context;
	return r && atomic_load_explicit(&r->rec.on, memory_order_acquire);
}

void dj_router_record_near_pcm16(void *context, const int16_t *samples, size_t frames) {
	router *r = (router *)context;
	if (!r || !samples || frames == 0) return;
	float converted[512];
	while (frames > 0) {
		size_t count = frames > 512 ? 512 : frames;
		float peak = 0.0f;
		for (size_t i = 0; i < count; i++) {
			converted[i] = (float)samples[i] / 32768.0f;
			float level = fabsf(converted[i]);
			if (level > peak) peak = level;
		}
		if (atomic_load_explicit(&r->rec.on, memory_order_acquire)) {
			rb_write(&r->rec.nearRec, (const uint8_t *)converted, count * sizeof(float));
		}
		if (peak > r->nearPeak) r->nearPeak = peak;
		r->nearLive = peak;
		r->nearOutLive = peak;
		r->macInCalls++;
		r->modOutCalls++;
		samples += count;
		frames -= count;
	}
}

static router *router_start_legacy(void) {
	router *r = (router *)calloc(1, sizeof(router));
	if (!r) return NULL;
	atomic_init(&r->rec.on, 0);
	atomic_init(&r->rec.threadRun, 0);
	r->modIn = find_module_device(1);
	r->modOut = find_module_device(0);
	r->macIn = get_mac_default_device(kAudioHardwarePropertyDefaultInputDevice, kAudioDevicePropertyScopeInput);
	r->macOut = get_mac_default_device(kAudioHardwarePropertyDefaultOutputDevice, kAudioDevicePropertyScopeOutput);
	if (!r->modIn || !r->modOut || !r->macIn || !r->macOut) {
		router_set_err(r, "未找到模块音频设备或 Mac 默认音频设备");
		free(r);
		return NULL;
	}
	if (!lock_module_nominal_rate(r->modIn, &r->modInOriginalRate, &r->modInRateOverridden) ||
	    (r->modOut != r->modIn && !lock_module_nominal_rate(r->modOut, &r->modOutOriginalRate, &r->modOutRateOverridden))) {
		router_set_err(r, "模块 USB Audio 无法锁定为 8 kHz");
		restore_module_nominal_rates(r);
		free(r);
		return NULL;
	}
	if (r->modOut == r->modIn) r->modOutOriginalRate = r->modInOriginalRate;
	get_device_name(r->modIn, r->modInName, sizeof(r->modInName));
	get_device_name(r->modOut, r->modOutName, sizeof(r->modOutName));
	get_device_name(r->macIn, r->macInName, sizeof(r->macInName));
	get_device_name(r->macOut, r->macOutName, sizeof(r->macOutName));
	if (get_stream_format(r->modIn, kAudioDevicePropertyScopeInput, &r->modInFmt) != noErr ||
	    get_stream_format(r->modOut, kAudioDevicePropertyScopeOutput, &r->modOutFmt) != noErr ||
	    get_stream_format(r->macIn, kAudioDevicePropertyScopeInput, &r->macInFmt) != noErr ||
	    get_stream_format(r->macOut, kAudioDevicePropertyScopeOutput, &r->macOutFmt) != noErr) {
		router_set_err(r, "读取音频设备格式失败");
		restore_module_nominal_rates(r);
		free(r);
		return NULL;
	}
	if (!module_format_is_8k_mono_pcm(&r->modInFmt) || !module_format_is_8k_mono_pcm(&r->modOutFmt)) {
		router_set_err(r, "模块 USB Audio 未提供 8 kHz 单声道 PCM");
		restore_module_nominal_rates(r);
		free(r);
		return NULL;
	}
	Float64 modInActual = get_device_actual_rate(r->modIn, kAudioDevicePropertyScopeInput);
	Float64 macOutActual = get_device_actual_rate(r->macOut, kAudioDevicePropertyScopeOutput);
	// The module side is locked and validated at 8 kHz before converters are
	// created. Actual rate remains log-only so it cannot distort the chain.
	snprintf(r->fmtInfo, sizeof(r->fmtInfo),
		"modIn=%g/%u/%u/%u (nominal %g%s) modOut=%g/%u/%u/%u (nominal %g%s) macIn=%g/%u/%u/%u macOut=%g/%u/%u/%u actIn=%g actOut=%g",
		r->modInFmt.mSampleRate, r->modInFmt.mChannelsPerFrame, r->modInFmt.mBitsPerChannel, r->modInFmt.mFormatFlags,
		r->modInOriginalRate, r->modInRateOverridden ? "->8000" : "",
		r->modOutFmt.mSampleRate, r->modOutFmt.mChannelsPerFrame, r->modOutFmt.mBitsPerChannel, r->modOutFmt.mFormatFlags,
		r->modOutOriginalRate, r->modOutRateOverridden ? "->8000" : "",
		r->macInFmt.mSampleRate, r->macInFmt.mChannelsPerFrame, r->macInFmt.mBitsPerChannel, r->macInFmt.mFormatFlags,
		r->macOutFmt.mSampleRate, r->macOutFmt.mChannelsPerFrame, r->macOutFmt.mBitsPerChannel, r->macOutFmt.mFormatFlags,
		modInActual, macOutActual);
	fmt_float(&r->farFmt, 8000, 1);
	fmt_float(&r->nearFmt, 8000, 1);
	r->modInDirect = fmt_is_native_8k_float_mono(&r->modInFmt);
	r->modOutDirect = fmt_is_native_8k_float_mono(&r->modOutFmt);
	AudioStreamBasicDescription modInFmt16;
	fmt_int16(&modInFmt16, r->modInFmt.mSampleRate > 0 ? r->modInFmt.mSampleRate : 8000);
	if ((!r->modInDirect && make_conv(&r->modInFmt, &r->farFmt, &r->convToFar) != 0) ||
	    make_conv(&modInFmt16, &r->farFmt, &r->convToFar16) != 0 ||
	    make_conv(&r->macInFmt, &r->nearFmt, &r->convToNear) != 0 ||
	    (!r->modOutDirect && make_conv(&r->nearFmt, &r->modOutFmt, &r->convModOut) != 0)) {
		router_set_err(r, "创建音频转换器失败");
		router_stop(r);
		free(r);
		return NULL;
	}
	// 256 ms at 8 kHz Float32, matching the bounded UAC bridge used by MaVo.
	// Keeping this short prevents stale voice from ever accumulating behind a
	// callback jitter event; overflow becomes a short silence instead.
	rb_init(&r->farRing, 2048 * sizeof(float));
	rb_init(&r->nearRing, 2048 * sizeof(float));
	rb_init(&r->rec.farRec, 512 * 1024);
	rb_init(&r->rec.nearRec, 64 * 1024);
	r->captureWorkCap = 64 * 1024;
	r->nearOutWorkCap = 64 * 1024;
	r->farCaptureWork = (uint8_t *)malloc(r->captureWorkCap);
	r->nearCaptureWork = (uint8_t *)malloc(r->captureWorkCap);
	r->nearOutWork = (uint8_t *)malloc(r->nearOutWorkCap);
	if (!r->farCaptureWork || !r->nearCaptureWork || !r->nearOutWork) {
		router_set_err(r, "分配音频工作缓冲失败");
		router_stop(r);
		free(r);
		return NULL;
	}
	fprintf(stderr, "voice module native path: input=%s output=%s; callback buffers preallocated\n",
	        r->modInDirect ? "direct" : "converter", r->modOutDirect ? "direct" : "converter");
	r->farGain = 0.45f;
	lpf4_init(&r->farLpf, 3000.0f, 8000.0f);
	fprintf(stderr, "far chain ready: 8k -> LPF@3000 (linear 4th Butter) -> gain=%.2f -> CoreAudio shared mixer\n",
	        r->farGain);
	r->nearGain = 0.7f;
	r->farGateThresh = 0.004f;
	r->nearGateThresh = 0.001f;
	OSStatus st;
	st = AudioDeviceCreateIOProcID(r->modIn, modIn_proc, r, &r->modInProc);
	if (st != noErr) { router_set_err(r, "打开模块输入失败"); router_stop(r); free(r); return NULL; }
	st = AudioDeviceCreateIOProcID(r->modOut, modOut_proc, r, &r->modOutProc);
	if (st != noErr) { router_set_err(r, "打开模块输出失败"); router_stop(r); free(r); return NULL; }
	st = AudioDeviceCreateIOProcID(r->macIn, macIn_proc, r, &r->macInProc);
	if (st != noErr) { router_set_err(r, "打开 Mac 麦克风失败"); router_stop(r); free(r); return NULL; }
	if (mac_out_start(r) != 0) { router_set_err(r, "打开 Mac 扬声器失败"); router_stop(r); free(r); return NULL; }
	AudioDeviceStart(r->modIn, r->modInProc);
	AudioDeviceStart(r->modOut, r->modOutProc);
	AudioDeviceStart(r->macIn, r->macInProc);
	r->running = 1;
	return r;
}

static void router_stop(router *r) {
	if (!r) return;
	if (r->mavoBridge) {
		dj_mavo_uac_bridge_stop(r->mavoBridge);
		r->mavoBridge = NULL;
		router_rec_stop(r);
		if (r->rec.farRec.buf) { free(r->rec.farRec.buf); r->rec.farRec.buf = NULL; }
		if (r->rec.nearRec.buf) { free(r->rec.nearRec.buf); r->rec.nearRec.buf = NULL; }
		r->running = 0;
		return;
	}
	if (r->modInProc) { AudioDeviceStop(r->modIn, r->modInProc); AudioDeviceDestroyIOProcID(r->modIn, r->modInProc); r->modInProc = NULL; }
	if (r->modOutProc) { AudioDeviceStop(r->modOut, r->modOutProc); AudioDeviceDestroyIOProcID(r->modOut, r->modOutProc); r->modOutProc = NULL; }
	if (r->macInProc) { AudioDeviceStop(r->macIn, r->macInProc); AudioDeviceDestroyIOProcID(r->macIn, r->macInProc); r->macInProc = NULL; }
	mac_out_stop(r);
	if (r->convToFar) AudioConverterDispose(r->convToFar);
	if (r->convToFar16) AudioConverterDispose(r->convToFar16);
	if (r->convToNear) AudioConverterDispose(r->convToNear);
	if (r->convModOut) AudioConverterDispose(r->convModOut);
	if (r->farCaptureWork) free(r->farCaptureWork);
	if (r->nearCaptureWork) free(r->nearCaptureWork);
	if (r->nearOutWork) free(r->nearOutWork);
	if (r->farRing.buf) free(r->farRing.buf);
	if (r->nearRing.buf) free(r->nearRing.buf);
	router_rec_stop(r);
	if (r->rec.farRec.buf) free(r->rec.farRec.buf);
	if (r->rec.nearRec.buf) free(r->rec.nearRec.buf);
	restore_module_nominal_rates(r);
	r->running = 0;
}

static router *router_start(uint16_t vendor, uint16_t product, uint32_t location) {
	router *r = (router *)calloc(1, sizeof(router));
	if (!r) return NULL;
	atomic_init(&r->rec.on, 0);
	atomic_init(&r->rec.threadRun, 0);
	rb_init(&r->rec.farRec, 512 * 1024);
	rb_init(&r->rec.nearRec, 64 * 1024);
	if (!r->rec.farRec.buf || !r->rec.nearRec.buf) {
		router_stop(r);
		free(r);
		return NULL;
	}
	r->mavoBridge = dj_mavo_uac_bridge_start(
		r, vendor, product, location, r->err, sizeof(r->err));
	if (!r->mavoBridge) {
		fprintf(stderr, "MaVo audio bridge start failed: %s\n", r->err);
		router_stop(r);
		free(r);
		return NULL;
	}
	const char *name = dj_mavo_uac_bridge_name(r->mavoBridge);
	snprintf(r->modInName, sizeof(r->modInName), "%s", name && *name ? name : "QDC507 UAC");
	snprintf(r->modOutName, sizeof(r->modOutName), "%s", name && *name ? name : "QDC507 UAC");
	snprintf(r->macInName, sizeof(r->macInName), "AVAudioEngine input");
	snprintf(r->macOutName, sizeof(r->macOutName), "AVAudioEngine main mixer");
	snprintf(r->fmtInfo, sizeof(r->fmtInfo),
		"MaVo parity: USB %04x:%04x@0x%08x; UAC PCM16 mono 8000; AVAudioEngine",
		(unsigned)vendor, (unsigned)product, (unsigned)location);
	r->running = 1;
	return r;
}

static void router_set_muted(router *r, int muted) {
	if (!r) return;
	r->muted = muted;
	if (r->mavoBridge) dj_mavo_uac_bridge_set_muted(r->mavoBridge, muted);
}

static void router_peaks(router *r, float *farPeak, float *nearPeak) {
	if (!r) { *farPeak = 0; *nearPeak = 0; return; }
	*farPeak = r->farPeak;
	*nearPeak = r->nearPeak;
	// decay so stale peaks do not stick forever
	r->farPeak *= 0.95f;
	r->nearPeak *= 0.95f;
}

static void router_stats(router *r, long *mi, long *mo, long *ki, long *ko,
                         long *farUsed, long *nearUsed) {
	if (!r) { *mi=*mo=*ki=*ko=*farUsed=*nearUsed=0; return; }
	if (r->mavoBridge) {
		uint64_t inputCallbacks = 0, outputCallbacks = 0, inputFrames = 0, outputFrames = 0;
		dj_mavo_uac_bridge_stats(r->mavoBridge, &inputCallbacks, &outputCallbacks, &inputFrames, &outputFrames);
		*mi = (long)inputCallbacks;
		*mo = (long)outputCallbacks;
		*ki = r->macInCalls;
		*ko = r->macOutCalls;
		*farUsed = (long)inputFrames;
		*nearUsed = (long)outputFrames;
		return;
	}
	*mi = r->modInCalls;
	*mo = r->modOutCalls;
	*ki = r->macInCalls;
	*ko = r->macOutCalls;
	*farUsed = (long)rb_used(&r->farRing);
	*nearUsed = (long)rb_used(&r->nearRing);
}

static void router_device_names(router *r,
                                char *modIn, size_t modInN,
                                char *modOut, size_t modOutN,
                                char *macIn, size_t macInN,
                                char *macOut, size_t macOutN) {
	const char *mi = "?", *mo = "?", *ki = "?", *ko = "?";
	if (r) { mi = r->modInName; mo = r->modOutName; ki = r->macInName; ko = r->macOutName; }
	strncpy(modIn, mi, modInN - 1); modIn[modInN - 1] = 0;
	strncpy(modOut, mo, modOutN - 1); modOut[modOutN - 1] = 0;
	strncpy(macIn, ki, macInN - 1); macIn[macInN - 1] = 0;
	strncpy(macOut, ko, macOutN - 1); macOut[macOutN - 1] = 0;
}

static void router_live(router *r, float *farLive, float *nearLive) {
	if (!r) { *farLive = 0; *nearLive = 0; return; }
	*farLive = r->farLive;
	*nearLive = r->nearLive;
}

static void router_live_out(router *r, float *farOutLive, float *nearOutLive) {
	if (!r) { *farOutLive = 0; *nearOutLive = 0; return; }
	*farOutLive = r->farOutLive;
	*nearOutLive = r->nearOutLive;
}

static void router_fmt_info(router *r, char *buf, size_t n) {
	if (!r) { strncpy(buf, "?", n - 1); buf[n - 1] = 0; return; }
	strncpy(buf, r->fmtInfo, n - 1);
	buf[n - 1] = 0;
}

// Re-reads the four device stream formats and reports any that changed since
// router_start. A mid-call format switch (common on USB audio modules) makes
// our converters write garbage, which looks exactly like a dead voice path.
static void router_check_formats(router *r, char *buf, size_t n) {
	if (!r) { strncpy(buf, "?", n - 1); buf[n - 1] = 0; return; }
	char out[512] = "";
	struct { AudioDeviceID dev; AudioObjectPropertyScope scope; const char *tag;
	         AudioStreamBasicDescription *base; } devs[4];
	devs[0].dev = r->modIn;  devs[0].scope = kAudioDevicePropertyScopeInput;  devs[0].tag = "modIn";  devs[0].base = &r->modInFmt;
	devs[1].dev = r->modOut; devs[1].scope = kAudioDevicePropertyScopeOutput; devs[1].tag = "modOut"; devs[1].base = &r->modOutFmt;
	devs[2].dev = r->macIn;  devs[2].scope = kAudioDevicePropertyScopeInput;  devs[2].tag = "macIn";  devs[2].base = &r->macInFmt;
	devs[3].dev = r->macOut; devs[3].scope = kAudioDevicePropertyScopeOutput; devs[3].tag = "macOut"; devs[3].base = &r->macOutFmt;
	for (int i = 0; i < 4; i++) {
		AudioStreamBasicDescription f;
		if (get_stream_format(devs[i].dev, devs[i].scope, &f) != noErr) continue;
		AudioStreamBasicDescription *b = devs[i].base;
		if (f.mSampleRate != b->mSampleRate || f.mChannelsPerFrame != b->mChannelsPerFrame ||
		    f.mBitsPerChannel != b->mBitsPerChannel || f.mFormatFlags != b->mFormatFlags) {
			size_t used = strlen(out);
			snprintf(out + used, sizeof(out) - used, " %s:%g/%u/%u/%u->%g/%u/%u/%u",
				devs[i].tag,
				b->mSampleRate, b->mChannelsPerFrame, b->mBitsPerChannel, b->mFormatFlags,
				f.mSampleRate, f.mChannelsPerFrame, f.mBitsPerChannel, f.mFormatFlags);
			*b = f;
		}
	}
	if (out[0] == 0) strncpy(buf, "stable", n - 1);
	else strncpy(buf, out, n - 1);
	buf[n - 1] = 0;
}
*/
import "C"

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// audioRouter routes the module's 8 kHz USB audio to/from the Mac's default
// input/output devices so voice calls can be taken on the Mac.
type audioRouter struct {
	mu        sync.Mutex
	impl      *C.router
	lastError string
	running   bool
	muted     bool
	stopCh    chan struct{}
	formatLog string
	recPath   string
}

func callRecordingsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return filepath.Join(home, "Library", "Application Support", "MacCellular", "recordings")
}

func (r *audioRouter) startRecording() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl == nil {
		return "", fmt.Errorf("通话音频未开启，无法录音")
	}
	dir := callRecordingsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "通话录音_"+time.Now().Format("20060102_150405")+".wav")
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	if C.router_rec_start(r.impl, cpath) != 0 {
		return "", fmt.Errorf("录音启动失败")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		C.router_rec_stop(r.impl)
		return "", err
	}
	r.recPath = path
	return path, nil
}

func (r *audioRouter) stopRecording() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := r.recPath
	r.recPath = ""
	if r.impl != nil {
		C.router_rec_stop(r.impl)
	}
	return path, nil
}

func (r *audioRouter) isRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl == nil {
		return false
	}
	return C.router_rec_on(r.impl) != 0
}

func (r *audioRouter) recordingPath() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recPath
}

func newAudioRouter() *audioRouter {
	return &audioRouter{}
}

func (r *audioRouter) start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl != nil {
		return nil
	}
	device := discoverDJIUSBDevice()
	if device == nil {
		r.lastError = "未找到 DJI/Quectel USB 模块"
		return fmt.Errorf("%s", r.lastError)
	}
	parseHex := func(raw string, bits int) (uint64, error) {
		raw = strings.TrimSpace(strings.ToLower(raw))
		if !strings.HasPrefix(raw, "0x") {
			raw = "0x" + raw
		}
		return strconv.ParseUint(raw, 0, bits)
	}
	vendor, vendorErr := parseHex(device.VendorID, 16)
	product, productErr := parseHex(device.ProductID, 16)
	location, locationErr := parseHex(device.LocationID, 32)
	if vendorErr != nil || productErr != nil || locationErr != nil || location == 0 {
		r.lastError = fmt.Sprintf("模块 USB 身份不完整：%s:%s location=%s", device.VendorID, device.ProductID, device.LocationID)
		return fmt.Errorf("%s", r.lastError)
	}
	impl := C.router_start(C.uint16_t(vendor), C.uint16_t(product), C.uint32_t(location))
	if impl == nil {
		errText := "未找到模块音频设备或 Mac 默认音频设备"
		r.lastError = errText
		return fmt.Errorf("%s", errText)
	}
	r.impl = impl
	r.running = true
	r.lastError = ""
	r.stopCh = make(chan struct{})
	go r.formatWatchdog()
	return nil
}

func (r *audioRouter) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl == nil {
		return
	}
	C.router_stop(r.impl)
	C.free(unsafe.Pointer(r.impl))
	r.impl = nil
	r.running = false
	if r.stopCh != nil {
		close(r.stopCh)
		r.stopCh = nil
	}
}

func (r *audioRouter) setMuted(muted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.muted = muted
	if r.impl != nil {
		v := 0
		if muted {
			v = 1
		}
		C.router_set_muted(r.impl, C.int(v))
	}
}

func (r *audioRouter) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

func (r *audioRouter) state() (bool, float64, float64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl == nil {
		return r.running, 0, 0, r.lastError
	}
	var farPeak C.float
	var nearPeak C.float
	C.router_peaks(r.impl, &farPeak, &nearPeak)
	errText := ""
	if r.impl.err[0] != 0 {
		errText = C.GoString(&r.impl.err[0])
	}
	return true, float64(farPeak), float64(nearPeak), errText
}

// audioStats returns per-direction IOProc call counts and ring fill levels
// (bytes) for diagnosing where a voice path stalls.
func (r *audioRouter) audioStats() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int64{
		"mod_in_calls": 0, "mod_out_calls": 0,
		"mac_in_calls": 0, "mac_out_calls": 0,
		"far_ring_used": 0, "near_ring_used": 0,
	}
	if r.impl == nil {
		return out
	}
	var mi, mo, ki, ko, farUsed, nearUsed C.long
	C.router_stats(r.impl, &mi, &mo, &ki, &ko, &farUsed, &nearUsed)
	out["mod_in_calls"] = int64(mi)
	out["mod_out_calls"] = int64(mo)
	out["mac_in_calls"] = int64(ki)
	out["mac_out_calls"] = int64(ko)
	out["far_ring_used"] = int64(farUsed)
	out["near_ring_used"] = int64(nearUsed)
	return out
}

// audioDevices returns the names of the four audio devices the router is
// currently using, so a dead voice path can be traced to a device switch.
func (r *audioRouter) audioDevices() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{"mod_in": "", "mod_out": "", "mac_in": "", "mac_out": ""}
	if r.impl == nil {
		return out
	}
	var mi, mo, ki, ko [64]C.char
	C.router_device_names(r.impl, &mi[0], C.size_t(len(mi)), &mo[0], C.size_t(len(mo)),
		&ki[0], C.size_t(len(ki)), &ko[0], C.size_t(len(ko)))
	out["mod_in"] = C.GoString(&mi[0])
	out["mod_out"] = C.GoString(&mo[0])
	out["mac_in"] = C.GoString(&ki[0])
	out["mac_out"] = C.GoString(&ko[0])
	return out
}

// live returns the fast-decaying live input levels (far = module input,
// near = Mac input) and processed output levels (what we feed the module
// and the Mac), so a quiet path can be told apart from a dead device.
func (r *audioRouter) live() (float64, float64, float64, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl == nil {
		return 0, 0, 0, 0
	}
	var farLive, nearLive C.float
	C.router_live(r.impl, &farLive, &nearLive)
	var farOutLive, nearOutLive C.float
	C.router_live_out(r.impl, &farOutLive, &nearOutLive)
	return float64(farLive), float64(nearLive), float64(farOutLive), float64(nearOutLive)
}

// formats returns the device stream formats captured when the route started.
func (r *audioRouter) formats() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.impl == nil {
		return ""
	}
	var buf [512]C.char
	C.router_fmt_info(r.impl, &buf[0], C.size_t(len(buf)))
	return C.GoString(&buf[0])
}

// formatChanges returns any format changes the watchdog has detected.
func (r *audioRouter) formatChanges() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.formatLog
}

// formatWatchdog re-checks the device stream formats every few seconds while
// the route is running and logs any mid-call format switch, which would make
// the converters write unusable audio (symptom: one voice direction dies).
func (r *audioRouter) formatWatchdog() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.mu.Lock()
			if r.impl == nil {
				r.mu.Unlock()
				return
			}
			var buf [512]C.char
			C.router_check_formats(r.impl, &buf[0], C.size_t(len(buf)))
			chg := C.GoString(&buf[0])
			if chg != "stable" && chg != r.formatLog {
				r.formatLog = chg
				log.Printf("audio format change detected: %s", chg)
			}
			r.mu.Unlock()
		}
	}
}

func logAudioRouterState(r *audioRouter) {
	if r == nil {
		return
	}
	running, far, near, errText := r.state()
	log.Printf("audio router: running=%v far_peak=%.3f near_peak=%.3f err=%q", running, far, near, errText)
}
