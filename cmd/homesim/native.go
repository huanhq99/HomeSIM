package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type nativeRegistration struct {
	ID         string    `json:"id"`
	Token      string    `json:"token"`
	Production bool      `json:"production"`
	Hash       string    `json:"hash"`
	Expires    time.Time `json:"expires"`
}
type nativeState struct{ mu sync.Mutex }

func apnsReady() bool {
	return os.Getenv("HOMESIM_APNS_KEY_FILE") != "" && os.Getenv("HOMESIM_APNS_KEY_ID") != "" && os.Getenv("HOMESIM_APNS_TEAM_ID") != ""
}
func tokenHash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func (a *app) nativeRecords() ([]nativeRegistration, error) {
	rows := []nativeRegistration{}
	b, e := os.ReadFile(filepath.Join(a.data, "native-devices.json"))
	if errors.Is(e, os.ErrNotExist) {
		return rows, nil
	}
	if e == nil {
		e = json.Unmarshal(b, &rows)
	}
	return rows, e
}
func (a *app) nativeAuthenticated(r *http.Request) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(token) {
		return false
	}
	a.native.mu.Lock()
	defer a.native.mu.Unlock()
	rows, err := a.nativeRecords()
	if err != nil {
		return false
	}
	hash := tokenHash(token)
	for _, row := range rows {
		if time.Now().Before(row.Expires) && subtle.ConstantTimeCompare([]byte(hash), []byte(row.Hash)) == 1 {
			return true
		}
	}
	return false
}
func (a *app) nativeRegister(w http.ResponseWriter, r *http.Request) {
	a.native.mu.Lock()
	defer a.native.mu.Unlock()
	rows, err := a.nativeRecords()
	if err != nil {
		fail(w, 503, "配对信息无法读取")
		return
	}
	if r.Method == "DELETE" {
		hash := tokenHash(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		next := []nativeRegistration{}
		for _, row := range rows {
			if row.Hash != hash {
				next = append(next, row)
			}
		}
		if atomicJSON(filepath.Join(a.data, "native-devices.json"), next) != nil {
			fail(w, 503, "配对信息未移除")
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	if !secure(r) || !apnsReady() {
		fail(w, 503, "NAS 尚未配置 Apple 来电推送，或连接未使用 HTTPS")
		return
	}
	// Pairing requires a browser login; an existing bearer cannot enroll new devices.
	var b struct {
		ID          string `json:"id"`
		Token       string `json:"token"`
		Environment string `json:"environment"`
	}
	if !body(w, r, &b) {
		return
	}
	enrolled := false
	if cookie, e := r.Cookie("homesim"); e == nil {
		a.mu.Lock()
		s, ok := a.sessions[cookie.Value]
		a.mu.Unlock()
		enrolled = ok && time.Now().Before(s.Expires)
	}
	if !enrolled {
		hash := tokenHash(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		for _, row := range rows {
			if row.ID == b.ID && row.Hash == hash && time.Now().Before(row.Expires) {
				enrolled = true
				break
			}
		}
	}
	if !enrolled {
		fail(w, 403, "请登录后开启来电推送；已配对设备只能更新自己的推送令牌")
		return
	}
	if b.Environment != "sandbox" && b.Environment != "production" {
		fail(w, 400, "推送环境不正确")
		return
	}
	if _, err = uuid.Parse(b.ID); err != nil || !regexp.MustCompile(`^[a-f0-9]{64,200}$`).MatchString(b.Token) {
		fail(w, 400, "设备推送信息不正确")
		return
	}
	token := randomID()
	row := nativeRegistration{b.ID, b.Token, b.Environment == "production", tokenHash(token), time.Now().Add(90 * 24 * time.Hour)}
	next := []nativeRegistration{}
	for _, old := range rows {
		if old.ID != b.ID && time.Now().Before(old.Expires) {
			next = append(next, old)
		}
	}
	if len(next) >= 20 {
		fail(w, 409, "已达到设备数量上限，请先移除旧设备")
		return
	}
	next = append(next, row)
	if atomicJSON(filepath.Join(a.data, "native-devices.json"), next) != nil {
		fail(w, 503, "设备配对未保存")
		return
	}
	reply(w, 200, map[string]string{"auth_token": token, "expires": row.Expires.Format(time.RFC3339)})
}
func (a *app) nativeCapabilities(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"callkit": true, "apns_configured": apnsReady(), "turn_configured": len(voiceICE().ICEServers) > 0})
}
func apnsJWT() (string, error) {
	b, err := os.ReadFile(os.Getenv("HOMESIM_APNS_KEY_FILE"))
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", errors.New("invalid APNs key")
	}
	v, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	key, ok := v.(*ecdsa.PrivateKey)
	if !ok {
		return "", errors.New("invalid APNs key type")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": os.Getenv("HOMESIM_APNS_TEAM_ID"), "iat": time.Now().Unix()})
	token.Header["kid"] = os.Getenv("HOMESIM_APNS_KEY_ID")
	return token.SignedString(key)
}
func (a *app) pushIncoming(row callRecord) {
	if !apnsReady() {
		return
	}
	root := a.accountApp()
	root.native.mu.Lock()
	devices, err := root.nativeRecords()
	root.native.mu.Unlock()
	if err != nil {
		return
	}
	// No SMS payloads through PushKit; only a newly detected real incoming call.
	for _, device := range devices {
		if time.Now().After(device.Expires) {
			continue
		}
		device := device
		go func() {
			token, err := apnsJWT()
			if err != nil {
				a.addEvent("push", "Apple 推送密钥无法使用")
				return
			}
			host := "https://api.sandbox.push.apple.com"
			if device.Production {
				host = "https://api.push.apple.com"
			}
			data, _ := json.Marshal(map[string]any{"aps": map[string]any{"content-available": 1}, "call_id": row.ID, "device_id": a.deviceID, "number": row.Number})
			req, _ := http.NewRequest("POST", host+"/3/device/"+device.Token, bytes.NewReader(data))
			req.Header.Set("authorization", "bearer "+token)
			req.Header.Set("apns-topic", env("HOMESIM_APNS_BUNDLE_ID", "com.huanhq.hblog")+".voip")
			req.Header.Set("apns-push-type", "voip")
			req.Header.Set("apns-priority", "10")
			req.Header.Set("apns-expiration", "0")
			client := &http.Client{Timeout: 15 * time.Second}
			response, err := client.Do(req)
			if err != nil {
				a.addEvent("push", "Apple 来电推送未确认")
				return
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				a.addEvent("push", "Apple 来电推送被拒绝，请检查签名与设备配置")
			}
		}()
	}
}
