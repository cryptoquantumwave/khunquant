package triggers

import (
	"context"
	"strings"
	"testing"
)

// mandated switches the env's plan-7 spec to the given mode with a mandate id.
func (d *dcaEnv) mandated(mode string) {
	d.t.Helper()
	p := dcaParamsFor(nil)
	p.MandateID = "mnd_01J0"
	sp := Spec{ID: "plan-7", Kind: KindDCADue, Origin: "test", Params: p.JSON(), Mode: mode}
	if err := d.r.Update(context.Background(), sp); err != nil {
		d.t.Fatal(err)
	}
}

// A plan with a mandate gets a LIVE candidate that asks for the dca.execute workflow;
// a plan without one stays shadow, whatever the spec says.
func TestDCADue_MandatedPlanMakesALiveCandidate(t *testing.T) {
	d := newDCAEnv(t, nil)
	d.mandated(ModeLive)
	cs := d.due(7, health(dcaSlot))
	if len(cs) != 1 {
		t.Fatalf("%d candidates, want 1", len(cs))
	}
	c := cs[0]
	if c.Shadow {
		t.Error("a mandated plan's candidate is shadow")
	}
	if c.Evidence["action"] != "execute_dca" || c.Evidence["mandate_id"] != "mnd_01J0" || c.Evidence["plan_id"] != int64(7) ||
		c.Evidence["due_at_ms"] != dcaSlot.UnixMilli() || c.Evidence["shadow"] != false {
		t.Errorf("evidence = %v", c.Evidence)
	}
	if strings.Contains(c.Title, "no order") || strings.Contains(c.Detail, "places none") {
		t.Errorf("a live candidate says it places nothing: %q / %q", c.Title, c.Detail)
	}
	if c.TriggerID != "dca_due:plan-7" {
		t.Errorf("trigger id = %q", c.TriggerID)
	}
}

func TestDCADue_MandatedPlanInAShadowSpecIsStillShadow(t *testing.T) {
	d := newDCAEnv(t, nil)
	d.mandated(ModeShadow)
	cs := d.due(7, health(dcaSlot))
	if len(cs) != 1 || !cs[0].Shadow {
		t.Fatalf("candidates = %+v; a shadow spec must stay shadow", cs)
	}
}

func TestDCADue_MandatedPlanUnderAShadowGlobalModeIsStillShadow(t *testing.T) {
	d := newDCAEnv(t, nil)
	d.mandated(ModeLive)
	d.setGlobal(ModeShadow)
	cs := d.due(7, health(dcaSlot))
	if len(cs) != 1 || !cs[0].Shadow {
		t.Fatalf("candidates = %+v; the global mode must keep it shadow", cs)
	}
}
