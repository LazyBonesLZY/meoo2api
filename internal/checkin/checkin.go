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

func Run(st *store.Store, c *model.Checkin) model.CheckinLast {
	t0 := time.Now()
	last := model.CheckinLast{At: t0.UnixMilli()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		last.Error = err.Error()
	} else {
		req.Header.Set("Cookie", c.Cookie)
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
	c.Last = &last
	switch {
	case last.Granted:
		c.LastGrantDate = model.Today()
		c.TotalGranted += last.Amount
		c.GrantCount++
	case last.SkipReason == "ALREADY_GRANTED_TODAY":
		c.LastGrantDate = model.Today()
	}
	st.Mu.Unlock()
	st.Touch()
	return last
}

func RunAll(st *store.Store, force bool) []Result {
	out := []Result{}
	st.Mu.RLock()
	list := append([]*model.Checkin(nil), st.Data.Checkins...)
	interval := int64(st.Data.Config.CheckinIntervalH)
	st.Mu.RUnlock()
	for _, c := range list {
		if !c.Enabled {
			continue
		}
		if !force && c.LastGrantDate == model.Today() {
			out = append(out, Result{ID: c.ID, Label: c.Label, Skipped: "今日已到账"})
			continue
		}
		if !force && c.Last != nil && time.Now().UnixMilli()-c.Last.At < interval*3600*1000 {
			continue
		}
		out = append(out, Result{ID: c.ID, Label: c.Label, CheckinLast: Run(st, c)})
	}
	_ = st.Save()
	return out
}

// StartScheduler 启动 45 秒后先跑一轮，此后每 30 分钟检查一次
func StartScheduler(st *store.Store) {
	go func() {
		tick := func() {
			st.Mu.RLock()
			on := st.Data.Config.AutoCheckin
			st.Mu.RUnlock()
			if !on {
				return
			}
			RunAll(st, false)
			st.Mu.Lock()
			st.Data.LastAutoRun = time.Now().UnixMilli()
			st.Mu.Unlock()
			st.Touch()
		}
		time.Sleep(45 * time.Second)
		tick()
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for range t.C {
			tick()
		}
	}()
}
