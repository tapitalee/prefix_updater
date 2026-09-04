package dnsx

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/tapitalee/prefix_updater/internal/endpoints"
)

func addrs(t *testing.T, in ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(in))
	for _, s := range in {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		out = append(out, a)
	}
	return out
}

func liveCIDRs(tracked []TrackedAddr) []string {
	out := make([]string, 0, len(tracked))
	for _, t := range tracked {
		out = append(out, t.Addr.String())
	}
	return out
}

func TestTrackerUnionWithinTTL(t *testing.T) {
	tr := NewTracker(time.Hour)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tr.Observe("logs", addrs(t, "10.0.0.1", "10.0.0.2"), t0)
	tr.Observe("logs", addrs(t, "10.0.0.2", "10.0.0.3"), t0.Add(30*time.Second))

	got := liveCIDRs(tr.Live(t0.Add(time.Minute)))
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTrackerExpiry(t *testing.T) {
	tr := NewTracker(time.Minute)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tr.Observe("logs", addrs(t, "10.0.0.1"), t0)
	tr.Observe("logs", addrs(t, "10.0.0.2"), t0.Add(50*time.Second))

	got := liveCIDRs(tr.Live(t0.Add(90 * time.Second)))
	if len(got) != 1 || got[0] != "10.0.0.2" {
		t.Fatalf("got %v, want [10.0.0.2]", got)
	}
	// Expired addresses are dropped from the tracker, not just filtered.
	if tr.Len() != 1 {
		t.Fatalf("tracker holds %d addresses, want 1", tr.Len())
	}
}

func TestTrackerZeroTTLKeepsOnlyLatest(t *testing.T) {
	tr := NewTracker(0)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tr.Observe("logs", addrs(t, "10.0.0.1"), t0)
	t1 := t0.Add(30 * time.Second)
	tr.Observe("logs", addrs(t, "10.0.0.2"), t1)

	got := liveCIDRs(tr.Live(t1))
	if len(got) != 1 || got[0] != "10.0.0.2" {
		t.Fatalf("got %v, want [10.0.0.2]", got)
	}
}

func TestTrackerRecordsAllServices(t *testing.T) {
	tr := NewTracker(time.Hour)
	now := time.Now()
	tr.Observe("logs", addrs(t, "10.0.0.1"), now)
	tr.Observe("api.ecr", addrs(t, "10.0.0.1"), now)

	live := tr.Live(now)
	if len(live) != 1 {
		t.Fatalf("got %d addresses, want 1", len(live))
	}
	if want := []string{"api.ecr", "logs"}; live[0].Services[0] != want[0] || live[0].Services[1] != want[1] {
		t.Fatalf("services = %v, want %v", live[0].Services, want)
	}
}

func TestResolveAllReportsPerHostErrors(t *testing.T) {
	hosts := []endpoints.Host{
		{Service: "logs", Name: "good"},
		{Service: "api.ecr", Name: "bad"},
		{Service: "dkr.ecr", Name: "empty"},
		{Service: "sts", Name: "panic"},
	}
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "good":
			return addrs(t, "10.0.0.1"), nil
		case "bad":
			return nil, errors.New("boom")
		case "empty":
			return nil, nil
		default:
			panic("resolver exploded")
		}
	}

	results := ResolveAll(context.Background(), lookup, hosts)
	if len(results) != len(hosts) {
		t.Fatalf("got %d results, want %d", len(results), len(hosts))
	}
	if results[0].Err != nil || len(results[0].Addrs) != 1 {
		t.Errorf("good host: %+v", results[0])
	}
	if results[1].Err == nil {
		t.Error("bad host should report an error")
	}
	if results[2].Err == nil {
		t.Error("an empty answer should be treated as an error")
	}
	if results[3].Err == nil {
		t.Error("a panicking resolver should be reported as an error")
	}
}
