package reports

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/founderstack/api/internal/db/dbgen"
)

// maxRunRows caps the run-level section; a report is a summary, not an export.
const maxRunRows = 200

// Sections are the operator's visibility toggles. Everything off by default:
// a shared report shows outcomes, never the operator's cost or token margins,
// unless they explicitly opt in.
type Sections struct {
	Cost   bool `json:"cost"`
	Tokens bool `json:"tokens"`
	Runs   bool `json:"runs"`
}

type Summary struct {
	HoursSaved      float64 `json:"hours_saved"`
	RunsCompleted   int64   `json:"runs_completed"`
	RunsFailed      int64   `json:"runs_failed"`
	RunsTotal       int64   `json:"runs_total"`
	WorkflowsActive int64   `json:"workflows_active"`
}

type WorkflowOutcome struct {
	Name          string  `json:"name"`
	RunsCompleted int64   `json:"runs_completed"`
	RunsTotal     int64   `json:"runs_total"`
	HoursSaved    float64 `json:"hours_saved"`
}

type AgentCost struct {
	AgentName string  `json:"agent_name"`
	CostUSD   float64 `json:"cost_usd"`
}

type CostSection struct {
	TotalUSD float64     `json:"total_usd"`
	ByAgent  []AgentCost `json:"by_agent"`
}

type TokenSection struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Cached int64 `json:"cached"`
	Total  int64 `json:"total"`
}

type RunRow struct {
	WorkflowName string   `json:"workflow_name"`
	Status       string   `json:"status"`
	StartedAt    string   `json:"started_at"`
	CompletedAt  *string  `json:"completed_at,omitempty"`
	HoursSaved   *float64 `json:"hours_saved,omitempty"`
	// Only present when the cost section is visible too.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// Snapshot is the frozen report. Hidden sections are nil and omitted, and
// were never computed — nothing the operator hid is stored anywhere.
type Snapshot struct {
	ClientName  string            `json:"client_name"`
	PreparedBy  string            `json:"prepared_by"`
	DateFrom    string            `json:"date_from"`
	DateTo      string            `json:"date_to"`
	Timezone    string            `json:"timezone"`
	GeneratedAt string            `json:"generated_at"`
	Summary     Summary           `json:"summary"`
	Workflows   []WorkflowOutcome `json:"workflows"`
	Cost        *CostSection      `json:"cost,omitempty"`
	Tokens      *TokenSection     `json:"tokens,omitempty"`
	Runs        []RunRow          `json:"runs,omitempty"`
}

// window turns an inclusive [from, to] date range into the half-open
// timestamp range [from 00:00, to+1 00:00) in the workspace's own timezone,
// so "September" means the client's September, not UTC's.
func window(from, to time.Time, loc *time.Location) (time.Time, time.Time) {
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, loc)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1)
	return start, end
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// buildSnapshot aggregates one workspace's window. q must be scoped to
// orgID's own tenant transaction.
func buildSnapshot(ctx context.Context, q *dbgen.Queries, orgID pgtype.UUID, clientName, preparedBy string,
	from, to time.Time, loc *time.Location, sections Sections) (Snapshot, error) {
	start, end := window(from, to, loc)
	snap := Snapshot{
		ClientName: clientName, PreparedBy: preparedBy,
		DateFrom: from.Format("2006-01-02"), DateTo: to.Format("2006-01-02"), Timezone: loc.String(),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339), Workflows: []WorkflowOutcome{},
	}

	sum, err := q.GetReportRunSummary(ctx, dbgen.GetReportRunSummaryParams{OrgID: orgID, WindowStart: ts(start), WindowEnd: ts(end)})
	if err != nil {
		return snap, fmt.Errorf("run summary: %w", err)
	}
	snap.Summary = Summary{
		HoursSaved: sum.HoursSaved, RunsCompleted: sum.RunsCompleted, RunsFailed: sum.RunsFailed,
		RunsTotal: sum.RunsTotal, WorkflowsActive: sum.WorkflowsActive,
	}

	outcomes, err := q.ListReportWorkflowOutcomes(ctx, dbgen.ListReportWorkflowOutcomesParams{OrgID: orgID, WindowStart: ts(start), WindowEnd: ts(end)})
	if err != nil {
		return snap, fmt.Errorf("workflow outcomes: %w", err)
	}
	for _, o := range outcomes {
		snap.Workflows = append(snap.Workflows, WorkflowOutcome{Name: o.Name, RunsCompleted: o.RunsCompleted, RunsTotal: o.RunsTotal, HoursSaved: o.HoursSaved})
	}

	if sections.Cost || sections.Tokens {
		totals, err := q.GetReportCostTotals(ctx, dbgen.GetReportCostTotalsParams{OrgID: orgID, WindowStart: ts(start), WindowEnd: ts(end)})
		if err != nil {
			return snap, fmt.Errorf("cost totals: %w", err)
		}
		if sections.Tokens {
			snap.Tokens = &TokenSection{
				Input: totals.InputTokens, Output: totals.OutputTokens, Cached: totals.CachedTokens,
				Total: totals.InputTokens + totals.OutputTokens + totals.CachedTokens,
			}
		}
		if sections.Cost {
			byAgent, err := q.ListReportCostByAgent(ctx, dbgen.ListReportCostByAgentParams{OrgID: orgID, WindowStart: ts(start), WindowEnd: ts(end)})
			if err != nil {
				return snap, fmt.Errorf("cost by agent: %w", err)
			}
			cost := &CostSection{TotalUSD: totals.TotalCostUsd, ByAgent: []AgentCost{}}
			for _, a := range byAgent {
				cost.ByAgent = append(cost.ByAgent, AgentCost{AgentName: a.AgentName, CostUSD: a.CostUsd})
			}
			snap.Cost = cost
		}
	}

	if sections.Runs {
		rows, err := q.ListReportRuns(ctx, dbgen.ListReportRunsParams{OrgID: orgID, WindowStart: ts(start), WindowEnd: ts(end), RowLimit: maxRunRows})
		if err != nil {
			return snap, fmt.Errorf("runs: %w", err)
		}
		snap.Runs = []RunRow{}
		for _, r := range rows {
			started := r.CreatedAt.Time
			if r.StartedAt.Valid {
				started = r.StartedAt.Time
			}
			row := RunRow{WorkflowName: r.WorkflowName, Status: r.Status, StartedAt: started.UTC().Format(time.RFC3339), HoursSaved: r.HoursSaved}
			if r.CompletedAt.Valid {
				c := r.CompletedAt.Time.UTC().Format(time.RFC3339)
				row.CompletedAt = &c
			}
			if sections.Cost {
				cost := r.CostUsd
				row.CostUSD = &cost
			}
			snap.Runs = append(snap.Runs, row)
		}
	}
	return snap, nil
}
