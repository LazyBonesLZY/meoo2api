// Package checkin 负责每日登录积分领取与调度。
package checkin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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

func Run(st *store.Store, a *model.Account) model.CheckinLast {
	t0 := time.Now()
	last := model.CheckinLast{At: t0.UnixMilli()}
	if a.Cookie == "" {
		last.Error = "该账号没有登录态 Cookie"
		st.Mu.Lock()
		a.Last = &last
		st.Mu.Unlock()
		return last
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		last.Error = err.Error()
	} else {
		req.Header.Set("Cookie", a.Cookie)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Referer", "https://meoo.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126 Safari/537.36")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last.Error = err.Error()
		} else {
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			last.Ms = time.Since(t0).Milliseconds()
			var d response
			switch {
			case json.Unmarshal(raw, &d) != nil:
				last.Error = "响应不是 JSON"
			case resp.StatusCode != 200 || !d.Success:
				last.Error = d.Error.Message
				if last.Error == "" {
					last.Error = resp.Status
				}
			default:
				last.OK = true
				last.Initialized = d.Data.Initialized
				last.Granted = d.Data.Granted
				if d.Data.Amount != nil {
					last.Amount = *d.Data.Amount
				}
				last.SkipReason = d.Data.SkipReason
				last.BalanceAfter = d.Data.BalanceAfter
			}
		}
	}
	st.Mu.Lock()
	a.Last = &last
	switch {
	case last.Granted:
		a.LastGrantDate = model.Today()
		a.TotalGranted += last.Amount
		a.GrantCount++
	case last.SkipReason == "ALREADY_GRANTED_TODAY":
		a.LastGrantDate = model.Today()
	}
	st.Mu.Unlock()
	st.Touch()
	return last
}

func RunAll(st *store.Store, force bool) []Result {
	out := []Result{}
	st.Mu.RLock()
	list := append([]*model.Account(nil), st.Data.Accounts...)
	st.Mu.RUnlock()
	for _, a := range list {
		if !a.Enabled || a.Expired {
			continue
		}
		if !force && a.LastGrantDate == model.Today() {
			out = append(out, Result{ID: a.ID, Label: a.Label, Skipped: "今日已到账"})
			continue
		}
		out = append(out, Result{ID: a.ID, Label: a.Label, CheckinLast: Run(st, a)})
	}
	_ = st.Save()
	return out
}

// StartScheduler 每天在 checkinHour 后领一次，其余时间只做会话保活。
// 不需要外部 cron：调度、保活都在进程内完成。
func StartScheduler(st *store.Store, ping func(*model.Account) (bool, error)) {
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			run(st, ping)
			<-t.C
		}
	}()
}

func run(st *store.Store, ping func(*model.Account) (bool, error)) {
	st.Mu.RLock()
	on := st.Data.Config.AutoCheckin
	hour := st.Data.Config.CheckinHour
	keep := st.Data.Config.KeepAliveHours
	list := append([]*model.Account(nil), st.Data.Accounts...)
	st.Mu.RUnlock()

	now := time.Now()
	for _, a := range list {
		if !a.Enabled {
			continue
		}
		if a.Expired {
			continue
		}
		if keep > 0 && (a.LastPing == 0 || now.UnixMilli()-a.LastPing > int64(keep)*3600*1000) {
			if ping != nil {
				_, _ = ping(a)
			}
		}
		if on && now.Hour() >= hour && a.LastGrantDate != model.Today() {
			Run(st, a)
		}
	}
	st.Mu.Lock()
	st.Data.LastAutoRun = now.UnixMilli()
	st.Mu.Unlock()
	st.Touch()
}
