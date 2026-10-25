package cron

import (
	"path/filepath"
	"testing"
)

func newMigSvc(t *testing.T) (*CronService, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cron", "jobs.json")
	cs := NewCronService(path, nil)
	return cs, path
}

func addEvery(t *testing.T, cs *CronService, name string) *CronJob {
	t.Helper()
	j, err := cs.AddJob(name, CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "payload", false, "telegram", "1")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestDisableMigrated_KeepsTheJobAndMarksIt(t *testing.T) {
	cs, path := newMigSvc(t)
	j := addEvery(t, cs, "price_alert:x")

	if err := cs.DisableMigrated(j.ID, CronMigration{Target: "spec-1", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	got := cs.GetJob(j.ID)
	if got == nil || got.Enabled || got.State.NextRunAtMS != nil {
		t.Fatalf("job = %+v, want kept, disabled and unscheduled", got)
	}
	if got.State.Migrated == nil || got.State.Migrated.Target != "spec-1" || got.State.Migrated.Fingerprint != "fp" {
		t.Fatalf("marker = %+v", got.State.Migrated)
	}
	if len(cs.ListJobs(false)) != 0 || len(cs.ListJobs(true)) != 1 {
		t.Fatal("a disabled job must be hidden from the enabled listing and kept in the full one")
	}

	// The marker is on disk: another process (a generation that does not run the
	// registry) finds it.
	reloaded := NewCronService(path, nil)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if g := reloaded.GetJob(j.ID); g == nil || g.State.Migrated == nil || g.Enabled {
		t.Fatalf("reloaded job = %+v", g)
	}

	// UpdateJob (an edit by someone holding a stale copy) keeps the marker.
	edit := *got
	edit.Name = "renamed"
	if err := cs.UpdateJob(&edit); err != nil {
		t.Fatal(err)
	}
	if g := cs.GetJob(j.ID); g.State.Migrated == nil || g.Name != "renamed" {
		t.Fatalf("after UpdateJob: %+v", g)
	}

	if err := cs.DisableMigrated("nope", CronMigration{}); err == nil {
		t.Fatal("a missing job must be an error")
	}
}

func TestRestoreMigrated(t *testing.T) {
	t.Run("unchanged job is switched back on", func(t *testing.T) {
		cs, _ := newMigSvc(t)
		j := addEvery(t, cs, "price_alert:x")
		if err := cs.DisableMigrated(j.ID, CronMigration{Target: "s", Fingerprint: "fp"}); err != nil {
			t.Fatal(err)
		}
		out, err := cs.RestoreMigrated(j.ID, func(job CronJob) bool { return job.State.Migrated.Fingerprint == "fp" })
		if err != nil || out != RestoreEnabled {
			t.Fatalf("outcome = %v, %v", out, err)
		}
		got := cs.GetJob(j.ID)
		if !got.Enabled || got.State.Migrated != nil || got.State.NextRunAtMS == nil {
			t.Fatalf("job = %+v, want enabled, unmarked and scheduled", got)
		}
	})
	t.Run("a changed job is left as it is", func(t *testing.T) {
		cs, _ := newMigSvc(t)
		j := addEvery(t, cs, "price_alert:x")
		_ = cs.DisableMigrated(j.ID, CronMigration{Target: "s", Fingerprint: "fp"})
		out, err := cs.RestoreMigrated(j.ID, func(CronJob) bool { return false })
		if err != nil || out != RestoreLeft {
			t.Fatalf("outcome = %v, %v", out, err)
		}
		got := cs.GetJob(j.ID)
		if got.Enabled || got.State.Migrated != nil {
			t.Fatalf("job = %+v, want still disabled, marker cleared", got)
		}
	})
	t.Run("a job the user switched on again is left on", func(t *testing.T) {
		cs, _ := newMigSvc(t)
		j := addEvery(t, cs, "price_alert:x")
		_ = cs.DisableMigrated(j.ID, CronMigration{Target: "s", Fingerprint: "fp"})
		on := cs.GetJob(j.ID)
		on.Enabled = true
		if err := cs.UpdateJob(on); err != nil {
			t.Fatal(err)
		}
		out, _ := cs.RestoreMigrated(j.ID, func(CronJob) bool { t.Fatal("must not ask about an enabled job"); return true })
		if out != RestoreLeft || !cs.GetJob(j.ID).Enabled || cs.GetJob(j.ID).State.Migrated != nil {
			t.Fatalf("outcome = %v, job = %+v", out, cs.GetJob(j.ID))
		}
	})
	t.Run("no marker, no job: nothing", func(t *testing.T) {
		cs, _ := newMigSvc(t)
		j := addEvery(t, cs, "price_alert:x")
		if out, _ := cs.RestoreMigrated(j.ID, func(CronJob) bool { return true }); out != RestoreNone {
			t.Fatalf("outcome = %v", out)
		}
		if out, _ := cs.RestoreMigrated("missing", func(CronJob) bool { return true }); out != RestoreNone {
			t.Fatalf("outcome = %v", out)
		}
		if !cs.GetJob(j.ID).Enabled {
			t.Fatal("an unmarked job must be untouched")
		}
	})
}

func TestMigratedMarker_CloneIsDeep(t *testing.T) {
	cs, _ := newMigSvc(t)
	j := addEvery(t, cs, "price_alert:x")
	_ = cs.DisableMigrated(j.ID, CronMigration{Target: "s", Fingerprint: "fp"})
	a := cs.GetJob(j.ID)
	a.State.Migrated.Fingerprint = "tampered"
	if b := cs.GetJob(j.ID); b.State.Migrated.Fingerprint != "fp" {
		t.Fatal("a caller changed the stored marker through a copy")
	}
}
