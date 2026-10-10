package config

import (
	"encoding/json"
	"fmt"
)

// CurrentConfigVersion is the schema version this build writes and understands.
//
// Version 1 is the shape the config already had when versioning was introduced,
// so the 0 -> 1 step ("baseline" in migrations.go) changes nothing but the
// recorded version. A recorded version lets a later migration know what it is
// reading instead of guessing from the data, as the unconditional conversions in
// migration.go (migrateChannelConfigs, providers -> model_list) have to.
//
// To change the schema: bump this constant and append the matching step to
// migrationSteps in migrations.go (a test fails if the chain has a gap), and
// state the effect for existing users in the MR.
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
// Deliberately separate from the migration steps: this records what shape was
// written, it does not change the shape. Loaders run migrationSteps but never
// save, so a migrated config reaches disk at the new version only on the next
// SaveConfig, through here. The legacy conversions in migration.go still run
// unconditionally on load, exactly as before.
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
