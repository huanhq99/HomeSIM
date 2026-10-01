package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type call struct {
	ID        string `json:"id"`
	Direction int    `json:"direction"`
	State     int    `json:"state"`
	Number    string `json:"number"`
}
type voiceSession struct {
	id, owner   string
	pc          *webrtc.PeerConnection
	cancel      context.CancelFunc
	connected   bool
	ownedCall   bool
	ownedNumber string
	ownedID     string
	closeOnce   sync.Once
	mu          sync.Mutex
	captured    atomic.Uint64
	played      atomic.Uint64
}

var clccPattern = regexp.MustCompile(`^\+CLCC:\s*(\d+)\s*,\s*([01])\s*,\s*([0-5])\s*,\s*(\d+)\s*,\s*[01](?:\s*,\s*"([+0-9]*)")?(?:\s*,.*)?$`)

func parseCalls(s string) ([]call, error) {
	rows := []call{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		m := clccPattern.FindStringSubmatch(line)
		if m == nil {
			return nil, errors.New("模块返回的通话状态无法识别，请暂勿拨号")
		}
		// CLCC mode 1/2 describes data/fax calls, not telephone calls.
		if m[4] == "1" || m[4] == "2" {
			continue
		}
		if m[4] != "0" {
			return nil, errors.New("模块返回了未知通话类型，请暂勿拨号")
		}
		d, _ := strconv.Atoi(m[2])
		state, _ := strconv.Atoi(m[3])
		rows = append(rows, call{m[1], d, state, m[5]})
	}
	return rows, nil
}
func (a *app) currentCalls() ([]call, error) {
	m := a.getModem()
	if m == nil {
		return nil, errors.New("模块未连接")
	}
	s, err := m.ExecuteATSilent("AT+CLCC", 3*time.Second)
	if err != nil {
		return nil, errors.New("暂时无法读取通话状态")
	}
	return parseCalls(s)
}
func dialProblem(number string, rows []call, audioReady bool) string {
	if !phonePattern.MatchString(number) {
		return "号码格式不正确，请输入电话号码（国际号码请带 + 国家码）"
	}
	if len(rows) != 0 {
		return "线路已有语音通话，请结束当前通话后再拨打"
	}
	if !audioReady {
		return "音频链路尚未连接，请连接麦克风并等待连接完成"
	}
	return ""
}
func (a *app) calls(w http.ResponseWriter, r *http.Request) {
	c, err := a.currentCalls()
	if err != nil {
		fail(w, 503, err.Error())
		return
	}
	reply(w, 200, c)
}
func owner(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return "native:" + tokenHash(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	c, _ := r.Cookie("homesim")
	if c == nil {
		return ""
	}
	return c.Value
}
func (a *app) callAction(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Action  string `json:"action"`
		Number  string `json:"number"`
		Digit   string `json:"digit"`
		VoiceID string `json:"voice_id"`
		CallID  string `json:"call_id"`
	}
	if !body(w, r, &b) {
		return
	}
	m := a.getModem()
	if m == nil {
		fail(w, 503, "模块未连接")
		return
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	rows, err := a.currentCalls()
	if err != nil {
		fail(w, 503, err.Error())
		return
	}
	if len(rows) > 1 {
		fail(w, 409, "检测到多路通话，请在模块原有管理工具中处理")
		return
	}
	a.voiceMu.Lock()
	defer a.voiceMu.Unlock()
	v := a.voice
	audioReady := false
	if v != nil {
		v.mu.Lock()
		audioReady = v.id == b.VoiceID && v.owner == owner(r) && v.connected
		v.mu.Unlock()
	}
	cmd := ""
	switch b.Action {
	case "dial":
		if problem := dialProblem(b.Number, rows, audioReady); problem != "" {
			fail(w, 409, problem)
			return
		}
		cmd = "ATD" + b.Number + ";"
	case "answer":
		if len(rows) != 1 || rows[0].ID != b.CallID || (rows[0].State != 4 && rows[0].State != 5) || !audioReady {
			fail(w, 409, "来电状态已改变或音频尚未连接")
			return
		}
		cmd = "ATA"
	case "reject":
		if len(rows) != 1 || rows[0].ID != b.CallID || (rows[0].State != 4 && rows[0].State != 5) {
			fail(w, 409, "来电状态已改变")
			return
		}
		cmd = "ATH"
	case "hangup":
		if len(rows) != 1 || rows[0].ID != b.CallID {
			fail(w, 409, "通话状态已改变")
			return
		}
		cmd = "ATH"
	case "dtmf":
		if len(rows) != 1 || rows[0].ID != b.CallID || rows[0].State != 0 || !audioReady || len(b.Digit) != 1 || !strings.Contains("0123456789*#", b.Digit) {
			fail(w, 409, "通话状态或按键不正确")
			return
		}
		cmd = `AT+VTS="` + b.Digit + `"`
	default:
		fail(w, 400, "未知操作")
		return
	}
	_, err = m.ExecuteAT(cmd, 10*time.Second)
	if err != nil {
		fail(w, 409, "操作结果暂时无法确认，请先查看通话状态")
		return
	}
	if v != nil {
		v.mu.Lock()
		if b.Action == "dial" || b.Action == "answer" {
			v.ownedCall = true
			if b.Action == "dial" {
				v.ownedNumber = b.Number
				v.ownedID = ""
			} else {
				v.ownedNumber = rows[0].Number
				v.ownedID = rows[0].ID
			}
		}
		if b.Action == "hangup" {
			v.ownedCall = false
		}
		v.mu.Unlock()
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (v *voiceSession) close() { v.closeOnce.Do(func() { v.cancel(); _ = v.pc.Close() }) }
func (a *app) stopVoice(w http.ResponseWriter, r *http.Request) {
	a.voiceMu.Lock()
	v := a.voice
	if v != nil && v.owner != owner(r) {
		a.voiceMu.Unlock()
		fail(w, 409, "音频正在另一个窗口使用")
		return
	}
	a.voice = nil
	a.voiceMu.Unlock()
	if v != nil {
		a.closeVoice(v)
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *app) closeVoice(v *voiceSession) {
	v.mu.Lock()
	owned := v.ownedCall
	number, id := v.ownedNumber, v.ownedID
	v.ownedCall = false
	v.mu.Unlock()
	if owned {
		if rows, err := a.currentCalls(); err == nil && len(rows) == 1 && rows[0].State != 4 && rows[0].State != 5 && rows[0].Number == number && (id == "" || rows[0].ID == id) {
			if m := a.getModem(); m != nil {
				_, _ = m.ExecuteAT("ATH", 3*time.Second)
			}
		}
	}
	v.close()
}
func (a *app) offer(w http.ResponseWriter, r *http.Request) {
	if !a.sendMu.TryLock() {
		fail(w, 409, "线路正在执行其他操作，请稍后连接音频")
		return
	}
	defer a.sendMu.Unlock()
	if a.getModem() == nil {
		fail(w, 503, "模块未连接")
		return
	}
	device := a.audioName()
	if device == "disabled" {
		fail(w, 503, "音频未启用")
		return
	}
	var b struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if !body(w, r, &b) {
		return
	}
	if b.Type != "offer" || len(b.SDP) > 14000 {
		fail(w, 400, "音频连接请求不正确")
		return
	}
	a.voiceMu.Lock()
	if a.voice != nil {
		a.voiceMu.Unlock()
		fail(w, 409, "音频连接已被占用，请先断开原窗口")
		return
	}
	engine := &webrtc.MediaEngine{}
	err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, PayloadType: 0}, webrtc.RTPCodecTypeAudio)
	settings := webrtc.SettingEngine{}
	_ = settings.SetEphemeralUDPPortRange(8590, 8600)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithSettingEngine(settings))
	pc, err := api.NewPeerConnection(voiceICE())
	if err != nil {
		a.voiceMu.Unlock()
		fail(w, 500, "语音初始化失败")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	v := &voiceSession{id: randomID(), owner: owner(r), pc: pc, cancel: cancel}
	a.voice = v
	a.voiceMu.Unlock()
	cleanup := func() {
		a.voiceMu.Lock()
		if a.voice == v {
			a.voice = nil
		}
		a.voiceMu.Unlock()
		a.closeVoice(v)
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "homesim")
	if err != nil {
		cleanup()
		fail(w, 500, "语音初始化失败")
		return
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		cleanup()
		fail(w, 500, "语音初始化失败")
		return
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	rec := exec.CommandContext(ctx, "arecord", "-q", "-D", device, "-t", "raw", "-f", "S16_LE", "-r", "8000", "-c", "1")
	pipe, err := rec.StdoutPipe()
	if err != nil {
		cleanup()
		fail(w, 503, "无法打开音频设备")
		return
	}
	if err = rec.Start(); err != nil {
		cleanup()
		fail(w, 503, "无法打开模块录音设备")
		return
	}
	go func() {
		defer rec.Wait()
		defer pipe.Close()
		raw := make([]byte, 320)
		encoded := make([]byte, 160)
		for {
			if _, err := io.ReadFull(pipe, raw); err != nil {
				break
			}
			for i := range encoded {
				encoded[i] = linearToMu(int16(binary.LittleEndian.Uint16(raw[i*2:])))
			}
			if err := track.WriteSample(media.Sample{Data: encoded, Duration: 20 * time.Millisecond}); err != nil {
				break
			}
			v.captured.Add(1)
		}
		if ctx.Err() == nil {
			a.addEvent("audio", "模块录音流停止，音频连接已关闭")
			cleanup()
		}
	}()
	var playOnce sync.Once
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		playOnce.Do(func() {
			go func() {
				defer func() {
					if ctx.Err() == nil {
						a.addEvent("audio", "模块播放流停止，音频连接已关闭")
						cleanup()
					}
				}()
				if remote.Codec().MimeType != webrtc.MimeTypePCMU {
					cleanup()
					return
				}
				play := exec.CommandContext(ctx, "aplay", "-q", "-D", device, "-t", "raw", "-f", "S16_LE", "-r", "8000", "-c", "1")
				input, err := play.StdinPipe()
				if err != nil {
					cleanup()
					return
				}
				if err = play.Start(); err != nil {
					cleanup()
					return
				}
				defer play.Wait()
				defer input.Close()
				for {
					packet, _, err := remote.ReadRTP()
					if err != nil {
						break
					}
					if len(packet.Payload) > 1600 {
						continue
					}
					pcm := make([]byte, len(packet.Payload)*2)
					for i, b := range packet.Payload {
						binary.LittleEndian.PutUint16(pcm[i*2:], uint16(muToLinear(b)))
					}
					if _, err = input.Write(pcm); err != nil {
						break
					}
					v.played.Add(1)
				}
			}()
		})
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		v.mu.Lock()
		v.connected = s == webrtc.PeerConnectionStateConnected
		v.mu.Unlock()
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateDisconnected {
			cleanup()
		}
	})
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: b.SDP}); err != nil {
		cleanup()
		fail(w, 400, "语音协商失败")
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		cleanup()
		fail(w, 500, "语音协商失败")
		return
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		cleanup()
		fail(w, 500, "语音协商失败")
		return
	}
	select {
	case <-gather:
	case <-time.After(10 * time.Second):
		cleanup()
		fail(w, 504, "语音连接超时")
		return
	case <-r.Context().Done():
		cleanup()
		return
	}
	reply(w, 200, map[string]any{"type": "answer", "sdp": pc.LocalDescription().SDP, "voice_id": v.id})
	go func() {
		timer := time.NewTimer(25 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		v.mu.Lock()
		connected := v.connected
		v.mu.Unlock()
		if !connected {
			cleanup()
		}
	}()
}
func linearToMu(x int16) byte {
	n := int(x)
	sign := 0
	if n < 0 {
		sign = 0x80
		n = -n
	}
	if n > 32635 {
		n = 32635
	}
	n += 132
	exponent := 7
	for mask := 0x4000; exponent > 0 && n&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (n >> (exponent + 3)) & 15
	return ^byte(sign | (exponent << 4) | mantissa)
}
func muToLinear(x byte) int16 {
	x = ^x
	n := ((int(x) & 15) << 3) + 132
	n <<= (x & 0x70) >> 4
	n -= 132
	if x&0x80 != 0 {
		n = -n
	}
	return int16(n)
}
