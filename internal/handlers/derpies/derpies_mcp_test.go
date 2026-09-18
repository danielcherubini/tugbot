// The mcp.DecisionSource surface of the derpies handler (Task 4):
// ReadDecisions is the read the read_derpies_decisions tool serves.
package derpies

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danielcherubini/tugbot/internal/mcp"
)

// TestReadDecisionsFilter — the ReadDecisions public surface (the
// mcp.DecisionSource impl behind read_derpies_decisions): the filter
// clauses (author, channel, path, deleted, score range, since/until)
// pass through to store.queryDecisions unchanged, the Limit default
// (<= 0 → 50) + clamp (> 500 → 500) is applied HERE, Since/Until are
// normalized to UTC before binding (the created_at column is timestamp
// without time zone; pgx encodes a *time.Time with its offset, so a
// non-UTC value would compare against the session's timezone
// interpretation), and a store error propagates (the MCP tool surfaces
// it as a tool error — a read tool failing is not a silent
// degradation).
func TestReadDecisionsFilter(t *testing.T) {
	store := &fakeStore{
		queryRows: []mcp.DecisionRow{{ID: 1, MessageID: "m1"}},
	}
	h := newTestDerpies(store, &fakeOps{}, nil)

	// (1) All clauses compose through unchanged; an in-range limit is
	//     untouched; Since/Until are UTC-normalized.
	deleted := true
	since := time.Date(2026, 1, 1, 10, 0, 0, 0, time.FixedZone("CET", 3*3600))
	until := time.Date(2026, 1, 2, 10, 0, 0, 0, time.FixedZone("CET", 3*3600))
	smin, smax := 40, 80
	rows, err := h.ReadDecisions(context.Background(), mcp.DecisionFilter{
		AuthorID: "u1", ChannelID: "c1", Path: "fast", Deleted: &deleted,
		ScoreMin: &smin, ScoreMax: &smax, Since: &since, Until: &until, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ReadDecisions: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 1 {
		t.Fatalf("ReadDecisions rows = %v, want the store's rows", rows)
	}
	q := store.queried
	if q.AuthorID != "u1" || q.ChannelID != "c1" || q.Path != "fast" ||
		q.Deleted == nil || !*q.Deleted ||
		q.ScoreMin == nil || *q.ScoreMin != 40 ||
		q.ScoreMax == nil || *q.ScoreMax != 80 {
		t.Errorf("queried filter = %+v, want the clauses passed through unchanged", q)
	}
	if q.Since == nil || !q.Since.Equal(since.UTC()) || q.Since.Location() != time.UTC {
		t.Errorf("queried Since = %v, want the UTC-normalized %v", q.Since, since.UTC())
	}
	if q.Until == nil || !q.Until.Equal(until.UTC()) || q.Until.Location() != time.UTC {
		t.Errorf("queried Until = %v, want the UTC-normalized %v", q.Until, until.UTC())
	}
	if q.Limit != 100 {
		t.Errorf("queried Limit = %d, want the in-range 100 unchanged", q.Limit)
	}

	// (2) Limit default: <= 0 → 50.
	if _, err := h.ReadDecisions(context.Background(), mcp.DecisionFilter{}); err != nil {
		t.Fatalf("ReadDecisions (zero filter): %v", err)
	}
	if store.queried.Limit != 50 {
		t.Errorf("queried Limit = %d, want the default 50", store.queried.Limit)
	}
	if _, err := h.ReadDecisions(context.Background(), mcp.DecisionFilter{Limit: -5}); err != nil {
		t.Fatalf("ReadDecisions (negative limit): %v", err)
	}
	if store.queried.Limit != 50 {
		t.Errorf("queried Limit = %d, want the default 50 (negative limit)", store.queried.Limit)
	}

	// (3) Limit clamp: > 500 → 500 (clamped, not an error).
	if _, err := h.ReadDecisions(context.Background(), mcp.DecisionFilter{Limit: 1000}); err != nil {
		t.Fatalf("ReadDecisions (limit 1000): %v", err)
	}
	if store.queried.Limit != 500 {
		t.Errorf("queried Limit = %d, want the clamped 500", store.queried.Limit)
	}

	// (4) A store error propagates (not a silent degradation).
	store.queryErr = errors.New("db down")
	if _, err := h.ReadDecisions(context.Background(), mcp.DecisionFilter{}); !errors.Is(err, store.queryErr) {
		t.Errorf("ReadDecisions error = %v, want the store error to propagate", err)
	}
}
