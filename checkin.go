package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const checkinURL = "https://meoo.com/api/mc/entitlement/init"

type checkinResp struct {
	Success bool `json:"success"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Data struct {
		Initialized          bool   `json:"initialized"`
		DailyLoginGranted    bool   `json:"dailyLoginGranted"`
		DailyLoginGrantNo    any    `json:"dailyLoginGrantNo"`
		DailyLoginGrantAmt   *int64 `json:"dailyLoginGrantAmount"`
		DailyLoginSkipReason string `json:"dailyLoginSkipReason"`
		BalanceAfter         *int64 `json:"dailyLoginBalanceAfter"`
	} `json:"data"`
}

func today() string { return time.Now().Format("2006-01-02") }

func RunCheckin(s *State, c *Checkin) CheckinLast {
	t0 := time.Now()
	last := CheckinLast{At: t0.UnixMilli()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkinURL, nil)
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
			var d checkinResp
			if json.Unmarshal(raw, &d) != nil {
				last.Error = "响应不是 JSON"
			} else if resp.StatusCode != 200 || !d.Success {
				last.Error = d.Error.Message
				if last.Error == "" {
					last.Error = resp.Status
				}
			} else {
				last.OK = true
				last.Initialized = d.Data.Initialized
				last.Granted = d.Data.DailyLoginGranted
				if d.Data.DailyLoginGrantAmt != nil {
					last.Amount = *d.Data.DailyLoginGrantAmt
				}
				last.SkipReason = d.Data.DailyLoginSkipReason
				last.BalanceAfter = d.Data.BalanceAfter
				switch {
				case last.Granted:
					c.LastGrantDate = today()
					c.TotalGranted += last.Amount
					c.GrantCount++
				case last.SkipReason == "ALREADY_GRANTED_TODAY":
					// 今天已在网页端领过，记下来避免每 6 小时重复打一次
					c.LastGrantDate = today()
				}
			}
		}
	}
	s.mu.Lock()
	c.Last = &last
	s.mu.Unlock()
	s.Touch()
	return last
}

type CheckinResult struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Skipped string `json:"skipped,omitempty"`
	CheckinLast
}

func RunAllCheckins(s *State, force bool) []CheckinResult {
	out := []CheckinResult{}
	s.mu.RLock()
	list := append([]*Checkin(nil), s.Checkins...)
	s.mu.RUnlock()
	for _, c := range list {
		if !c.Enabled {
			continue
		}
		if !force && c.LastGrantDate == today() {
			out = append(out, CheckinResult{ID: c.ID, Label: c.Label, Skipped: "今日已到账"})
			continue
		}
		if !force && c.Last != nil && time.Now().UnixMilli()-c.Last.At < int64(s.Config.CheckinIntervalH)*3600*1000 {
			continue
		}
		r := CheckinResult{ID: c.ID, Label: c.Label, CheckinLast: RunCheckin(s, c)}
		out = append(out, r)
	}
	_ = s.Save()
	return out
}

func StartCheckinScheduler(s *State) {
	go func() {
		// 启动后先跑一次，避免等满一个周期
		time.Sleep(45 * time.Second)
		if s.Config.AutoCheckin {
			RunAllCheckins(s, false)
			s.mu.Lock()
			s.LastAutoRun = time.Now().UnixMilli()
			s.mu.Unlock()
			s.Touch()
		}
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for range t.C {
			if !s.Config.AutoCheckin {
				continue
			}
			RunAllCheckins(s, false)
			s.mu.Lock()
			s.LastAutoRun = time.Now().UnixMilli()
			s.mu.Unlock()
			s.Touch()
		}
	}()
}

func IsProjectAK(k string) bool {
	return strings.HasPrefix(k, "meoo_project_") && len(k) > 16
}
