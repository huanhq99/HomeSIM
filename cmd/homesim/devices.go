package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type deviceConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    string `json:"port"`
	Audio   string `json:"audio"`
	Enabled bool   `json:"enabled"`
}
type deviceRegistry struct {
	mu      sync.Mutex
	ctx     context.Context
	configs map[string]deviceConfig
	apps    map[string]*app
	cancels map[string]context.CancelFunc
	done    map[string]chan struct{}
}

func (a *app) deviceTTY() string {
	if a.devicePort != "" {
		return a.devicePort
	}
	return env("HOMESIM_TTY", "/dev/ttyUSB2")
}
func (a *app) audioName() string {
	if a.registry != nil {
		a.registry.mu.Lock()
		defer a.registry.mu.Unlock()
		if c, ok := a.registry.configs[a.deviceID]; ok {
			return c.Audio
		}
	}
	if a.audioDevice != "" {
		return a.audioDevice
	}
	return env("HOMESIM_AUDIO_DEVICE", "hw:Baiwang,0")
}
func (a *app) accountApp() *app {
	if a.root != nil {
		return a.root
	}
	return a
}
func (a *app) startDevices(ctx context.Context) error {
	a.deviceID = "primary"
	a.devicePort = env("HOMESIM_TTY", "/dev/ttyUSB2")
	d := &deviceRegistry{ctx: ctx, configs: map[string]deviceConfig{}, apps: map[string]*app{"primary": a}, cancels: map[string]context.CancelFunc{}, done: map[string]chan struct{}{}}
	a.registry = d
	configs := []deviceConfig{}
	if b, err := os.ReadFile(filepath.Join(a.data, "devices.json")); err == nil {
		if err = json.Unmarshal(b, &configs); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, c := range configs {
		if !validDevice(c) {
			return errors.New("invalid saved device configuration")
		}
		d.configs[c.ID] = c
	}
	primary, ok := d.configs["primary"]
	if !ok {
		primary = deviceConfig{"primary", "主线路", a.devicePort, env("HOMESIM_AUDIO_DEVICE", "hw:Baiwang,0"), true}
		d.configs[primary.ID] = primary
	}
	a.devicePort = primary.Port
	if err := a.loadHistory(); err != nil {
		return err
	}
	if os.Getenv("HOMESIM_DEMO") == "1" {
		return nil
	}
	d.mu.Lock()
	for _, c := range d.configs {
		if c.Enabled {
			if err := a.startDeviceLocked(c); err != nil {
				d.mu.Unlock()
				return err
			}
		}
	}
	d.mu.Unlock()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		a.scanDevices()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.scanDevices()
			}
		}
	}()
	return nil
}
func (a *app) startDeviceLocked(c deviceConfig) error {
	d := a.registry
	if d.cancels[c.ID] != nil {
		select {
		case <-d.done[c.ID]:
			delete(d.cancels, c.ID)
		default:
			return errors.New("line is still stopping")
		}
	}
	slot := d.apps[c.ID]
	if slot == nil {
		var err error
		slot, err = openApp(filepath.Join(a.data, "devices", c.ID))
		if err != nil {
			return err
		}
		slot.root = a
		slot.registry = d
		slot.deviceID = c.ID
		slot.devicePort = c.Port
		slot.audioDevice = c.Audio
		if err = slot.loadHistory(); err != nil {
			return err
		}
		d.apps[c.ID] = slot
	}
	ctx, cancel := context.WithCancel(d.ctx)
	slot.proxyStart(d.ctx)
	d.cancels[c.ID] = cancel
	done := make(chan struct{})
	d.done[c.ID] = done
	go func() { defer close(done); slot.modemLoop(ctx) }()
	go slot.watchCalls(ctx)
	return nil
}

var deviceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var audioPattern = regexp.MustCompile(`^hw:[A-Za-z0-9_]+,[0-9]+$`)
var portPattern = regexp.MustCompile(`^/(?:dev|host-dev)/(?:tty(?:USB|ACM)[0-9]+|serial/by-(?:id|path)/[A-Za-z0-9_.:+-]+)$`)

func validDevice(c deviceConfig) bool {
	clean := filepath.Clean(c.Port)
	return deviceIDPattern.MatchString(c.ID) && len([]rune(c.Name)) >= 1 && len([]rune(c.Name)) <= 40 &&
		clean == c.Port && portPattern.MatchString(clean) &&
		(c.Audio == "disabled" || audioPattern.MatchString(c.Audio))
}
func sameSerial(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	x, e1 := os.Stat(a)
	y, e2 := os.Stat(b)
	if e1 == nil && e2 == nil && os.SameFile(x, y) {
		return true
	}
	x1, _ := filepath.EvalSymlinks(a)
	x2, _ := filepath.EvalSymlinks(b)
	return x1 != "" && x2 != "" && filepath.Base(x1) == filepath.Base(x2)
}
func (a *app) scanDevices() {
	// Only the known Baiwang AT interface is auto-opened. Other interfaces are
	// listed for explicit selection; probing every serial interface is unsafe.
	ports, _ := filepath.Glob(filepath.Join(env("HOMESIM_DEV_ROOT", "/dev"), "serial/by-path/*"))
	ids, _ := filepath.Glob(filepath.Join(env("HOMESIM_DEV_ROOT", "/dev"), "serial/by-id/*"))
	ports = append(ports, ids...)
	d := a.registry
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for _, port := range ports {
		if !knownBaiwangAT(port) {
			continue
		}
		known := false
		for _, c := range d.configs {
			if sameSerial(c.Port, port) {
				known = true
				break
			}
		}
		if known {
			continue
		}
		target, _ := filepath.EvalSymlinks(port)
		sum := sha256.Sum256([]byte(usbParent(filepath.Join("/sys/class/tty", filepath.Base(target), "device"))))
		id := "usb-" + hex.EncodeToString(sum[:8])
		if _, ok := d.configs[id]; ok {
			continue
		}
		c := deviceConfig{id, "新线路", port, discoverAudio(port), true}
		if !validDevice(c) {
			continue
		}
		d.configs[id] = c
		if err := a.saveDevicesLocked(); err != nil {
			delete(d.configs, id)
			continue
		}
		if err := a.startDeviceLocked(c); err != nil {
			c.Enabled = false
			d.configs[id] = c
		}
		changed = true
	}
	if changed {
		_ = a.saveDevicesLocked()
	}
}
func knownBaiwangAT(port string) bool {
	target, err := filepath.EvalSymlinks(port)
	if err != nil {
		return false
	}
	path, err := filepath.EvalSymlinks(filepath.Join("/sys/class/tty", filepath.Base(target), "device"))
	if err != nil {
		return false
	}
	usb := usbParent(path)
	if usb == "" {
		return false
	}
	product, _ := os.ReadFile(filepath.Join(usb, "product"))
	if !strings.Contains(strings.ToLower(string(product)), "baiwang") {
		return false
	}
	for path != "/" && path != "." && path != "" {
		if b, err := os.ReadFile(filepath.Join(path, "bInterfaceNumber")); err == nil {
			return strings.TrimSpace(string(b)) == "02"
		}
		path = filepath.Dir(path)
	}
	return false
}
func usbParent(path string) string {
	path, _ = filepath.EvalSymlinks(path)
	for path != "/" && path != "." && path != "" {
		if _, err := os.Stat(filepath.Join(path, "idVendor")); err == nil {
			return path
		}
		path = filepath.Dir(path)
	}
	return ""
}
func discoverAudio(port string) string {
	target, _ := filepath.EvalSymlinks(port)
	usb := usbParent(filepath.Join("/sys/class/tty", filepath.Base(target), "device"))
	if usb == "" {
		return "disabled"
	}
	cards, _ := filepath.Glob("/sys/class/sound/card[0-9]*")
	for _, card := range cards {
		if usbParent(filepath.Join(card, "device")) == usb {
			return "hw:" + strings.TrimPrefix(filepath.Base(card), "card") + ",0"
		}
	}
	return "disabled"
}
func (a *app) saveDevicesLocked() error {
	rows := []deviceConfig{}
	for _, c := range a.registry.configs {
		rows = append(rows, c)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return atomicJSON(filepath.Join(a.data, "devices.json"), rows)
}
func (a *app) scoped(fn func(*app, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("device_id")
		if id == "" || id == "primary" {
			fn(a, w, r)
			return
		}
		if a.registry == nil {
			fail(w, 404, "线路不存在")
			return
		}
		a.registry.mu.Lock()
		slot := a.registry.apps[id]
		a.registry.mu.Unlock()
		if slot == nil {
			fail(w, 404, "线路不存在")
			return
		}
		fn(slot, w, r)
	}
}
func (a *app) listDevices(w http.ResponseWriter, r *http.Request) {
	if a.registry == nil {
		reply(w, 200, []any{})
		return
	}
	d := a.registry
	d.mu.Lock()
	configs := []deviceConfig{}
	slots := map[string]*app{}
	for id, c := range d.configs {
		configs = append(configs, c)
		slots[id] = d.apps[id]
	}
	d.mu.Unlock()
	sort.Slice(configs, func(i, j int) bool { return configs[i].ID < configs[j].ID })
	rows := []any{}
	for _, c := range configs {
		line := map[string]any{"state": "offline"}
		if s := slots[c.ID]; s != nil {
			line = s.lineInfo(s.getModem() != nil)
		}
		rows = append(rows, map[string]any{"config": c, "line": line})
	}
	reply(w, 200, rows)
}
func (a *app) updateDevice(w http.ResponseWriter, r *http.Request) {
	var c deviceConfig
	if !body(w, r, &c) {
		return
	}
	if !validDevice(c) {
		fail(w, 400, "线路名称、串口或音频设备不正确")
		return
	}
	d := a.registry
	if d == nil {
		fail(w, 503, "设备管理未启动")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	previous, exists := d.configs[c.ID]
	if exists && previous.Port != c.Port {
		fail(w, 409, "更换串口请先添加一条新线路")
		return
	}
	for id, other := range d.configs {
		if id != c.ID && sameSerial(other.Port, c.Port) {
			fail(w, 409, "此串口已有线路使用")
			return
		}
		if id != c.ID && c.Enabled && other.Enabled && c.Audio != "disabled" && sameAudio(c.Audio, other.Audio) {
			fail(w, 409, "此声卡已被另一条线路使用，请选择本模块对应声卡")
			return
		}
	}
	if slot := d.apps[c.ID]; slot != nil {
		if !slot.sendMu.TryLock() {
			fail(w, 409, "线路正在执行操作，请稍后修改")
			return
		}
		defer slot.sendMu.Unlock()
		slot.voiceMu.Lock()
		busy := slot.voice != nil
		slot.voiceMu.Unlock()
		if busy {
			fail(w, 409, "请先结束通话并断开音频再修改线路")
			return
		}
	}
	d.configs[c.ID] = c
	if err := a.saveDevicesLocked(); err != nil {
		if exists {
			d.configs[c.ID] = previous
		} else {
			delete(d.configs, c.ID)
		}
		fail(w, 503, "线路设置未保存")
		return
	}
	// Wait for the sole serial owner to exit before permitting a restart.
	if exists && previous.Enabled && !c.Enabled {
		if slot := d.apps[c.ID]; slot != nil {
			slot.proxy.mu.Lock()
			if slot.proxy.listener != nil {
				_ = slot.proxy.listener.Close()
				slot.proxy.listener = nil
			}
			for conn := range slot.proxy.connections {
				_ = conn.Close()
			}
			slot.proxy.mu.Unlock()
		}
		if cancel := d.cancels[c.ID]; cancel != nil {
			cancel()
			select {
			case <-d.done[c.ID]:
				delete(d.cancels, c.ID)
			case <-time.After(20 * time.Second):
				fail(w, 503, "已停用，串口正在释放，请稍后重试")
				return
			}
		}
	}
	if c.Enabled && (!exists || !previous.Enabled) {
		if err := a.startDeviceLocked(c); err != nil {
			fail(w, 503, "线路启动失败")
			return
		}
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func sameAudio(a, b string) bool {
	if a == "disabled" || b == "disabled" {
		return false
	}
	if a == b {
		return true
	}
	resolve := func(value string) string {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "hw:"), ",0")
		paths, _ := filepath.Glob("/proc/asound/card[0-9]*/id")
		for _, path := range paths {
			v, _ := os.ReadFile(path)
			number := strings.TrimPrefix(filepath.Base(filepath.Dir(path)), "card")
			if value == number || value == strings.TrimSpace(string(v)) {
				return number
			}
		}
		return value
	}
	return resolve(a) == resolve(b)
}
func (a *app) discoveredDevices(w http.ResponseWriter, r *http.Request) {
	ports, _ := filepath.Glob(filepath.Join(env("HOMESIM_DEV_ROOT", "/dev"), "serial/by-id/*"))
	paths, _ := filepath.Glob(filepath.Join(env("HOMESIM_DEV_ROOT", "/dev"), "serial/by-path/*"))
	ports = append(ports, paths...)
	reply(w, 200, ports)
}
