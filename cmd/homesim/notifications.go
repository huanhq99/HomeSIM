package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type notifyConfig struct {
	Channel        string `json:"channel"`
	Enabled        bool   `json:"enabled"`
	URL            string `json:"url,omitempty"`
	Secret         string `json:"secret,omitempty"`
	Target         string `json:"target,omitempty"`
	Username       string `json:"username,omitempty"`
	Host           string `json:"host,omitempty"`
	IncludeContent bool   `json:"include_content"`
}

func (a *app) notifications() (notifyConfig, error) {
	var c notifyConfig
	b, err := os.ReadFile(filepath.Join(a.data, "notifications.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}
func (a *app) notificationConfig(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.notifications()
	if err != nil {
		fail(w, 503, "通知配置无法读取")
		return
	}
	if r.Method == "GET" {
		hasURL, hasSecret := c.URL != "", c.Secret != ""
		c.URL = ""
		c.Secret = ""
		reply(w, 200, map[string]any{"config": c, "has_url": hasURL, "has_secret": hasSecret})
		return
	}
	var next notifyConfig
	if !body(w, r, &next) {
		return
	}
	if next.Channel == c.Channel {
		if next.URL == "" {
			next.URL = c.URL
		}
		if next.Secret == "" {
			next.Secret = c.Secret
		}
	}
	if next.Enabled {
		switch next.Channel {
		case "bark", "webhook":
			u, e := url.Parse(next.URL)
			if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(next.URL) > 2048 {
				fail(w, 400, "通知地址需要完整 HTTPS URL")
				return
			}
		case "telegram":
			if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]{20,100}$`).MatchString(next.Secret) || !regexp.MustCompile(`^-?[0-9]{1,30}$`).MatchString(next.Target) {
				fail(w, 400, "Telegram Bot Token 或 Chat ID 不正确")
				return
			}
		case "smtp":
			if !regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`).MatchString(next.Host) || strings.ContainsAny(next.Username+next.Target, "\r\n") || len(next.Secret) > 500 {
				fail(w, 400, "SMTP 参数不正确")
				return
			}
			if _, e := mail.ParseAddress(next.Username); e != nil {
				fail(w, 400, "发件地址不正确")
				return
			}
			if _, e := mail.ParseAddress(next.Target); e != nil {
				fail(w, 400, "收件地址不正确")
				return
			}
		default:
			fail(w, 400, "不支持的通知渠道")
			return
		}
	}
	if atomicJSON(filepath.Join(a.data, "notifications.json"), next) != nil {
		fail(w, 503, "通知配置未保存")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *app) notify(title, detail string) {
	root := a.accountApp()
	root.mu.Lock()
	c, err := root.notifications()
	root.mu.Unlock()
	if err != nil || !c.Enabled {
		return
	}
	if !c.IncludeContent {
		detail = "打开家信查看详情。"
	}
	// Notifications are opt-in. A bounded queue avoids launching unlimited workers.
	select {
	case root.noticeQueue <- notificationTask{c, title, detail, a}:
	default:
		a.addEvent("notification", "通知队列已满，本次未发送")
	}
}

type notificationTask struct {
	Config        notifyConfig
	Title, Detail string
	Line          *app
}

func (a *app) notificationLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-a.noticeQueue:
			if err := sendNotification(ctx, task); err != nil {
				task.Line.addEvent("notification", "通知发送失败，未自动重发，请检查配置")
			} else {
				task.Line.addEvent("notification", "通知已提交通知服务")
			}
		}
	}
}
func sendNotification(ctx context.Context, task notificationTask) error {
	c := task.Config
	endpoint := c.URL
	payload := map[string]any{"title": task.Title, "body": task.Detail}
	switch c.Channel {
	case "telegram":
		endpoint = "https://api.telegram.org/bot" + c.Secret + "/sendMessage"
		payload = map[string]any{"chat_id": c.Target, "text": task.Title + "\n" + task.Detail}
	case "smtp":
		// Port 587 with mandatory STARTTLS. Never transmit account credentials in cleartext.
		conn, err := net.DialTimeout("tcp", c.Host+":587", 10*time.Second)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		client, err := smtp.NewClient(conn, c.Host)
		if err != nil {
			return err
		}
		defer client.Close()
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP STARTTLS required")
		}
		if err = client.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
		if err = client.Auth(smtp.PlainAuth("", c.Username, c.Secret, c.Host)); err != nil {
			return err
		}
		if err = client.Mail(c.Username); err != nil {
			return err
		}
		if err = client.Rcpt(c.Target); err != nil {
			return err
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: HomeSIM notification\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n%s\r\n", c.Username, c.Target, task.Title, task.Detail)
		if err != nil {
			return err
		}
		if err = w.Close(); err != nil {
			return err
		}
		return client.Quit()
	}
	data, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("notification service rejected request")
	}
	return nil
}
