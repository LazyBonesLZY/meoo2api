// Package account 管理 Meoo 账号、凭证及项目接入。
package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

var AllScopes = []string{
	"cli.compat", "project.read", "project.write", "cloud.read", "cloud.write",
	"cloud.credentials.read", "cloud.database.read", "cloud.function.read",
	"credit.read", "entitlement.read", "user.read", "release.read", "source.read",
}

type Service struct {
	St *store.Store
	Up *upstream.Client
	// All mutating credential/project operations share a cancellable gate. The
	// store mutex is never held during network requests.
	gateOnce sync.Once
	gate     chan struct{}
	deviceMu sync.Mutex
	devices  map[string]*deviceSession
}

func (s *Service) acquire(ctx context.Context) (func(), error) {
	s.gateOnce.Do(func() { s.gate = make(chan struct{}, 1) })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Find preserves the historical pointer API. Read fields under St.Mu, or use Snapshot.
func (s *Service) Find(id string) *model.Account {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	return s.findLocked(id)
}
func (s *Service) findLocked(id string) *model.Account {
	for _, a := range s.St.Data.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}
func cloneAccount(a *model.Account) *model.Account {
	if a == nil {
		return nil
	}
	cp := *a
	cp.PersonalKeyIDs = append([]string(nil), a.PersonalKeyIDs...)
	cp.ProjectKeyIDs = append([]string(nil), a.ProjectKeyIDs...)
	if a.Last != nil {
		last := *a.Last
		if last.BalanceAfter != nil {
			b := *last.BalanceAfter
			last.BalanceAfter = &b
		}
		cp.Last = &last
	}
	return &cp
}
func (s *Service) Snapshot(id string) *model.Account {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	return cloneAccount(s.findLocked(id))
}
func (s *Service) snapshot(a *model.Account) *model.Account {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	return cloneAccount(a)
}
func (s *Service) attachedLocked(a *model.Account) bool {
	return a != nil && s.findLocked(a.ID) == a
}
func (s *Service) persist() error { s.St.Touch(); return s.St.Save() }
func (s *Service) request(ctx context.Context, method, endpoint string, body any, hdr map[string]string) (int, []byte, error) {
	if s.Up == nil || s.Up.HTTP == nil {
		return 0, nil, fmt.Errorf("上游客户端未配置")
	}
	// Redirects are never valid API results; do not forward browser/AK credentials.
	client := *s.Up.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	up := *s.Up
	up.HTTP = &client
	code, raw, err := up.DoJSON(ctx, method, endpoint, body, hdr)
	if err == nil {
		var envelope struct {
			Success *bool  `json:"success"`
			Code    string `json:"errCode"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Success != nil && !*envelope.Success {
			return code, raw, &BusinessError{Code: envelope.Code, Message: envelope.Message}
		}
	}
	return code, raw, err
}

type credentialError struct{ status int }

func (e *credentialError) Error() string { return fmt.Sprintf("身份验证失败 HTTP %d", e.status) }

type identity struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (s *Service) identity(ctx context.Context, cookie, ak string) (identity, error) {
	var v identity
	endpoint := "https://meoo.com/api/v1/user/me"
	hdr := map[string]string{"Cookie": cookie, "Referer": "https://meoo.com/"}
	if cookie == "" {
		if ak == "" {
			return v, fmt.Errorf("该账号没有可用身份凭证")
		}
		endpoint = "https://meoo.com/open/v1/cli-compat/account/identity"
		hdr = map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": "cli"}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, endpoint, nil, hdr)
	if code == 401 || code == 403 {
		return v, &credentialError{status: code}
	}
	if err != nil {
		return v, err
	}
	var d struct {
		Data identity `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &d) != nil || d.Data.UserID == "" {
		return v, fmt.Errorf("识别失败 HTTP %d", code)
	}
	if d.Data.Name == "" {
		d.Data.Name = d.Data.Username
	}
	return d.Data, nil
}
func (s *Service) Identify(a *model.Account) error { return s.IdentifyContext(context.Background(), a) }
func (s *Service) IdentifyContext(ctx context.Context, a *model.Account) error {
	if a == nil {
		return fmt.Errorf("账号为空")
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return s.identify(ctx, a)
}
func (s *Service) identify(ctx context.Context, a *model.Account) error {
	cp := s.snapshot(a)
	v, err := s.identity(ctx, cp.Cookie, s.personalAKOf(a))
	if err != nil {
		return err
	}
	s.St.Mu.Lock()
	if a.UserID != "" && a.UserID != v.UserID {
		s.St.Mu.Unlock()
		return fmt.Errorf("凭证账号与已存身份不一致")
	}
	a.UserID, a.Name = v.UserID, v.Name
	if a.Label == "" || a.Label == "未命名" {
		a.Label = v.Name
	}
	s.St.Mu.Unlock()
	s.St.Touch()
	return nil
}

// Ping does not mark a credential expired for timeouts, 429, 5xx or malformed responses.
// Cookie and personal AK have independent lifetimes: a device-only account is probed by AK.
func (s *Service) Ping(a *model.Account) (bool, error) { return s.PingContext(context.Background(), a) }
func (s *Service) PingContext(ctx context.Context, a *model.Account) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("账号为空")
	}
	cp := s.snapshot(a)
	ak := s.personalAKOf(a)
	started := time.Now().UnixMilli()
	endpoint := "https://meoo.com/api/v1/user/me"
	hdr := map[string]string{"Cookie": cp.Cookie, "Referer": "https://meoo.com/"}
	if cp.Cookie == "" {
		if ak == "" {
			return false, fmt.Errorf("该账号没有可用身份凭证")
		}
		endpoint = "https://meoo.com/open/v1/cli-compat/account/identity"
		hdr = map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": "cli"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, endpoint, nil, hdr)
	var d struct {
		Data identity `json:"data"`
	}
	validJSON := json.Unmarshal(raw, &d) == nil
	ok := err == nil && code == 200 && validJSON && d.Data.UserID != "" && (cp.UserID == "" || cp.UserID == d.Data.UserID)
	invalid := code == 401 || code == 403 || (err == nil && code == 200 && validJSON && d.Data.UserID != "" && cp.UserID != "" && cp.UserID != d.Data.UserID)
	s.St.Mu.Lock()
	// A response for an old/replaced credential must never poison the new one.
	if s.attachedLocked(a) && a.Cookie == cp.Cookie && a.LastPing <= started && (cp.Cookie != "" || s.personalAKLocked(a) == ak) {
		a.LastPing = time.Now().UnixMilli()
		a.PingOK = ok
		if ok {
			a.Expired = false
		} else if invalid {
			a.Expired = true
		}
	}
	s.St.Mu.Unlock()
	s.St.Touch()
	if !ok {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("保活失败 HTTP %d", code)
	}
	return true, nil
}

func (s *Service) Points(a *model.Account) (int64, error) {
	return s.PointsContext(context.Background(), a)
}
func (s *Service) PointsContext(ctx context.Context, a *model.Account) (int64, error) {
	if a == nil {
		return 0, fmt.Errorf("账号为空")
	}
	ak := s.personalAKOf(a)
	if ak == "" {
		return 0, fmt.Errorf("该账号没有个人 AK")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, "https://meoo.com/open/v1/cli-compat/account/points", nil, map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": "cli"})
	if err != nil {
		return 0, err
	}
	var d struct {
		Data struct {
			Account *struct {
				Available *int64 `json:"availableBalance"`
			} `json:"account"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.Account == nil || d.Data.Account.Available == nil {
		return 0, fmt.Errorf("查询积分失败 HTTP %d", code)
	}
	value := *d.Data.Account.Available
	s.St.Mu.Lock()
	if s.attachedLocked(a) && s.personalAKLocked(a) == ak {
		a.Points = value
		a.PointsAt = time.Now().UnixMilli()
	}
	s.St.Mu.Unlock()
	s.St.Touch()
	return value, nil
}

func (s *Service) PersonalKey(a *model.Account) (*model.PersonalKey, error) {
	return s.PersonalKeyContext(context.Background(), a)
}
func (s *Service) PersonalKeyContext(ctx context.Context, a *model.Account) (*model.PersonalKey, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.personalKey(ctx, a)
}
func clonePersonal(pk *model.PersonalKey) *model.PersonalKey {
	cp := *pk
	cp.Scopes = append([]string(nil), pk.Scopes...)
	return &cp
}
func (s *Service) personalKey(ctx context.Context, a *model.Account) (*model.PersonalKey, error) {
	s.St.Mu.RLock()
	if !s.attachedLocked(a) {
		s.St.Mu.RUnlock()
		return nil, fmt.Errorf("账号已删除")
	}
	var existing *model.PersonalKey
	for i := len(s.St.Data.PersonalKeys) - 1; i >= 0; i-- {
		pk := s.St.Data.PersonalKeys[i]
		if belongs(pk.AccountID, a) && pk.Key != "" {
			existing = clonePersonal(pk)
			break
		}
	}
	cookie, userID := a.Cookie, a.UserID
	s.St.Mu.RUnlock()
	if existing != nil {
		v, err := s.identity(ctx, "", existing.Key)
		if err == nil && (userID == "" || v.UserID == userID) {
			return existing, nil
		}
		_, invalid := err.(*credentialError)
		if err != nil && !invalid {
			return nil, err
		}
		// Definitive credential rejection (or another user's identity) is not a
		// transport failure. Remove stale credentials before minting a replacement.
		s.St.Mu.Lock()
		kept := s.St.Data.PersonalKeys[:0]
		for _, pk := range s.St.Data.PersonalKeys {
			if pk.ID != existing.ID {
				kept = append(kept, pk)
			}
		}
		s.St.Data.PersonalKeys = kept
		ids := a.PersonalKeyIDs[:0]
		for _, id := range a.PersonalKeyIDs {
			if id != existing.ID {
				ids = append(ids, id)
			}
		}
		a.PersonalKeyIDs = ids
		s.St.Mu.Unlock()
		if err := s.persist(); err != nil {
			return nil, err
		}
		// Older explicitly managed credentials may still work. Validate the next
		// candidate instead of throwing away every credential after one rejection.
		return s.personalKey(ctx, a)
	}
	if cookie == "" {
		return nil, fmt.Errorf("创建个人 AK 需要登录态 Cookie")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodPost, "https://meoo.com/api/v1/user/api-keys", map[string]any{"name": "meoo2api-console", "description": "created by meoo2api console", "scopes": AllScopes}, map[string]string{"Cookie": cookie, "Referer": "https://meoo.com/"})
	if err != nil {
		return nil, err
	}
	var d struct {
		Data struct {
			APIKey string   `json:"apiKey"`
			Scopes []string `json:"scopes"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.APIKey == "" {
		return nil, fmt.Errorf("创建个人 AK 失败 HTTP %d", code)
	}
	s.St.Mu.Lock()
	pk := s.upsertPersonalLocked(a, d.Data.APIKey, "meoo2api-console", d.Data.Scopes)
	result := clonePersonal(pk)
	s.St.Mu.Unlock()
	return result, s.persist()
}
func belongs(owner string, a *model.Account) bool {
	return owner == a.ID || (a.UserID != "" && owner == a.UserID)
}
func appendID(ids []string, id string) []string {
	for _, v := range ids {
		if v == id {
			return ids
		}
	}
	return append(ids, id)
}
func (s *Service) upsertPersonalLocked(a *model.Account, key, name string, scopes []string) *model.PersonalKey {
	for i, pk := range s.St.Data.PersonalKeys {
		if belongs(pk.AccountID, a) && pk.Key == key {
			pk.AccountID = a.ID
			a.PersonalKeyIDs = appendID(a.PersonalKeyIDs, pk.ID)
			// A freshly verified import is the preferred credential, even if an
			// identical key already exists earlier in a migrated list.
			copy(s.St.Data.PersonalKeys[i:], s.St.Data.PersonalKeys[i+1:])
			s.St.Data.PersonalKeys[len(s.St.Data.PersonalKeys)-1] = pk
			return pk
		}
	}
	pk := &model.PersonalKey{ID: model.RandID("pk_", 6), AccountID: a.ID, Name: name, Key: key, Scopes: append([]string(nil), scopes...), CreatedAt: time.Now().UnixMilli()}
	s.St.Data.PersonalKeys = append(s.St.Data.PersonalKeys, pk)
	a.PersonalKeyIDs = appendID(a.PersonalKeyIDs, pk.ID)
	return pk
}
func (s *Service) personalAKLocked(a *model.Account) string {
	if a == nil {
		return ""
	}
	for i := len(s.St.Data.PersonalKeys) - 1; i >= 0; i-- {
		pk := s.St.Data.PersonalKeys[i]
		if belongs(pk.AccountID, a) && pk.Key != "" {
			return pk.Key
		}
	}
	return ""
}
func (s *Service) personalAKOf(a *model.Account) string {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	return s.personalAKLocked(a)
}
func (s *Service) PersonalKeyOf(a *model.Account) []*model.PersonalKey {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	out := []*model.PersonalKey{}
	if a != nil {
		for _, pk := range s.St.Data.PersonalKeys {
			if belongs(pk.AccountID, a) {
				out = append(out, clonePersonal(pk))
			}
		}
	}
	return out
}

func (s *Service) ProjectKey(a *model.Account, name string) (map[string]any, error) {
	return s.ProjectKeyContext(context.Background(), a, name)
}
func (s *Service) ProjectKeyContext(ctx context.Context, a *model.Account, name string) (map[string]any, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.projectKey(ctx, a, name)
}
func keyBelongs(k *model.Key, a *model.Account) bool {
	if k.AccountID == a.ID || (k.AccountID == "" && k.Account != nil && belongs(k.Account.ID, a)) {
		return true
	}
	if k.ID == a.KeyID {
		return true
	}
	for _, id := range a.ProjectKeyIDs {
		if k.ID == id {
			return true
		}
	}
	return false
}
func (s *Service) existingProjectLocked(a *model.Account) *model.Key {
	for _, k := range s.St.Data.Keys {
		if keyBelongs(k, a) && k.Key != "" && k.Health.State != "invalid" {
			a.KeyID = k.ID
			a.ProjectKeyIDs = appendID(a.ProjectKeyIDs, k.ID)
			return k
		}
	}
	return nil
}
func projectResult(k *model.Key, project string) map[string]any {
	return map[string]any{"ok": true, "project": project, "keyId": k.ID, "key": model.Mask(k.Key)}
}
func (s *Service) projectKey(ctx context.Context, a *model.Account, name string) (map[string]any, error) {
	s.St.Mu.Lock()
	if !s.attachedLocked(a) {
		s.St.Mu.Unlock()
		return nil, fmt.Errorf("账号已删除")
	}
	if k := s.existingProjectLocked(a); k != nil {
		out := projectResult(k, a.ProjectID)
		s.St.Mu.Unlock()
		return out, s.persist()
	}
	project := a.ProjectID
	newProject := project == ""
	if project == "" {
		for _, k := range s.St.Data.Keys {
			if keyBelongs(k, a) && k.Health.State == "invalid" {
				s.St.Mu.Unlock()
				return nil, fmt.Errorf("已有失效项目凭证，但缺少项目 ID")
			}
		}
	}
	ak := s.personalAKLocked(a)
	s.St.Mu.Unlock()
	if ak == "" {
		return nil, fmt.Errorf("该账号没有可用个人 AK")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "meoo2api-" + strings.ToLower(model.RandID("", 3))
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	hdr := map[string]string{"Authorization": "Bearer " + ak, "X-Meoo-Client": "cli"}
	if project == "" {
		code, raw, err := s.request(ctx, http.MethodPost, "https://meoo.com/open/v1/cli-compat/projects", map[string]any{"name": name, "type": "create-desktop"}, hdr)
		if err != nil {
			return nil, err
		}
		var d struct {
			Data struct {
				URLID string `json:"url_id"`
			} `json:"data"`
		}
		if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.URLID == "" {
			return nil, fmt.Errorf("建项目失败 HTTP %d", code)
		}
		project = d.Data.URLID
		// Persist the server-created project before cloud provisioning. A retry must
		// resume this project instead of creating another one.
		s.St.Mu.Lock()
		a.ProjectID = project
		s.St.Mu.Unlock()
		if err := s.persist(); err != nil {
			return map[string]any{"project": project, "stage": "persist"}, err
		}
	}
	base := "https://meoo.com/open/v1/cli-compat/projects/" + url.PathEscape(project) + "/cloud"
	readEnvironmentOnce := func() (string, int, error) {
		code, raw, err := s.request(ctx, http.MethodGet, base+"/environment", nil, hdr)
		var d struct {
			Data struct {
				Key     string `json:"meooProjectApiKey"`
				Enabled *bool  `json:"enabled"`
			} `json:"data"`
		}
		if err != nil {
			return "", code, err
		}
		if code != 200 || json.Unmarshal(raw, &d) != nil {
			return "", code, fmt.Errorf("读取项目凭证失败 HTTP %d", code)
		}
		if d.Data.Enabled != nil && !*d.Data.Enabled {
			return "", code, errCloudDisabled
		}
		if d.Data.Key == "" {
			return "", code, fmt.Errorf("读取项目凭证失败 HTTP %d", code)
		}
		return d.Data.Key, code, nil
	}
	readEnvironment := func() (string, int, error) {
		for attempt := 0; ; attempt++ {
			key, code, err := readEnvironmentOnce()
			var business *BusinessError
			if err == nil || errors.Is(err, errCloudDisabled) || errors.As(err, &business) || code != 200 || attempt >= 5 {
				return key, code, err
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", code, ctx.Err()
			case <-timer.C:
			}
		}
	}
	// A quota failure permits one bounded, read-only discovery of an already
	// enabled same-user project. If discovery fails, preserve the original quota.
	provision := func() (string, error) {
		code, _, err := s.request(ctx, http.MethodPost, base, map[string]any{"description": name + " cloud"}, hdr)
		if err != nil {
			var business *BusinessError
			if errors.As(err, &business) && business.Code == "QUOTA_EXCEEDED" {
				cp := s.snapshot(a)
				if existing, key, findErr := s.reusableProject(ctx, cp.UserID, hdr); findErr == nil {
					project = existing
					return key, nil
				}
			}
			return "", err
		}
		if code != 200 && code != 201 && code != 409 {
			return "", fmt.Errorf("开通云服务失败 HTTP %d", code)
		}
		return "", nil
	}
	var key string
	if newProject {
		var err error
		key, err = provision()
		if err != nil {
			return map[string]any{"project": project, "stage": "cloud"}, err
		}
	}
	if key == "" {
		var envCode int
		var envErr error
		key, envCode, envErr = readEnvironment()
		if envErr != nil {
			var business *BusinessError
			if newProject || errors.As(envErr, &business) || (!errors.Is(envErr, errCloudDisabled) && envCode != 400 && envCode != 404 && envCode != 409) {
				return map[string]any{"project": project, "stage": "environment"}, envErr
			}
			if ctx.Err() != nil {
				return map[string]any{"project": project, "stage": "environment"}, ctx.Err()
			}
			var err error
			key, err = provision()
			if err != nil {
				return map[string]any{"project": project, "stage": "cloud"}, err
			}
			if key == "" {
				key, _, err = readEnvironment()
				if err != nil {
					return map[string]any{"project": project, "stage": "environment"}, err
				}
			}
		}
	}
	s.St.Mu.Lock()
	var k *model.Key
	for _, x := range s.St.Data.Keys {
		if x.Key == key {
			k = x
			break
		}
	}
	if k != nil && ((k.AccountID != "" && k.AccountID != a.ID) || (k.Account != nil && k.Account.ID != "" && !belongs(k.Account.ID, a))) {
		s.St.Mu.Unlock()
		return nil, fmt.Errorf("项目凭证已属于其他账号")
	}
	if k == nil {
		for _, x := range s.St.Data.Keys {
			if keyBelongs(x, a) && x.Health.State == "invalid" {
				k = x
				k.Key = key
				k.Health = model.Health{State: "unknown"}
				k.CooldownUntil = 0
				break
			}
		}
	}
	if k == nil {
		k = store.NewKey(project, key, 1)
		s.St.Data.Keys = append(s.St.Data.Keys, k)
	}
	if k.Health.State == "invalid" {
		k.Health = model.Health{State: "unknown"}
		k.CooldownUntil = 0
	}
	a.ProjectID = project
	k.AccountID = a.ID
	k.Account = &model.Identity{ID: a.UserID, Name: a.Name, Kind: "key"}
	k.Enabled = a.Enabled
	a.ProjectKeyIDs = appendID(a.ProjectKeyIDs, k.ID)
	a.KeyID = k.ID
	out := projectResult(k, project)
	s.St.Mu.Unlock()
	return out, s.persist()
}
func (s *Service) keyInfo(a *model.Account) string {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	for _, k := range s.St.Data.Keys {
		if k.ID == a.KeyID {
			return model.Mask(k.Key)
		}
	}
	return ""
}
func (s *Service) Onboard(a *model.Account) (map[string]any, error) {
	return s.OnboardContext(context.Background(), a)
}
func (s *Service) OnboardContext(ctx context.Context, a *model.Account) (map[string]any, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.onboard(ctx, a)
}
func (s *Service) onboard(ctx context.Context, a *model.Account) (map[string]any, error) {
	if a == nil {
		return nil, fmt.Errorf("账号为空")
	}
	cp := s.snapshot(a)
	if cp.UserID == "" {
		if err := s.identify(ctx, a); err != nil {
			return nil, err
		}
		cp = s.snapshot(a)
	}
	out := map[string]any{"account": map[string]any{"userId": cp.UserID, "name": cp.Name}}
	s.St.Mu.Lock()
	if !s.attachedLocked(a) {
		s.St.Mu.Unlock()
		return out, fmt.Errorf("账号已删除")
	}
	if k := s.existingProjectLocked(a); k != nil {
		for key, value := range projectResult(k, a.ProjectID) {
			out[key] = value
		}
		s.St.Mu.Unlock()
		return out, s.persist()
	}
	s.St.Mu.Unlock()
	pk, err := s.personalKey(ctx, a)
	if err != nil {
		return out, err
	}
	out["personalKey"] = model.Mask(pk.Key)
	proj, err := s.projectKey(ctx, a, "")
	for k, v := range proj {
		out[k] = v
	}
	return out, err
}

// Add validates Cookie identity before storing anything. Refreshing an existing
// user is idempotent and reuses both personal and project credentials.
func (s *Service) Add(cookie string) (*model.Account, map[string]any, error) {
	return s.AddContext(context.Background(), cookie)
}
func (s *Service) AddContext(ctx context.Context, cookie string) (*model.Account, map[string]any, error) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return nil, nil, fmt.Errorf("登录态 Cookie 为空")
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	v, err := s.identity(ctx, cookie, "")
	if err != nil {
		return nil, nil, err
	}
	s.St.Mu.Lock()
	var a *model.Account
	for _, x := range s.St.Data.Accounts {
		if x.UserID == v.UserID {
			a = x
			break
		}
	}
	if a == nil {
		a = &model.Account{ID: model.RandID("a_", 6), Enabled: true, CreatedAt: time.Now().UnixMilli(), PersonalKeyIDs: []string{}, ProjectKeyIDs: []string{}}
		s.St.Data.Accounts = append(s.St.Data.Accounts, a)
	}
	if a.Cookie != cookie {
		a.Last = nil
	}
	a.UserID, a.Name, a.Cookie = v.UserID, v.Name, cookie
	a.Expired = false
	a.PingOK = true
	a.LastPing = time.Now().UnixMilli()
	if a.Label == "" || a.Label == "未命名" {
		a.Label = v.Name
	}
	s.St.Mu.Unlock()
	if err := s.persist(); err != nil {
		return a, nil, err
	}
	out, err := s.onboard(ctx, a)
	if out == nil {
		out = map[string]any{}
	}
	out["session"] = true
	if ctx.Err() == nil {
		if points, pointsErr := s.PointsContext(ctx, a); pointsErr == nil {
			out["points"] = points
		}
	}
	if saveErr := s.persist(); err == nil {
		err = saveErr
	}
	return a, out, err
}

// UpdateContext synchronizes lifecycle changes with onboarding and deletion.
func (s *Service) UpdateContext(ctx context.Context, id string, enabled *bool, cookie *string) error {
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	a := s.Find(id)
	if a == nil {
		return fmt.Errorf("账号不存在")
	}
	var v identity
	if cookie != nil && strings.TrimSpace(*cookie) != "" {
		v, err = s.identity(ctx, strings.TrimSpace(*cookie), "")
		if err != nil {
			return err
		}
		cp := s.snapshot(a)
		if cp.UserID != "" && cp.UserID != v.UserID {
			return fmt.Errorf("Cookie 属于其他账号")
		}
		s.St.Mu.RLock()
		for _, x := range s.St.Data.Accounts {
			if x != a && x.UserID == v.UserID {
				s.St.Mu.RUnlock()
				return fmt.Errorf("该身份已有账号")
			}
		}
		s.St.Mu.RUnlock()
	}
	s.St.Mu.Lock()
	if cookie != nil {
		if a.Cookie != strings.TrimSpace(*cookie) {
			a.Last = nil
		}
		a.Cookie = strings.TrimSpace(*cookie)
		a.PingOK = false
		if a.Cookie != "" {
			a.UserID, a.Name = v.UserID, v.Name
			a.Expired = false
			a.PingOK = true
			a.LastPing = time.Now().UnixMilli()
		}
	}
	if enabled != nil {
		a.Enabled = *enabled
		for _, k := range s.St.Data.Keys {
			if keyBelongs(k, a) {
				k.Enabled = *enabled
			}
		}
	}
	s.St.Mu.Unlock()
	return s.persist()
}
func (s *Service) DeleteContext(ctx context.Context, id string) error {
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	s.St.Mu.Lock()
	a := s.findLocked(id)
	if a == nil {
		s.St.Mu.Unlock()
		return fmt.Errorf("账号不存在")
	}
	accounts := s.St.Data.Accounts[:0]
	for _, x := range s.St.Data.Accounts {
		if x != a {
			accounts = append(accounts, x)
		}
	}
	s.St.Data.Accounts = accounts
	keys := s.St.Data.Keys[:0]
	for _, k := range s.St.Data.Keys {
		if !keyBelongs(k, a) {
			keys = append(keys, k)
		}
	}
	s.St.Data.Keys = keys
	pks := s.St.Data.PersonalKeys[:0]
	for _, pk := range s.St.Data.PersonalKeys {
		if !belongs(pk.AccountID, a) {
			pks = append(pks, pk)
		}
	}
	s.St.Data.PersonalKeys = pks
	s.St.Mu.Unlock()
	return s.persist()
}
