package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/muse2api/muse2api/internal/engine"
)

func TestRenewAndPersistHealthMatrix(t *testing.T) {
	s, _ := newTestServer(t, "")
	const vmlExpiry = int64(1900012345)
	acc, _ := s.store.AddAccount(
		map[string]string{"hatch_sess": "old", "hatch_vml": "v"},
		"health",
		map[string]int64{"hatch_vml": vmlExpiry, "hatch_sess": 1900020000},
	)

	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		return &engine.SessionRenewResult{
			OK: true, Status: "assigned", VMID: "vm-1", VMState: "RUNNING",
			Cookies:    map[string]string{"hatch_sess": "renewed", "hatch_vml": "v"},
			CookiesExp: map[string]int64{"hatch_vml": vmlExpiry, "hatch_sess": 1900020000},
		}, nil
	}
	updated, err := s.renewAndPersist(context.Background(), acc.ID, true, true)
	if err != nil || updated == nil {
		t.Fatalf("renewAndPersist assigned: %v", err)
	}
	stored := s.store.GetAccount(acc.ID)
	if stored.Cookies["hatch_sess"] != "renewed" {
		t.Fatalf("cookies were not persisted: %+v", stored.Cookies)
	}
	if stored.OK == nil || !*stored.OK || stored.LastKeepalive == nil {
		t.Fatalf("assigned session should mark the account healthy: %+v", stored)
	}
	if stored.ExpiresAt == nil || *stored.ExpiresAt != vmlExpiry {
		t.Fatalf("real hatch_vml expiry must be preserved: %+v", stored.ExpiresAt)
	}
	if !strings.Contains(stored.Note, "Session healthy") {
		t.Fatalf("unexpected note: %q", stored.Note)
	}

	// Recent successful renewals must be reused without another HTTP round trip.
	var calls int32
	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		atomic.AddInt32(&calls, 1)
		return &engine.SessionRenewResult{OK: true, Cookies: cookies, CookiesExp: expires}, nil
	}
	if _, err := s.renewAndPersist(context.Background(), acc.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("recent renewal should have been reused, calls=%d", calls)
	}

	// 401 is the only failure that marks the account unhealthy.
	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		return nil, &engine.MuseAuthError{Message: "HTTP 401"}
	}
	if _, err := s.renewAndPersist(context.Background(), acc.ID, true, true); err == nil || !engine.IsAuthError(err) {
		t.Fatalf("expected auth error, got %v", err)
	}
	stored = s.store.GetAccount(acc.ID)
	if stored.OK == nil || *stored.OK {
		t.Fatalf("401 must mark the account unhealthy: %+v", stored.OK)
	}

	// Restore a healthy state, then verify ambiguous upstream failures preserve it.
	okTrue := true
	s.store.TouchKeepalive(acc.ID, &okTrue, "prior confirmed state")
	prior := s.store.GetAccount(acc.ID)
	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		return nil, &engine.MuseGenerationError{Message: "/api/session HTTP 403: access denied"}
	}
	if _, err := s.renewAndPersist(context.Background(), acc.ID, true, true); err != nil {
		t.Fatalf("ambiguous failure should be swallowed: %v", err)
	}
	stored = s.store.GetAccount(acc.ID)
	if stored.OK == nil || !*stored.OK {
		t.Fatalf("403 must preserve the prior healthy state: %+v", stored.OK)
	}
	if stored.Cookies["hatch_sess"] != prior.Cookies["hatch_sess"] {
		t.Fatalf("cookies changed on an ambiguous failure")
	}
	if stored.ExpiresAt == nil || prior.ExpiresAt == nil || *stored.ExpiresAt != *prior.ExpiresAt {
		t.Fatalf("expiry changed on an ambiguous failure")
	}
	if stored.LastKeepalive == nil || prior.LastKeepalive == nil || *stored.LastKeepalive != *prior.LastKeepalive {
		t.Fatalf("last_keepalive changed on an ambiguous failure")
	}
}

func TestProbeAccountHealthMatrix(t *testing.T) {
	s, _ := newTestServer(t, "")
	const vmlExpiry = int64(1900012345)
	acc, _ := s.store.AddAccount(
		map[string]string{"hatch_sess": "old", "hatch_vml": "v"},
		"probe",
		map[string]int64{"hatch_vml": vmlExpiry},
	)

	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		return &engine.SessionRenewResult{
			OK: true, Status: "assigned", VMID: "vm-1", VMState: "RUNNING", WakeOK: true,
			Cookies:    map[string]string{"hatch_sess": "new", "hatch_vml": "v"},
			CookiesExp: map[string]int64{"hatch_vml": vmlExpiry},
		}, nil
	}
	res := s.keepalive.probeAccountSync(context.Background(), acc.ID, false)
	if res["ok"] != true {
		t.Fatalf("expected a successful probe: %+v", res)
	}
	stored := s.store.GetAccount(acc.ID)
	if stored.OK == nil || !*stored.OK || stored.LastKeepalive == nil {
		t.Fatalf("probe did not mark the account healthy: %+v", stored)
	}
	if stored.ExpiresAt == nil || *stored.ExpiresAt != vmlExpiry {
		t.Fatalf("probe changed the real expiry: %+v", stored.ExpiresAt)
	}

	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		return nil, &engine.MuseAuthError{Message: "HTTP 401"}
	}
	res = s.keepalive.probeAccountSync(context.Background(), acc.ID, false)
	if res["ok"] != false {
		t.Fatalf("expected a failed probe: %+v", res)
	}
	stored = s.store.GetAccount(acc.ID)
	if stored.OK == nil || *stored.OK {
		t.Fatalf("401 probe must mark the account unhealthy")
	}

	okTrue := true
	s.store.TouchKeepalive(acc.ID, &okTrue, "prior")
	prior := s.store.GetAccount(acc.ID)
	s.renewSessionFn = func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
		return nil, &engine.MuseGenerationError{Message: "/api/session HTTP 503"}
	}
	res = s.keepalive.probeAccountSync(context.Background(), acc.ID, false)
	if res["ok"] != false {
		t.Fatalf("expected a failed probe: %+v", res)
	}
	stored = s.store.GetAccount(acc.ID)
	if stored.OK == nil || !*stored.OK {
		t.Fatalf("503 probe must preserve the prior healthy state")
	}
	if stored.Cookies["hatch_sess"] != prior.Cookies["hatch_sess"] {
		t.Fatalf("ambiguous probe changed cookies")
	}
	if stored.ExpiresAt == nil || prior.ExpiresAt == nil || *stored.ExpiresAt != *prior.ExpiresAt {
		t.Fatalf("ambiguous probe changed expiry")
	}
}
