package dca

import (
	"context"
	"testing"
	"time"
)

func TestNoteMutedSkip_OncePerDayAndCounts(t *testing.T) {
	st, id := storeWithPlan(t)
	ctx := context.Background()
	note := func(slot int64, day string) (bool, int) {
		t.Helper()
		tell, earlier, err := st.NoteMutedSkip(ctx, id, slot, day)
		if err != nil {
			t.Fatal(err)
		}
		return tell, earlier
	}
	if tell, e := note(1, "2026-10-12"); !tell || e != 0 {
		t.Fatalf("first skip: tell=%v earlier=%d", tell, e)
	}
	for _, slot := range []int64{2, 3} {
		if tell, _ := note(slot, "2026-10-12"); tell {
			t.Fatalf("slot %d the same day was told", slot)
		}
	}
	if tell, _ := note(1, "2026-10-13"); tell {
		t.Fatal("a slot already reported was told again")
	}
	if tell, e := note(4, "2026-10-13"); !tell || e != 2 {
		t.Fatalf("next day: tell=%v earlier=%d, want true and 2", tell, e)
	}
	if tell, _ := note(5, "2026-10-13"); tell {
		t.Fatal("second skip of the next day was told")
	}
	// Another plan has its own day.
	if tell, _, _ := st.NoteMutedSkip(ctx, id+1, 1, "2026-10-13"); !tell {
		t.Fatal("another plan's first skip was not told")
	}
}

// MutedSkipNotice is built by code: the exact text is the contract.
func TestMutedSkipNotice_Golden(t *testing.T) {
	slot := time.Date(2026, 10, 12, 9, 0, 0, 0, time.FixedZone("x", 7*3600))
	en := MutedSkipNotice(false, 5, "BTC/THB", slot, "Asia/Bangkok", 2, "2026-10-12")
	if en.Title != "DCA plan 5: buy skipped because of a mute" || en.Severity != "notice" || en.Silent || en.DedupKey != "dca-muted-skip:5:2026-10-12" ||
		en.Body != "The 2026-10-12 09:00 (Asia/Bangkok) slot of DCA plan 5 (BTC/THB) was skipped because you muted this alert. Nothing was bought. "+
			"To buy as usual, reply /unmute dca_due (or /unmute dca_due:plan-5 if you muted only this plan); a /snooze ends by itself. "+
			"2 earlier slot(s) were also skipped and not reported." {
		t.Errorf("en notice = %+v", en)
	}
	th := MutedSkipNotice(true, 5, "BTC/THB", slot, "Asia/Bangkok", 0, "2026-10-12")
	if th.Title != "แผน DCA 5: ข้ามการซื้อเพราะปิดเสียงแจ้งเตือน" ||
		th.Body != "รอบซื้อ 2026-10-12 09:00 (Asia/Bangkok) ของแผน DCA 5 (BTC/THB) ถูกข้าม เพราะคุณปิดเสียงการแจ้งเตือนนี้ไว้ ยังไม่มีการซื้อเกิดขึ้น "+
			"หากต้องการให้ซื้อตามปกติ ตอบ /unmute dca_due (หรือ /unmute dca_due:plan-5 ถ้าปิดเสียงเฉพาะแผนนี้) ส่วน /snooze จะหมดอายุเอง" {
		t.Errorf("th notice = %+v", th)
	}
}
