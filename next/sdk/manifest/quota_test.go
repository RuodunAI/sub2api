package manifest

import (
	"net/http"
	"testing"
	"time"
)

// Anthropic's unified rate-limit headers, as sub2api samples them
// (backend/internal/service/ratelimit_service.go samplePassiveUsageFromHeaders):
// utilization is a 0-1 ratio, reset is Unix seconds.
func TestReadQuotaHeadersAnthropic(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	decls := []QuotaHeader{
		{Key: "5h", Utilization: "anthropic-ratelimit-unified-5h-utilization",
			Reset: "anthropic-ratelimit-unified-5h-reset", Status: "anthropic-ratelimit-unified-5h-status"},
		{Key: "7d", Utilization: "anthropic-ratelimit-unified-7d-utilization",
			Reset: "anthropic-ratelimit-unified-7d-reset", Status: "anthropic-ratelimit-unified-7d-status"},
		{Key: "7d_fable", Utilization: "anthropic-ratelimit-unified-7d_oi-utilization",
			Reset: "anthropic-ratelimit-unified-7d_oi-reset", Status: "anthropic-ratelimit-unified-7d_oi-status"},
	}
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1790003600")
	h.Set("anthropic-ratelimit-unified-5h-status", "Allowed_Warning")
	// Milliseconds are detected.
	h.Set("anthropic-ratelimit-unified-7d-reset", "1790500000000")
	h.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	got := ReadQuotaHeaders(decls, h.Get, now)
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2 (7d_fable has no headers): %+v", len(got), got)
	}
	w := got[0]
	if w.Key != "5h" || w.Utilization < 41.99 || w.Utilization > 42.01 || w.Status != QuotaAllowedWarning ||
		w.ResetsAt == nil || w.ResetsAt.Unix() != 1790003600 {
		t.Fatalf("5h = %+v", w)
	}
	w = got[1]
	if w.Key != "7d" || w.Utilization != 0 || w.Status != QuotaRejected || w.ResetsAt == nil || w.ResetsAt.Unix() != 1790500000 {
		t.Fatalf("7d = %+v", w)
	}
}

func TestReadQuotaHeadersFormats(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	decls := []QuotaHeader{
		{Key: "day", Utilization: "x-used", UtilizationUnit: QuotaUnitPercent, Reset: "x-reset", ResetFormat: QuotaResetRFC3339},
		{Key: "min", Reset: "x-retry", ResetFormat: QuotaResetDelta, Status: "x-state"},
		{Key: "bad", Utilization: "x-bad", Reset: "x-bad-reset"},
	}
	h := http.Header{}
	h.Set("x-used", "87.5%")
	h.Set("x-reset", "2026-10-06T00:00:00Z")
	h.Set("x-retry", "30")
	h.Set("x-state", "throttled")
	h.Set("x-bad", "-1")
	h.Set("x-bad-reset", "soon")
	got := ReadQuotaHeaders(decls, h.Get, now)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Utilization != 87.5 || !got[0].ResetsAt.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day = %+v", got[0])
	}
	// An unknown status value is recorded as unknown, but still counts as
	// the window being present.
	if got[1].Status != "" || !got[1].ResetsAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("min = %+v", got[1])
	}
}

// Codex's x-codex-{primary,secondary}-* headers: which slot is the 5-hour
// window is not fixed, so the window is named by the length header.
func TestReadQuotaHeadersWindowMinutes(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	slot := func(name string) QuotaHeader {
		p := "x-codex-" + name + "-"
		return QuotaHeader{WindowMinutes: p + "window-minutes", Utilization: p + "used-percent",
			UtilizationUnit: QuotaUnitPercent, Reset: p + "reset-at"}
	}
	decls := []QuotaHeader{slot("primary"), slot("secondary")}
	h := http.Header{}
	h.Set("x-codex-primary-window-minutes", "10080")
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-reset-at", "1790300000")
	h.Set("x-codex-secondary-window-minutes", "300")
	h.Set("x-codex-secondary-used-percent", "12.5")
	got := ReadQuotaHeaders(decls, h.Get, now)
	if len(got) != 2 || got[0].Key != "7d" || got[0].Utilization != 100 || got[0].ResetsAt.Unix() != 1790300000 ||
		got[1].Key != "5h" || got[1].Utilization != 12.5 || got[1].ResetsAt != nil {
		t.Fatalf("got %+v", got)
	}
	// Two slots of one length keep the more used one, whichever comes first.
	h.Set("x-codex-secondary-window-minutes", "10080")
	if got := ReadQuotaHeaders(decls, h.Get, now); len(got) != 1 || got[0].Key != "7d" || got[0].Utilization != 100 {
		t.Fatalf("same length: got %+v", got)
	}
	if got := ReadQuotaHeaders([]QuotaHeader{slot("secondary"), slot("primary")}, h.Get, now); len(got) != 1 || got[0].Utilization != 100 {
		t.Fatalf("same length, more used declared later: got %+v", got)
	}
	// At equal use the later reset wins; a known reset beats none.
	h.Set("x-codex-secondary-used-percent", "100")
	h.Set("x-codex-secondary-reset-at", "1790400000")
	if got := ReadQuotaHeaders(decls, h.Get, now); len(got) != 1 || got[0].ResetsAt.Unix() != 1790400000 {
		t.Fatalf("tie, later reset declared later: got %+v", got)
	}
	h.Set("x-codex-secondary-reset-at", "1790200000")
	if got := ReadQuotaHeaders(decls, h.Get, now); len(got) != 1 || got[0].ResetsAt.Unix() != 1790300000 {
		t.Fatalf("tie, earlier reset declared later: got %+v", got)
	}
	h.Del("x-codex-primary-reset-at")
	if got := ReadQuotaHeaders(decls, h.Get, now); len(got) != 1 || got[0].ResetsAt == nil || got[0].ResetsAt.Unix() != 1790200000 {
		t.Fatalf("tie, only the later declaration has a reset: got %+v", got)
	}
	// No usable length: nothing for that slot.
	h.Set("x-codex-secondary-window-minutes", "soon")
	h.Del("x-codex-primary-window-minutes")
	if got := ReadQuotaHeaders(decls, h.Get, now); len(got) != 0 {
		t.Fatalf("no length: got %+v", got)
	}
}

func TestQuotaWindowKey(t *testing.T) {
	for in, want := range map[string]string{
		"300": "5h", "10080": "7d", " 1440 ": "1d", "90": "90m", "60": "1h",
		"0": "", "-5": "", "1.5": "", "": "", "527041": "",
	} {
		if got := QuotaWindowKey(in); got != want {
			t.Errorf("QuotaWindowKey(%q) = %q, want %q", in, got, want)
		}
	}
}
