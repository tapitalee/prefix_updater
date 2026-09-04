package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/tapitalee/prefix_updater/internal/awsx"
	"github.com/tapitalee/prefix_updater/internal/config"
)

// fakeEC2 is an in-memory managed prefix list.
type fakeEC2 struct {
	state       types.PrefixListState
	family      string
	maxEntries  int32
	version     int64
	entries     []awsx.Entry
	describeErr error
	modifyErr   error
	modifyCalls []*ec2.ModifyManagedPrefixListInput
}

func newFakeEC2(entries ...awsx.Entry) *fakeEC2 {
	return &fakeEC2{
		state:      types.PrefixListStateCreateComplete,
		family:     "IPv4",
		maxEntries: 60,
		version:    1,
		entries:    entries,
	}
}

func (f *fakeEC2) DescribeManagedPrefixLists(_ context.Context, in *ec2.DescribeManagedPrefixListsInput, _ ...func(*ec2.Options)) (*ec2.DescribeManagedPrefixListsOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return &ec2.DescribeManagedPrefixListsOutput{
		PrefixLists: []types.ManagedPrefixList{{
			PrefixListId:   aws.String(in.PrefixListIds[0]),
			PrefixListName: aws.String("fargate-endpoints"),
			State:          f.state,
			AddressFamily:  aws.String(f.family),
			MaxEntries:     aws.Int32(f.maxEntries),
			Version:        aws.Int64(f.version),
		}},
	}, nil
}

func (f *fakeEC2) GetManagedPrefixListEntries(_ context.Context, _ *ec2.GetManagedPrefixListEntriesInput, _ ...func(*ec2.Options)) (*ec2.GetManagedPrefixListEntriesOutput, error) {
	out := &ec2.GetManagedPrefixListEntriesOutput{}
	for _, e := range f.entries {
		out.Entries = append(out.Entries, types.PrefixListEntry{
			Cidr:        aws.String(e.CIDR),
			Description: aws.String(e.Description),
		})
	}
	return out, nil
}

func (f *fakeEC2) ModifyManagedPrefixList(_ context.Context, in *ec2.ModifyManagedPrefixListInput, _ ...func(*ec2.Options)) (*ec2.ModifyManagedPrefixListOutput, error) {
	if f.modifyErr != nil {
		return nil, f.modifyErr
	}
	if got := aws.ToInt64(in.CurrentVersion); got != f.version {
		return nil, fmt.Errorf("IncorrectState: current version is %d, got %d", f.version, got)
	}
	f.modifyCalls = append(f.modifyCalls, in)

	kept := make([]awsx.Entry, 0, len(f.entries))
	for _, e := range f.entries {
		removed := false
		for _, r := range in.RemoveEntries {
			if aws.ToString(r.Cidr) == e.CIDR {
				removed = true
				break
			}
		}
		if !removed {
			kept = append(kept, e)
		}
	}
	for _, a := range in.AddEntries {
		kept = append(kept, awsx.Entry{CIDR: aws.ToString(a.Cidr), Description: aws.ToString(a.Description)})
	}
	f.entries = kept
	f.version++

	return &ec2.ModifyManagedPrefixListOutput{
		PrefixList: &types.ManagedPrefixList{Version: aws.Int64(f.version)},
	}, nil
}

func (f *fakeEC2) cidrs() []string {
	out := make([]string, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.CIDR)
	}
	sort.Strings(out)
	return out
}

type fakeSTS struct {
	account string
	err     error
}

func (f fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.account)}, nil
}

// staticLookup returns a fixed answer for every host.
func staticLookup(byHost map[string][]string) Lookup {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ips, ok := byHost[host]
		if !ok {
			return nil, fmt.Errorf("no fixture for %s", host)
		}
		out := make([]netip.Addr, 0, len(ips))
		for _, s := range ips {
			addr, err := netip.ParseAddr(s)
			if err != nil {
				return nil, err
			}
			out = append(out, addr)
		}
		return out, nil
	}
}

// Lookup mirrors dnsx.Lookup so the fixtures stay readable.
type Lookup = func(ctx context.Context, host string) ([]netip.Addr, error)

const (
	apiECR   = "api.ecr.us-west-2.amazonaws.com"
	dkrECR   = "dkr.ecr.us-west-2.amazonaws.com"
	registry = "123456789012.dkr.ecr.us-west-2.amazonaws.com"
	secrets  = "secretsmanager.us-west-2.amazonaws.com"
	logsHost = "logs.us-west-2.amazonaws.com"
)

func fixture() map[string][]string {
	return map[string][]string{
		apiECR:   {"10.0.0.1"},
		dkrECR:   {"10.0.0.2"},
		registry: {"10.0.0.2"},
		secrets:  {"10.0.0.3"},
		logsHost: {"10.0.0.4"},
	}
}

func testConfig(t *testing.T, args ...string) *config.Config {
	t.Helper()
	for _, key := range []string{
		"PREFIX_LIST_ID", "REGION", "INTERVAL", "IP_TTL", "SERVICES", "EXTRA_HOSTS",
		"REGISTRY_HOST", "DESCRIPTION_PREFIX", "MANAGE_ALL", "MAX_CHANGES_PER_CALL",
		"DRY_RUN", "ONCE", "LOG_LEVEL", "LOG_FORMAT",
	} {
		t.Setenv(key, "")
	}
	cfg, err := config.Load(append([]string{"pl-test"}, args...), io.Discard)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func newTestUpdater(t *testing.T, ec2api *fakeEC2, lookup Lookup, args ...string) *Updater {
	t.Helper()
	cfg := testConfig(t, args...)
	client := &awsx.Client{EC2: ec2api, STS: fakeSTS{account: "123456789012"}}
	u := New(cfg, client, "us-west-2", slog.New(slog.NewTextHandler(io.Discard, nil)))
	u.lookup = lookup
	return u
}

func TestRunOnceAddsAllResolvedAddresses(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, staticLookup(fixture()))

	res, err := u.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Added != 4 || res.Removed != 0 || !res.Changed {
		t.Fatalf("unexpected result: %+v", res)
	}
	want := []string{"10.0.0.1/32", "10.0.0.2/32", "10.0.0.3/32", "10.0.0.4/32"}
	if got := ec2api.cidrs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", got, want)
	}

	// The shared address is attributed to every service that resolved to it.
	for _, e := range ec2api.entries {
		if e.CIDR != "10.0.0.2/32" {
			continue
		}
		if e.Description != "prefix_updater dkr.ecr" {
			t.Fatalf("description = %q", e.Description)
		}
	}
}

func TestRunOnceIsIdempotent(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, staticLookup(fixture()))

	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	res, err := u.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if res.Changed {
		t.Fatalf("second cycle should not change anything: %+v", res)
	}
	if len(ec2api.modifyCalls) != 1 {
		t.Fatalf("got %d modify calls, want 1", len(ec2api.modifyCalls))
	}
}

func TestRunOnceRemovesExpiredOwnedEntriesOnly(t *testing.T) {
	ec2api := newFakeEC2(
		awsx.Entry{CIDR: "192.168.0.1/32", Description: "office VPN"},
		awsx.Entry{CIDR: "10.9.9.9/32", Description: "prefix_updater logs"},
	)
	u := newTestUpdater(t, ec2api, staticLookup(fixture()), "--ip-ttl", "1m")

	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// The stale owned entry is gone, the foreign entry stays.
	got := strings.Join(ec2api.cidrs(), ",")
	want := "10.0.0.1/32,10.0.0.2/32,10.0.0.3/32,10.0.0.4/32,192.168.0.1/32"
	if got != want {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestRunOnceExpiresAddressesAfterTTL(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, staticLookup(fixture()), "--ip-ttl", "1m")

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	u.now = func() time.Time { return now }
	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}

	// DNS starts answering with a different address for logs.
	moved := fixture()
	moved[logsHost] = []string{"10.0.0.5"}
	u.lookup = staticLookup(moved)

	// Within the TTL both addresses are kept.
	now = now.Add(30 * time.Second)
	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if got, want := strings.Join(ec2api.cidrs(), ","), "10.0.0.1/32,10.0.0.2/32,10.0.0.3/32,10.0.0.4/32,10.0.0.5/32"; got != want {
		t.Fatalf("entries = %v, want %v", got, want)
	}

	// Past the TTL the old address is dropped.
	now = now.Add(2 * time.Minute)
	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if got, want := strings.Join(ec2api.cidrs(), ","), "10.0.0.1/32,10.0.0.2/32,10.0.0.3/32,10.0.0.5/32"; got != want {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestRunOnceSkipsRemovalsWhenDegraded(t *testing.T) {
	ec2api := newFakeEC2(awsx.Entry{CIDR: "10.9.9.9/32", Description: "prefix_updater logs"})
	partial := fixture()
	delete(partial, logsHost) // this lookup now fails
	u := newTestUpdater(t, ec2api, staticLookup(partial), "--ip-ttl", "1m")

	res, err := u.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !res.Degraded {
		t.Fatal("result should be marked degraded")
	}
	if res.Removed != 0 {
		t.Fatalf("removed %d entries while degraded", res.Removed)
	}
	if got, want := strings.Join(ec2api.cidrs(), ","), "10.0.0.1/32,10.0.0.2/32,10.0.0.3/32,10.9.9.9/32"; got != want {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestRunOnceFailsWhenAllLookupsFail(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, staticLookup(nil))

	if _, err := u.RunOnce(context.Background()); err == nil {
		t.Fatal("expected an error when every lookup fails")
	}
	if len(ec2api.modifyCalls) != 0 {
		t.Fatalf("prefix list was modified despite total DNS failure")
	}
}

func TestRunOnceSkipsWhileModificationInProgress(t *testing.T) {
	ec2api := newFakeEC2()
	ec2api.state = types.PrefixListStateModifyInProgress
	u := newTestUpdater(t, ec2api, staticLookup(fixture()))

	res, err := u.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Skipped == "" {
		t.Fatal("cycle should have been skipped")
	}
	if len(ec2api.modifyCalls) != 0 {
		t.Fatal("prefix list must not be modified while a change is in progress")
	}
}

func TestRunOnceRejectsExceedingMaxEntries(t *testing.T) {
	ec2api := newFakeEC2()
	ec2api.maxEntries = 2
	u := newTestUpdater(t, ec2api, staticLookup(fixture()))

	err := errorFrom(u.RunOnce(context.Background()))
	if err == nil || !strings.Contains(err.Error(), "MaxEntries") {
		t.Fatalf("err = %v, want a MaxEntries error", err)
	}
	if len(ec2api.modifyCalls) != 0 {
		t.Fatal("nothing should be modified when the change does not fit")
	}
}

func TestRunOnceBatchesLargeChanges(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, staticLookup(fixture()), "--max-changes-per-call", "2")

	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(ec2api.modifyCalls) != 2 {
		t.Fatalf("got %d modify calls, want 2", len(ec2api.modifyCalls))
	}
	if len(ec2api.entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(ec2api.entries))
	}
}

func TestRunOnceDryRunDoesNotModify(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, staticLookup(fixture()), "--dry-run")

	res, err := u.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Added != 4 || !res.Changed {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(ec2api.modifyCalls) != 0 {
		t.Fatal("dry run must not call ModifyManagedPrefixList")
	}
}

func TestRunOnceIPv6PrefixList(t *testing.T) {
	ec2api := newFakeEC2()
	ec2api.family = "IPv6"
	hosts := fixture()
	hosts[logsHost] = []string{"10.0.0.4", "2600:1f14::1"}
	u := newTestUpdater(t, ec2api, staticLookup(hosts))

	if _, err := u.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got, want := strings.Join(ec2api.cidrs(), ","), "2600:1f14::1/128"; got != want {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

func TestRunOnceReturnsAWSErrors(t *testing.T) {
	ec2api := newFakeEC2()
	ec2api.describeErr = errors.New("throttled")
	u := newTestUpdater(t, ec2api, staticLookup(fixture()))

	if _, err := u.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the describe error to surface")
	}
}

func TestRunKeepsGoingAfterAPanic(t *testing.T) {
	ec2api := newFakeEC2()
	u := newTestUpdater(t, ec2api, func(context.Context, string) ([]netip.Addr, error) {
		panic("kaboom")
	}, "--interval", "1ms")

	// runOnceSafe converts the panic into an error instead of crashing.
	if _, err := u.runOnceSafe(context.Background()); err == nil {
		t.Fatal("expected the panic to be reported as an error")
	}

	// The loop keeps running: a later cycle succeeds and applies the changes.
	u.lookup = staticLookup(fixture())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	u.cfg.Once = true
	if err := u.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ec2api.modifyCalls) != 1 {
		t.Fatalf("got %d modify calls, want 1", len(ec2api.modifyCalls))
	}
}

func TestRegistryHostFailureIsNotFatal(t *testing.T) {
	ec2api := newFakeEC2()
	cfg := testConfig(t)
	client := &awsx.Client{EC2: ec2api, STS: fakeSTS{err: errors.New("no sts")}}
	u := New(cfg, client, "us-west-2", slog.New(slog.NewTextHandler(io.Discard, nil)))

	hosts := fixture()
	delete(hosts, registry)
	u.lookup = staticLookup(hosts)

	res, err := u.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Added != 4 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestDiff(t *testing.T) {
	current := []awsx.Entry{
		{CIDR: "10.0.0.1/32", Description: "prefix_updater logs"},
		{CIDR: "10.0.0.2/32", Description: "prefix_updater logs"},
		{CIDR: "192.168.0.1/32", Description: "office"},
	}
	desired := []awsx.Entry{
		{CIDR: "10.0.0.2/32", Description: "prefix_updater logs"},
		{CIDR: "10.0.0.3/32", Description: "prefix_updater logs"},
	}

	adds, removes := diff(current, desired, "prefix_updater", false)
	if len(adds) != 1 || adds[0].CIDR != "10.0.0.3/32" {
		t.Fatalf("adds = %+v", adds)
	}
	if len(removes) != 1 || removes[0].CIDR != "10.0.0.1/32" {
		t.Fatalf("removes = %+v", removes)
	}

	adds, removes = diff(current, desired, "prefix_updater", true)
	if len(adds) != 1 {
		t.Fatalf("adds = %+v", adds)
	}
	if len(removes) != 2 {
		t.Fatalf("manage-all should also remove foreign entries: %+v", removes)
	}
}

func TestBatch(t *testing.T) {
	adds := []awsx.Entry{{CIDR: "a"}, {CIDR: "b"}, {CIDR: "c"}}
	removes := []awsx.Entry{{CIDR: "x"}, {CIDR: "y"}}

	batches := batch(adds, removes, 2)
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3: %+v", len(batches), batches)
	}
	// Removals come first so that capacity is freed before adding.
	if len(batches[0].removes) != 2 || len(batches[0].adds) != 0 {
		t.Fatalf("first batch = %+v", batches[0])
	}
	total := 0
	for _, b := range batches {
		if n := len(b.adds) + len(b.removes); n > 2 {
			t.Fatalf("batch too large: %+v", b)
		}
		total += len(b.adds) + len(b.removes)
	}
	if total != 5 {
		t.Fatalf("batched %d changes, want 5", total)
	}

	if batches := batch(nil, nil, 2); len(batches) != 0 {
		t.Fatalf("expected no batches, got %+v", batches)
	}
}

func TestDescriptionIsTruncated(t *testing.T) {
	services := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		services = append(services, fmt.Sprintf("service-%d", i))
	}
	if got := len(description("prefix_updater", services)); got != maxDescriptionLen {
		t.Fatalf("description length = %d, want %d", got, maxDescriptionLen)
	}
}

func errorFrom(_ Result, err error) error { return err }
