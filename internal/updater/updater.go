// Package updater reconciles a managed prefix list with the current DNS
// answers for a set of AWS endpoints.
package updater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tapitalee/prefix_updater/internal/awsx"
	"github.com/tapitalee/prefix_updater/internal/config"
	"github.com/tapitalee/prefix_updater/internal/dnsx"
	"github.com/tapitalee/prefix_updater/internal/endpoints"
)

// maxDescriptionLen is the AWS limit for a prefix list entry description.
const maxDescriptionLen = 255

// Updater performs reconcile cycles.
type Updater struct {
	cfg     *config.Config
	aws     *awsx.Client
	log     *slog.Logger
	lookup  dnsx.Lookup
	tracker *dnsx.Tracker
	now     func() time.Time

	region        string
	hosts         []endpoints.Host
	registryAdded bool
}

// New builds an Updater. region is the region used to derive hostnames.
func New(cfg *config.Config, client *awsx.Client, region string, log *slog.Logger) *Updater {
	return &Updater{
		cfg:     cfg,
		aws:     client,
		log:     log,
		lookup:  dnsx.SystemLookup,
		tracker: dnsx.NewTracker(cfg.IPTTL),
		now:     time.Now,
		region:  region,
	}
}

// Result summarises one cycle.
type Result struct {
	Added    int
	Removed  int
	Desired  int
	Current  int
	Degraded bool
	Changed  bool
	Skipped  string
}

// RunOnce executes a single reconcile cycle.
func (u *Updater) RunOnce(ctx context.Context) (Result, error) {
	var res Result

	pl, err := u.aws.DescribePrefixList(ctx, u.cfg.PrefixListID)
	if err != nil {
		return res, err
	}
	if !pl.Stable() {
		res.Skipped = "prefix list is " + pl.State
		return res, nil
	}
	if pl.Failed() {
		return res, fmt.Errorf("prefix list %s is in state %s; manual intervention needed", pl.ID, pl.State)
	}

	hosts, err := u.resolveHostList(ctx)
	if err != nil {
		return res, err
	}

	dnsCtx, cancel := context.WithTimeout(ctx, u.cfg.DNSTimeout)
	defer cancel()
	results := dnsx.ResolveAll(dnsCtx, u.lookup, hosts)

	now := u.now()
	var failures []string
	for _, r := range results {
		if r.Err != nil {
			failures = append(failures, r.Host.Name)
			u.log.Warn("dns lookup failed", "host", r.Host.Name, "service", r.Host.Service, "error", r.Err)
			continue
		}
		u.tracker.Observe(r.Host.Service, r.Addrs, now)
		u.log.Debug("resolved", "host", r.Host.Name, "service", r.Host.Service, "addrs", len(r.Addrs))
	}
	// Losing every lookup means we know nothing new; never act on that.
	if len(failures) == len(hosts) {
		return res, fmt.Errorf("all %d dns lookups failed (%s)", len(hosts), strings.Join(failures, ", "))
	}
	res.Degraded = len(failures) > 0

	desired := desiredEntries(u.tracker.Live(now), pl.AddressFamily, u.cfg.DescriptionPrefix)
	res.Desired = len(desired)
	if len(desired) == 0 {
		res.Skipped = "no addresses resolved for this address family"
		return res, nil
	}

	current, err := u.aws.Entries(ctx, pl.ID)
	if err != nil {
		return res, err
	}
	res.Current = len(current)

	adds, removes := diff(current, desired, u.cfg.DescriptionPrefix, u.cfg.ManageAll)

	// A degraded round has incomplete information, so additions are safe but
	// removals are not.
	if res.Degraded && len(removes) > 0 {
		u.log.Warn("skipping removals because some lookups failed",
			"failed_hosts", strings.Join(failures, ","), "would_remove", len(removes))
		removes = nil
	}
	// Refuse to empty the list; that is never the intended outcome.
	if len(removes) > 0 && len(current)-len(removes) == 0 && len(adds) == 0 {
		return res, fmt.Errorf("refusing to remove all %d entries from %s", len(current), pl.ID)
	}

	if len(adds) == 0 && len(removes) == 0 {
		u.log.Debug("no change", "entries", len(current), "desired", len(desired))
		return res, nil
	}

	if final := len(current) + len(adds) - len(removes); int32(final) > pl.MaxEntries {
		return res, fmt.Errorf("prefix list %s needs %d entries but MaxEntries is %d; raise MaxEntries or reduce --services/--ip-ttl",
			pl.ID, final, pl.MaxEntries)
	}

	res.Added, res.Removed, res.Changed = len(adds), len(removes), true

	if u.cfg.DryRun {
		u.log.Info("dry run: would update prefix list",
			"prefix_list_id", pl.ID, "add", cidrs(adds), "remove", cidrs(removes))
		return res, nil
	}

	if err := u.apply(ctx, pl, adds, removes); err != nil {
		return res, err
	}
	return res, nil
}

// apply writes the changes in batches, waiting for the list to settle between
// calls because EC2 rejects concurrent modifications.
func (u *Updater) apply(ctx context.Context, pl awsx.PrefixList, adds, removes []awsx.Entry) error {
	version := pl.Version
	batches := batch(adds, removes, u.cfg.MaxChangesPerCall)

	for i, b := range batches {
		if i > 0 {
			settled, err := u.aws.WaitStable(ctx, pl.ID, 2*time.Second, time.Minute)
			if err != nil {
				return err
			}
			version = settled.Version
		}
		newVersion, err := u.aws.Modify(ctx, pl.ID, version, b.adds, b.removes)
		if err != nil {
			return err
		}
		u.log.Info("updated prefix list",
			"prefix_list_id", pl.ID,
			"batch", fmt.Sprintf("%d/%d", i+1, len(batches)),
			"added", cidrs(b.adds),
			"removed", cidrs(b.removes),
			"version", newVersion)
		version = newVersion
	}
	return nil
}

// resolveHostList builds the host list once and, when enabled, appends the
// account specific ECR registry hostname.
func (u *Updater) resolveHostList(ctx context.Context) ([]endpoints.Host, error) {
	if u.hosts == nil {
		hosts, err := endpoints.Hosts(u.cfg.Services, u.cfg.ExtraHosts, u.region)
		if err != nil {
			return nil, err
		}
		u.hosts = hosts
		u.log.Info("resolving endpoints", "region", u.region, "hosts", hostNames(hosts))
	}

	if u.cfg.IncludeRegistryHost && !u.registryAdded && u.cfg.HasService("dkr.ecr") {
		account, err := u.aws.AccountID(ctx)
		if err != nil {
			// Non fatal: dkr.ecr.<region> is still resolved.
			u.log.Warn("could not determine account ID for the ECR registry host", "error", err)
		} else {
			name := endpoints.RegistryHostname(account, u.region)
			u.hosts = append(u.hosts, endpoints.Host{Service: "dkr.ecr", Name: name})
			u.registryAdded = true
			u.log.Info("added ECR registry host", "host", name)
		}
	}
	return u.hosts, nil
}

// desiredEntries turns tracked addresses into prefix list entries for the
// prefix list's address family.
func desiredEntries(tracked []dnsx.TrackedAddr, family awsx.AddressFamily, descPrefix string) []awsx.Entry {
	entries := make([]awsx.Entry, 0, len(tracked))
	for _, t := range tracked {
		if !matchesFamily(t.Addr, family) {
			continue
		}
		prefix := netip.PrefixFrom(t.Addr, t.Addr.BitLen())
		entries = append(entries, awsx.Entry{
			CIDR:        prefix.String(),
			Description: description(descPrefix, t.Services),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CIDR < entries[j].CIDR })
	return entries
}

func matchesFamily(a netip.Addr, family awsx.AddressFamily) bool {
	switch family {
	case awsx.FamilyIPv4:
		return a.Is4()
	case awsx.FamilyIPv6:
		return a.Is6() && !a.Is4In6()
	default:
		return false
	}
}

func description(prefix string, services []string) string {
	parts := strings.Join(services, ",")
	desc := strings.TrimSpace(prefix + " " + parts)
	if len(desc) > maxDescriptionLen {
		desc = desc[:maxDescriptionLen]
	}
	return desc
}

// owns reports whether an entry was created by this program.
func owns(desc, descPrefix string, manageAll bool) bool {
	if manageAll {
		return true
	}
	return descPrefix != "" && strings.HasPrefix(desc, descPrefix)
}

// diff computes the entries to add and remove. Existing CIDRs are never
// re-added, so descriptions of pre-existing entries are left untouched.
func diff(current, desired []awsx.Entry, descPrefix string, manageAll bool) (adds, removes []awsx.Entry) {
	currentByCIDR := make(map[string]awsx.Entry, len(current))
	for _, e := range current {
		currentByCIDR[e.CIDR] = e
	}
	desiredByCIDR := make(map[string]awsx.Entry, len(desired))
	for _, e := range desired {
		desiredByCIDR[e.CIDR] = e
	}

	for _, e := range desired {
		if _, exists := currentByCIDR[e.CIDR]; !exists {
			adds = append(adds, e)
		}
	}
	for _, e := range current {
		if _, wanted := desiredByCIDR[e.CIDR]; wanted {
			continue
		}
		if owns(e.Description, descPrefix, manageAll) {
			removes = append(removes, e)
		}
	}

	sort.Slice(adds, func(i, j int) bool { return adds[i].CIDR < adds[j].CIDR })
	sort.Slice(removes, func(i, j int) bool { return removes[i].CIDR < removes[j].CIDR })
	return adds, removes
}

type change struct {
	adds    []awsx.Entry
	removes []awsx.Entry
}

// batch splits the changes so no single API call exceeds size entries.
// Removals go first so that freed capacity is available to the additions.
func batch(adds, removes []awsx.Entry, size int) []change {
	if size <= 0 {
		size = 50
	}
	var out []change
	cur := change{}
	n := 0
	flush := func() {
		if n > 0 {
			out = append(out, cur)
			cur, n = change{}, 0
		}
	}
	for _, e := range removes {
		cur.removes = append(cur.removes, e)
		if n++; n == size {
			flush()
		}
	}
	for _, e := range adds {
		cur.adds = append(cur.adds, e)
		if n++; n == size {
			flush()
		}
	}
	flush()
	return out
}

func cidrs(entries []awsx.Entry) string {
	if len(entries) == 0 {
		return ""
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.CIDR)
	}
	return strings.Join(out, ",")
}

func hostNames(hosts []endpoints.Host) string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Name)
	}
	return strings.Join(out, ",")
}

// Run loops until the context is cancelled, recovering from panics so a
// transient problem cannot stop the process.
func (u *Updater) Run(ctx context.Context) error {
	ticker := time.NewTicker(u.cfg.Interval)
	defer ticker.Stop()

	consecutiveFailures := 0
	for {
		start := time.Now()
		res, err := u.runOnceSafe(ctx)
		switch {
		case err != nil && errors.Is(err, context.Canceled):
			return nil
		case err != nil:
			consecutiveFailures++
			u.log.Error("cycle failed; continuing",
				"error", err, "consecutive_failures", consecutiveFailures, "next_attempt_in", u.cfg.Interval)
		default:
			if consecutiveFailures > 0 {
				u.log.Info("recovered after failures", "consecutive_failures", consecutiveFailures)
			}
			consecutiveFailures = 0
			attrs := []any{
				"duration", time.Since(start).Round(time.Millisecond),
				"desired", res.Desired,
				"current", res.Current,
				"degraded", res.Degraded,
			}
			if res.Skipped != "" {
				u.log.Info("cycle skipped", append(attrs, "reason", res.Skipped)...)
			} else if res.Changed {
				u.log.Info("cycle applied changes", append(attrs, "added", res.Added, "removed", res.Removed)...)
			} else {
				u.log.Debug("cycle complete, no changes", attrs...)
			}
		}

		if u.cfg.Once {
			if err != nil {
				return err
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// runOnceSafe converts panics into errors so the loop survives them.
func (u *Updater) runOnceSafe(ctx context.Context) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic during cycle: %v", r)
			u.log.Error("recovered from panic", "panic", fmt.Sprint(r), "stack", stack())
		}
	}()

	cycleCtx, cancel := context.WithTimeout(ctx, u.cfg.AWSTimeout)
	defer cancel()
	return u.RunOnce(cycleCtx)
}

// stack returns the current goroutine stack, for panic diagnostics.
func stack() string {
	buf := make([]byte, 8192)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}
