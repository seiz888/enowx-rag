package core

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func logStore(t *testing.T, keep int) *SQLiteMetricsStore {
	t.Helper()
	s, err := NewSQLiteMetricsStore(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetQueryLogKeep(keep)
	return s
}

// Off unless explicitly switched on: this is the one path that writes
// user-authored text to disk, so a default-on would be a decision made for the
// operator instead of by them.
func TestQueryLogOffByDefault(t *testing.T) {
	t.Setenv("RAG_QUERY_LOG", "")
	if on, _ := QueryLogEnabled(); on {
		t.Error("query log should be off when unset")
	}
	for _, v := range []string{"0", "no", "off", "maybe"} {
		t.Setenv("RAG_QUERY_LOG", v)
		if on, _ := QueryLogEnabled(); on {
			t.Errorf("RAG_QUERY_LOG=%q should not enable logging", v)
		}
	}
	for _, v := range []string{"1", "true", "yes"} {
		t.Setenv("RAG_QUERY_LOG", v)
		on, keep := QueryLogEnabled()
		if !on || keep != DefaultQueryLogKeep {
			t.Errorf("RAG_QUERY_LOG=%q: on=%v keep=%d", v, on, keep)
		}
	}
	t.Setenv("RAG_QUERY_LOG", "1")
	t.Setenv("RAG_QUERY_LOG_KEEP", "17")
	if _, keep := QueryLogEnabled(); keep != 17 {
		t.Errorf("keep = %d, want 17", keep)
	}
}

func TestQueryLogRoundTripNewestFirst(t *testing.T) {
	s := logStore(t, 0)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		e := QueryLogEntry{
			Ts: time.Unix(int64(1700000000+i), 0), ProjectID: "memory",
			Query: fmt.Sprintf("q%d", i), Results: i, TopScore: float64(i) / 10,
			TopDocID: fmt.Sprintf("doc%d#1", i), Reranked: i%2 == 0, LatencyMs: 12.5,
		}
		if err := s.LogQuery(ctx, e); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	got, err := s.RecentQueries(ctx, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Query != "q2" || got[2].Query != "q0" {
		t.Errorf("wrong order: %s .. %s", got[0].Query, got[2].Query)
	}
	if got[0].TopDocID != "doc2#1" || got[0].Results != 2 || !got[0].Reranked {
		t.Errorf("fields not round-tripped: %+v", got[0])
	}
	if got[0].LatencyMs != 12.5 {
		t.Errorf("latency = %v", got[0].LatencyMs)
	}
}

// The bound must actually delete. An unbounded log on a VPS whose root was 72%
// full is a disk-full incident waiting for a busy week.
func TestQueryLogPrunesToKeep(t *testing.T) {
	s := logStore(t, 5)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		if err := s.LogQuery(ctx, QueryLogEntry{
			Ts: time.Now(), ProjectID: "memory", Query: fmt.Sprintf("q%d", i),
		}); err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
	}
	got, err := s.RecentQueries(ctx, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) > 5 {
		t.Fatalf("kept %d entries, want at most 5", len(got))
	}
	if got[0].Query != "q39" {
		t.Errorf("pruning kept the wrong end: newest is %s", got[0].Query)
	}
}

// A nil log must not make Search's deferred block panic, and must be reported as
// disabled rather than as an empty log -- those are different facts.
func TestServiceReportsDisabledVersusEmpty(t *testing.T) {
	svc := NewService(nil, nil, nil)
	e, enabled, err := svc.RecentQueries(context.Background(), 10)
	if err != nil || enabled || e != nil {
		t.Errorf("nil log: entries=%v enabled=%v err=%v", e, enabled, err)
	}

	s := logStore(t, 0)
	svc.SetQueryLog(s)
	e, enabled, err = svc.RecentQueries(context.Background(), 10)
	if err != nil || !enabled || len(e) != 0 {
		t.Errorf("empty log: entries=%v enabled=%v err=%v", e, enabled, err)
	}
}
