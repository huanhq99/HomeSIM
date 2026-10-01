package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type proxyConfig struct {
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`
	Listen    string `json:"listen"`
	Port      int    `json:"port"`
	Interface string `json:"interface"`
	Username  string `json:"username"`
	Password  string `json:"password,omitempty"`
}
type trafficPoint struct {
	At       time.Time `json:"at"`
	Upload   uint64    `json:"upload"`
	Download uint64    `json:"download"`
}
type proxyState struct {
	mu          sync.Mutex
	listener    net.Listener
	config      proxyConfig
	up, down    atomic.Uint64
	points      []trafficPoint
	connections map[net.Conn]bool
	lastError   string
	started     bool
}

func (a *app) networkInterfaces() []string {
	target, _ := filepath.EvalSymlinks(a.deviceTTY())
	usb := usbParent(filepath.Join("/sys/class/tty", filepath.Base(target), "device"))
	out := []string{}
	if usb == "" {
		return out
	}
	paths, _ := filepath.Glob("/sys/class/net/*")
	for _, p := range paths {
		if usbParent(filepath.Join(p, "device")) == usb {
			out = append(out, filepath.Base(p))
		}
	}
	return out
}
func validProxy(c proxyConfig, interfaces []string) bool {
	if !c.Enabled {
		return true
	}
	if (c.Mode != "http" && c.Mode != "socks5") || c.Port < 1024 || c.Port > 65535 || len(c.Username) < 1 || len(c.Username) > 64 || len(c.Password) < 12 || len(c.Password) > 128 {
		return false
	}
	ip := net.ParseIP(c.Listen)
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return false
	}
	for _, n := range interfaces {
		if n == c.Interface {
			return true
		}
	}
	return false
}
func (a *app) proxyInfo(w http.ResponseWriter, r *http.Request) {
	p := &a.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.config
	c.Password = ""
	reply(w, 200, map[string]any{"config": c, "running": p.listener != nil, "interfaces": a.networkInterfaces(), "upload": p.up.Load(), "download": p.down.Load(), "history": p.points, "error": p.lastError, "password_set": p.config.Password != ""})
}
func (a *app) proxyUpdate(w http.ResponseWriter, r *http.Request) {
	var c proxyConfig
	if !body(w, r, &c) {
		return
	}
	p := &a.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.Password == "" {
		c.Password = p.config.Password
	}
	if !validProxy(c, a.networkInterfaces()) {
		fail(w, 400, "请选择本模块的 USB 网络接口、局域网监听地址和至少 12 字符密码")
		return
	}
	if err := atomicJSON(filepath.Join(a.data, "proxy.json"), c); err != nil {
		fail(w, 503, "代理设置未保存")
		return
	}
	if p.listener != nil {
		_ = p.listener.Close()
		p.listener = nil
	}
	for conn := range p.connections {
		_ = conn.Close()
	}
	p.config = c
	p.lastError = ""
	if c.Enabled {
		listener, err := net.Listen("tcp", net.JoinHostPort(c.Listen, strconv.Itoa(c.Port)))
		if err != nil {
			p.lastError = "监听失败，请检查地址与端口占用"
			fail(w, 409, p.lastError)
			return
		}
		p.listener = listener
		go a.serveProxy(listener, c)
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *app) proxyStart(ctx context.Context) {
	p := &a.proxy
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.connections = map[net.Conn]bool{}
	b, err := os.ReadFile(filepath.Join(a.data, "traffic.json"))
	if err == nil {
		_ = json.Unmarshal(b, &p.points)
	}
	b, err = os.ReadFile(filepath.Join(a.data, "proxy.json"))
	if err == nil && json.Unmarshal(b, &p.config) == nil && p.config.Enabled {
		p.lastError = "服务重启后请确认 USB 网口并手动启用代理"
	}
	p.mu.Unlock()
	go func() {
		timer := time.NewTicker(5 * time.Minute)
		defer timer.Stop()
		var up, down uint64
		save := func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			u, d := p.up.Load(), p.down.Load()
			p.points = append(p.points, trafficPoint{time.Now(), u - up, d - down})
			up, down = u, d
			if len(p.points) > 8640 {
				p.points = p.points[len(p.points)-8640:]
			}
			_ = atomicJSON(filepath.Join(a.data, "traffic.json"), p.points)
		}
		for {
			select {
			case <-ctx.Done():
				p.mu.Lock()
				if p.listener != nil {
					_ = p.listener.Close()
					p.listener = nil
				}
				for conn := range p.connections {
					_ = conn.Close()
				}
				p.mu.Unlock()
				save()
				return
			case <-timer.C:
				save()
			}
		}
	}()
}

type trafficConn struct {
	net.Conn
	p *proxyState
}

func (c *trafficConn) Read(b []byte) (int, error) {
	n, e := c.Conn.Read(b)
	c.p.down.Add(uint64(n))
	return n, e
}
func (c *trafficConn) Write(b []byte) (int, error) {
	n, e := c.Conn.Write(b)
	c.p.up.Add(uint64(n))
	return n, e
}
func (c *trafficConn) Close() error {
	err := c.Conn.Close()
	c.p.mu.Lock()
	delete(c.p.connections, c.Conn)
	c.p.mu.Unlock()
	return err
}
func (a *app) proxyDial(ctx context.Context, c proxyConfig, target string) (net.Conn, error) {
	conn, err := boundDialer(c.Interface).DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	a.proxy.mu.Lock()
	if a.proxy.connections == nil {
		a.proxy.connections = map[net.Conn]bool{}
	}
	a.proxy.connections[conn] = true
	a.proxy.mu.Unlock()
	return &trafficConn{conn, &a.proxy}, nil
}
func (a *app) serveProxy(listener net.Listener, c proxyConfig) {
	defer func() {
		a.proxy.mu.Lock()
		defer a.proxy.mu.Unlock()
		if a.proxy.listener == listener {
			a.proxy.listener = nil
			a.proxy.lastError = "代理监听已停止，请检查网口与配置"
		}
	}()
	if c.Mode == "http" {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, target string) (net.Conn, error) { return a.proxyDial(ctx, c, target) }, ResponseHeaderTimeout: 20 * time.Second}
		defer transport.CloseIdleConnections()
		server := &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := proxyBasicAuth(r)
			if !ok || !proxyCredentials(c, u, p) {
				w.Header().Set("Proxy-Authenticate", `Basic realm="HomeSIM"`)
				w.WriteHeader(407)
				return
			}
			if r.Method == "CONNECT" {
				conn, err := a.proxyDial(r.Context(), c, r.Host)
				if err != nil {
					http.Error(w, "上游连接失败", 502)
					return
				}
				h, ok := w.(http.Hijacker)
				if !ok {
					conn.Close()
					return
				}
				client, buf, err := h.Hijack()
				if err != nil {
					conn.Close()
					return
				}
				defer client.Close()
				defer conn.Close()
				_ = client.SetDeadline(time.Now().Add(10 * time.Minute))
				_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
				buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				buf.Flush()
				a.trackProxy(client, true)
				defer a.trackProxy(client, false)
				done := make(chan struct{})
				go func() { io.Copy(conn, buf); close(done); conn.Close() }()
				io.Copy(client, conn)
				client.Close()
				<-done
				return
			}
			if r.URL.Scheme != "http" || r.URL.Host == "" {
				http.Error(w, "无效代理请求", 400)
				return
			}
			request := r.Clone(r.Context())
			request.RequestURI = ""
			stripProxyHeaders(request.Header)
			response, err := transport.RoundTrip(request)
			if err != nil {
				http.Error(w, "上游连接失败", 502)
				return
			}
			defer response.Body.Close()
			stripProxyHeaders(response.Header)
			for k, v := range response.Header {
				w.Header()[k] = v
			}
			w.WriteHeader(response.StatusCode)
			io.Copy(w, response.Body)
		})}
		_ = server.Serve(listener)
		return
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			a.trackProxy(conn, true)
			defer a.trackProxy(conn, false)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
			reader := bufio.NewReader(conn)
			if socksHandshake(reader, conn, c) != nil {
				return
			}
			target, err := socksTarget(reader)
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			up, err := a.proxyDial(ctx, c, target)
			if err != nil {
				conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
				return
			}
			defer up.Close()
			conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
			_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
			_ = up.SetDeadline(time.Now().Add(10 * time.Minute))
			done := make(chan struct{})
			go func() { io.Copy(up, reader); up.Close(); close(done) }()
			io.Copy(conn, up)
			conn.Close()
			<-done
		}()
	}
}
func (a *app) trackProxy(c net.Conn, add bool) {
	a.proxy.mu.Lock()
	defer a.proxy.mu.Unlock()
	if add {
		a.proxy.connections[c] = true
	} else {
		delete(a.proxy.connections, c)
	}
}
func proxyCredentials(c proxyConfig, u, p string) bool {
	return subtle.ConstantTimeCompare([]byte(c.Username), []byte(u)) == 1 && subtle.ConstantTimeCompare([]byte(c.Password), []byte(p)) == 1
}
func proxyBasicAuth(r *http.Request) (string, string, bool) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", r.Header.Get("Proxy-Authorization"))
	return clone.BasicAuth()
}
func stripProxyHeaders(h http.Header) {
	for _, key := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(key))
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}
func socksHandshake(r io.Reader, w io.Writer, c proxyConfig) error {
	head := make([]byte, 2)
	if _, e := io.ReadFull(r, head); e != nil || head[0] != 5 {
		return errors.New("invalid socks version")
	}
	methods := make([]byte, int(head[1]))
	if _, e := io.ReadFull(r, methods); e != nil {
		return e
	}
	if !strings.ContainsRune(string(methods), 2) {
		w.Write([]byte{5, 255})
		return errors.New("auth required")
	}
	w.Write([]byte{5, 2})
	if _, e := io.ReadFull(r, head); e != nil || head[0] != 1 {
		return errors.New("invalid auth")
	}
	u := make([]byte, int(head[1]))
	if _, e := io.ReadFull(r, u); e != nil {
		return e
	}
	length := make([]byte, 1)
	if _, e := io.ReadFull(r, length); e != nil {
		return e
	}
	p := make([]byte, int(length[0]))
	if _, e := io.ReadFull(r, p); e != nil {
		return e
	}
	if !proxyCredentials(c, string(u), string(p)) {
		w.Write([]byte{1, 1})
		return errors.New("bad auth")
	}
	_, e := w.Write([]byte{1, 0})
	return e
}
func socksTarget(r io.Reader) (string, error) {
	head := make([]byte, 4)
	if _, e := io.ReadFull(r, head); e != nil || head[0] != 5 || head[1] != 1 {
		return "", errors.New("only CONNECT supported")
	}
	var host string
	switch head[3] {
	case 1:
		b := make([]byte, 4)
		if _, e := io.ReadFull(r, b); e != nil {
			return "", e
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, e := io.ReadFull(r, b); e != nil {
			return "", e
		}
		host = net.IP(b).String()
	case 3:
		b := make([]byte, 1)
		if _, e := io.ReadFull(r, b); e != nil {
			return "", e
		}
		name := make([]byte, int(b[0]))
		if _, e := io.ReadFull(r, name); e != nil {
			return "", e
		}
		host = string(name)
	default:
		return "", errors.New("invalid address")
	}
	port := make([]byte, 2)
	if _, e := io.ReadFull(r, port); e != nil {
		return "", e
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), nil
}
