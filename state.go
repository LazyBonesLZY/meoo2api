package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Health struct {
	State     string `json:"state"`
	CheckedAt int64  `json:"checkedAt"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
}

type KeyStats struct {
	Req      int64 `json:"req"`
	OK       int64 `json:"ok"`
	Fail     int64 `json:"fail"`
	PT       int64 `json:"pt"`
	CT       int64 `json:"ct"`
	LastUsed int64 `json:"lastUsed"`
}

type Key struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Key           string   `json:"key"`
	Enabled       bool     `json:"enabled"`
	Weight        int      `json:"weight"`
	Note          string   `json:"note"`
	CreatedAt     int64    `json:"createdAt"`
	Health        Health   `json:"health"`
	Stats         KeyStats `json:"stats"`
	CooldownUntil int64    `json:"cooldownUntil"`
}

type ClientKey struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Key       string   `json:"key"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"createdAt"`
	Stats     KeyStats `json:"stats"`
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
	CheckinIntervalH int               `json:"checkinIntervalH"`
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
	Version    int                 `json:"version"`
	Config     Config              `json:"config"`
	Keys       []*Key              `json:"keys"`
	ClientKeys []*ClientKey        `json:"clientKeys"`
	Checkins   []*Checkin          `json:"checkins"`
	Logs       []*LogRow           `json:"logs"`
	Usage      map[string]*Bucket  `json:"usage"`
	mu         sync.RWMutex
	dir        string
	dirty      chan struct{}
}

func randID(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func shaHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func DefaultAliases() map[string]string {
	return map[string]string{
		"deepseek v4.1 flash":  "deepseek-v4.1-flash-1017",
		"deepseek-v4.1-flash":  "deepseek-v4.1-flash-1017",
		"deepseek-v4-1-flash":  "deepseek-v4.1-flash-1017",
		"deepseek-flash":       "deepseek-v4.1-flash-1017",
		"deepseek v4 flash":    "deepseek-v4-flash-0731",
		"deepseek-v4-flash":    "deepseek-v4-flash-0731",
		"deepseek v4 pro":      "deepseek-v4-pro-0813",
		"deepseek-v4-pro":      "deepseek-v4-pro-0813",
		"qwen":                 "qwen3.8-flash",
		"qwen-max":             "qwen3.8-max",
		"glm":                  "glm-5.2",
	}
}

func defaultState() *State {
	return &State{
		Version: 1,
		Config: Config{
			TimeoutMs:        180000,
			DefaultMaxTokens: 800,
			CooldownSec:      60,
			HardCooldownSec:  900,
			MaxLogs:          2000,
			MaxUsageHours:    168,
			Aliases:          DefaultAliases(),
			HiddenModels:     []string{},
			AdminUser:        "admin",
			CheckinIntervalH: 6,
		},
		Keys:       []*Key{},
		ClientKeys: []*ClientKey{},
		Checkins:   []*Checkin{},
		Logs:       []*LogRow{},
		Usage:      map[string]*Bucket{},
	}
}

func LoadState(dir, bootAK string, bootKeys []string) (*State, string, error) {
	s := defaultState()
	s.dir = dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "state.json")
	generated := ""
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, s); err != nil {
			s = defaultState()
			s.dir = dir
		}
		if s.Config.Aliases == nil {
			s.Config.Aliases = DefaultAliases()
		}
		if s.Usage == nil {
			s.Usage = map[string]*Bucket{}
		}
	} else {
		generated = ensureAdminPass(s)
	}
	if s.Config.AdminPass == "" {
		if p := ensureAdminPass(s); p != "" {
			generated = p
		}
	}
	if len(s.Keys) == 0 && bootAK != "" {
		s.Keys = append(s.Keys, NewKey("bootstrap", bootAK, 1))
	}
	if len(s.ClientKeys) == 0 {
		for _, k := range bootKeys {
			s.ClientKeys = append(s.ClientKeys, NewClientKey("bootstrap", k))
		}
	}
	s.dirty = make(chan struct{}, 1)
	_ = s.Save()
	return s, generated, nil
}

func ensureAdminPass(s *State) string {
	pw := os.Getenv("MEOO2API_ADMIN_PASSWORD")
	explicit := pw != ""
	if !explicit {
		pw = randID("", 9)
	}
	s.Config.AdminPass = shaHex(pw)
	if explicit {
		return ""
	}
	return pw
}

func NewKey(label, key string, weight int) *Key {
	if weight < 1 {
		weight = 1
	}
	return &Key{
		ID: randID("k_", 6), Label: label, Key: key, Enabled: true, Weight: weight,
		CreatedAt: time.Now().UnixMilli(), Health: Health{State: "unknown"},
	}
}

func NewClientKey(label, key string) *ClientKey {
	if key == "" {
		key = "sk-" + randID("", 16)
	}
	return &ClientKey{ID: randID("c_", 6), Label: label, Key: key, Enabled: true, CreatedAt: time.Now().UnixMilli()}
}

func (s *State) Save() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "state.json"))
}

func (s *State) Touch() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *State) SaveLoop() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for range t.C {
		select {
		case <-s.dirty:
			_ = s.Save()
		default:
		}
	}
}

func (s *State) Record(row *LogRow) {
	now := time.Now()
	if row.TS == 0 {
		row.TS = now.UnixMilli()
	}
	if row.ID == "" {
		row.ID = randID("r_", 5)
	}
	s.mu.Lock()
	s.Logs = append([]*LogRow{row}, s.Logs...)
	if len(s.Logs) > s.Config.MaxLogs {
		s.Logs = s.Logs[:s.Config.MaxLogs]
	}
	h := now.Format("2006-01-02T15")
	b := s.Usage[h]
	if b == nil {
		b = &Bucket{Lat: []float64{}, Models: map[string]int64{}}
		s.Usage[h] = b
	}
	b.Req++
	if row.Status < 400 {
		b.OK++
	} else {
		b.Fail++
	}
	b.PT += row.PT
	b.CT += row.CT
	if row.Ms > 0 {
		b.Lat = append(b.Lat, float64(row.Ms))
		if len(b.Lat) > 500 {
			b.Lat = b.Lat[1:]
		}
	}
	if row.Model != "" {
		b.Models[row.Model]++
	}
	hours := make([]string, 0, len(s.Usage))
	for k := range s.Usage {
		hours = append(hours, k)
	}
	sort.Strings(hours)
	for len(hours) > s.Config.MaxUsageHours {
		delete(s.Usage, hours[0])
		hours = hours[1:]
	}
	s.mu.Unlock()
	s.Touch()
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

func Mask(k string) string {
	if len(k) <= 18 {
		return k
	}
	return k[:14] + "…" + k[len(k)-4:]
}
