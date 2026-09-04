// Command prefix_updater keeps an AWS managed prefix list in sync with the
// current IP addresses of the AWS endpoints an ECS Fargate task needs in order
// to start: ECR (api and dkr), Secrets Manager and CloudWatch Logs.
//
// It resolves those endpoints every interval (30s by default) and only calls
// EC2 when the address set actually changed. Any error is logged and the loop
// keeps running, so a transient DNS or API problem never stops the process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/tapitalee/prefix_updater/internal/awsx"
	"github.com/tapitalee/prefix_updater/internal/config"
	"github.com/tapitalee/prefix_updater/internal/updater"
)

// Build information, overridden with -ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "prefix_updater: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args, os.Stderr)
	if err != nil {
		if errors.Is(err, config.ErrVersionRequested) {
			fmt.Printf("prefix_updater %s (commit %s, built %s)\n", versionString(), commit, date)
			return nil
		}
		return err
	}

	log := newLogger(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	region := cfg.Region
	if region == "" {
		region = awsCfg.Region
	}
	if region == "" {
		return errors.New("no AWS region: set --region, REGION or AWS_REGION")
	}
	// Keep the API calls in the same region as the endpoints being resolved.
	awsCfg.Region = region

	log.Info("starting prefix_updater",
		"version", versionString(),
		"commit", commit,
		"prefix_list_id", cfg.PrefixListID,
		"region", region,
		"interval", cfg.Interval,
		"ip_ttl", cfg.IPTTL,
		"services", cfg.Services,
		"extra_hosts", cfg.ExtraHosts,
		"dry_run", cfg.DryRun,
		"manage_all", cfg.ManageAll)

	u := updater.New(cfg, awsx.New(awsCfg), region, log)
	if err := u.Run(ctx); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// versionString prefers the linker-provided version and falls back to the
// version stamped into the binary by `go install`.
func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
