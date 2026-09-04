package config

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// clearEnv makes a test independent of the developer's shell environment.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"PREFIX_LIST_ID", "REGION", "INTERVAL", "IP_TTL", "DNS_TIMEOUT", "AWS_TIMEOUT",
		"SERVICES", "EXTRA_HOSTS", "REGISTRY_HOST", "DESCRIPTION_PREFIX", "MANAGE_ALL",
		"MAX_CHANGES_PER_CALL", "DRY_RUN", "ONCE", "LOG_LEVEL", "LOG_FORMAT",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadPositionalArg(t *testing.T) {
	clearEnv(t)

	cfg, err := Load([]string{"pl-0123456789abcdef0"}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrefixListID != "pl-0123456789abcdef0" {
		t.Errorf("PrefixListID = %q", cfg.PrefixListID)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %s, want 30s", cfg.Interval)
	}
	if cfg.IPTTL != time.Hour {
		t.Errorf("IPTTL = %s, want 1h", cfg.IPTTL)
	}
	if len(cfg.Services) != 4 {
		t.Errorf("Services = %v, want the 4 Fargate defaults", cfg.Services)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("PREFIX_LIST_ID", "pl-abc")
	t.Setenv("INTERVAL", "45")
	t.Setenv("IP_TTL", "2h")
	t.Setenv("SERVICES", "logs, api.ecr ,")
	t.Setenv("EXTRA_HOSTS", "example.com")
	t.Setenv("DRY_RUN", "true")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load(nil, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrefixListID != "pl-abc" {
		t.Errorf("PrefixListID = %q", cfg.PrefixListID)
	}
	if cfg.Interval != 45*time.Second {
		t.Errorf("Interval = %s, want 45s (bare numbers mean seconds)", cfg.Interval)
	}
	if cfg.IPTTL != 2*time.Hour {
		t.Errorf("IPTTL = %s", cfg.IPTTL)
	}
	if len(cfg.Services) != 2 || cfg.Services[0] != "logs" || cfg.Services[1] != "api.ecr" {
		t.Errorf("Services = %v", cfg.Services)
	}
	if len(cfg.ExtraHosts) != 1 || cfg.ExtraHosts[0] != "example.com" {
		t.Errorf("ExtraHosts = %v", cfg.ExtraHosts)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true")
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
}

func TestFlagsBeatEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("PREFIX_LIST_ID", "pl-fromenv")
	t.Setenv("INTERVAL", "5m")

	cfg, err := Load([]string{"--prefix-list-id", "pl-fromflag", "--interval", "10s"}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrefixListID != "pl-fromflag" {
		t.Errorf("PrefixListID = %q", cfg.PrefixListID)
	}
	if cfg.Interval != 10*time.Second {
		t.Errorf("Interval = %s", cfg.Interval)
	}
}

func TestFlagsAfterPositionalArg(t *testing.T) {
	clearEnv(t)

	cfg, err := Load([]string{"pl-abc", "--dry-run", "--interval", "5s"}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrefixListID != "pl-abc" {
		t.Errorf("PrefixListID = %q", cfg.PrefixListID)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true")
	}
	if cfg.Interval != 5*time.Second {
		t.Errorf("Interval = %s", cfg.Interval)
	}
}

func TestLoadErrors(t *testing.T) {
	clearEnv(t)

	cases := []struct {
		name string
		args []string
	}{
		{"missing id", nil},
		{"bad id", []string{"vpc-123"}},
		{"id twice", []string{"--prefix-list-id", "pl-a", "pl-b"}},
		{"extra args", []string{"pl-a", "pl-b", "pl-c"}},
		{"bad interval", []string{"pl-a", "--interval", "0s"}},
		{"negative ttl", []string{"pl-a", "--ip-ttl", "-1s"}},
		{"unknown service", []string{"pl-a", "--services", "nope"}},
		{"bad log level", []string{"pl-a", "--log-level", "loud"}},
		{"bad log format", []string{"pl-a", "--log-format", "yaml"}},
		{"empty services", []string{"pl-a", "--services", "", "--extra-hosts", ""}},
		{"bad max changes", []string{"pl-a", "--max-changes-per-call", "0"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearEnv(t)
			if _, err := Load(c.args, io.Discard); err == nil {
				t.Fatalf("Load(%v) should have failed", c.args)
			}
		})
	}
}

func TestVersionRequested(t *testing.T) {
	if _, err := Load([]string{"--version"}, io.Discard); err != ErrVersionRequested {
		t.Fatalf("err = %v, want ErrVersionRequested", err)
	}
}

func TestHasService(t *testing.T) {
	cfg, err := Load([]string{"pl-a"}, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HasService("dkr.ecr") {
		t.Error("dkr.ecr should be enabled by default")
	}
	if cfg.HasService("s3") {
		t.Error("s3 should not be enabled by default")
	}
}
