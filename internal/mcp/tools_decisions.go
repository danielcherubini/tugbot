package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// readDecisionsArgs mirrors DecisionFilter with json tags (all optional,
// AND-combined; the Limit default/clamp is the handler's — the tool
// passes it through).
type readDecisionsArgs struct {
	AuthorID  string     `json:"author_id,omitempty"`
	ChannelID string     `json:"channel_id,omitempty"`
	Path      string     `json:"path,omitempty"` // "fast" | "slow" | "slowmode" | "" (unfiltered)
	Deleted   *bool      `json:"deleted,omitempty"`
	ScoreMin  *int       `json:"score_min,omitempty"`
	ScoreMax  *int       `json:"score_max,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	Until     *time.Time `json:"until,omitempty"`
	Limit     int        `json:"limit,omitempty"`
}

// registerDecisionsTools adds the read_derpies_decisions tool to the SDK
// server (called from NewServer). The tool is read-only — ADR 0004's
// LAN-trust posture applies (the embedded server is LAN-bound).
func registerDecisionsTools(srv *mcpSDK.Server, ds DecisionSource) {
	mcpSDK.AddTool(srv, &mcpSDK.Tool{
		Name:        "read_derpies_decisions",
		Description: "Reads the derpies decision log (one row per judged message/edit) newest first. All filters are optional and AND-combined: author_id, channel_id, path (\"fast\"/\"slow\"/\"slowmode\"), deleted, score_min/score_max, since/until (RFC3339), and limit (default 50, clamped to 500). The text result renders one line per decision — [id] created_at path score/threshold word learned deleted reject_reason: content — followed by a trailing \"--- N decision(s) (first .. last)\" summary; embedded newlines in content collapse to \" ⏎ \". The rows also ride under a \"decisions\" key in the structured payload; a database error surfaces as an error result.",
	}, func(ctx context.Context, _ *mcpSDK.CallToolRequest, args readDecisionsArgs) (*mcpSDK.CallToolResult, any, error) {
		return handleReadDecisions(ctx, ds, args)
	})
}

// handleReadDecisions converts the args to a DecisionFilter (the structs
// are field-identical, so a direct conversion), calls the seam with the
// SDK's request context (NOT context.Background() — a slow queryDecisions
// 500-row scan must be cancellable on client disconnect/timeout), and
// renders the rows — one TEXT line per row (so any MCP client that
// surfaces only the text sees the decision content itself, mirroring
// read_messages' c64516d fix) + a "N decision(s)" trailing summary
// (with the first/last created_at); the rows ALSO ride under a
// "decisions" key (the record payload — newlines stay raw there). A
// ReadDecisions error → a toolErr IsError result (a read tool failing is
// not a silent degradation).
func handleReadDecisions(ctx context.Context, ds DecisionSource, a readDecisionsArgs) (*mcpSDK.CallToolResult, any, error) {
	rows, err := ds.ReadDecisions(ctx, DecisionFilter(a))
	if err != nil {
		return toolErr("tugbot", err.Error()), nil, nil
	}
	out := make([]map[string]any, 0, len(rows))
	var fb strings.Builder
	firstTs, lastTs := "", ""
	for _, r := range rows {
		out = append(out, decisionRowPayload(r))
		ts := ""
		if !r.CreatedAt.IsZero() {
			ts = r.CreatedAt.UTC().Format(time.RFC3339)
		}
		if firstTs == "" {
			firstTs = ts
		}
		lastTs = ts
		fb.WriteString(renderDecisionLine(r))
		fb.WriteByte('\n')
	}
	noun := "decisions"
	if len(out) == 1 {
		noun = "decision"
	}
	var text string
	if len(out) == 0 {
		// Empty page: exactly "0 decisions" (no lines, no timestamp range).
		text = "0 decisions"
	} else {
		text = fb.String() + fmt.Sprintf("--- %d %s (%s .. %s)", len(out), noun, firstTs, lastTs)
	}
	return textResult(text), map[string]any{"decisions": out}, nil
}

// renderDecisionLine renders ONE DecisionRow as a single text line:
//
//	[<id>] <created_at RFC3339 UTC> <path|-> <score|->/<threshold|-> <word|-> <learned> <deleted> <reject_reason|->: <content>
//
// The NULL optionals (score + threshold NULL for fast / pre-matrix arms;
// an absent word; an absent reject_reason) render as "-"; the real
// booleans as lowercase "true"/"false". A zero created_at renders as an
// empty token (decisionRowPayload shows "", same source). Embedded
// newlines in content collapse to one " ⏎ " (U+23CE LINE SEPARATOR
// between two spaces) each, so a multi-line message stays a single line
// — deterministic, using strings.ReplaceAll (mirroring
// renderMessageLine).
func renderDecisionLine(r DecisionRow) string {
	path := "-"
	if r.Path != nil {
		path = *r.Path
	}
	score := "-"
	if r.Score != nil {
		score = strconv.Itoa(*r.Score)
	}
	threshold := "-"
	if r.Threshold != nil {
		threshold = strconv.Itoa(*r.Threshold)
	}
	word := "-"
	if r.Word != nil {
		word = *r.Word
	}
	reject := "-"
	if r.RejectReason != nil {
		reject = *r.RejectReason
	}
	ts := ""
	if !r.CreatedAt.IsZero() {
		ts = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("[%d] %s %s %s/%s %s %t %t %s: %s",
		r.ID, ts, path, score, threshold, word, r.Learned, r.Deleted, reject,
		strings.ReplaceAll(r.Content, "\n", " ⏎ "))
}

// decisionRowPayload renders one DecisionRow (the nullable pointer fields
// marshal as null; created_at is rendered RFC3339 in UTC — the column is
// timestamp without time zone).
func decisionRowPayload(r DecisionRow) map[string]any {
	createdAt := ""
	if !r.CreatedAt.IsZero() {
		createdAt = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id":            r.ID,
		"message_id":    r.MessageID,
		"channel_id":    r.ChannelID,
		"author_id":     r.AuthorID,
		"content":       r.Content,
		"path":          r.Path,
		"score":         r.Score,
		"threshold":     r.Threshold,
		"word":          r.Word,
		"learned":       r.Learned,
		"deleted":       r.Deleted,
		"reject_reason": r.RejectReason,
		"created_at":    createdAt,
	}
}
