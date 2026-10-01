package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"github.com/pion/webrtc/v4"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Coturn REST credentials expire after one hour; its shared secret stays on NAS.
func voiceICE() webrtc.Configuration {
	c := webrtc.Configuration{}
	urls, secret := os.Getenv("HOMESIM_TURN_URLS"), os.Getenv("HOMESIM_TURN_SECRET")
	if urls == "" || secret == "" {
		return c
	}
	user := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + ":homesim"
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(user))
	c.ICEServers = []webrtc.ICEServer{{URLs: strings.Split(urls, ","), Username: user, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil))}}
	if os.Getenv("HOMESIM_FORCE_RELAY") == "1" {
		c.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	return c
}
func (a *app) voiceConfig(w http.ResponseWriter, r *http.Request) {
	c := voiceICE()
	servers := []map[string]any{}
	for _, s := range c.ICEServers {
		servers = append(servers, map[string]any{"urls": s.URLs, "username": s.Username, "credential": s.Credential})
	}
	reply(w, 200, map[string]any{"iceServers": servers, "iceTransportPolicy": c.ICETransportPolicy.String(), "relay_configured": len(servers) > 0})
}
