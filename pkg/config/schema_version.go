package config

import (
	"encoding/json"
	"fmt"
)

// CurrentConfigVersion is the schema version this build writes and understands.
//
// Version 1 is the shape the config already had when versioning was introduced,
// so the 0 → 1 transition migrates nothing. The point of the field is future
// changes: today the migrations in this package infer what needs converting by
// inspecting the data (see migrateChannelConfigs and the providers → model_list
// conversion), which means they run on every load and have to keep guessing.
// A recorded version lets a later migration know what it is reading instead.
const CurrentConfigVersion = 1

// legacyConfigVersion is what an absent "version" key means: a config written
// before the field existed.
const legacyConfigVersion = 0

// ErrConfigTooNew reports a config written by a newer build than this one.
var ErrConfigTooNew = fmt.Errorf("config was written by a newer version of khunquant")

// checkConfigVersion decides whether a loaded config can be used.
//
// A config from the future is refused rather than loaded. The alternative —
// loading it, silently dropping the fields this build does not know, and
// writing it back at our own version — destroys settings a newer build wrote,
// and does so invisibly. Refusing is recoverable; a silent downgrade is not.
func checkConfigVersion(version int) error {
	if version > CurrentConfigVersion {
		return fmt.Errorf("%w (config version %d, this build understands %d)",
			ErrConfigTooNew, version, CurrentConfigVersion)
	}
	return nil
}

// applyConfigVersion stamps the current version onto a config being saved.
//
// Deliberately separate from the migration functions: this records what shape
// was written, it does not change the shape. The existing migrations still run
// unconditionally on load, exactly as before — gating those on version is a
// change with real user-data risk and buys nothing until there is a second
// version to gate against.
func applyConfigVersion(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.Version = CurrentConfigVersion
}

// onDiskConfigVersion reports the version recorded in raw config bytes, or
// legacyConfigVersion when the key is absent or the bytes do not parse.
//
// Read separately from the main unmarshal because that overlays onto
// DefaultConfig, whose version is already current: an absent key would leave
// the default in place and a legacy config would look like a current one.
func onDiskConfigVersion(data []byte) int {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return legacyConfigVersion
	}
	return probe.Version
}
