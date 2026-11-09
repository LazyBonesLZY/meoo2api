package model

import "fmt"

// Validate is shared by loading and the management API. All integer settings
// are bounded before they can affect timers, allocations, slicing or fan-out.
func (c Config) Validate() error {
	bounds := []struct {
		name            string
		value, min, max int
	}{
		{"timeoutMs", c.TimeoutMs, 1000, 900000},
		{"defaultMaxTokens", c.DefaultMaxTokens, 0, 262144},
		{"cooldownSec", c.CooldownSec, 0, 86400},
		{"hardCooldownSec", c.HardCooldownSec, 0, 604800},
		{"maxLogs", c.MaxLogs, 0, 100000},
		{"maxUsageHours", c.MaxUsageHours, 1, 8760},
		{"checkinHour", c.CheckinHour, 0, 23},
		{"keepAliveHours", c.KeepAliveHours, 0, 168},
	}
	for _, b := range bounds {
		if b.value < b.min || b.value > b.max {
			return fmt.Errorf("%s 必须在 %d 到 %d 之间", b.name, b.min, b.max)
		}
	}
	if c.AdminUser == "" || len(c.AdminUser) > 128 {
		return fmt.Errorf("adminUser 长度必须为 1 到 128")
	}
	if len(c.Aliases) > 200 || len(c.HiddenModels) > 200 {
		return fmt.Errorf("模型配置项过多")
	}
	for alias, target := range c.Aliases {
		if alias == "" || target == "" || len(alias) > 128 || len(target) > 128 {
			return fmt.Errorf("模型别名或目标无效")
		}
	}
	return nil
}
