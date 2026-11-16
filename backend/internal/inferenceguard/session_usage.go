package inferenceguard

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SessionUsageRow describes local evidence for one host session/project/model/
// effort group. Turns count admitted outer requests, not native response calls.
// Failed counts only completed requests with an observed error; uncertain
// acknowledgements remain separate. Token counters never imply allowance units
// or a dollar cost, and cached/reasoning tokens are subsets of input/output.
type SessionUsageRow struct {
	Session                string     `json:"session_id"`
	Project                string     `json:"project_id"`
	ProjectAttribution     string     `json:"project_attribution"`
	Model                  string     `json:"model"`
	Effort                 string     `json:"effort"`
	Turns                  int        `json:"dispatched_turns"`
	Failed                 int        `json:"failed_turns"`
	Active                 int        `json:"active_turns"`
	Uncertain              int        `json:"uncertain_turns"`
	Receipts               int        `json:"response_receipts"`
	Input                  int64      `json:"input_tokens"`
	CachedInput            int64      `json:"cached_input_tokens"`
	Output                 int64      `json:"output_tokens"`
	ReasoningOutput        int64      `json:"reasoning_output_tokens"`
	FirstDispatchAt        *time.Time `json:"first_dispatch_at"`
	FirstReceiptObservedAt *time.Time `json:"first_receipt_observed_at"`
	DispatchConfidence     string     `json:"dispatch_confidence"`
	UsageConfidence        string     `json:"usage_confidence"`
	BillableUnits          string     `json:"billable_units"`
	AllowanceUnits         string     `json:"allowance_units"`
	DollarCost             string     `json:"dollar_cost"`
}

// SessionUsage is a read-only projection over durable admissions and immutable
// independent response receipts. It never mutates or fills gaps in either table.
// Receipt attribution requires a unique project on an exact admitted session +
// native root turn. Historical receipt-only records retain UNKNOWN project;
// another turn in the same session is insufficient evidence for attribution.
func (g *Gate) SessionUsage(ctx context.Context) ([]SessionUsageRow, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, err := g.current()
	if err != nil {
		return nil, err
	}
	// Aggregate admissions and receipts independently, then sum their union.
	// Joining raw attempts to response rows would multiply both turn counts and
	// tokens when a native turn has several responses or a replayed binding.
	//
	// Stored timestamps are UTC RFC3339Nano. Removing the final Z before MIN
	// preserves ordering of zero and fractional seconds (00Z vs 00.1Z);
	// parsing below restores the original full precision.
	rows, err := g.db.QueryContext(ctx, `WITH receipt_rows AS (
 SELECT r.*,
  coalesce((SELECT CASE WHEN count(DISTINCT a.project_id)=1 THEN min(a.project_id) ELSE '' END
    FROM attempts a
    WHERE a.account_key=r.account_key AND a.session_id=r.session_id
      AND a.provider_turn_id=r.provider_turn_id AND a.dispatched_at<>''), '') AS project_id
 FROM usage_receipts r WHERE r.account_key=?
), facts AS (
 SELECT session_id,project_id,model,effort,
  count(*) AS turns,
  sum(CASE WHEN state='completed' AND error_class<>'' THEN 1 ELSE 0 END) AS failed,
  sum(CASE WHEN state='active' THEN 1 ELSE 0 END) AS active,
  sum(CASE WHEN state='uncertain' THEN 1 ELSE 0 END) AS uncertain,
  0 AS receipts,0 AS input_tokens,0 AS cached_input_tokens,
  0 AS output_tokens,0 AS reasoning_output_tokens,
  min(rtrim(dispatched_at,'Z')) AS first_dispatch,NULL AS first_receipt
 FROM attempts WHERE account_key=? AND dispatched_at<>''
 GROUP BY session_id,project_id,model,effort
 UNION ALL
 SELECT session_id,project_id,model,effort,
  0,0,0,0,count(*),sum(input_tokens),sum(cached_input_tokens),
  sum(output_tokens),sum(reasoning_output_tokens),
  NULL,min(rtrim(observed_at,'Z'))
 FROM receipt_rows
 GROUP BY session_id,project_id,model,effort
)
SELECT session_id,project_id,model,effort,
 sum(turns),sum(failed),sum(active),sum(uncertain),sum(receipts),
 sum(input_tokens),sum(cached_input_tokens),sum(output_tokens),sum(reasoning_output_tokens),
 min(first_dispatch),min(first_receipt)
FROM facts
GROUP BY session_id,project_id,model,effort
ORDER BY session_id,project_id,model,effort`, p.AccountKey, p.AccountKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]SessionUsageRow, 0)
	for rows.Next() {
		var row SessionUsageRow
		var firstDispatch, firstReceipt sql.NullString
		if err := rows.Scan(&row.Session, &row.Project, &row.Model, &row.Effort,
			&row.Turns, &row.Failed, &row.Active, &row.Uncertain, &row.Receipts,
			&row.Input, &row.CachedInput, &row.Output, &row.ReasoningOutput,
			&firstDispatch, &firstReceipt); err != nil {
			return nil, err
		}
		if row.FirstDispatchAt, err = sessionUsageTime(firstDispatch); err != nil {
			return nil, err
		}
		if row.FirstReceiptObservedAt, err = sessionUsageTime(firstReceipt); err != nil {
			return nil, err
		}
		row.ProjectAttribution = "UNKNOWN"
		if row.Project != "" {
			row.ProjectAttribution = "recorded_governed_turn"
		}
		row.DispatchConfidence = "no_recorded_outer_turn"
		if row.Turns != 0 {
			row.DispatchConfidence = "durable_outer_turn_admission"
		}
		row.UsageConfidence = "UNKNOWN: no independent response receipts"
		if row.Receipts != 0 {
			row.UsageConfidence = "provider_response_receipts; completeness UNKNOWN"
		}
		row.BillableUnits, row.AllowanceUnits, row.DollarCost = "UNKNOWN", "UNKNOWN", "UNKNOWN"
		result = append(result, row)
	}
	return result, rows.Err()
}

func sessionUsageTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value.String+"Z")
	if err != nil {
		return nil, fmt.Errorf("invalid stored session usage timestamp: %w", err)
	}
	return &at, nil
}
