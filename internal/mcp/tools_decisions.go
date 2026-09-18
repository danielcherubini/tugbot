package mcp

import (
	"context"
	"fmt"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
)

// readDecisionsArgs mirrors DecisionFilter with json tags (all optional,
// AND-combined; the Limit default/clamp is the handler's — the tool
// passes it through).
type readDecisionsArgs struct {
	AuthorID  string     `json:"author_id,omitempty"`
	ChannelID string     `json:"channel_id,omitempty"`
	Path      string     `json:"path,omitempty"` // "fast" | "slow" | "" (unfiltered)
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
		Description: "Reads the derpies decision log (one row per judged message/edit) newest first. All filters are optional and AND-combined: author_id, channel_id, path (\"fast\"/\"slow\"), deleted, score_min/score_max, since/until (RFC3339), and limit (default 50, clamped to 500). Returns the rows under a \"decisions\" key with a \"N decision(s)\" text summary; a database error surfaces as an error result.",
	}, func(ctx context.Context, _ *mcpSDK.CallToolRequest, args readDecisionsArgs) (*mcpSDK.CallToolResult, any, error) {
		return handleReadDecisions(ctx, ds, args)
	})
}

// handleReadDecisions converts the args to a DecisionFilter (the structs
// are field-identical, so a direct conversion), calls the seam with the
// SDK's request context (NOT context.Background() — a slow queryDecisions
// 500-row scan must be cancellable on client disconnect/timeout), and
// renders the rows (JSON under a "decisions" key — mirroring
// read_messages' "messages" convention) + a "N decision(s)" text
// summary. A ReadDecisions error → a toolErr IsError result (a read tool
// failing is not a silent degradation).
func handleReadDecisions(ctx context.Context, ds DecisionSource, a readDecisionsArgs) (*mcpSDK.CallToolResult, any, error) {
	rows, err := ds.ReadDecisions(ctx, DecisionFilter(a))
	if err != nil {
		return toolErr("tugbot", err.Error()), nil, nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, decisionRowPayload(r))
	}
	noun := "decisions"
	if len(out) == 1 {
		noun = "decision"
	}
	return textResult(fmt.Sprintf("%d %s", len(out), noun)), map[string]any{"decisions": out}, nil
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
