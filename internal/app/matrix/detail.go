package matrix

import (
	"fmt"

	"github.com/abradner/hoist/internal/app/flight"
)

// detail.go is the wide-terminal detail pane (v2·01b): what the matrix already knows
// about the cell under the cursor, spelled out in full rather than abbreviated to fit a table
// cell. Detail is pure — Table, Running and the in-flight list, no terminal dependency — the
// same split cells.go/grid.go/menu.go already keep.
//
// Deviation from the v2·01b mockup, recorded rather than silently shipped smaller (AGENTS.md
// §8, "building structure where no convention is stated is a decision"): the mockup draws this
// as a FOURTH grid column, sharing the table's own column dividers. grid.go's renderer has no
// notion of a column whose rows are independent multi-line prose rather than one aligned cell
// per family row, and reshaping it to support that is a bigger change than this file's own
// scope. Detail instead renders as its own block, shown as an extra Frame Section below the
// table at width >= detailMinWidth — real information, differently laid out; the 4-column
// mockup shape is left as a follow-up (named in a handoff report).
//
// Further deviation, forced by the data actually available here: the mockup's own lines
// "declared 3 days ago · a1b2c3d", "running 2/2 pods on this digest", "ahead of <env> ·
// 18 commits · 2 migrations", and "last write · deploy vX · PR #101" all need history the
// matrix does not hold (a commit-history bundle, per-pod counts from the cluster beyond
// drift/not-drift, engine.PromotionState detail beyond what flight.Summary carries). Detail
// ships only what Table, Running and the in-flight list can actually answer; the rest is
// ticketed rather than faked.

// detailMinWidth is the terminal width at which the detail pane appears (v2·01b's own
// width — 120 columns is the only mockup frame with one; "uncertain threshold" per the design,
// resolved here as the same 110 the mockup's own subheader env/family counts already used).
const detailMinWidth = 110

// Detail is the cell's own story: family, env, what the manifest says, what state word that
// earned, the drift sentence when applicable, whether the env is production, and — matched on
// Target only, the same coarse correlation MenuFor uses, since flight.Summary carries no family
// — any in-flight promotion into this env.
func Detail(t Table, fam, env string, envIsProduction bool, inflight []flight.Summary) []string {
	if fam == "" || env == "" {
		return nil
	}
	col := -1
	for i, e := range t.Envs {
		if e == env {
			col = i
		}
	}
	if col < 0 {
		return nil
	}
	var row *Row
	for i := range t.Rows {
		if t.Rows[i].Family == fam {
			row = &t.Rows[i]
			break
		}
	}
	if row == nil || col >= len(row.Cells) {
		return nil
	}
	c := row.Cells[col]
	lines := []string{fmt.Sprintf("%s · %s", fam, env)}
	if !c.Present {
		return append(lines, "not declared in this env")
	}
	lines = append(lines, c.Text)
	if c.State != "" {
		lines = append(lines, "  "+string(c.State))
	}
	if c.State == StateDrifted {
		suffix := ""
		if c.Compared != "" {
			suffix = " (compared " + c.Compared + ")"
		}
		lines = append(lines, "", fmt.Sprintf("running %s%s", c.Running, suffix))
	}
	if envIsProduction {
		lines = append(lines, "", "⚠ production: writes here always open a PR")
	}
	for _, s := range inflight {
		if s.Target != env || s.Done {
			continue
		}
		lines = append(lines, "", fmt.Sprintf("in flight: %s (%s)", paneID(s), s.Verdict()))
	}
	return lines
}
