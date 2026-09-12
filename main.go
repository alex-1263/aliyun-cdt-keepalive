package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 配置结构 ----------

type Instance struct {
	Name           string  `json:"name"`
	Region         string  `json:"region"`
	InstanceID     string  `json:"instance_id"`
	AK             string  `json:"ak"`
	SK             string  `json:"sk"`
	TrafficLimitGB float64 `json:"traffic_limit_gb"`
	Paused         bool    `json:"paused,omitempty"`
}

type Config struct {
	FeishuWebhook string     `json:"feishu_webhook"`
	Instances     []Instance `json:"instances"`
}

type InstState struct {
	OverLimitStopped bool `json:"over_limit_stopped"`
}

type State struct {
	Instances map[string]InstState `json:"instances"`
}

var (
	feishuWebhook string
	configPath    string
	statePath     string
	dryRun        bool
	reportMode    bool
	stateMu       sync.Mutex
	state         State
)

// ---------- 阿里云 RPC 签名（V1, HMAC-SHA1）----------

func percentEncode(s string) string {
	s = url.QueryEscape(s)
	s = strings.ReplaceAll(s, "+", "%20")
	s = strings.ReplaceAll(s, "*", "%2A")
	s = strings.ReplaceAll(s, "%7E", "~")
	return s
}

func randNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// aliyunRPC 调用阿里云 RPC 风格 API（GET, JSON 返回）
func aliyunRPC(ak, sk, domain, version, action string, extra map[string]string) (map[string]any, error) {
	p := url.Values{}
	p.Set("AccessKeyId", ak)
	p.Set("Action", action)
	p.Set("Format", "JSON")
	p.Set("SignatureMethod", "HMAC-SHA1")
	p.Set("SignatureNonce", randNonce())
	p.Set("SignatureVersion", "1.0")
	p.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	p.Set("Version", version)
	for k, v := range extra {
		p.Set(k, v)
	}

	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(percentEncode(k))
		b.WriteString("=")
		b.WriteString(percentEncode(p.Get(k)))
		b.WriteString("&")
	}
	cqs := strings.TrimSuffix(b.String(), "&")
	sts := "GET&" + percentEncode("/") + "&" + percentEncode(cqs)

	mac := hmac.New(sha1.New, []byte(sk+"&"))
	mac.Write([]byte(sts))
	p.Set("Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get("https://" + domain + "/?" + p.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("响应非JSON: %s", string(body[:min(len(body), 200)]))
	}
	if code, ok := m["Code"].(string); ok && code != "" {
		return m, fmt.Errorf("API错误 %s: %s", code, m["Message"])
	}
	return m, nil
}

// ---------- 阿里云业务封装 ----------

func cdtInternetTrafficGB(ak, sk string) (float64, error) {
	m, err := aliyunRPC(ak, sk, "cdt.aliyuncs.com", "2021-08-13",
		"ListCdtInternetTraffic", nil)
	if err != nil {
		return 0, err
	}
	total := 0.0
	if td, ok := m["TrafficDetails"].([]any); ok {
		for _, item := range td {
			if d, ok := item.(map[string]any); ok {
				if v, ok := d["Traffic"].(float64); ok {
					total += v
				}
			}
		}
	}
	return total / (1024 * 1024 * 1024), nil
}

const (
	statusRunning = "Running"
	statusStopped = "Stopped"
)

func instanceStatus(ak, sk, region, id string) (string, error) {
	m, err := aliyunRPC(ak, sk, "ecs."+region+".aliyuncs.com", "2014-05-26",
		"DescribeInstances", map[string]string{
			"RegionId":    region,
			"InstanceIds": `["` + id + `"]`,
		})
	if err != nil {
		return "", err
	}
	set, _ := m["Instances"].(map[string]any)
	arr, _ := set["Instance"].([]any)
	if len(arr) == 0 {
		return "", fmt.Errorf("实例不存在或未授权: %s", id)
	}
	inst, _ := arr[0].(map[string]any)
	s, _ := inst["Status"].(string)
	return s, nil
}

func setInstancePower(ak, sk, region, id string, start bool) error {
	action := "StopInstance"
	if start {
		action = "StartInstance"
	}
	_, err := aliyunRPC(ak, sk, "ecs."+region+".aliyuncs.com", "2014-05-26",
		action, map[string]string{"InstanceId": id})
	return err
}

func accountBalance(ak, sk string) (string, float64, error) {
	m, err := aliyunRPC(ak, sk, "business.aliyuncs.com", "2017-12-14",
		"QueryAccountBalance", nil)
	if err != nil {
		return "", 0, err
	}
	data, _ := m["Data"].(map[string]any)
	amount, _ := data["AvailableAmount"].(string)
	cur, _ := data["Currency"].(string)
	f := 0.0
	fmt.Sscanf(strings.ReplaceAll(amount, ",", ""), "%f", &f)
	return cur, f, nil
}

// ---------- 飞书推送 ----------

func feishuPush(text string) bool {
	if feishuWebhook == "" {
		log.Println("[飞书] 未配置 webhook，跳过推送")
		return false
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(feishuWebhook, "application/json", strings.NewReader(string(body)))
	if err != nil {
		log.Printf("[飞书] 推送失败: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("[飞书] 推送异常: HTTP %d %s", resp.StatusCode, string(b))
		return false
	}
	return true
}

// ---------- 状态持久化 ----------

func loadState() {
	state.Instances = map[string]InstState{}
	f, err := os.Open(statePath)
	if err != nil {
		return
	}
	defer f.Close()
	json.NewDecoder(f).Decode(&state)
	if state.Instances == nil {
		state.Instances = map[string]InstState{}
	}
}

func saveState() {
	stateMu.Lock()
	defer stateMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(statePath), 0o700)
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(statePath, data, 0o600)
}

// ---------- 核心逻辑 ----------

func processInstance(inst Instance) {
	if inst.Paused {
		log.Printf("[%s] 已暂停，跳过", inst.Name)
		return
	}
	icfg := inst
	st := state.Instances[icfg.InstanceID]

	gb, err := cdtInternetTrafficGB(icfg.AK, icfg.SK)
	if err != nil {
		log.Printf("[%s] 查询CDT流量失败: %v", icfg.Name, err)
		return
	}
	status, err := instanceStatus(icfg.AK, icfg.SK, icfg.Region, icfg.InstanceID)
	if err != nil {
		log.Printf("[%s] 查询实例状态失败: %v", icfg.Name, err)
		return
	}
	log.Printf("[%s] 本月CDT出向流量: %.2fGB / %.0fGB，实例状态: %s", icfg.Name, gb, icfg.TrafficLimitGB, status)

	over := gb >= icfg.TrafficLimitGB

	switch {
	case over && status == statusRunning:
		log.Printf("[%s] ⚠️ 流量超限，执行止损关机", icfg.Name)
		if !dryRun {
			if err := setInstancePower(icfg.AK, icfg.SK, icfg.Region, icfg.InstanceID, false); err != nil {
				log.Printf("[%s] 关机失败: %v", icfg.Name, err)
				return
			}
		}
		st.OverLimitStopped = true
		state.Instances[icfg.InstanceID] = st
		feishuPush(fmt.Sprintf("🚨 [%s] CDT流量已达 %.1fGB（阈值 %.0fGB），实例已自动关机止损。\n流量将于本月重置后自动恢复开机。", icfg.Name, gb, icfg.TrafficLimitGB))

	case over && status == statusStopped:
		log.Printf("[%s] 已超限且处于关机状态，等待流量重置", icfg.Name)

	case !over && status == statusStopped && st.OverLimitStopped:
		log.Printf("[%s] 流量已重置（%.2fGB），自动开机恢复", icfg.Name, gb)
		if !dryRun {
			if err := setInstancePower(icfg.AK, icfg.SK, icfg.Region, icfg.InstanceID, true); err != nil {
				log.Printf("[%s] 开机失败: %v", icfg.Name, err)
				return
			}
		}
		st.OverLimitStopped = false
		state.Instances[icfg.InstanceID] = st
		feishuPush(fmt.Sprintf("✅ [%s] 月度CDT流量已重置（当前 %.2fGB），实例已自动开机恢复。", icfg.Name, gb))

	case !over && status == statusStopped:
		log.Printf("[%s] 检测到非预期停机，执行保活开机", icfg.Name)
		if !dryRun {
			if err := setInstancePower(icfg.AK, icfg.SK, icfg.Region, icfg.InstanceID, true); err != nil {
				log.Printf("[%s] 开机失败: %v", icfg.Name, err)
				return
			}
		}
		feishuPush(fmt.Sprintf("🔄 [%s] 检测到实例被停止（疑似抢占回收），已自动开机保活。", icfg.Name))

	default:
		if st.OverLimitStopped {
			st.OverLimitStopped = false
			state.Instances[icfg.InstanceID] = st
		}
		log.Printf("[%s] 状态正常", icfg.Name)
	}
}

func dailyReport() {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	feishuWebhook = cfg.FeishuWebhook
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("📊 阿里云 CDT 每日保活报表\n\n")
	for _, inst := range cfg.Instances {
		if inst.Paused {
			b.WriteString(fmt.Sprintf("· %s：监控已暂停\n", inst.Name))
			continue
		}
		gb, err := cdtInternetTrafficGB(inst.AK, inst.SK)
		status := "查询失败"
		if err == nil {
			if s, e := instanceStatus(inst.AK, inst.SK, inst.Region, inst.InstanceID); e == nil {
				status = s
			}
		}
		if err != nil {
			b.WriteString(fmt.Sprintf("· %s：流量查询失败（%v）\n", inst.Name, err))
			continue
		}
		pct := 0.0
		if inst.TrafficLimitGB > 0 {
			pct = gb / inst.TrafficLimitGB * 100
		}
		line := fmt.Sprintf("· %s：%0.1fGB / %0.0fGB（%.0f%%），状态 %s", inst.Name, gb, inst.TrafficLimitGB, pct, status)
		cur, amount, err := accountBalance(inst.AK, inst.SK)
		if err == nil {
			sym := map[string]string{"CNY": "¥", "USD": "$"}[cur]
			if sym == "" {
				sym = cur
			}
			line += fmt.Sprintf("，余额 %s%.2f", sym, amount)
		}
		b.WriteString(line + "\n")
	}
	log.Printf("日报内容:\n%s", b.String())
	feishuPush(b.String())
}

// ---------- 配置与入口 ----------

func loadConfig() (Config, error) {
	var cfg Config
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("读取配置 %s 失败: %v", configPath, err)
	}
	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func main() {
	flag.StringVar(&configPath, "config", "/opt/aliyun-keepalive/config.json", "配置文件路径")
	flag.StringVar(&statePath, "state", "/opt/aliyun-keepalive/state.json", "状态文件路径")
	flag.BoolVar(&dryRun, "dry-run", false, "只检查并打印动作，不实际执行")
	flag.BoolVar(&reportMode, "report", false, "发送每日报表")
	probe := flag.String("probe", "", "探测实例ID所在地域（传入实例ID）")
	flag.Parse()

	if *probe != "" {
		cfg, err := loadConfig()
		if err != nil {
			log.Fatal(err)
		}
		inst := cfg.Instances[0]
		for _, region := range []string{"cn-hongkong", "us-west-1", "us-east-1", "ap-southeast-1", "ap-northeast-1", "eu-central-1", "me-east-1", "ap-southeast-2", "cn-beijing", "cn-hangzhou", "cn-shanghai", "cn-shenzhen"} {
			st, err := instanceStatus(inst.AK, inst.SK, region, *probe)
			if err == nil {
				fmt.Printf("✅ 找到: region=%s status=%s\n", region, st)
				return
			}
			fmt.Printf("· %s: %v\n", region, err)
		}
		return
	}
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	loadState()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("解析配置失败: %v", err)
	}
	feishuWebhook = cfg.FeishuWebhook
	if err != nil {
		log.Fatalf("解析配置失败: %v", err)
	}

	if reportMode {
		dailyReport()
		return
	}

	var wg sync.WaitGroup
	for _, inst := range cfg.Instances {
		wg.Add(1)
		go func(c Instance) {
			defer wg.Done()
			processInstance(c)
		}(inst)
	}
	wg.Wait()
}
