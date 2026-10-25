package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// buildFinancialContributors opens each financial feature's store and returns a list
// of contributors. Stores that fail to open are skipped (logged, not fatal).
// This is the single registration point — to add grid-bot or snowball, add a block here.
//
// Parameters are passed explicitly (not via cfg) so this function can be called
// without depending on any newly-added config fields — the caller (instance.go) extracts
// them from cfg.Agents.Defaults.
//
// profileStore is the owner's profile store (nil to leave the profile line out).
func buildFinancialContributors(
	workspace string,
	maxAssets, maxDCA, maxDN int,
	profileStore *intent.Store,
) []FinancialContributor {
	var contributors []FinancialContributor

	// Portfolio snapshot contributor.
	if snap, err := snapshot.NewStore(workspace); err != nil {
		logger.DebugCF("agent", "financial context: snapshot store open failed, skipping portfolio section",
			map[string]any{"error": err.Error()})
	} else {
		contributors = append(contributors, &portfolioContributor{store: snap, maxAssets: maxAssets})
	}

	// DCA plan contributor.
	if dcaStore, err := dca.NewStore(workspace); err != nil {
		logger.DebugCF("agent", "financial context: dca store open failed, skipping DCA section",
			map[string]any{"error": err.Error()})
	} else {
		contributors = append(contributors, &dcaContributor{store: dcaStore, maxPlans: maxDCA})
	}

	// Delta-Neutral plan contributor.
	if dnStore, err := deltaneutral.NewStore(workspace); err != nil {
		logger.DebugCF("agent", "financial context: delta-neutral store open failed, skipping DN section",
			map[string]any{"error": err.Error()})
	} else {
		contributors = append(contributors, &dnContributor{store: dnStore, maxPlans: maxDN})
	}

	// Owner profile contributor: silent until a profile version is stored.
	if profileStore != nil {
		contributors = append(contributors, &profileContributor{store: profileStore})
	}

	return contributors
}

// --- Profile contributor ---

// profileContributor renders the owner's typed profile as one line. It
// contributes nothing until at least one version has been stored, so a clean
// install's prompt is byte-identical to one built without it. The store belongs
// to the shared database handle, so Close does not close it.
type profileContributor struct {
	store *intent.Store
}

func (p *profileContributor) Name() string { return "profile" }

func (p *profileContributor) Section(ctx context.Context) (string, error) {
	v, ok, err := p.store.Latest(ctx)
	if err != nil {
		return "", fmt.Errorf("read profile: %w", err)
	}
	if !ok {
		return "", nil
	}
	// PromptLine re-checks every field it prints and leaves out the free-text
	// ones, whatever the stored row holds.
	line := v.Profile.PromptLine()
	if line == "" {
		return "", nil
	}
	return line + "\n", nil
}

func (p *profileContributor) Close() error { return nil }

// --- Portfolio contributor ---

// Bounds on the exchange-supplied names printed in the portfolio section. They
// are far above any real ticker or account name, so an ordinary row is rendered
// exactly as it always was.
const (
	maxPortfolioAssetRunes   = 64
	maxPortfolioAccountRunes = 64
	maxPortfolioQuoteRunes   = 16
)

type portfolioContributor struct {
	store     *snapshot.Store
	maxAssets int
	now       func() time.Time // nil = time.Now; set by tests
}

func (p *portfolioContributor) Name() string { return "portfolio" }

// Section renders the newest snapshot of either kind: the owner's own latest, or
// the latest automatic one (perception's 15-minute snapshot) when that was taken
// after it. An own snapshot renders as it always has. An automatic one says in
// the header that it is automatic and how old it was when this context was built
// (the section is cached for the collector's TTL, so the age is that of the
// build), and, when some assets could not be priced, that it is partial and how
// many assets the figures leave out. The section stays one header line plus at
// most maxAssets rows either way.
func (p *portfolioContributor) Section(ctx context.Context) (string, error) {
	snapshots, err := p.store.QuerySnapshots(ctx, snapshot.QueryFilter{Limit: 1})
	if err != nil {
		return "", fmt.Errorf("query snapshots: %w", err)
	}

	// A failed read of the automatic rows leaves the owner's snapshot as it was.
	auto, autoErr := p.store.LatestAuto(ctx)
	if autoErr != nil {
		logger.DebugCF("agent", "financial context: latest automatic snapshot read failed",
			map[string]any{"error": autoErr.Error()})
		auto = nil
	}
	useAuto := auto != nil && (len(snapshots) == 0 || auto.TakenAt.After(snapshots[0].TakenAt))

	if len(snapshots) == 0 && !useAuto {
		return "", nil
	}

	var snap *snapshot.Snapshot
	if useAuto {
		snap = auto
	} else {
		snap, err = p.store.GetSnapshot(ctx, snapshots[0].ID)
		if err != nil {
			return "", fmt.Errorf("get snapshot: %w", err)
		}
	}

	quote := sanitizeInline(snap.Quote, maxPortfolioQuoteRunes)
	var sb strings.Builder
	if useAuto {
		fmt.Fprintf(&sb, "Portfolio (%s, as of %s UTC, %s): %.2f\n",
			quote, snap.TakenAt.UTC().Format("2006-01-02 15:04"), p.autoNote(snap), snap.TotalValue)
	} else {
		fmt.Fprintf(&sb, "Portfolio (%s, as of %s): %.2f\n",
			quote, snap.TakenAt.Format("2006-01-02 15:04"), snap.TotalValue)
	}

	// Sort positions by Value descending, take top N.
	positions := snap.Positions
	sort.Slice(positions, func(i, j int) bool {
		return positions[i].Value > positions[j].Value
	})
	if len(positions) > p.maxAssets {
		positions = positions[:p.maxAssets]
	}
	for _, pos := range positions {
		accountPart := ""
		if pos.Account != "" {
			accountPart = sanitizeInline(pos.Account, maxPortfolioAccountRunes) + " "
		}
		fmt.Fprintf(&sb, "- %s%s: %.6g (%.2f %s)\n",
			accountPart, sanitizeInline(pos.Asset, maxPortfolioAssetRunes), pos.Quantity, pos.Value, quote)
	}

	return strings.TrimRight(sb.String(), "\n"), nil
}

// autoNote is the header phrase for an automatic snapshot: its kind, its age at
// this moment, and the partial marker when the note records unpriced assets.
func (p *portfolioContributor) autoNote(snap *snapshot.Snapshot) string {
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	note := "automatic snapshot, " + formatSnapshotAge(now().Sub(snap.TakenAt)) + " old when this context was built"
	switch n := len(snapshot.UnpricedFromNote(snap.Note)); {
	case n == 1:
		note += ", PARTIAL: 1 asset could not be priced and is left out of these figures"
	case n > 1:
		note += fmt.Sprintf(", PARTIAL: %d assets could not be priced and are left out of these figures", n)
	}
	return note
}

// formatSnapshotAge renders an age as whole minutes, hours and minutes, or days
// and hours. A negative age (a clock that moved back) is shown as zero.
func formatSnapshotAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	mins := int(d / time.Minute)
	switch {
	case mins < 1:
		return "under 1 min"
	case mins < 60:
		return fmt.Sprintf("%d min", mins)
	case mins < 24*60:
		return fmt.Sprintf("%d h %d min", mins/60, mins%60)
	default:
		return fmt.Sprintf("%d d %d h", mins/(24*60), mins%(24*60)/60)
	}
}

func (p *portfolioContributor) Close() error { return p.store.Close() }

// --- DCA contributor ---

type dcaContributor struct {
	store    *dca.Store
	maxPlans int
}

func (d *dcaContributor) Name() string { return "dca" }

func (d *dcaContributor) Section(ctx context.Context) (string, error) {
	trueVal := true
	plans, err := d.store.ListPlans(ctx, dca.QueryFilter{Enabled: &trueVal, Limit: d.maxPlans})
	if err != nil {
		return "", fmt.Errorf("list dca plans: %w", err)
	}
	if len(plans) == 0 {
		return "", nil
	}

	var parts []string
	for _, plan := range plans {
		s := fmt.Sprintf("%s: %s %s %.6g %s %s",
			plan.Name, plan.Symbol, plan.Side,
			plan.AmountPerOrder, plan.AmountUnit, plan.FrequencyExpr)
		if plan.AvgCost > 0 {
			s += fmt.Sprintf(" (avg %.4g, total invested %.2f)", plan.AvgCost, plan.TotalInvested)
		}
		parts = append(parts, s)
	}

	return fmt.Sprintf("Active DCA (%d): %s", len(plans), strings.Join(parts, "; ")), nil
}

func (d *dcaContributor) Close() error { return d.store.Close() }

// --- Delta-Neutral contributor ---

type dnContributor struct {
	store    *deltaneutral.Store
	maxPlans int
}

func (d *dnContributor) Name() string { return "dn" }

func (d *dnContributor) Section(ctx context.Context) (string, error) {
	activeStatus := deltaneutral.PlanStatusActive
	plans, err := d.store.ListPlans(ctx, deltaneutral.QueryFilter{Status: &activeStatus, Limit: d.maxPlans})
	if err != nil {
		return "", fmt.Errorf("list dn plans: %w", err)
	}
	if len(plans) == 0 {
		return "", nil
	}

	var parts []string
	for _, plan := range plans {
		s := plan.Name
		// Fetch latest monitor snapshot for health info (N+1 bounded by maxPlans).
		if ms, err := d.store.LatestSnapshot(ctx, plan.ID); err == nil && ms != nil {
			s += fmt.Sprintf(" [%s", ms.HealthLabel)
			if ms.CombinedAPYPct != 0 {
				s += fmt.Sprintf(", APY %.1f%%", ms.CombinedAPYPct)
			}
			s += "]"
		}
		parts = append(parts, s)
	}

	return fmt.Sprintf("Active DN (%d): %s", len(plans), strings.Join(parts, "; ")), nil
}

func (d *dnContributor) Close() error { return d.store.Close() }
