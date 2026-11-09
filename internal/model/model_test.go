package model

import (
	"strings"
	"testing"
)

func TestPasswordHashAndLegacy(t *testing.T) {
	a, b := HashPassword("correct-horse"), HashPassword("correct-horse")
	if a == b || strings.Contains(a, "correct-horse") {
		t.Fatal("password not independently salted")
	}
	if !VerifyPassword(a, "correct-horse") || VerifyPassword(a, "wrong") {
		t.Fatal("password verification incorrect")
	}
	if !VerifyPassword(ShaHex("old"), "old") || VerifyPassword(ShaHex("old"), "wrong") {
		t.Fatal("legacy verification incorrect")
	}
	for _, bad := range []string{"", "anything", "pbkdf2-sha256$99999999999$00$00", "pbkdf2-sha256$210000$gg$gg"} {
		if VerifyPassword(bad, "x") {
			t.Fatalf("accepted malformed hash %q", bad)
		}
	}
}

func TestMaskNeverRevealsShortSecret(t *testing.T) {
	for _, k := range []string{"s", "sk-test", "123456789012345678"} {
		if Mask(k) == k {
			t.Fatalf("unmasked key %q", k)
		}
	}
	if Mask("") != "" {
		t.Fatal("empty mask")
	}
	if got := Mask("meoo_project_0123456789abcdef"); got != "meoo_project_0…cdef" {
		t.Fatalf("unexpected mask %q", got)
	}
}

func TestConfigValidation(t *testing.T) {
	valid := Config{TimeoutMs: 180000, DefaultMaxTokens: 800, MaxLogs: 2000, MaxUsageHours: 168, AdminUser: "admin", KeepAliveHours: 6}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	uncapped := valid
	uncapped.DefaultMaxTokens = 0
	if err := uncapped.Validate(); err != nil {
		t.Fatalf("upstream default output budget rejected: %v", err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.TimeoutMs = 0 }, func(c *Config) { c.DefaultMaxTokens = -1 },
		func(c *Config) { c.MaxLogs = -1 }, func(c *Config) { c.MaxUsageHours = 0 },
		func(c *Config) { c.CheckinHour = 24 }, func(c *Config) { c.KeepAliveHours = 999 },
	} {
		c := valid
		change(&c)
		if c.Validate() == nil {
			t.Fatalf("accepted invalid config %+v", c)
		}
	}
}
