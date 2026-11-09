// Package checkin 负责每日登录积分领取与可取消调度。
package checkin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
)

const endpoint = "https://meoo.com/api/mc/entitlement/init"

type response struct {
	Success bool `json:"success"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
	Data struct {
		Initialized  bool   `json:"initialized"`
		Granted      bool   `json:"dailyLoginGranted"`
		Amount       *int64 `json:"dailyLoginGrantAmount"`
		SkipReason   string `json:"dailyLoginSkipReason"`
		BalanceAfter *int64 `json:"dailyLoginBalanceAfter"`
	} `json:"data"`
}
type Result struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Skipped string `json:"skipped,omitempty"`
	model.CheckinLast
}

// Runner makes the HTTP dependency explicit for mock tests and custom transports.
type Runner struct{ HTTP *http.Client }

type gateKey struct {
	st *store.Store
	a  *model.Account
}
type gate struct {
	ch         chan struct{}
	refs       int
	generation atomic.Uint64
	last       *model.CheckinLast
	cookie     string
}

var gates = struct {
	sync.Mutex
	values map[gateKey]*gate
}{values: map[gateKey]*gate{}}

func acquire(ctx context.Context, st *store.Store, a *model.Account) (*gate, uint64, func(), error) {
	key := gateKey{st, a}
	gates.Lock()
	g := gates.values[key]
	if g == nil {
		g = &gate{ch: make(chan struct{}, 1)}
		gates.values[key] = g
	}
	g.refs++
	generation := g.generation.Load()
	gates.Unlock()
	unref := func() {
		gates.Lock()
		g.refs--
		if g.refs == 0 {
			delete(gates.values, key)
		}
		gates.Unlock()
	}
	if err := ctx.Err(); err != nil {
		unref()
		return nil, 0, nil, err
	}
	select {
	case g.ch <- struct{}{}:
		return g, generation, func() { <-g.ch; unref() }, nil
	case <-ctx.Done():
		unref()
		return nil, 0, nil, ctx.Err()
	}
}
func attached(st *store.Store, a *model.Account) bool {
	for _, x := range st.Data.Accounts {
		if x == a {
			return true
		}
	}
	return false
}
func copyLast(last model.CheckinLast) model.CheckinLast {
	if last.BalanceAfter != nil {
		n := *last.BalanceAfter
		last.BalanceAfter = &n
	}
	return last
}
func Run(st *store.Store, a *model.Account) model.CheckinLast {
	return RunContext(context.Background(), st, a)
}
func RunContext(ctx context.Context, st *store.Store, a *model.Account) model.CheckinLast {
	return (&Runner{HTTP: http.DefaultClient}).RunContext(ctx, st, a)
}
func (r *Runner) RunContext(ctx context.Context, st *store.Store, a *model.Account) model.CheckinLast {
	started := time.Now()
	last := model.CheckinLast{At: started.UnixMilli()}
	if a == nil {
		last.Error = "账号为空"
		return last
	}
	g, generation, release, err := acquire(ctx, st, a)
	if err != nil {
		last.Error = err.Error()
		return last
	}
	defer release()
	st.Mu.RLock()
	exists := attached(st, a)
	cookie := a.Cookie
	enabled := a.Enabled
	// All calls queued behind the same attempt share its result. Do not infer
	// overlap from wall-clock milliseconds: truncation loses sub-ms boundaries.
	if exists && enabled && cookie != "" && g.generation.Load() != generation && g.last != nil && g.cookie == cookie {
		result := copyLast(*g.last)
		st.Mu.RUnlock()
		return result
	}
	st.Mu.RUnlock()
	if !exists {
		last.Error = "账号已删除"
		return last
	}
	if !enabled {
		last.Error = "账号已停用"
		return last
	}
	last.At = time.Now().UnixMilli()
	switch {
	case cookie == "":
		last.Error = "该账号没有登录态 Cookie"
	case ctx.Err() != nil:
		last.Error = ctx.Err().Error()
	default:
		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			last.Error = err.Error()
			break
		}
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Referer", "https://meoo.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126 Safari/537.36")
		client := r.HTTP
		if client == nil {
			client = http.DefaultClient
		}
		isolated := *client
		isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := isolated.Do(req)
		if err != nil {
			last.Error = err.Error()
			break
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8193))
		resp.Body.Close()
		var d response
		switch {
		case readErr != nil:
			last.Error = readErr.Error()
		case len(raw) > 8192:
			last.Error = "签到响应过大"
		case json.Unmarshal(raw, &d) != nil:
			last.Error = "响应不是 JSON"
		case resp.StatusCode != 200 || !d.Success:
			last.Error = d.Error.Message
			if last.Error == "" {
				last.Error = fmt.Sprintf("签到失败 HTTP %d", resp.StatusCode)
			}
		default:
			last.OK = true
			last.Initialized = d.Data.Initialized
			last.Granted = d.Data.Granted
			last.SkipReason = d.Data.SkipReason
			if d.Data.Amount != nil {
				last.Amount = *d.Data.Amount
			}
			last.BalanceAfter = d.Data.BalanceAfter
		}
		st.Mu.Lock()
		if attached(st, a) && a.Cookie == cookie {
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				a.Expired = true
				a.PingOK = false
			} else if last.OK {
				a.Expired = false
			}
		}
		st.Mu.Unlock()
	}
	last.Ms = time.Since(started).Milliseconds()
	st.Mu.Lock()
	if attached(st, a) && a.Cookie == cookie {
		saved := copyLast(last)
		a.Last = &saved
		cached := copyLast(last)
		g.last = &cached
		g.cookie = cookie
		g.generation.Add(1)
		// Upstream may repeat a granted response on retries. Account it at most once
		// per calendar date, using the attempt's date rather than the completion date.
		date := time.UnixMilli(last.At).Format("2006-01-02")
		if last.OK && (last.Granted || last.SkipReason == "ALREADY_GRANTED_TODAY") {
			if last.Granted && a.LastGrantDate != date {
				a.TotalGranted += last.Amount
				a.GrantCount++
			}
			a.LastGrantDate = date
		}
		if last.OK && last.BalanceAfter != nil {
			a.Points = *last.BalanceAfter
			a.PointsAt = time.Now().UnixMilli()
		}
	}
	st.Mu.Unlock()
	st.Touch()
	return last
}
func RunAll(st *store.Store, force bool) []Result {
	return RunAllContext(context.Background(), st, force)
}
func RunAllContext(ctx context.Context, st *store.Store, force bool) []Result {
	out := []Result{}
	st.Mu.RLock()
	list := append([]*model.Account(nil), st.Data.Accounts...)
	st.Mu.RUnlock()
	for _, a := range list {
		if ctx.Err() != nil {
			break
		}
		st.Mu.RLock()
		exists := attached(st, a)
		id, label := a.ID, a.Label
		eligible := a.Enabled && !a.Expired && a.Cookie != ""
		done := a.LastGrantDate == model.Today()
		st.Mu.RUnlock()
		if !exists || !eligible {
			continue
		}
		if !force && done {
			out = append(out, Result{ID: id, Label: label, Skipped: "今日已到账"})
			continue
		}
		out = append(out, Result{ID: id, Label: label, CheckinLast: RunContext(ctx, st, a)})
	}
	_ = st.Save()
	return out
}

// StartScheduler is the legacy process-lifetime wrapper. New callers should use
// StartSchedulerWithContext so shutdown also cancels an in-flight keepalive.
func StartScheduler(st *store.Store, ping func(*model.Account) (bool, error)) {
	StartSchedulerContext(context.Background(), st, ping)
}
func StartSchedulerContext(ctx context.Context, st *store.Store, ping func(*model.Account) (bool, error)) <-chan struct{} {
	var fn func(context.Context, *model.Account) (bool, error)
	if ping != nil {
		fn = func(_ context.Context, a *model.Account) (bool, error) { return ping(a) }
	}
	return StartSchedulerWithContext(ctx, st, fn)
}
func StartSchedulerWithContext(ctx context.Context, st *store.Store, ping func(context.Context, *model.Account) (bool, error)) <-chan struct{} {
	return startScheduler(ctx, st, ping, 5*time.Minute)
}
func startScheduler(ctx context.Context, st *store.Store, ping func(context.Context, *model.Account) (bool, error), interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			runContext(ctx, st, ping)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
func run(st *store.Store, ping func(*model.Account) (bool, error)) {
	var fn func(context.Context, *model.Account) (bool, error)
	if ping != nil {
		fn = func(_ context.Context, a *model.Account) (bool, error) { return ping(a) }
	}
	runContext(context.Background(), st, fn)
}
func runContext(ctx context.Context, st *store.Store, ping func(context.Context, *model.Account) (bool, error)) {
	st.Mu.RLock()
	on, hour, keep := st.Data.Config.AutoCheckin, st.Data.Config.CheckinHour, st.Data.Config.KeepAliveHours
	list := append([]*model.Account(nil), st.Data.Accounts...)
	st.Mu.RUnlock()
	now := time.Now()
	if hour < 0 {
		hour = 0
	}
	if hour > 23 {
		hour = 23
	}
	for _, a := range list {
		if ctx.Err() != nil {
			return
		}
		st.Mu.RLock()
		exists := attached(st, a)
		enabled, lastPing := a.Enabled, a.LastPing
		st.Mu.RUnlock()
		if !exists || !enabled {
			continue
		}
		// Expired credentials are still periodically probed: replacing/recovering a
		// session must not leave the account permanently excluded from the scheduler.
		due := keep > 0 && (lastPing == 0 || now.Sub(time.UnixMilli(lastPing)).Hours() >= float64(keep))
		if due && ping != nil {
			ok, err := ping(ctx, a)
			if err != nil || !ok {
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		st.Mu.RLock()
		eligible := attached(st, a) && a.Enabled && !a.Expired && a.Cookie != "" && a.LastGrantDate != now.Format("2006-01-02")
		st.Mu.RUnlock()
		if on && now.Hour() >= hour && eligible {
			RunContext(ctx, st, a)
		}
	}
	if ctx.Err() != nil {
		return
	}
	st.Mu.Lock()
	st.Data.LastAutoRun = now.UnixMilli()
	st.Mu.Unlock()
	st.Touch()
}
