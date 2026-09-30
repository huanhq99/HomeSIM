package main

import (
	"encoding/csv"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iniwex5/vohive/pkg/smscodec"
)

type peerInfo struct {
	Name     string `json:"name"`
	Starred  bool   `json:"starred"`
	Archived bool   `json:"archived"`
}

func (a *app) listPeers(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	reply(w, 200, a.peers)
}

func (a *app) updatePeer(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Peer     string  `json:"peer"`
		Name     *string `json:"name"`
		Starred  *bool   `json:"starred"`
		Archived *bool   `json:"archived"`
	}
	if !body(w, r, &b) {
		return
	}
	// Incoming sender IDs may be alphanumeric. Permit existing IDs as well as
	// validated numbers; never reinterpret them as paths or AT commands.
	a.mu.Lock()
	defer a.mu.Unlock()
	known := phonePattern.MatchString(b.Peer)
	for _, m := range a.messages {
		if m.Peer == b.Peer {
			known = true
			break
		}
	}
	if !known || len(b.Peer) > 128 || (b.Name == nil && b.Starred == nil && b.Archived == nil) {
		fail(w, 400, "请选择有效会话")
		return
	}
	info := a.peers[b.Peer]
	if b.Name != nil {
		name := strings.TrimSpace(*b.Name)
		if !utf8.ValidString(name) || len([]rune(name)) > 40 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			fail(w, 400, "备注最多 40 个字，不能包含换行或控制字符")
			return
		}
		info.Name = name
	}
	if b.Starred != nil {
		info.Starred = *b.Starred
	}
	if b.Archived != nil {
		info.Archived = *b.Archived
	}
	next := make(map[string]peerInfo, len(a.peers)+1)
	for key, value := range a.peers {
		next[key] = value
	}
	next[b.Peer] = info
	if err := atomicJSON(filepath.Join(a.data, "peers.json"), next); err != nil {
		fail(w, 503, "会话设置未保存，请检查 NAS 空间")
		return
	}
	a.peers = next
	reply(w, 200, info)
}

func (a *app) markRead(w http.ResponseWriter, r *http.Request) {
	var b struct {
		IDs  []string `json:"ids"`
		Read *bool    `json:"read"`
	}
	if !body(w, r, &b) {
		return
	}
	if len(b.IDs) == 0 || len(b.IDs) > 500 || b.Read == nil {
		fail(w, 400, "每次请选择 1–500 条短信")
		return
	}
	wanted := make(map[string]bool, len(b.IDs))
	for _, id := range b.IDs {
		wanted[id] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := append([]message{}, a.messages...)
	for i, m := range next {
		if wanted[m.ID] {
			delete(wanted, m.ID)
			if m.Direction == "incoming" {
				next[i].Read = *b.Read
			}
		}
	}
	if len(wanted) > 0 {
		fail(w, 404, "部分短信已不存在，请刷新列表")
		return
	}
	if err := atomicJSON(filepath.Join(a.data, "messages.json"), next); err != nil {
		fail(w, 503, "阅读状态未保存")
		return
	}
	a.messages = next
	reply(w, 200, map[string]bool{"ok": true})
}

func (a *app) estimate(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Message string `json:"message"`
	}
	if !body(w, r, &b) {
		return
	}
	characters := len([]rune(b.Message))
	if characters > 2000 {
		fail(w, 400, "短信最多 2000 字")
		return
	}
	segments := 0
	if characters > 0 {
		// This is the same encoder used to transmit. No serial/network commands.
		pdus, _, err := smscodec.BuildSubmitTPDUsWithOptions("10086", b.Message, smscodec.SubmitOptions{})
		if err != nil {
			fail(w, 400, "内容无法编码为短信")
			return
		}
		segments = len(pdus)
	}
	reply(w, 200, map[string]int{"characters": characters, "segments": segments})
}

func csvSafe(value string) string {
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}

func (a *app) exportMessages(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "json" && format != "csv" {
		fail(w, 400, "请选择 JSON 或 CSV")
		return
	}
	a.mu.Lock()
	rows := append([]message{}, a.messages...)
	peers := make(map[string]peerInfo, len(a.peers))
	for k, v := range a.peers {
		peers[k] = v
	}
	a.mu.Unlock()
	stamp := time.Now().UTC().Format("20060102-150405")
	w.Header().Set("Content-Disposition", `attachment; filename="homesim-`+stamp+`.`+format+`"`)
	if format == "json" {
		// Account hashes and setup tokens are deliberately outside this export.
		reply(w, 200, map[string]any{"schema_version": 1, "exported_at": time.Now().UTC(), "messages": rows, "peers": peers})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
	c := csv.NewWriter(w)
	_ = c.Write([]string{"时间", "号码", "备注", "方向", "状态", "已读", "正文"})
	for _, m := range rows {
		direction := "接收"
		if m.Direction == "outgoing" {
			direction = "发送"
		}
		read := "否"
		if m.Read || m.Direction == "outgoing" {
			read = "是"
		}
		_ = c.Write([]string{m.Timestamp.Format(time.RFC3339), csvSafe(m.Peer), csvSafe(peers[m.Peer].Name), direction, csvSafe(m.Status), read, csvSafe(m.Content)})
	}
	c.Flush()
}
