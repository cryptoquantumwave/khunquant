package config

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// migrationStep is one versioned transformation of a loaded config, taking it
// from schema version From to To (always From+1).
//
// Steps answer "what does an existing user's config look like after this build
// is installed". They run once per version bump, in order, and only for configs
// whose recorded version is older than CurrentConfigVersion. They are distinct
// from the legacy conversions in migration.go, which infer what to convert from
// the data itself and run unconditionally on every load.
//
// To add a step:
//  1. bump CurrentConfigVersion in schema_version.go;
//  2. append a step {From: old, To: new} to migrationSteps;
//  3. state the effect for existing users in the MR description.
//
// TestMigrationSteps_ChainIsContiguousToCurrent fails if (1) is done without (2).
type migrationStep struct {
	From, To int
	Name     string
	// Apply mutates cfg in place; raw is the decoded on-disk JSON so steps can
	// tell "field absent" from "field explicitly set". Returns human-readable
	// notes, which the loader logs at WARN.
	Apply func(raw map[string]any, cfg *Config) (notes []string, err error)
}

// migrationSteps is the production step list.
var migrationSteps = []migrationStep{
	{
		// Configs written before the "version" key existed read as version 0.
		// Nothing about their shape changes; this step exists so that path goes
		// through the framework and the next save records the current version.
		From: legacyConfigVersion,
		To:   1,
		Name: "baseline",
		Apply: func(_ map[string]any, _ *Config) ([]string, error) {
			return []string{"config schema version recorded"}, nil
		},
	},
}

// runMigrations brings cfg from the on-disk schema version up to
// CurrentConfigVersion using migrationSteps. See runMigrationSteps.
func runMigrations(raw map[string]any, cfg *Config, onDisk int) ([]string, error) {
	return runMigrationSteps(migrationSteps, raw, cfg, onDisk, CurrentConfigVersion)
}

// runMigrationSteps applies, in order, the chain of steps leading from onDisk
// to target. It is a no-op when onDisk >= target: a config from a newer build
// is refused earlier, by checkConfigVersion, and never reaches here.
//
// The chain is planned in full before any step runs, so a gap, a duplicate or a
// step that is not From -> From+1 is reported without having touched cfg. If a
// step itself fails, cfg may be partially mutated and the notes so far are
// returned alongside the error: the caller must fail the load rather than carry
// on with a half-migrated config.
func runMigrationSteps(
	steps []migrationStep, raw map[string]any, cfg *Config, onDisk, target int,
) ([]string, error) {
	if onDisk < legacyConfigVersion {
		onDisk = legacyConfigVersion
	}
	if onDisk >= target {
		return nil, nil
	}

	byFrom := make(map[int]migrationStep, len(steps))
	for _, s := range steps {
		if s.To != s.From+1 {
			return nil, fmt.Errorf("migration %q: invalid step %d -> %d (must advance by exactly one version)",
				s.Name, s.From, s.To)
		}
		if prev, dup := byFrom[s.From]; dup {
			return nil, fmt.Errorf("migrations %q and %q both start at version %d", prev.Name, s.Name, s.From)
		}
		byFrom[s.From] = s
	}

	plan := make([]migrationStep, 0, target-onDisk)
	for v := onDisk; v < target; v++ {
		s, ok := byFrom[v]
		if !ok {
			return nil, fmt.Errorf("no migration from config version %d to %d", v, v+1)
		}
		if s.Apply == nil {
			return nil, fmt.Errorf("migration %q (%d -> %d) has no Apply function", s.Name, s.From, s.To)
		}
		plan = append(plan, s)
	}

	if raw == nil {
		raw = map[string]any{}
	}
	var notes []string
	for _, s := range plan {
		stepNotes, err := s.Apply(raw, cfg)
		if err != nil {
			return notes, fmt.Errorf("migration %q (%d -> %d): %w", s.Name, s.From, s.To, err)
		}
		notes = append(notes, stepNotes...)
	}
	return notes, nil
}

// migrateLoadedConfig runs the versioned migrations for a config just loaded
// from data and logs what they did. Shared by both loaders so they cannot drift.
//
// It never writes: the new version is persisted by the next SaveConfig, via
// applyConfigVersion.
func migrateLoadedConfig(path string, data []byte, cfg *Config, onDisk int) error {
	if onDisk >= CurrentConfigVersion {
		return nil
	}

	// data already unmarshalled cleanly into Config, so it is valid JSON; a
	// non-object document (e.g. null) just yields a nil map, which the runner
	// replaces with an empty one.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		raw = nil
	}

	notes, err := runMigrations(raw, cfg, onDisk)
	if err != nil {
		return fmt.Errorf("migrating config %s from version %d: %w", path, onDisk, err)
	}
	logMigrationNotes(path, onDisk, CurrentConfigVersion, notes)
	return nil
}

// migrationLogKey identifies one config file at one version transition.
type migrationLogKey struct {
	path     string
	from, to int
}

// loggedMigrations records which transitions have already been logged in this
// process. Loaders never save, so a config that is not re-saved is migrated in
// memory on every load, and the web backend loads config per request: without
// this, each of those loads would repeat the same WARN. Only the logging is
// deduplicated; the migration itself still runs every time.
var loggedMigrations sync.Map

// logMigrationNotes logs notes at WARN the first time a (path, from -> to)
// transition is seen in this process, and reports whether it logged.
func logMigrationNotes(path string, from, to int, notes []string) bool {
	if len(notes) == 0 {
		return false
	}
	if _, seen := loggedMigrations.LoadOrStore(migrationLogKey{path, from, to}, struct{}{}); seen {
		return false
	}
	for _, note := range notes {
		logger.WarnF("config migrated", map[string]any{
			"note":         note,
			"path":         path,
			"from_version": from,
			"to_version":   to,
		})
	}
	return true
}
