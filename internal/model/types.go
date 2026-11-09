// Package model 定义控制台与网关共用的数据结构。
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"
)

type Health struct {
	State     string `json:"state"`
	CheckedAt int64  `json:"checkedAt"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
}

type Stats struct {
	Req      int64 `json:"req"`
	OK       int64 `json:"ok"`
	Fail     int64 `json:"fail"`
	PT       int64 `json:"pt"`
	CT       int64 `json:"ct"`
	LastUsed int64 `json:"lastUsed"`
}

// Identity 是挂 key / 签到记录上的账号标识
type Identity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

type Key struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	Key           string    `json:"key"`
	Enabled       bool      `json:"enabled"`
	Weight        int       `json:"weight"`
	Note          string    `json:"note"`
	CreatedAt     int64     `json:"createdAt"`
	Health        Health    `json:"health"`
	Stats         Stats     `json:"stats"`
	CooldownUntil int64     `json:"cooldownUntil"`
	Account       *Identity `json:"account,omitempty"`
	AccountID     string    `json:"accountId,omitempty"`
}

type ClientKey struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Key       string `json:"key"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"createdAt"`
	Stats     Stats  `json:"stats"`
}

type CheckinLast struct {
	At           int64  `json:"at"`
	OK           bool   `json:"ok"`
	Ms           int64  `json:"ms"`
	Error        string `json:"error,omitempty"`
	Granted      bool   `json:"granted"`
	Amount       int64  `json:"amount"`
	Initialized  bool   `json:"initialized"`
	SkipReason   string `json:"skipReason,omitempty"`
	BalanceAfter *int64 `json:"balanceAfter"`
}

// Account 是一个 Meoo 账号：持有登录态、个人 AK，以及它产出的项目 AK（入池）
type Account struct {
	ID             string       `json:"id"`
	Label          string       `json:"label"`
	UserID         string       `json:"userId,omitempty"`
	Name           string       `json:"name,omitempty"`
	Cookie         string       `json:"cookie,omitempty"`
	Enabled        bool         `json:"enabled"`
	CreatedAt      int64        `json:"createdAt"`
	LastPing       int64        `json:"lastPing"`
	PingOK         bool         `json:"pingOk"`
	Expired        bool         `json:"expired"`
	LastGrantDate  string       `json:"lastGrantDate"`
	TotalGranted   int64        `json:"totalGranted"`
	GrantCount     int64        `json:"grantCount"`
	Last           *CheckinLast `json:"last,omitempty"`
	PersonalKeyIDs []string     `json:"personalKeyIds"`
	ProjectKeyIDs  []string     `json:"projectKeyIds"`
	KeyID          string       `json:"keyId,omitempty"`
}

type Checkin struct {
	ID            string       `json:"id"`
	Label         string       `json:"label"`
	Cookie        string       `json:"cookie"`
	Enabled       bool         `json:"enabled"`
	CreatedAt     int64        `json:"createdAt"`
	LastGrantDate string       `json:"lastGrantDate"`
	TotalGranted  int64        `json:"totalGranted"`
	GrantCount    int64        `json:"grantCount"`
	Last          *CheckinLast `json:"last"`
	Account       *Identity    `json:"account,omitempty"`
	LastPing      int64        `json:"lastPing"`
	PingOK        bool         `json:"pingOk"`
	Expired       bool         `json:"expired"`
}

type PersonalKey struct {
	ID        string   `json:"id"`
	AccountID string   `json:"accountId"`
	Name      string   `json:"name"`
	Key       string   `json:"key"`
	Scopes    []string `json:"scopes"`
	CreatedAt int64    `json:"createdAt"`
}

type Config struct {
	TimeoutMs        int               `json:"timeoutMs"`
	DefaultMaxTokens int               `json:"defaultMaxTokens"`
	CooldownSec      int               `json:"cooldownSec"`
	HardCooldownSec  int               `json:"hardCooldownSec"`
	Sticky           bool              `json:"sticky"`
	MaxLogs          int               `json:"maxLogs"`
	MaxUsageHours    int               `json:"maxUsageHours"`
	Aliases          map[string]string `json:"aliases"`
	HiddenModels     []string          `json:"hiddenModels"`
	AdminUser        string            `json:"adminUser"`
	AdminPass        string            `json:"adminPass"`
	AutoCheckin      bool              `json:"autoCheckin"`
	AutoOnboard      bool              `json:"autoOnboard"`
	CheckinHour      int               `json:"checkinHour"`
	KeepAliveHours   int               `json:"keepAliveHours"`
}

type LogRow struct {
	ID       string `json:"id"`
	TS       int64  `json:"ts"`
	Rid      string `json:"rid"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Status   int    `json:"status"`
	Ms       int64  `json:"ms"`
	Model    string `json:"model,omitempty"`
	PT       int64  `json:"pt,omitempty"`
	CT       int64  `json:"ct,omitempty"`
	KeyLabel string `json:"keyLabel,omitempty"`
	Byok     bool   `json:"byok,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Bucket struct {
	Req    int64            `json:"req"`
	OK     int64            `json:"ok"`
	Fail   int64            `json:"fail"`
	PT     int64            `json:"pt"`
	CT     int64            `json:"ct"`
	Lat    []float64        `json:"lat"`
	Models map[string]int64 `json:"models"`
}

type State struct {
	Version      int                `json:"version"`
	Config       Config             `json:"config"`
	Keys         []*Key             `json:"keys"`
	ClientKeys   []*ClientKey       `json:"clientKeys"`
	Checkins     []*Checkin         `json:"checkins"`
	Accounts     []*Account         `json:"accounts"`
	PersonalKeys []*PersonalKey     `json:"personalKeys"`
	Logs         []*LogRow          `json:"logs"`
	Usage        map[string]*Bucket `json:"usage"`
	LastAutoRun  int64              `json:"lastAutoRun"`
}

var NativeModels = []string{
	"qwen3.8-flash", "qwen3.8-max", "qwen3.7-max", "qwen3.7-plus",
	"deepseek-v4.1-flash-1017", "deepseek-v4-pro-0813", "deepseek-v4-flash-0731",
	"glm-5.2-fast-preview", "glm-5.2",
}

func DefaultAliases() map[string]string {
	return map[string]string{
		"deepseek v4.1 flash": "deepseek-v4.1-flash-1017",
		"deepseek-v4.1-flash": "deepseek-v4.1-flash-1017",
		"deepseek-v4-1-flash": "deepseek-v4.1-flash-1017",
		"deepseek-flash":      "deepseek-v4.1-flash-1017",
		"deepseek v4 flash":   "deepseek-v4-flash-0731",
		"deepseek-v4-flash":   "deepseek-v4-flash-0731",
		"deepseek v4 pro":     "deepseek-v4-pro-0813",
		"deepseek-v4-pro":     "deepseek-v4-pro-0813",
		"qwen":                "qwen3.8-flash",
		"qwen-max":            "qwen3.8-max",
		"glm":                 "glm-5.2",
	}
}

func RandID(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func ShaHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func Mask(k string) string {
	if len(k) <= 18 {
		return k
	}
	return k[:14] + "…" + k[len(k)-4:]
}

func Percentile(lat []float64, p float64) int64 {
	if len(lat) == 0 {
		return 0
	}
	cp := append([]float64(nil), lat...)
	sort.Float64s(cp)
	idx := int(float64(len(cp)) * p / 100)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return int64(cp[idx])
}

func Today() string { return time.Now().Format("2006-01-02") }
