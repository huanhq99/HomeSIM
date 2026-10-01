package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/iniwex5/vohive/internal/backend"
	"github.com/iniwex5/vohive/internal/esim"
	"github.com/iniwex5/vohive/internal/modem"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type operationJob struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Step     string `json:"step"`
	Progress int    `json:"progress"`
	Error    string `json:"error,omitempty"`
	Result   any    `json:"result,omitempty"`
}
type cardNetwork struct {
	ICCID string `json:"iccid"`
	APN   string `json:"apn"`
	Mode  string `json:"mode"`
	PLMN  string `json:"plmn"`
}
type featureState struct {
	mu    sync.Mutex
	esim  *esim.Manager
	modem *modem.Manager
	job   *operationJob
}

func (a *app) hardwareOperation() error {
	a.voiceMu.Lock()
	busy := a.voice != nil
	a.voiceMu.Unlock()
	if busy {
		return errors.New("请先结束通话并断开音频")
	}
	c, err := a.currentCalls()
	if err != nil {
		return err
	}
	if len(c) > 0 {
		return errors.New("线路正在通话，请稍后再操作")
	}
	return nil
}
func (a *app) cardNetworks() (map[string]cardNetwork, error) {
	rows := map[string]cardNetwork{}
	b, err := os.ReadFile(filepath.Join(a.accountApp().data, "network-cards.json"))
	if errors.Is(err, os.ErrNotExist) {
		return rows, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &rows)
	return rows, err
}
func (a *app) networkInfo(w http.ResponseWriter, r *http.Request) {
	m := a.getModem()
	if m == nil {
		fail(w, 503, "模块未连接")
		return
	}
	apn, e1 := m.QueryAPN()
	ims, e2 := m.QueryIMSStatus()
	selection, e3 := m.QueryOperatorSelection()
	account := a.accountApp()
	account.mu.Lock()
	cards, err := a.cardNetworks()
	account.mu.Unlock()
	if err != nil {
		fail(w, 503, "卡设置文件无法读取")
		return
	}
	line := a.lineInfo(true)
	saved := cards[line["iccid"].(string)]
	reply(w, 200, map[string]any{"apn": apn, "apn_known": e1 == nil, "ims": ims, "ims_known": e2 == nil, "selection": selection, "selection_known": e3 == nil, "saved": saved, "iccid": line["iccid"], "card_verified": line["manual_allowed"]})
}

var apnPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,99}$`)
var plmnPattern = regexp.MustCompile(`^[0-9]{5,6}$`)
var ussdPattern = regexp.MustCompile(`^[*#0-9+]{1,40}#$`)

func (a *app) networkUpdate(w http.ResponseWriter, r *http.Request) {
	var b cardNetwork
	if !body(w, r, &b) {
		return
	}
	if !iccidPattern.MatchString(b.ICCID) || !apnPattern.MatchString(b.APN) || (b.Mode != "automatic" && b.Mode != "manual") || (b.Mode == "manual" && !plmnPattern.MatchString(b.PLMN)) {
		fail(w, 400, "卡号、APN 或运营商代码不正确")
		return
	}
	if !a.sendMu.TryLock() {
		fail(w, 409, "线路正在操作，请稍后重试")
		return
	}
	defer a.sendMu.Unlock()
	if err := a.hardwareOperation(); err != nil {
		fail(w, 409, err.Error())
		return
	}
	m := a.getModem()
	if m == nil {
		fail(w, 503, "模块已断开")
		return
	}
	iccid, err := m.QueryICCID()
	if err != nil || cleanICCID(iccid) != b.ICCID {
		fail(w, 409, "SIM 已变化，请重新检测")
		return
	}
	_, err = m.ExecuteAT(`AT+CGDCONT=1,"IPV4V6","`+b.APN+`"`, 5*time.Second)
	if err == nil {
		if b.Mode == "automatic" {
			err = m.SetOperatorAutomatic()
		} else {
			err = m.SetOperatorManual(b.PLMN, 0, false)
		}
	}
	if err != nil {
		a.addEvent("network", "网络设置部分指令未确认，请重新检测")
		fail(w, 409, "部分设置可能已生效，请重新检测网络后再操作")
		return
	}
	account := a.accountApp()
	account.mu.Lock()
	defer account.mu.Unlock()
	cards, err := a.cardNetworks()
	if err == nil {
		cards[b.ICCID] = b
		err = atomicJSON(filepath.Join(account.data, "network-cards.json"), cards)
	}
	if err != nil {
		fail(w, 503, "模块设置已提交，但卡设置未保存")
		return
	}
	a.addEvent("network", "网络设置已提交")
	reply(w, 200, map[string]bool{"ok": true})
}
func (a *app) ussd(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Command string `json:"command"`
	}
	if !body(w, r, &b) {
		return
	}
	if !ussdPattern.MatchString(b.Command) {
		fail(w, 400, "USSD 格式不正确，例如 *100#")
		return
	}
	if !a.sendMu.TryLock() {
		fail(w, 409, "线路正在操作")
		return
	}
	defer a.sendMu.Unlock()
	if err := a.hardwareOperation(); err != nil {
		fail(w, 409, err.Error())
		return
	}
	m := a.getModem()
	if m == nil {
		fail(w, 503, "模块已断开")
		return
	}
	result, err := m.ExecuteUSSD(b.Command, 45*time.Second)
	if err != nil {
		fail(w, 409, "运营商未确认 USSD 结果，请先核对套餐状态")
		return
	}
	reply(w, 200, result)
}
func (a *app) esimManager() (*esim.Manager, error) {
	m := a.getModem()
	if m == nil {
		return nil, errors.New("模块未连接")
	}
	f := &a.features
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.esim == nil || f.modem != m {
		e, err := esim.NewManager(esim.ManagerOptions{DeviceID: a.deviceID, Transport: "at", Modem: m, Backend: backend.NewATBackend(m)})
		if err != nil {
			return nil, err
		}
		f.esim = e
		f.modem = m
	}
	return f.esim, nil
}
func (a *app) beginJob(w http.ResponseWriter, work func(context.Context) (any, error)) {
	if !a.sendMu.TryLock() {
		fail(w, 409, "线路已有操作正在执行")
		return
	}
	if err := a.hardwareOperation(); err != nil {
		a.sendMu.Unlock()
		fail(w, 409, err.Error())
		return
	}
	j := operationJob{ID: randomID(), State: "running", Step: "准备中"}
	a.features.mu.Lock()
	a.features.job = &j
	a.features.mu.Unlock()
	reply(w, 202, j)
	go func() {
		defer a.sendMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		result, err := work(ctx)
		a.features.mu.Lock()
		j.State = "completed"
		j.Progress = 100
		j.Result = result
		if err != nil {
			j.State = "failed"
			j.Error = "操作未完成或结果未确认，请重新检测设备后再操作"
		}
		a.features.mu.Unlock()
		a.addEvent("esim", "eSIM 操作结束，请查看结果")
	}()
}
func (a *app) esimInspect(w http.ResponseWriter, r *http.Request) {
	a.beginJob(w, func(ctx context.Context) (any, error) {
		m, err := a.esimManager()
		if err != nil {
			return nil, err
		}
		if err = m.RefreshOverview(); err != nil {
			return nil, err
		}
		return m.GetEsimOverview()
	})
}
func (a *app) esimAction(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Action       string `json:"action"`
		ICCID        string `json:"iccid"`
		AID          string `json:"aid"`
		EID          string `json:"eid"`
		Name         string `json:"name"`
		SMDP         string `json:"smdp"`
		MatchingID   string `json:"matching_id"`
		Confirmation string `json:"confirmation"`
	}
	if !body(w, r, &b) {
		return
	}
	if !regexp.MustCompile(`^[0-9A-Fa-f]{10,64}$`).MatchString(b.AID) || !regexp.MustCompile(`^[0-9]{32}$`).MatchString(b.EID) {
		fail(w, 400, "请选择有效的 eSIM 芯片")
		return
	}
	switch b.Action {
	case "enable", "disable", "delete", "rename":
		if !iccidPattern.MatchString(b.ICCID) || len([]rune(b.Name)) > 40 {
			fail(w, 400, "Profile 参数不正确")
			return
		}
	case "download":
		if !regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`).MatchString(b.SMDP) || len(b.MatchingID) > 512 || len(b.Confirmation) > 128 {
			fail(w, 400, "激活码参数不正确")
			return
		}
	default:
		fail(w, 400, "未知 eSIM 操作")
		return
	}
	a.beginJob(w, func(ctx context.Context) (any, error) {
		m, err := a.esimManager()
		if err != nil {
			return nil, err
		}
		if err = m.RefreshOverview(); err != nil {
			return nil, err
		}
		overview, err := m.GetEsimOverview()
		if err != nil {
			return nil, err
		}
		same := false
		for _, group := range overview.Profiles {
			if group.EID == b.EID && group.AIDHex == strings.ToUpper(b.AID) {
				same = true
			}
		}
		if !same {
			return nil, errors.New("eSIM card changed")
		}
		switch b.Action {
		case "enable":
			return m.SwitchProfileWithResult(ctx, b.ICCID, b.AID)
		case "disable":
			return nil, m.DisableProfile(ctx, b.ICCID, b.AID)
		case "delete":
			return m.DeleteProfile(b.ICCID, b.AID)
		case "rename":
			return nil, m.RenameProfile(b.ICCID, b.Name, b.AID)
		case "download":
			return m.DownloadProfile(ctx, b.AID, b.SMDP, b.MatchingID, b.Confirmation, "", func(e esim.DownloadProgressEvent) {
				a.features.mu.Lock()
				a.features.job.Step = e.Step
				a.features.job.Progress = e.Pct
				a.features.mu.Unlock()
			})
		}
		return nil, errors.New("invalid operation")
	})
}
func (a *app) jobStatus(w http.ResponseWriter, r *http.Request) {
	a.features.mu.Lock()
	defer a.features.mu.Unlock()
	if a.features.job == nil || a.features.job.ID != r.URL.Query().Get("id") {
		fail(w, 404, "操作记录不存在，服务重启后请重新检测设备")
		return
	}
	reply(w, 200, a.features.job)
}
