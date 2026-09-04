// Package config parses prefix_updater configuration from command line flags
// and environment variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tapitalee/prefix_updater/internal/endpoints"
)

// Config holds the runtime configuration.
type Config struct {
	// PrefixListID is the ID of the managed prefix list to keep in sync.
	PrefixListID string
	// Region overrides the AWS region used for endpoint hostnames.
	Region string
	// Interval is the delay between reconcile cycles.
	Interval time.Duration
	// IPTTL is how long an IP stays in the prefix list after it was last
	// returned by DNS. AWS endpoints return a rotating subset of a larger
	// address pool, so a TTL longer than the interval keeps the list stable.
	// Zero means "only keep addresses from the most recent lookup".
	IPTTL time.Duration
	// DNSTimeout bounds a single DNS resolution round.
	DNSTimeout time.Duration
	// AWSTimeout bounds the AWS calls of a single cycle.
	AWSTimeout time.Duration
	// Services are the logical AWS service keys to resolve.
	Services []string
	// ExtraHosts are additional hostnames to resolve verbatim.
	ExtraHosts []string
	// IncludeRegistryHost adds <account>.dkr.ecr.<region>.<suffix> when the
	// dkr.ecr service is enabled.
	IncludeRegistryHost bool
	// DescriptionPrefix marks the prefix list entries owned by this program.
	DescriptionPrefix string
	// ManageAll lets the updater remove entries it does not own.
	ManageAll bool
	// MaxChangesPerCall caps entries per ModifyManagedPrefixList request.
	MaxChangesPerCall int
	// DryRun logs the changes instead of applying them.
	DryRun bool
	// Once runs a single cycle and exits.
	Once bool
	// LogLevel and LogFormat configure the slog handler.
	LogLevel  slog.Level
	LogFormat string
}

// ErrVersionRequested is returned when --version was passed.
var ErrVersionRequested = errors.New("version requested")

// Load builds a Config from args (without the program name) and the process
// environment. Flags win over environment variables.
func Load(args []string, out io.Writer) (*Config, error) {
	cfg := &Config{}

	fs := flag.NewFlagSet("prefix_updater", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintf(out, "prefix_updater keeps an AWS managed prefix list in sync with the\n")
		fmt.Fprintf(out, "resolved IPs of the AWS endpoints needed to boot ECS Fargate tasks.\n\n")
		fmt.Fprintf(out, "Usage:\n  prefix_updater [flags] <pl-xxxxxxxx>\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(out, "\nEvery flag has an environment variable equivalent, e.g. PREFIX_LIST_ID,\n")
		fmt.Fprintf(out, "INTERVAL, IP_TTL, SERVICES, EXTRA_HOSTS, DRY_RUN, LOG_LEVEL.\n")
		fmt.Fprintf(out, "\nKnown services: %s\n", strings.Join(endpoints.KnownServices(), ", "))
	}

	var (
		showVersion bool
		services    string
		extraHosts  string
		logLevel    string
	)

	fs.StringVar(&cfg.PrefixListID, "prefix-list-id", env("PREFIX_LIST_ID", ""),
		"managed prefix list ID to update (env PREFIX_LIST_ID); may also be given as the first argument")
	fs.StringVar(&cfg.Region, "region", env("REGION", ""),
		"AWS region for endpoint hostnames (env REGION, defaults to the resolved SDK region)")
	fs.DurationVar(&cfg.Interval, "interval", envDuration("INTERVAL", 30*time.Second),
		"delay between reconcile cycles (env INTERVAL)")
	fs.DurationVar(&cfg.IPTTL, "ip-ttl", envDuration("IP_TTL", time.Hour),
		"how long an IP is kept after it was last seen in DNS; 0 keeps only the latest lookup (env IP_TTL)")
	fs.DurationVar(&cfg.DNSTimeout, "dns-timeout", envDuration("DNS_TIMEOUT", 10*time.Second),
		"timeout for one DNS resolution round (env DNS_TIMEOUT)")
	fs.DurationVar(&cfg.AWSTimeout, "aws-timeout", envDuration("AWS_TIMEOUT", 2*time.Minute),
		"timeout for the AWS calls of one cycle (env AWS_TIMEOUT)")
	fs.StringVar(&services, "services", env("SERVICES", strings.Join(endpoints.DefaultServices, ",")),
		"comma separated service keys to resolve (env SERVICES)")
	fs.StringVar(&extraHosts, "extra-hosts", env("EXTRA_HOSTS", ""),
		"comma separated extra hostnames to resolve (env EXTRA_HOSTS)")
	fs.BoolVar(&cfg.IncludeRegistryHost, "registry-host", envBool("REGISTRY_HOST", true),
		"also resolve <account>.dkr.ecr.<region> when dkr.ecr is enabled (env REGISTRY_HOST)")
	fs.StringVar(&cfg.DescriptionPrefix, "description-prefix", env("DESCRIPTION_PREFIX", "prefix_updater"),
		"description marker identifying entries owned by this program (env DESCRIPTION_PREFIX)")
	fs.BoolVar(&cfg.ManageAll, "manage-all", envBool("MANAGE_ALL", false),
		"allow removing prefix list entries that are not owned by this program (env MANAGE_ALL)")
	fs.IntVar(&cfg.MaxChangesPerCall, "max-changes-per-call", envInt("MAX_CHANGES_PER_CALL", 50),
		"maximum entry changes per ModifyManagedPrefixList call (env MAX_CHANGES_PER_CALL)")
	fs.BoolVar(&cfg.DryRun, "dry-run", envBool("DRY_RUN", false),
		"log the changes that would be made without applying them (env DRY_RUN)")
	fs.BoolVar(&cfg.Once, "once", envBool("ONCE", false),
		"run a single cycle and exit (env ONCE)")
	fs.StringVar(&logLevel, "log-level", env("LOG_LEVEL", "info"),
		"log level: debug, info, warn or error (env LOG_LEVEL)")
	fs.StringVar(&cfg.LogFormat, "log-format", env("LOG_FORMAT", "text"),
		"log format: text or json (env LOG_FORMAT)")
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if showVersion {
		return nil, ErrVersionRequested
	}

	// The stdlib flag package stops at the first operand, so keep parsing to
	// allow `prefix_updater pl-xxxxxxxx --dry-run` as well as flags first.
	var operands []string
	for rest := fs.Args(); len(rest) > 0; rest = fs.Args() {
		operands = append(operands, rest[0])
		if err := fs.Parse(rest[1:]); err != nil {
			return nil, err
		}
		if showVersion {
			return nil, ErrVersionRequested
		}
	}

	switch len(operands) {
	case 0:
	case 1:
		if cfg.PrefixListID != "" && cfg.PrefixListID != operands[0] {
			return nil, fmt.Errorf("prefix list ID given twice: %q and %q", cfg.PrefixListID, operands[0])
		}
		cfg.PrefixListID = operands[0]
	default:
		return nil, fmt.Errorf("unexpected extra arguments: %s", strings.Join(operands[1:], " "))
	}

	cfg.Services = splitList(services)
	cfg.ExtraHosts = splitList(extraHosts)

	level, err := parseLevel(logLevel)
	if err != nil {
		return nil, err
	}
	cfg.LogLevel = level

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.PrefixListID == "" {
		return errors.New("no prefix list ID: pass it as an argument, --prefix-list-id or PREFIX_LIST_ID")
	}
	if !strings.HasPrefix(c.PrefixListID, "pl-") {
		return fmt.Errorf("invalid prefix list ID %q: expected the pl-xxxxxxxx form", c.PrefixListID)
	}
	if c.Interval <= 0 {
		return fmt.Errorf("invalid interval %s: must be positive", c.Interval)
	}
	if c.IPTTL < 0 {
		return fmt.Errorf("invalid ip-ttl %s: must not be negative", c.IPTTL)
	}
	if c.DNSTimeout <= 0 {
		return fmt.Errorf("invalid dns-timeout %s: must be positive", c.DNSTimeout)
	}
	if c.AWSTimeout <= 0 {
		return fmt.Errorf("invalid aws-timeout %s: must be positive", c.AWSTimeout)
	}
	if c.MaxChangesPerCall <= 0 {
		return fmt.Errorf("invalid max-changes-per-call %d: must be positive", c.MaxChangesPerCall)
	}
	if len(c.Services) == 0 && len(c.ExtraHosts) == 0 {
		return errors.New("nothing to resolve: set --services or --extra-hosts")
	}
	for _, svc := range c.Services {
		if _, err := endpoints.Hostname(svc, "us-east-1"); err != nil {
			return err
		}
	}
	if c.DescriptionPrefix == "" && !c.ManageAll {
		return errors.New("empty --description-prefix requires --manage-all")
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log-format %q: want text or json", c.LogFormat)
	}
	return nil
}

// HasService reports whether the given service key is enabled.
func (c *Config) HasService(name string) bool {
	for _, svc := range c.Services {
		if svc == name {
			return true
		}
	}
	return false
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log-level %q: want debug, info, warn or error", s)
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	raw := env(key, "")
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	// Bare numbers are treated as seconds, which is the friendlier reading of
	// something like INTERVAL=30.
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func envBool(key string, def bool) bool {
	if b, err := strconv.ParseBool(env(key, "")); err == nil {
		return b
	}
	return def
}
