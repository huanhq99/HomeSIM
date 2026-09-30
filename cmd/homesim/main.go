// HomeSIM uses the MacCellular modem and SMS codec under the root license.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iniwex5/vohive/internal/config"
	"github.com/iniwex5/vohive/internal/modem"
	"golang.org/x/crypto/bcrypt"
)

//go:embed web/*
var assets embed.FS

type message struct {
	ID        string    `json:"id"`
	Peer      string    `json:"peer"`
	Content   string    `json:"content"`
	Direction string    `json:"direction"`
	Timestamp time.Time `json:"timestamp"`
	Status    string    `json:"status"`
	Code      string    `json:"code,omitempty"`
	Read      bool      `json:"read"`
}
type credentials struct {
	Username string `json:"username"`
	Hash     string `json:"hash"`
}
type session struct{ Expires time.Time }
type app struct {
	modemMu     sync.RWMutex
	manager     *modem.Manager
	data        string
	mu          sync.Mutex
	messages    []message
	peers       map[string]peerInfo
	line        lineSnapshot
	lineNumbers map[string]string
	auth        credentials
	setupToken  string
	sessions    map[string]session
	attempts    map[string][]time.Time
	sendMu      sync.Mutex
	pollMu      sync.Mutex
	voiceMu     sync.Mutex
	voice       *voiceSession
	lastError   string
}

func (a *app) getModem() *modem.Manager {
	a.modemMu.RLock()
	defer a.modemMu.RUnlock()
	return a.manager
}
func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func atomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".homesim-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func openApp(data string) (*app, error) {
	if err := os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(data, 0700); err != nil {
		return nil, err
	}
	a := &app{data: data, messages: []message{}, peers: map[string]peerInfo{}, lineNumbers: map[string]string{}, sessions: map[string]session{}, attempts: map[string][]time.Time{}}
	for _, item := range []struct {
		file  string
		value any
	}{{"messages.json", &a.messages}, {"auth.json", &a.auth}, {"peers.json", &a.peers}, {"lines.json", &a.lineNumbers}} {
		b, err := os.ReadFile(filepath.Join(data, item.file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, item.value); err != nil {
			return nil, fmt.Errorf("%s damaged: %w", item.file, err)
		}
	}
	if a.peers == nil {
		a.peers = map[string]peerInfo{}
	}
	if a.lineNumbers == nil {
		a.lineNumbers = map[string]string{}
	}
	changed := false
	for i := range a.messages {
		if a.messages[i].Status == "pending" {
			a.messages[i].Status = "unknown"
			changed = true
		}
	}
	if changed {
		if err := atomicJSON(filepath.Join(data, "messages.json"), a.messages); err != nil {
			return nil, err
		}
	}
	if a.auth.Hash == "" {
		p := filepath.Join(data, "setup-token")
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			b = []byte(randomID())
			err = os.WriteFile(p, b, 0600)
		}
		if err != nil {
			return nil, err
		}
		a.setupToken = strings.TrimSpace(string(b))
		if len(a.setupToken) != 64 {
			return nil, errors.New("invalid setup-token file")
		}
	}
	return a, nil
}
func (a *app) receive(peer, body string, ts time.Time) {
	if ts.IsZero() {
		ts = time.Now()
	}
	sum := sha256.Sum256([]byte(peer + "\x00" + body + "\x00" + ts.UTC().Format(time.RFC3339Nano)))
	id := hex.EncodeToString(sum[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.messages {
		if m.ID == id {
			return
		}
	}
	code := ""
	if strings.Contains(body, "验证码") || strings.Contains(strings.ToLower(body), "code") {
		code = regexp.MustCompile(`\b[0-9]{4,8}\b`).FindString(body)
	}
	next := append(append([]message{}, a.messages...), message{ID: id, Peer: peer, Content: body, Direction: "incoming", Timestamp: ts, Status: "received", Code: code})
	if err := atomicJSON(filepath.Join(a.data, "messages.json"), next); err != nil {
		a.lastError = "短信保存失败，请检查 NAS 空间和权限"
		log.Print("incoming SMS persistence failed")
		return
	}
	a.messages = next
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, s string) {
	reply(w, status, map[string]string{"error": s})
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "请求格式不正确")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "请求格式不正确")
		return false
	}
	return true
}
func (a *app) authenticated(r *http.Request) bool {
	c, err := r.Cookie("homesim")
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	return ok && time.Now().Before(s.Expires)
}
func secure(r *http.Request) bool {
	return r.TLS != nil || (os.Getenv("HOMESIM_TRUST_PROXY") == "1" && r.Header.Get("X-Forwarded-Proto") == "https")
}
func (a *app) setSession(w http.ResponseWriter, r *http.Request) {
	token := randomID()
	a.mu.Lock()
	for k, s := range a.sessions {
		if time.Now().After(s.Expires) {
			delete(a.sessions, k)
		}
	}
	a.sessions[token] = session{time.Now().Add(12 * time.Hour)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "homesim", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r), MaxAge: 43200})
}
func (a *app) authAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.mu.Lock()
		setup := a.auth.Hash == ""
		a.mu.Unlock()
		reply(w, 200, map[string]any{"setup_required": setup, "authenticated": a.authenticated(r), "version": "0.3.1", "voice_implemented": true})
		return
	}
	if r.Method == "DELETE" {
		c, _ := r.Cookie("homesim")
		if c != nil {
			a.mu.Lock()
			delete(a.sessions, c.Value)
			a.mu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "homesim", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure(r)})
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "方法不支持")
		return
	}
	var b struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if !body(w, r, &b) {
		return
	}
	// Never trust a forwarded client IP for authentication throttling.
	key := strings.Split(r.RemoteAddr, ":")[0]
	a.mu.Lock()
	now := time.Now()
	times := a.attempts[key]
	fresh := times[:0]
	for _, t := range times {
		if now.Sub(t) < time.Minute {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= 8 {
		a.mu.Unlock()
		fail(w, 429, "尝试过于频繁，请稍后再试")
		return
	}
	a.attempts[key] = append(fresh, now)
	c := a.auth
	setup := c.Hash == ""
	token := a.setupToken
	a.mu.Unlock()
	if setup {
		if len(b.SetupToken) != len(token) || subtle.ConstantTimeCompare([]byte(b.SetupToken), []byte(token)) != 1 {
			fail(w, 401, "安装码不正确")
			return
		}
		if len(b.Password) < 12 || len(b.Password) > 72 || len(b.Username) < 1 || len(b.Username) > 64 {
			fail(w, 400, "请设置用户名和 12–72 字节的密码")
			return
		}
		h, err := bcrypt.GenerateFromPassword([]byte(b.Password), bcrypt.DefaultCost)
		if err != nil {
			fail(w, 500, "账户创建失败")
			return
		}
		a.mu.Lock()
		if a.auth.Hash != "" {
			a.mu.Unlock()
			fail(w, 409, "账户已设置，请登录")
			return
		}
		c = credentials{b.Username, string(h)}
		err = atomicJSON(filepath.Join(a.data, "auth.json"), c)
		if err == nil {
			a.auth = c
			a.setupToken = ""
		}
		a.mu.Unlock()
		if err != nil {
			fail(w, 500, "账户保存失败")
			return
		}
	} else if b.Username != c.Username || bcrypt.CompareHashAndPassword([]byte(c.Hash), []byte(b.Password)) != nil {
		fail(w, 401, "用户名或密码不正确")
		return
	}
	a.setSession(w, r)
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *app) status(w http.ResponseWriter, r *http.Request) {
	m := a.getModem()
	if m == nil {
		reply(w, 200, map[string]any{"connected": false, "error": "模块未连接", "voice_available": false, "line": a.lineInfo(false)})
		return
	}
	s := m.GetFullStatus()
	a.mu.Lock()
	e := a.lastError
	a.mu.Unlock()
	voice := env("HOMESIM_AUDIO_DEVICE", "hw:Baiwang,0")
	reply(w, 200, map[string]any{"connected": m.WaitReady(time.Millisecond), "operator": s.Operator, "network_mode": s.NetworkMode, "signal_dbm": s.SignalDBM, "sim_inserted": s.SimInserted, "reg_status": s.RegStatus, "reg_status_text": s.RegStatusText, "firmware": s.Firmware, "error": e, "voice_available": voice != "disabled", "audio_device": voice, "https": secure(r), "line": a.lineInfo(true)})
}
func (a *app) list(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	rows := append([]message{}, a.messages...)
	a.mu.Unlock()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Timestamp.After(rows[j].Timestamp) })
	reply(w, 200, rows)
}

var phonePattern = regexp.MustCompile(`^\+?[0-9]{3,20}$`)

func (a *app) send(w http.ResponseWriter, r *http.Request) {
	mod := a.getModem()
	var b struct {
		Phone     string `json:"phone"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if !body(w, r, &b) {
		return
	}
	b.Phone = strings.TrimSpace(b.Phone)
	if !phonePattern.MatchString(b.Phone) || len(strings.TrimSpace(b.Message)) == 0 || len([]rune(b.Message)) > 2000 || len(b.RequestID) < 16 || len(b.RequestID) > 128 {
		fail(w, 400, "请检查号码、短信内容和请求标识")
		return
	}
	if mod == nil {
		fail(w, 503, "模块尚未连接")
		return
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	a.mu.Lock()
	for _, m := range a.messages {
		if m.ID == b.RequestID {
			a.mu.Unlock()
			reply(w, 200, m)
			return
		}
	}
	m := message{ID: b.RequestID, Peer: b.Phone, Content: b.Message, Direction: "outgoing", Timestamp: time.Now(), Status: "pending", Read: true}
	next := append(append([]message{}, a.messages...), m)
	if err := atomicJSON(filepath.Join(a.data, "messages.json"), next); err != nil {
		a.mu.Unlock()
		fail(w, 503, "短信保存失败，未发送")
		return
	}
	a.messages = next
	a.mu.Unlock()
	err := mod.SendSMS(b.Phone, b.Message)
	m.Status = "submitted"
	if err != nil {
		m.Status = "unknown"
	}
	a.mu.Lock()
	next = append([]message{}, a.messages...)
	for i := range next {
		if next[i].ID == m.ID {
			next[i] = m
		}
	}
	persistErr := atomicJSON(filepath.Join(a.data, "messages.json"), next)
	if persistErr == nil {
		a.messages = next
	}
	a.mu.Unlock()
	if err != nil || persistErr != nil {
		fail(w, 409, "发送结果暂时无法确认，请核对记录，避免重复发送")
		return
	}
	reply(w, 200, m)
}
func (a *app) refresh(w http.ResponseWriter, r *http.Request) {
	m := a.getModem()
	if m == nil {
		fail(w, 503, "模块未连接")
		return
	}
	if !a.pollMu.TryLock() {
		reply(w, 202, map[string]bool{"accepted": true})
		return
	}
	go func() { defer a.pollMu.Unlock(); a.updateLine(m, true); m.CheckAllSMS(); m.RefreshDeviceInfo() }()
	reply(w, 202, map[string]bool{"accepted": true})
}
func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", a.authAPI)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("POST /api/line/number", a.setLineNumber)
	mux.HandleFunc("GET /api/messages", a.list)
	mux.HandleFunc("POST /api/messages/send", a.send)
	mux.HandleFunc("POST /api/messages/refresh", a.refresh)
	mux.HandleFunc("POST /api/messages/read", a.markRead)
	mux.HandleFunc("POST /api/messages/estimate", a.estimate)
	mux.HandleFunc("GET /api/peers", a.listPeers)
	mux.HandleFunc("POST /api/peers", a.updatePeer)
	mux.HandleFunc("GET /api/export", a.exportMessages)
	mux.HandleFunc("GET /api/calls", a.calls)
	mux.HandleFunc("POST /api/calls/action", a.callAction)
	mux.HandleFunc("POST /api/voice/offer", a.offer)
	mux.HandleFunc("DELETE /api/voice", a.stopVoice)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]bool{"ok": true}) })
	files, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self' blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != "GET" && r.Method != "HEAD" {
				origin := r.Header.Get("Origin")
				if origin == "" {
					fail(w, 403, "缺少来源标识")
					return
				}
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					fail(w, 403, "请从同一网页操作")
					return
				}
			}
			if r.URL.Path != "/api/auth" && !a.authenticated(r) {
				fail(w, 401, "请先登录")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func healthURL() string {
	host, port, err := net.SplitHostPort(env("HOMESIM_LISTEN", ":8580"))
	if err != nil {
		return "http://127.0.0.1:8580/healthz"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		resp, err := http.Get(healthURL())
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "probe" {
		probe()
		return
	}
	a, err := openApp(env("HOMESIM_DATA", "/data"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Getenv("HOMESIM_DEMO") != "1" {
		go a.modemLoop(ctx)
	}
	server := &http.Server{Addr: env("HOMESIM_LISTEN", ":8580"), Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	}()
	log.Print("HomeSIM 0.3.1 listening")
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
func (a *app) modemLoop(ctx context.Context) {
	for ctx.Err() == nil {
		disconnected := make(chan struct{}, 1)
		m, err := modem.New(config.DeviceConfig{ID: "homesim", Name: "HomeSIM", ATPort: env("HOMESIM_TTY", "/dev/ttyUSB2"), BaudRate: 115200, DataBits: 8, StopBits: 1, Parity: "none", SMSEnabled: true, DeviceBackend: "at"})
		if err == nil {
			m.SetSMSCallback(a.receive)
			m.SetOnDisconnectWithReason(func(string) {
				select {
				case disconnected <- struct{}{}:
				default:
				}
			})
			err = m.Start()
		}
		if err != nil {
			log.Print("module connection failed; retrying")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		m.WaitReady(15 * time.Second)
		a.modemMu.Lock()
		a.manager = m
		a.mu.Lock()
		a.line = lineSnapshot{}
		a.mu.Unlock()
		a.modemMu.Unlock()
		ticker := time.NewTicker(12 * time.Second)
		go func() {
			if a.pollMu.TryLock() {
				defer a.pollMu.Unlock()
				a.updateLine(m, false)
				m.CheckAllSMS()
			}
		}()
	connectedLoop:
		for ctx.Err() == nil {
			select {
			case <-disconnected:
				break connectedLoop
			case <-ctx.Done():
			case <-ticker.C:
				if a.pollMu.TryLock() {
					a.updateLine(m, false)
					m.CheckAllSMS()
					m.RefreshDeviceInfo()
					a.pollMu.Unlock()
				}
			}
			if ctx.Err() != nil {
				break
			}
		}
		ticker.Stop()
		a.modemMu.Lock()
		if a.manager == m {
			a.manager = nil
		}
		a.modemMu.Unlock()
		m.StopAndWait(3 * time.Second)
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}
func probe() {
	p := env("HOMESIM_TTY", "/dev/ttyUSB2")
	s, err := modem.NewSerialAT(p, 115200, 8, 1, "N")
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	for _, cmd := range []string{"AT", "ATI", "AT+CPIN?", "AT+CSQ", "AT+CREG?", "AT+QNWINFO", "AT+CLCC"} {
		resp, err := s.Execute(cmd, 3*time.Second)
		fmt.Printf("%s\n%s\nerror=%v\n", cmd, resp, err)
	}
}
