package core

import (
	"context"
	"testing"
	"time"
)

// A search flagged NoLog must stay out of the query log while still being
// counted by metrics. The two answer different questions: metrics say "how much
// did we spend and how fast was it", which synthetic traffic is a legitimate
// part of, whereas the log says "which real questions came back with nothing"
// and is ruined by a harness firing 71 questions per run. On 2026-09-09, hours
// after the log was switched on, 280 of its 280 entries were the harness's own.
func TestNoLogKeepsQueryOutOfTheLog(t *testing.T) {
	p := &mockProvider{}
	svc := NewService(p, nil, nil)
	store := logStore(t, 0)
	svc.SetQueryLog(store)
	ctx := context.Background()

	if _, err := svc.Search(ctx, "proj", "pertanyaan asli", SearchOpts{K: 3}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, err := svc.Search(ctx, "proj", "pertanyaan eval", SearchOpts{K: 3, NoLog: true}); err != nil {
		t.Fatalf("search: %v", err)
	}

	// The log write is deliberately asynchronous, so poll instead of asserting
	// immediately. Polling for the count to REACH 1 and then holding still is
	// what catches the bug this test exists for: if NoLog were ignored the count
	// would go to 2, and a bare "== 1" check right after the call would pass by
	// accident whenever the second write simply had not landed yet.
	var got []QueryLogEntry
	for i := 0; i < 100; i++ {
		var err error
		if got, err = store.RecentQueries(ctx, 10); err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(got) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got, _ = store.RecentQueries(ctx, 10); len(got) != 1 {
		qs := make([]string, 0, len(got))
		for _, e := range got {
			qs = append(qs, e.Query)
		}
		t.Fatalf("expected exactly 1 logged query, got %d: %v", len(got), qs)
	}
	if got[0].Query != "pertanyaan asli" {
		t.Errorf("logged the wrong query: %q", got[0].Query)
	}

	// Metrics must have counted BOTH. If NoLog also hid the spend, the cost
	// column the eval harness prints would understate what a sweep actually
	// costs, which is the number the recall default was chosen on.
	if _, _, _, count, _, _ := svc.metrics.snapshot(); count != 2 {
		t.Errorf("expected both queries counted in metrics, got %d", count)
	}
}
