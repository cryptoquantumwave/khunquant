package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

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

type portfolioContributor struct {
	store     *snapshot.Store
	maxAssets int
}

func (p *portfolioContributor) Name() string { return "portfolio" }

func (p *portfolioContributor) Section(ctx context.Context) (string, error) {
	snapshots, err := p.store.QuerySnapshots(ctx, snapshot.QueryFilter{Limit: 1})
	if err != nil {
		return "", fmt.Errorf("query snapshots: %w", err)
	}
	if len(snapshots) == 0 {
		return "", nil
	}

	snap, err := p.store.GetSnapshot(ctx, snapshots[0].ID)
	if err != nil {
		return "", fmt.Errorf("get snapshot: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Portfolio (%s, as of %s): %.2f\n",
		snap.Quote, snap.TakenAt.Format("2006-01-02 15:04"), snap.TotalValue)

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
			accountPart = pos.Account + " "
		}
		fmt.Fprintf(&sb, "- %s%s: %.6g (%.2f %s)\n",
			accountPart, pos.Asset, pos.Quantity, pos.Value, snap.Quote)
	}

	return strings.TrimRight(sb.String(), "\n"), nil
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
