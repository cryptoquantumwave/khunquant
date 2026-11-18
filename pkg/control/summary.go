package control

import (
	"fmt"
	"sort"
	"strings"
)

// StopSummary is what /stop did, built by code from what each step reported. It is
// the reply to the command and the body of POST /api/control/kill: counts and names
// only (workflow kinds, "buy BTC/USDT on okx"), never an id, a price, a credential
// or a line from the model.
type StopSummary struct {
	// Engaged: the kill flag is set. AlreadyEngaged: it was set before this run.
	Engaged        bool `json:"engaged"`
	AlreadyEngaged bool `json:"already_engaged,omitempty"`
	// Busy: another stop was still running, so this call did nothing but say so.
	Busy bool `json:"busy,omitempty"`
	// Pending: some steps did not finish inside the reply budget and go on in the
	// background; their outcome follows as a second message.
	Pending bool `json:"pending,omitempty"`

	TurnsCancelled int `json:"turns_cancelled"`
	// WorkflowsCancelled are kinds of workflows cancelled at their next step boundary
	// (WorkflowsStopping: cancel requested, not yet at a boundary).
	WorkflowsCancelled []string `json:"workflows_cancelled,omitempty"`
	WorkflowsStopping  []string `json:"workflows_stopping,omitempty"`
	// WorkflowsProtective keep running because they only reduce risk;
	// WorkflowsFinishing because cutting them would skip the steps that record an
	// order or hedge a leg. Neither places anything more.
	WorkflowsProtective []string `json:"workflows_protective,omitempty"`
	WorkflowsFinishing  []string `json:"workflows_finishing,omitempty"`

	// ProposalsCancelled are the pending proposals that could add risk (or move no
	// money) and were cancelled, with their kinds; ProposalsKept are the codes of the
	// pending exit and protective proposals left for the owner to decide.
	ProposalsCancelled      int      `json:"proposals_cancelled"`
	ProposalsCancelledKinds []string `json:"proposals_cancelled_kinds,omitempty"`
	ProposalsKept           []string `json:"proposals_kept,omitempty"`
	MandatesRevoked    int `json:"mandates_revoked"`

	OrdersCancelled []string `json:"orders_cancelled,omitempty"`
	// OrdersClosed were found already filled or closed: nothing to withdraw.
	OrdersClosed []string `json:"orders_closed,omitempty"`
	// ExitOrdersLeft are exits the agent placed (reduce-only): not withdrawn.
	ExitOrdersLeft []string `json:"exit_orders_left,omitempty"`
	// OrdersMayExist could not be shown to exist or not to: check the exchange.
	OrdersMayExist []string `json:"orders_may_exist,omitempty"`
	// Errors are steps that failed; later steps ran regardless.
	Errors []string `json:"errors,omitempty"`
	// Lang is the language Text is worded in ("th" or, by default, English).
	Lang string `json:"-"`
}

func (s StopSummary) clone() StopSummary {
	c := s
	for _, p := range []*[]string{&c.WorkflowsCancelled, &c.WorkflowsStopping, &c.WorkflowsProtective, &c.WorkflowsFinishing,
		&c.ProposalsCancelledKinds, &c.ProposalsKept, &c.OrdersCancelled, &c.OrdersClosed, &c.ExitOrdersLeft, &c.OrdersMayExist, &c.Errors} {
		*p = append([]string(nil), *p...)
	}
	return c
}

// Text is the owner's reply, in English or, when Lang is "th", in Thai.
func (s StopSummary) Text() string {
	th := s.Lang == "th"
	tr := func(en, thai string) string {
		if th {
			return thai
		}
		return en
	}
	if s.Busy {
		if !s.Engaged {
			return tr("A stop is already running, but the kill flag could not be set again. Try /stop again.",
				"กำลังสั่งหยุดอยู่แล้ว แต่ตั้งสวิตช์หยุดอีกครั้งไม่สำเร็จ ลองพิมพ์ /stop อีกครั้ง")
		}
		return tr("A stop is already running; the kill flag is engaged and its summary arrives in this chat when it ends.",
			"กำลังสั่งหยุดอยู่แล้ว สวิตช์หยุดทำงานอยู่ และจะส่งสรุปมาในแชทนี้เมื่อเสร็จ")
	}
	var b strings.Builder
	switch {
	case !s.Engaged:
		b.WriteString(tr("WARNING: the kill flag could not be set, so automatic actions may continue. Try /stop again. ",
			"คำเตือน: ตั้งสวิตช์หยุดไม่สำเร็จ การทำงานอัตโนมัติอาจยังดำเนินต่อ ลองพิมพ์ /stop อีกครั้ง "))
	case s.AlreadyEngaged:
		b.WriteString(tr("Stopped again (it was already stopped). ", "สั่งหยุดอีกครั้ง (หยุดอยู่แล้ว) "))
	default:
		b.WriteString(tr("Stopped. ", "หยุดแล้ว "))
	}
	if th {
		fmt.Fprintf(&b, "ยกเลิก %d เทิร์น, %d เวิร์กโฟลว์%s, %d ข้อเสนอ%s; เพิกถอน %d มานเดต; ยกเลิกคำสั่งที่เปิดอยู่ %d รายการ",
			s.TurnsCancelled, len(s.WorkflowsCancelled), kindList(s.WorkflowsCancelled),
			s.ProposalsCancelled, kindList(s.ProposalsCancelledKinds), s.MandatesRevoked, len(s.OrdersCancelled))
	} else {
		fmt.Fprintf(&b, "Cancelled %s, %s%s, %s%s; revoked %s; cancelled %s.",
			count(s.TurnsCancelled, "turn", "turns"), count(len(s.WorkflowsCancelled), "workflow", "workflows"), kindList(s.WorkflowsCancelled),
			count(s.ProposalsCancelled, "proposal", "proposals"), kindList(s.ProposalsCancelledKinds),
			count(s.MandatesRevoked, "mandate", "mandates"), count(len(s.OrdersCancelled), "open order", "open orders"))
	}
	line := func(prefix, thaiPrefix string, items []string) {
		if len(items) > 0 {
			b.WriteString("\n" + tr(prefix, thaiPrefix) + strings.Join(items, "; "))
		}
	}
	if n := len(s.ProposalsKept); n > 0 {
		codes := strings.Join(s.ProposalsKept, ", ")
		b.WriteString("\n" + tr(
			fmt.Sprintf("Kept %s (%s) so you can still close positions: approve or reject them as usual.", count(n, "exit proposal", "exit proposals"), codes),
			fmt.Sprintf("เก็บข้อเสนอปิดสถานะไว้ %d รายการ (%s) เพื่อให้คุณยังปิดสถานะได้: อนุมัติหรือปฏิเสธได้ตามปกติ", n, codes)))
	}
	line("Protective, still running: ", "ป้องกันความเสี่ยง ยังทำงานต่อ: ", countKinds(s.WorkflowsProtective))
	line("Finishing without placing anything more: ", "กำลังจบงานโดยไม่ส่งคำสั่งเพิ่ม: ", countKinds(s.WorkflowsFinishing))
	line("Still stopping at their next step: ", "กำลังหยุดที่ขั้นตอนถัดไป: ", countKinds(s.WorkflowsStopping))
	line("Cancelled orders: ", "คำสั่งที่ยกเลิก: ", s.OrdersCancelled)
	line("Already filled or closed, nothing to cancel: ", "จับคู่หรือปิดไปแล้ว ไม่มีอะไรให้ยกเลิก: ", s.OrdersClosed)
	line("Exit orders left in place (they reduce risk): ", "คำสั่งปิดสถานะที่คงไว้ (ลดความเสี่ยง): ", s.ExitOrdersLeft)
	line("May exist: check the exchange: ", "อาจมีอยู่ ตรวจสอบที่กระดานเทรด: ", s.OrdersMayExist)
	line("Did not complete: ", "ทำไม่สำเร็จ: ", s.Errors)
	if s.Pending {
		b.WriteString("\n" + tr("Some steps are still running; the rest follows in a second message.",
			"บางขั้นตอนยังทำงานอยู่ ส่วนที่เหลือจะตามมาในข้อความที่สอง"))
	}
	b.WriteString("\n" + tr(
		"Monitoring and notifications stay on. /resume allows new trades and automatic actions again; mandates must be approved again.",
		"การเฝ้าระวังและการแจ้งเตือนยังทำงานอยู่ /resume จะอนุญาตให้เทรดใหม่และทำงานอัตโนมัติได้อีกครั้ง ส่วนมานเดตต้องอนุมัติใหม่"))
	return b.String()
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// kindList is " (dca.execute, dn.open x2)" for a non-empty list of kinds.
func kindList(kinds []string) string {
	if len(kinds) == 0 {
		return ""
	}
	return " (" + strings.Join(countKinds(kinds), ", ") + ")"
}

func countKinds(kinds []string) []string {
	counts := map[string]int{}
	for _, k := range kinds {
		counts[k]++
	}
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, k := range names {
		if counts[k] > 1 {
			out = append(out, fmt.Sprintf("%s x%d", k, counts[k]))
		} else {
			out = append(out, k)
		}
	}
	return out
}
