package config

import (
	"encoding/json"
	"testing"
)

func TestDefaultConfig_CronSchedulerDefaults(t *testing.T) {
	c := DefaultConfig().Tools.Cron
	if c.Workers != 4 {
		t.Errorf("Workers = %d, want 4", c.Workers)
	}
	if c.MisfireGraceMinutes != 60 {
		t.Errorf("MisfireGraceMinutes = %d, want 60", c.MisfireGraceMinutes)
	}
	if c.JobTimeoutMinutes != 10 {
		t.Errorf("JobTimeoutMinutes = %d, want 10", c.JobTimeoutMinutes)
	}
	// The pre-existing field keeps its meaning and default: it only bounds the
	// shell command of a command job.
	if c.ExecTimeoutMinutes != 5 {
		t.Errorf("ExecTimeoutMinutes = %d, want 5", c.ExecTimeoutMinutes)
	}
}

// A config file written before these fields existed must keep the defaults for
// the missing keys, and explicit values (including 0) must be honoured.
func TestCronConfig_JSONOverlay(t *testing.T) {
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"tools":{"cron":{"exec_timeout_minutes":7}}}`), cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.Tools.Cron
	if c.ExecTimeoutMinutes != 7 || c.Workers != 4 || c.MisfireGraceMinutes != 60 || c.JobTimeoutMinutes != 10 {
		t.Errorf("old-style config lost defaults: %+v", c)
	}

	cfg = DefaultConfig()
	raw := `{"tools":{"cron":{"workers":2,"misfire_grace_minutes":0,"job_timeout_minutes":0}}}`
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatal(err)
	}
	c = cfg.Tools.Cron
	if c.Workers != 2 || c.MisfireGraceMinutes != 0 || c.JobTimeoutMinutes != 0 {
		t.Errorf("explicit values not honoured: %+v", c)
	}
}
