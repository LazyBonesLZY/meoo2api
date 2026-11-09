// Package store 负责状态持久化：key 池、签到账号、日志与用量。
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"meoo2api/internal/model"
)

type Store struct {
	Mu    sync.RWMutex
	Data  *model.State
	Dir   string
	dirty chan struct{}
}

func defaults() *model.State {
	return &model.State{
		Version: 1,
		Config: model.Config{
			TimeoutMs: 180000, DefaultMaxTokens: 800, CooldownSec: 60, HardCooldownSec: 900,
			MaxLogs: 2000, MaxUsageHours: 168, Aliases: model.DefaultAliases(),
			HiddenModels: []string{}, AdminUser: "admin", CheckinHour: 0, KeepAliveHours: 6,
		},
		Keys: []*model.Key{}, ClientKeys: []*model.ClientKey{}, Checkins: []*model.Checkin{},
		PersonalKeys: []*model.PersonalKey{}, Logs: []*model.LogRow{}, Usage: map[string]*model.Bucket{},
	}
}

// Load 读取 state.json；首次启动会生成管理密码并通过第二个返回值返回（只显示一次）。
func Load(dir, bootAK string, bootKeys []string) (*Store, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	s := &Store{Dir: dir, Data: defaults(), dirty: make(chan struct{}, 1)}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	generated := ""
	if err == nil {
		if json.Unmarshal(raw, s.Data) != nil {
			s.Data = defaults()
		}
		if s.Data.Config.Aliases == nil {
			s.Data.Config.Aliases = model.DefaultAliases()
		}
		if s.Data.Usage == nil {
			s.Data.Usage = map[string]*model.Bucket{}
		}
	}
	if s.Data.Config.AdminPass == "" {
		pw := os.Getenv("MEOO2API_ADMIN_PASSWORD")
		if pw == "" {
			pw = model.RandID("", 9)
			generated = pw
		}
		s.Data.Config.AdminPass = model.ShaHex(pw)
	}
	s.migrate()
	if len(s.Data.Keys) == 0 && bootAK != "" {
		s.Data.Keys = append(s.Data.Keys, NewKey("bootstrap", bootAK, 1))
	}
	if len(s.Data.ClientKeys) == 0 {
		for _, k := range bootKeys {
			s.Data.ClientKeys = append(s.Data.ClientKeys, NewClientKey("bootstrap", k))
		}
	}
	if err := s.Save(); err != nil {
		return nil, "", err
	}
	return s, generated, nil
}

// migrate 把旧的 checkins 结构升级为 accounts
func (s *Store) migrate() {
	if len(s.Data.Accounts) == 0 && len(s.Data.Checkins) > 0 {
		for _, c := range s.Data.Checkins {
			acc := &model.Account{
				ID: model.RandID("a_", 6), Label: c.Label, Cookie: c.Cookie, Enabled: c.Enabled,
				CreatedAt: c.CreatedAt, LastGrantDate: c.LastGrantDate, TotalGranted: c.TotalGranted,
				GrantCount: c.GrantCount, Last: c.Last, PersonalKeyIDs: []string{}, ProjectKeyIDs: []string{},
			}
			if c.Account != nil {
				acc.UserID = c.Account.ID
				acc.Name = c.Account.Name
			}
			s.Data.Accounts = append(s.Data.Accounts, acc)
		}
	}
	// 个人 AK 挂到账号
	for _, pk := range s.Data.PersonalKeys {
		for _, a := range s.Data.Accounts {
			if pk.AccountID == a.UserID || pk.AccountID == a.ID {
				found := false
				for _, id := range a.PersonalKeyIDs {
					if id == pk.ID {
						found = true
					}
				}
				if !found {
					a.PersonalKeyIDs = append(a.PersonalKeyIDs, pk.ID)
				}
			}
		}
	}
	s.Data.Checkins = nil // 迁移完成后不再保留旧结构
	// 账号补 KeyID
	for _, a := range s.Data.Accounts {
		if a.KeyID == "" && len(a.ProjectKeyIDs) > 0 {
			a.KeyID = a.ProjectKeyIDs[len(a.ProjectKeyIDs)-1]
		}
	}
	// 池内 key 反查所属账号
	for _, k := range s.Data.Keys {
		if k.AccountID != "" {
			continue
		}
		for _, a := range s.Data.Accounts {
			if k.Account != nil && (k.Account.ID == a.UserID || k.Account.ID == a.ID) {
				k.AccountID = a.ID
				a.ProjectKeyIDs = append(a.ProjectKeyIDs, k.ID)
			}
		}
	}
}

func NewKey(label, key string, weight int) *model.Key {
	if weight < 1 {
		weight = 1
	}
	return &model.Key{ID: model.RandID("k_", 6), Label: label, Key: key, Enabled: true,
		Weight: weight, CreatedAt: time.Now().UnixMilli(), Health: model.Health{State: "unknown"}}
}

func NewClientKey(label, key string) *model.ClientKey {
	if key == "" {
		key = "sk-" + model.RandID("", 16)
	}
	return &model.ClientKey{ID: model.RandID("c_", 6), Label: label, Key: key, Enabled: true, CreatedAt: time.Now().UnixMilli()}
}

func (s *Store) Save() error {
	s.Mu.RLock()
	b, err := json.MarshalIndent(s.Data, "", "  ")
	s.Mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, "state.json"))
}

func (s *Store) Touch() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Store) SaveLoop() {
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

func (s *Store) Record(row *model.LogRow) {
	now := time.Now()
	if row.TS == 0 {
		row.TS = now.UnixMilli()
	}
	if row.ID == "" {
		row.ID = model.RandID("r_", 5)
	}
	s.Mu.Lock()
	s.Data.Logs = append([]*model.LogRow{row}, s.Data.Logs...)
	if len(s.Data.Logs) > s.Data.Config.MaxLogs {
		s.Data.Logs = s.Data.Logs[:s.Data.Config.MaxLogs]
	}
	h := now.Format("2006-01-02T15")
	b := s.Data.Usage[h]
	if b == nil {
		b = &model.Bucket{Lat: []float64{}, Models: map[string]int64{}}
		s.Data.Usage[h] = b
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
	hours := make([]string, 0, len(s.Data.Usage))
	for k := range s.Data.Usage {
		hours = append(hours, k)
	}
	sort.Strings(hours)
	for len(hours) > s.Data.Config.MaxUsageHours {
		delete(s.Data.Usage, hours[0])
		hours = hours[1:]
	}
	s.Mu.Unlock()
	s.Touch()
}

func (s *Store) Alerts() []map[string]string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.AlertsLocked()
}

// AlertsLocked 假设调用方已持有读锁
func (s *Store) AlertsLocked() []map[string]string {
	out := []map[string]string{}
	now := time.Now().UnixMilli()
	bad, cooling, usable := 0, 0, 0
	for _, k := range s.Data.Keys {
		if k.Health.State == "invalid" {
			bad++
		}
		if k.CooldownUntil > now {
			cooling++
		}
		if k.Enabled && k.CooldownUntil <= now && k.Key != "" {
			usable++
		}
	}
	n := len(s.Data.Keys)
	if bad > 0 {
		out = append(out, map[string]string{"level": "bad", "text": itoa(bad) + " 个 key 鉴权失败"})
	}
	if cooling > 0 {
		out = append(out, map[string]string{"level": "warn", "text": itoa(cooling) + " 个 key 处于冷却中"})
	}
	if usable == 0 && n > 0 {
		out = append(out, map[string]string{"level": "bad", "text": "没有可用 key，请求会失败"})
	}
	if n == 0 {
		out = append(out, map[string]string{"level": "warn", "text": "key 池为空，只能 BYOK"})
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
