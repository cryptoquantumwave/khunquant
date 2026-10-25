package config

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// recordingStep returns a From -> From+1 step that appends its name to *order
// and reports a note naming itself.
func recordingStep(from int, order *[]string) migrationStep {
	name := "step-" + string(rune('a'+from))
	return migrationStep{
		From: from,
		To:   from + 1,
		Name: name,
		Apply: func(_ map[string]any, _ *Config) ([]string, error) {
			*order = append(*order, name)
			return []string{name + " ran"}, nil
		},
	}
}

// setMigrationSteps swaps the package-level step list for one test. Tests that
// use it must not run in parallel.
func setMigrationSteps(t *testing.T, steps []migrationStep) {
	t.Helper()
	orig := migrationSteps
	migrationSteps = steps
	t.Cleanup(func() { migrationSteps = orig })
}

// A config already at the current version must be left exactly as it is, with
// no notes and no step invoked.
func TestRunMigrations_NoopWhenCurrent(t *testing.T) {
	cfg := DefaultConfig()
	before := DefaultConfig()

	notes, err := runMigrations(map[string]any{"version": float64(CurrentConfigVersion)}, cfg, CurrentConfigVersion)
	if err != nil {
		t.Fatalf("runMigrations() error = %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v, want none for a current config", notes)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Error("runMigrations modified a config that is already current")
	}

	// A step list that would fail if invoked proves nothing runs.
	var order []string
	steps := []migrationStep{recordingStep(0, &order), recordingStep(1, &order)}
	for _, onDisk := range []int{2, 3, 99} {
		notes, err = runMigrationSteps(steps, nil, cfg, onDisk, 2)
		if err != nil || len(notes) != 0 || len(order) != 0 {
			t.Errorf("onDisk=%d: notes=%v err=%v ran=%v, want a no-op", onDisk, notes, err, order)
		}
	}
}

func TestRunMigrations_AppliesChainInOrder(t *testing.T) {
	var order []string
	// Deliberately listed out of order: the chain is followed by version, not
	// by slice position.
	steps := []migrationStep{recordingStep(2, &order), recordingStep(0, &order), recordingStep(1, &order)}
	cfg := DefaultConfig()

	notes, err := runMigrationSteps(steps, nil, cfg, 0, 3)
	if err != nil {
		t.Fatalf("runMigrationSteps() error = %v", err)
	}
	if want := []string{"step-a", "step-b", "step-c"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if want := []string{"step-a ran", "step-b ran", "step-c ran"}; !reflect.DeepEqual(notes, want) {
		t.Errorf("notes = %v, want %v", notes, want)
	}

	// Starting part-way skips the steps already behind the config.
	order = nil
	notes, err = runMigrationSteps(steps, nil, cfg, 2, 3)
	if err != nil {
		t.Fatalf("runMigrationSteps(from 2) error = %v", err)
	}
	if want := []string{"step-c"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order from v2 = %v, want %v", order, want)
	}
	if want := []string{"step-c ran"}; !reflect.DeepEqual(notes, want) {
		t.Errorf("notes from v2 = %v, want %v", notes, want)
	}
}

func TestRunMigrations_StepErrorStopsAndReports(t *testing.T) {
	var order []string
	boom := errors.New("boom")
	steps := []migrationStep{
		recordingStep(0, &order),
		{
			From: 1, To: 2, Name: "explodes",
			Apply: func(_ map[string]any, _ *Config) ([]string, error) { return nil, boom },
		},
		recordingStep(2, &order),
	}

	notes, err := runMigrationSteps(steps, nil, DefaultConfig(), 0, 3)
	if err == nil {
		t.Fatal("a failing step did not fail the run")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the step's error", err)
	}
	if !strings.Contains(err.Error(), `"explodes"`) {
		t.Errorf("error = %q, want it to name the failing step", err.Error())
	}
	if want := []string{"step-a"}; !reflect.DeepEqual(order, want) {
		t.Errorf("steps run = %v, want only %v (nothing after the failure)", order, want)
	}
	if want := []string{"step-a ran"}; !reflect.DeepEqual(notes, want) {
		t.Errorf("notes = %v, want the notes gathered before the failure %v", notes, want)
	}
}

// A broken chain is reported before any step has touched the config.
func TestRunMigrations_InvalidChainRejectedBeforeApplying(t *testing.T) {
	var order []string
	cases := map[string][]migrationStep{
		"gap":            {recordingStep(0, &order), recordingStep(2, &order)},
		"skips version":  {{From: 0, To: 2, Name: "jump", Apply: recordingStep(0, &order).Apply}},
		"duplicate from": {recordingStep(0, &order), recordingStep(0, &order)},
		"nil apply":      {{From: 0, To: 1, Name: "empty"}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			order = nil
			_, err := runMigrationSteps(steps, nil, DefaultConfig(), 0, 3)
			if err == nil {
				t.Fatal("invalid chain was accepted")
			}
			if len(order) != 0 {
				t.Errorf("steps ran before the chain was validated: %v", order)
			}
		})
	}
}

// raw is the on-disk JSON, so a step can tell a field the user never wrote
// (cfg holds the default) from one they set explicitly to its zero value.
func TestRunMigrations_RawDistinguishesAbsentFromExplicit(t *testing.T) {
	// "heartbeat.enabled" defaults to a non-zero value only if DefaultConfig
	// says so; force a known default so the test does not depend on it.
	step := migrationStep{
		From: 0, To: 1, Name: "enabled-detect",
		Apply: func(raw map[string]any, cfg *Config) ([]string, error) {
			hb, _ := raw["heartbeat"].(map[string]any)
			if _, explicit := hb["enabled"]; explicit {
				return []string{"explicit"}, nil
			}
			cfg.Heartbeat.Enabled = true
			return []string{"absent"}, nil
		},
	}

	run := func(body string) (*Config, []string) {
		t.Helper()
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		cfg := DefaultConfig()
		cfg.Heartbeat.Enabled = false
		notes, err := runMigrationSteps([]migrationStep{step}, raw, cfg, 0, 1)
		if err != nil {
			t.Fatalf("runMigrationSteps() error = %v", err)
		}
		return cfg, notes
	}

	cfg, notes := run(`{"heartbeat": {"interval": 30}}`)
	if !reflect.DeepEqual(notes, []string{"absent"}) || !cfg.Heartbeat.Enabled {
		t.Errorf("absent key: notes=%v enabled=%v, want [absent] and the step to act", notes, cfg.Heartbeat.Enabled)
	}

	cfg, notes = run(`{"heartbeat": {"enabled": false}}`)
	if !reflect.DeepEqual(notes, []string{"explicit"}) || cfg.Heartbeat.Enabled {
		t.Errorf("explicit false: notes=%v enabled=%v, want [explicit] and the value left alone",
			notes, cfg.Heartbeat.Enabled)
	}

	// A nil raw (e.g. a document that is not an object) reads as "all absent".
	cfg = DefaultConfig()
	cfg.Heartbeat.Enabled = false
	notes, err := runMigrationSteps([]migrationStep{step}, nil, cfg, 0, 1)
	if err != nil || !reflect.DeepEqual(notes, []string{"absent"}) {
		t.Errorf("nil raw: notes=%v err=%v, want [absent]", notes, err)
	}
}

// Bumping CurrentConfigVersion without appending a step must fail here, not
// surface as every existing user's config refusing to load.
func TestMigrationSteps_ChainIsContiguousToCurrent(t *testing.T) {
	if len(migrationSteps) == 0 {
		t.Fatal("migrationSteps is empty")
	}
	byFrom := make(map[int]migrationStep, len(migrationSteps))
	for _, s := range migrationSteps {
		if s.Name == "" {
			t.Errorf("step %d -> %d has no Name", s.From, s.To)
		}
		if s.Apply == nil {
			t.Errorf("step %q has no Apply", s.Name)
		}
		if s.To != s.From+1 {
			t.Errorf("step %q goes %d -> %d, want exactly one version", s.Name, s.From, s.To)
		}
		if prev, dup := byFrom[s.From]; dup {
			t.Errorf("steps %q and %q both start at version %d", prev.Name, s.Name, s.From)
		}
		byFrom[s.From] = s
	}
	for v := legacyConfigVersion; v < CurrentConfigVersion; v++ {
		if _, ok := byFrom[v]; !ok {
			t.Errorf("no migration step from version %d to %d: bumped CurrentConfigVersion to %d without adding one",
				v, v+1, CurrentConfigVersion)
		}
	}
	for from, s := range byFrom {
		if s.To > CurrentConfigVersion {
			t.Errorf("step %q (from %d) targets version %d beyond CurrentConfigVersion %d",
				s.Name, from, s.To, CurrentConfigVersion)
		}
	}

	// The real list must run cleanly from every version it claims to cover.
	for v := legacyConfigVersion; v <= CurrentConfigVersion; v++ {
		if _, err := runMigrations(map[string]any{}, DefaultConfig(), v); err != nil {
			t.Errorf("runMigrations from version %d: %v", v, err)
		}
	}
}

const legacyConfigBody = `{
  "agents": {"defaults": {"model_name": "claude-sonnet-5", "max_tokens": 4096}},
  "gateway": {"host": "127.0.0.1", "port": 18790},
  "heartbeat": {"enabled": true, "interval": 45},
  "trading_risk": {"allow_leverage": true}
}`

// Legacy file -> baseline step runs -> save records the current version ->
// reload is stable, and nothing else about the config changes.
func TestLoadConfig_LegacyVersion0_RunsBaselineAndSavesCurrent(t *testing.T) {
	var ran []string
	baseline := migrationSteps[0]
	setMigrationSteps(t, []migrationStep{{
		From: baseline.From, To: baseline.To, Name: baseline.Name,
		Apply: func(raw map[string]any, cfg *Config) ([]string, error) {
			if _, has := raw["version"]; has {
				t.Error(`raw has a "version" key for a legacy file`)
			}
			ran = append(ran, baseline.Name)
			return baseline.Apply(raw, cfg)
		},
	}})

	path := writeRawConfig(t, legacyConfigBody)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"baseline"}) {
		t.Fatalf("steps run on load = %v, want [baseline]", ran)
	}
	if cfg.Version != legacyConfigVersion {
		t.Errorf("loaded Version = %d, want %d: the loader must not stamp the version, SaveConfig does",
			cfg.Version, legacyConfigVersion)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(onDisk) != legacyConfigBody {
		t.Error("LoadConfig wrote to the config file; only SaveConfig may persist the new version")
	}

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	ran = nil
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	if reloaded.Version != CurrentConfigVersion {
		t.Errorf("reloaded Version = %d, want %d", reloaded.Version, CurrentConfigVersion)
	}
	if len(ran) != 0 {
		t.Errorf("steps ran again on a current-version file: %v", ran)
	}
	if reloaded.Agents.Defaults.ModelName != "claude-sonnet-5" ||
		reloaded.Agents.Defaults.MaxTokens != 4096 ||
		reloaded.Gateway.Port != 18790 ||
		reloaded.Heartbeat.Interval != 45 ||
		!reloaded.Heartbeat.Enabled ||
		!reloaded.TradingRisk.AllowLeverage {
		t.Errorf("settings did not round-trip: %+v", reloaded)
	}
	// SaveConfig stamped cfg in place, so everything else must now match.
	if !reflect.DeepEqual(cfg, reloaded) {
		t.Error("reloaded config differs from the config that was saved")
	}
}

// A current-version file must load without any step running and without notes.
func TestLoadConfig_CurrentVersion_RunsNoMigrations(t *testing.T) {
	path := writeRawConfig(t, `{"version": 1, "heartbeat": {"interval": 45}}`)
	setMigrationSteps(t, []migrationStep{{
		From: 0, To: 1, Name: "must-not-run",
		Apply: func(_ map[string]any, _ *Config) ([]string, error) {
			t.Error("a step ran for a current-version config")
			return nil, nil
		},
	}})

	for name, load := range map[string]func(string) (*Config, error){
		"LoadConfig":             LoadConfig,
		"LoadConfigSkipSecurity": LoadConfigSkipSecurity,
	} {
		cfg, err := load(path)
		if err != nil {
			t.Fatalf("%s() error = %v", name, err)
		}
		if cfg.Version != CurrentConfigVersion || cfg.Heartbeat.Interval != 45 {
			t.Errorf("%s: Version=%d Interval=%d, want %d and 45", name, cfg.Version,
				cfg.Heartbeat.Interval, CurrentConfigVersion)
		}
	}
}

func TestLoadConfigSkipSecurity_RunsMigrations(t *testing.T) {
	var gotRaw map[string]any
	setMigrationSteps(t, []migrationStep{{
		From: 0, To: 1, Name: "marker",
		Apply: func(raw map[string]any, cfg *Config) ([]string, error) {
			gotRaw = raw
			cfg.Heartbeat.Interval = 77
			return []string{"marked"}, nil
		},
	}})

	path := writeRawConfig(t, legacyConfigBody)
	cfg, err := LoadConfigSkipSecurity(path)
	if err != nil {
		t.Fatalf("LoadConfigSkipSecurity() error = %v", err)
	}
	if cfg.Heartbeat.Interval != 77 {
		t.Errorf("Interval = %d, want 77: the migration step did not run", cfg.Heartbeat.Interval)
	}
	if _, ok := gotRaw["gateway"]; !ok {
		t.Errorf("step was not given the decoded on-disk JSON: %v", gotRaw)
	}
	if cfg.Agents.Defaults.ModelName != "claude-sonnet-5" {
		t.Errorf("ModelName = %q, want it preserved", cfg.Agents.Defaults.ModelName)
	}
}

// The live bug: LoadConfigSkipSecurity's callers go on to save, and a save
// stamps this build's version over whatever the file said.
func TestLoadConfigSkipSecurity_RefusesConfigFromNewerBuild(t *testing.T) {
	path := writeRawConfig(t, `{
	  "version": 99,
	  "agents": {"defaults": {"model_name": "some-future-model"}}
	}`)

	_, err := LoadConfigSkipSecurity(path)
	if err == nil {
		t.Fatal("a config from a newer build was loaded; saving it would downgrade it silently")
	}
	if !errors.Is(err, ErrConfigTooNew) {
		t.Errorf("error = %v, want it to wrap ErrConfigTooNew", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error = %q, want it to name the version found", err.Error())
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read back: %v", readErr)
	}
	if !strings.Contains(string(raw), "some-future-model") {
		t.Error("the refused config was modified on disk")
	}
}

// A failing step must fail the load in both loaders: the config may be
// half-migrated, and carrying on would let a later save persist it.
func TestLoadConfig_MigrationFailureFailsLoad(t *testing.T) {
	boom := errors.New("boom")
	setMigrationSteps(t, []migrationStep{{
		From: 0, To: 1, Name: "explodes",
		Apply: func(_ map[string]any, cfg *Config) ([]string, error) {
			cfg.Heartbeat.Interval = 1234 // partial mutation before failing
			return nil, boom
		},
	}})

	path := writeRawConfig(t, legacyConfigBody)
	for name, load := range map[string]func(string) (*Config, error){
		"LoadConfig":             LoadConfig,
		"LoadConfigSkipSecurity": LoadConfigSkipSecurity,
	} {
		cfg, err := load(path)
		if err == nil {
			t.Fatalf("%s: a failing migration did not fail the load", name)
		}
		if cfg != nil {
			t.Errorf("%s: returned a config alongside the error", name)
		}
		if !errors.Is(err, boom) {
			t.Errorf("%s: error = %v, want it to wrap the step's error", name, err)
		}
		if !strings.Contains(err.Error(), `"explodes"`) {
			t.Errorf("%s: error = %q, want it to name the step", name, err.Error())
		}
	}
}

// Inputs that are not a usable config behave as they did before migrations.
func TestLoadConfig_NonObjectInputsUnchanged(t *testing.T) {
	missing := t.TempDir() + "/absent.json"
	for name, load := range map[string]func(string) (*Config, error){
		"LoadConfig":             LoadConfig,
		"LoadConfigSkipSecurity": LoadConfigSkipSecurity,
	} {
		cfg, err := load(missing)
		if err != nil || cfg == nil || cfg.Version != CurrentConfigVersion {
			t.Errorf("%s(missing): cfg=%v err=%v, want the default config", name, cfg, err)
		}
		if _, err := load(writeRawConfig(t, ``)); err == nil {
			t.Errorf("%s(empty file): want the JSON error it always returned", name)
		}
		if _, err := load(writeRawConfig(t, `[1, 2]`)); err == nil {
			t.Errorf("%s(array): want the JSON error it always returned", name)
		}
		if _, err := load(writeRawConfig(t, `{not json`)); err == nil {
			t.Errorf("%s(garbage): want the JSON error it always returned", name)
		}
		// A literal null decodes to nothing; it must still load as legacy.
		if cfg, err := load(writeRawConfig(t, `null`)); err != nil || cfg.Version != legacyConfigVersion {
			t.Errorf("%s(null): cfg=%v err=%v, want it to load as a legacy config", name, cfg, err)
		}
	}
}

// resetMigrationLogGuard clears the once-per-process log guard now and again
// at cleanup, so tests stay independent of each other and of run order.
func resetMigrationLogGuard(t *testing.T) {
	t.Helper()
	reset := func() {
		loggedMigrations.Range(func(k, _ any) bool {
			loggedMigrations.Delete(k)
			return true
		})
	}
	reset()
	t.Cleanup(reset)
}

// Only the logging is deduplicated: the in-memory config must be migrated on
// every load, but the WARN fires once per (path, from -> to) per process.
func TestMigrateLoadedConfig_LogsNotesOncePerPath(t *testing.T) {
	resetMigrationLogGuard(t)
	notes := []string{"n"}

	if !logMigrationNotes("a.json", 0, 1, notes) {
		t.Error("first sighting of a transition was not logged")
	}
	if logMigrationNotes("a.json", 0, 1, notes) {
		t.Error("the same path and transition was logged twice")
	}
	if !logMigrationNotes("b.json", 0, 1, notes) {
		t.Error("a different path was suppressed")
	}
	if !logMigrationNotes("a.json", 1, 2, notes) {
		t.Error("a different version transition was suppressed")
	}
	if logMigrationNotes("c.json", 0, 1, nil) {
		t.Error("an empty note list reported a log")
	}
	if !logMigrationNotes("c.json", 0, 1, notes) {
		t.Error("an empty note list consumed the guard for that transition")
	}

	// Through the real loader: the step runs on every load, the guard holds.
	resetMigrationLogGuard(t)
	runs := 0
	setMigrationSteps(t, []migrationStep{{
		From: 0, To: 1, Name: "counting",
		Apply: func(_ map[string]any, cfg *Config) ([]string, error) {
			runs++
			cfg.Heartbeat.Interval = 77
			return []string{"counted"}, nil
		},
	}})
	path := writeRawConfig(t, legacyConfigBody)
	for i := 1; i <= 3; i++ {
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if cfg.Heartbeat.Interval != 77 {
			t.Errorf("load %d: config was not migrated in memory", i)
		}
	}
	if runs != 3 {
		t.Errorf("step ran %d times over 3 loads, want 3", runs)
	}
	if _, recorded := loggedMigrations.Load(migrationLogKey{path, 0, 1}); !recorded {
		t.Error("the loader did not record the transition in the guard")
	}
}
