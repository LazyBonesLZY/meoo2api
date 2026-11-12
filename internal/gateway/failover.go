package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/upstream"
)

const (
	// maxKeyAttempts bounds upstream calls per client request. Every attempt is
	// billed by the upstream, so this must stay small.
	maxKeyAttempts = 4
	// balanceParkCooldown keeps an account with no remaining balance out of
	// rotation long enough that client retries do not burn a call each time.
	balanceParkCooldown = 30 * time.Minute
)

// attempt performs one upstream call with the run's selected key. wrote reports
// whether the client already received response bytes; a retry with another key
// is only safe while nothing has been sent.
type attempt func(run *callRun) (wrote bool, pt, ct int64, err error)

// withFailover runs an attempt, and on an account-level upstream failure retries
// the same client request with a different usable key. Client errors (4xx that
// describe the request), cancellations and timeouts are never retried.
func (h *Handler) withFailover(w http.ResponseWriter, r *http.Request, c caller, resolved string, exec attempt) {
	tried := map[string]bool{}
	for len(tried) < maxKeyAttempts {
		run := h.startRun(w, r, c, resolved, tried)
		if run == nil {
			return
		}
		wrote, pt, ct, err := exec(run)
		if err == nil {
			run.finish(nil, pt, ct)
			return
		}
		if wrote {
			// The response has begun; an error event was already written by the
			// streaming path, so only accounting remains.
			run.finish(err, pt, ct)
			return
		}
		if run.picked == nil || !retryableKeyFailure(err) || !h.hasAlternativeKey(run.picked, tried) {
			run.finish(err, pt, ct)
			respondError(w, err)
			return
		}
		run.recordKey(false, err, pt, ct)
		tried[run.picked.ID] = true
	}
	// Bounded attempts exhausted with retryable failures only.
	err := &upstream.Error{Status: 503, Type: "api_error", Code: "no_available_key",
		Message: "可用账号均无法完成请求（余额不足或鉴权失败），请检查账号状态"}
	run := h.startRun(w, r, c, resolved, tried)
	if run == nil {
		return
	}
	run.finish(err, 0, 0)
	respondError(w, err)
}

// hasAlternativeKey reports whether another usable key remains untried.
func (h *Handler) hasAlternativeKey(used *model.Key, tried map[string]bool) bool {
	for _, k := range h.Picker.Usable() {
		if k.ID != used.ID && !tried[k.ID] {
			return true
		}
	}
	return false
}

// retryableKeyFailure distinguishes "this account cannot serve now" from "this
// request is wrong". Only the former may move to another account.
func retryableKeyFailure(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var e *upstream.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// insufficientBalance matches the upstream account-level billing rejection.
func insufficientBalance(err error) bool {
	var e *upstream.Error
	if !errors.As(err, &e) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(e.Code), "QUOTA_EXCEEDED") {
		return true
	}
	message := strings.ToLower(e.Message)
	return strings.Contains(e.Message, "积分余额不足") || strings.Contains(message, "insufficient balance")
}

// outcome maps an attempt error to cooldown policy. Request-level 4xx errors and
// cancellations must not punish a key.
func (run *callRun) outcome(err error) (cooldown, soft bool) {
	e := asError(err)
	soft = e.Status == http.StatusTooManyRequests || e.Status >= 500
	cooldown = soft || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
	return cooldown, soft
}

// recordKey applies one attempt to pool accounting. Failures cool the key and
// park accounts that cannot serve at all; success never clears a newer cooldown.
func (run *callRun) recordKey(ok bool, err error, pt, ct int64) {
	if run.picked == nil {
		return
	}
	cooldown, soft := false, false
	if !ok {
		cooldown, soft = run.outcome(err)
	}
	run.h.Picker.RecordKey(run.picked, ok, cooldown, soft, pt, ct)
	if !ok && insufficientBalance(err) {
		run.h.Picker.ParkKey(run.picked, balanceParkCooldown, "账号积分余额不足，已暂停使用")
	}
	run.h.St.Touch()
}
