// Package store 负责状态持久化：key 池、签到账号、日志与用量。
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/model"
)

type Store struct {
	Mu       sync.RWMutex
	Data     *model.State
	Dir      string
	dirty    chan struct{}
	saveMu   sync.Mutex
	lockFile *os.File
}

func defaults() *model.State {
	return &model.State{
		Version: 1,
		Config: model.Config{
			TimeoutMs: 180000, DefaultMaxTokens: 0, CooldownSec: 60, HardCooldownSec: 900,
			MaxLogs: 2000, MaxUsageHours: 168, Aliases: model.DefaultAliases(),
			HiddenModels: []string{}, AdminUser: "admin", CheckinHour: 0, KeepAliveHours: 6,
		},
		Keys: []*model.Key{}, ClientKeys: []*model.ClientKey{}, Accounts: []*model.Account{},
		PersonalKeys: []*model.PersonalKey{}, Logs: []*model.LogRow{}, Usage: map[string]*model.Bucket{},
	}
}

// Load 读取 state.json；首次启动会生成管理密码并通过第二个返回值返回（只显示一次）。
func Load(dir, bootAK string, bootKeys []string) (*Store, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	lf, err := lockDirectory(dir)
	if err != nil {
		return nil, "", err
	}
	s := &Store{Dir: dir, Data: defaults(), dirty: make(chan struct{}, 1), lockFile: lf}
	loaded := false
	defer func() {
		if !loaded {
			_ = s.Close()
		}
	}()
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	generated := ""
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("读取 state.json: %w", err)
	}
	if err == nil {
		if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, "", fmt.Errorf("state.json 必须为 JSON 对象，已保留原文件")
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, "", fmt.Errorf("state.json 内容损坏，已保留原文件: %w", err)
		}
		cfg := bytes.TrimSpace(envelope["config"])
		if len(cfg) == 0 || cfg[0] != '{' {
			return nil, "", fmt.Errorf("state.json 缺少有效 config，已保留原文件")
		}
		if err := json.Unmarshal(raw, s.Data); err != nil {
			return nil, "", fmt.Errorf("state.json 内容损坏，已保留原文件: %w", err)
		}
		if s.Data.Version > 2 {
			return nil, "", fmt.Errorf("state.json 版本 %d 高于当前支持的版本", s.Data.Version)
		}
	}
	if s.Data.Config.Aliases == nil {
		s.Data.Config.Aliases = model.DefaultAliases()
	}
	if s.Data.Usage == nil {
		s.Data.Usage = map[string]*model.Bucket{}
	}
	if err := s.Data.Config.Validate(); err != nil {
		return nil, "", fmt.Errorf("state.json 配置无效，已保留原文件: %w", err)
	}
	if s.Data.Config.AdminPass == "" {
		pw := os.Getenv("MEOO2API_ADMIN_PASSWORD")
		if pw == "" {
			pw = model.RandID("", 9)
			generated = pw
		}
		s.Data.Config.AdminPass = model.HashPassword(pw)
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
	loaded = true
	return s, generated, nil
}

// migrate 把旧的 checkins 结构升级为 accounts
func (s *Store) migrate() {
	// Discard null list entries from historical/manual state without keeping
	// dangling pointers that would panic later in the request path.
	s.Data.Accounts = nonNil(s.Data.Accounts)
	s.Data.Keys = nonNil(s.Data.Keys)
	s.Data.ClientKeys = nonNil(s.Data.ClientKeys)
	s.Data.PersonalKeys = nonNil(s.Data.PersonalKeys)
	s.Data.Checkins = nonNil(s.Data.Checkins)
	s.Data.Logs = nonNil(s.Data.Logs)
	if len(s.Data.Checkins) > 0 {
		for _, c := range s.Data.Checkins {
			duplicate := false
			for _, a := range s.Data.Accounts {
				if a.ID == c.ID || (c.Account != nil && c.Account.ID != "" && a.UserID == c.Account.ID) || (c.Cookie != "" && a.Cookie == c.Cookie) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			acc := &model.Account{
				ID: model.RandID("a_", 6), Label: c.Label, Cookie: c.Cookie, Enabled: c.Enabled,
				CreatedAt: c.CreatedAt, LastGrantDate: c.LastGrantDate, TotalGranted: c.TotalGranted,
				GrantCount: c.GrantCount, Last: c.Last, PersonalKeyIDs: []string{}, ProjectKeyIDs: []string{},
				LastPing: c.LastPing, PingOK: c.PingOK, Expired: c.Expired,
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
	// Normalize the relationships in both directions after every old-side
	// migration; the old implementation chose KeyID before backfilling links.
	for _, k := range s.Data.Keys {
		if k.Weight < 1 {
			k.Weight = 1
		}
		if k.Weight > 100 {
			k.Weight = 100
		}
		for _, a := range s.Data.Accounts {
			if k.AccountID == a.UserID && a.UserID != "" {
				k.AccountID = a.ID
			}
			if k.AccountID == "" && (contains(a.ProjectKeyIDs, k.ID) || a.KeyID == k.ID || (k.Account != nil && k.Account.ID != "" && (k.Account.ID == a.UserID || k.Account.ID == a.ID))) {
				k.AccountID = a.ID
			}
		}
	}
	for _, a := range s.Data.Accounts {
		ids := []string{}
		for _, k := range s.Data.Keys {
			if k.AccountID == a.ID {
				ids = append(ids, k.ID)
			}
		}
		a.ProjectKeyIDs = ids
		if !contains(ids, a.KeyID) {
			a.KeyID = ""
			if len(ids) > 0 {
				a.KeyID = ids[len(ids)-1]
			}
		}
		for _, k := range s.Data.Keys {
			if k.ID == a.KeyID && a.ProjectID == "" {
				// Some old UI labels were "account / project-id" instead of
				// the bare URL ID. Never use the human prefix as an API path.
				label := strings.TrimSpace(k.Label)
				if i := strings.LastIndex(label, " / "); i >= 0 {
					label = strings.TrimSpace(label[i+3:])
				}
				if !strings.ContainsAny(label, "/\\ ?#") {
					a.ProjectID = label
				}
			}
		}
	}
	for key, b := range s.Data.Usage {
		if b == nil {
			delete(s.Data.Usage, key)
			continue
		}
		if b.Models == nil {
			b.Models = map[string]int64{}
		}
	}
	s.Data.Version = 2
}

func nonNil[T any](items []*T) []*T {
	out := make([]*T, 0, len(items))
	for _, item := range items {
		if item != nil {
			out = append(out, item)
		}
	}
	return out
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
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
	// Serialize snapshot + publication together so an older concurrent save
	// cannot replace a newer snapshot or rename another save's temporary file.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.Mu.RLock()
	b, err := json.MarshalIndent(s.Data, "", "  ")
	s.Mu.RUnlock()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, filepath.Join(s.Dir, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return syncDirectory(dir)
}

// Close releases the single-writer lease. The caller must first stop workers
// and perform its final Save; releasing never implicitly rewrites state.
func (s *Store) Close() error {
	if s.lockFile == nil {
		return nil
	}
	err := unlockDirectory(s.lockFile)
	s.lockFile = nil
	return err
}

func (s *Store) Touch() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Store) SaveLoop() { s.SaveLoopContext(context.Background()) }

func (s *Store) SaveLoopContext(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case <-s.dirty:
				if err := s.Save(); err != nil {
					log.Printf("保存状态失败，将重试: %v", err)
					s.Touch()
				}
			default:
			}
		}
	}
}

func (s *Store) Record(row *model.LogRow) {
	if row == nil {
		return
	}
	cp := *row
	row = &cp // callers must not retain a mutable pointer into persisted logs
	now := time.Now()
	if row.TS == 0 {
		row.TS = now.UnixMilli()
	}
	if row.ID == "" {
		row.ID = model.RandID("r_", 5)
	}
	s.Mu.Lock()
	if row.Error != "" {
		for _, k := range s.Data.Keys {
			row.Error = redact(row.Error, k.Key)
		}
		for _, k := range s.Data.ClientKeys {
			row.Error = redact(row.Error, k.Key)
		}
		for _, k := range s.Data.PersonalKeys {
			row.Error = redact(row.Error, k.Key)
		}
		for _, a := range s.Data.Accounts {
			row.Error = redact(row.Error, a.Cookie)
		}
	}
	row.Rid = truncate(row.Rid, 256)
	row.Path = truncate(row.Path, 1024)
	row.Error = truncate(row.Error, 4096)
	row.KeyLabel = truncate(row.KeyLabel, 256)
	s.Data.Logs = append([]*model.LogRow{row}, s.Data.Logs...)
	maxLogs := s.Data.Config.MaxLogs
	if maxLogs < 0 {
		maxLogs = 0
	}
	if len(s.Data.Logs) > maxLogs {
		s.Data.Logs = s.Data.Logs[:maxLogs]
	}
	h := now.Format("2006-01-02T15")
	if s.Data.Usage == nil {
		s.Data.Usage = map[string]*model.Bucket{}
	}
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
	if b.Models == nil {
		b.Models = map[string]int64{}
	}
	if row.Model != "" {
		b.Models[row.Model]++
	}
	hours := make([]string, 0, len(s.Data.Usage))
	for k := range s.Data.Usage {
		hours = append(hours, k)
	}
	sort.Strings(hours)
	maxHours := s.Data.Config.MaxUsageHours
	if maxHours < 1 {
		maxHours = 1
	}
	for len(hours) > maxHours {
		delete(s.Data.Usage, hours[0])
		hours = hours[1:]
	}
	s.Mu.Unlock()
	s.Touch()
}

func redact(text, secret string) string {
	if len(secret) < 4 {
		return text
	}
	return strings.ReplaceAll(text, secret, "[redacted]")
}

func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return strings.ToValidUTF8(text[:n], "")
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
		if s.UsableKeyLocked(k, now) {
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

// UsableKeyLocked requires the caller to hold Mu. Account disable takes
// effect immediately for every linked key; Cookie expiry is independent of AK.
func (s *Store) UsableKeyLocked(k *model.Key, now int64) bool {
	if k == nil || !k.Enabled || k.Key == "" || k.CooldownUntil > now {
		return false
	}
	if k.AccountID == "" {
		return true
	}
	for _, a := range s.Data.Accounts {
		if a.ID == k.AccountID {
			return a.Enabled
		}
	}
	return false
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
