package dca

import (
	"fmt"
	"strconv"
	"time"
)

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
func fg(v float64) string { return strconv.FormatFloat(v, 'g', 8, 64) }

// notice words the owner-facing message of a run's end. It is a template over ids,
// numbers and fixed phrases: no model text and no text a person typed.
func (d *Deps) notice(planID, symbol string, chk checkOut, res recordOut) Notice {
	th := d.thai()
	key := "dca-exec:" + planID + ":" + res.Result
	switch res.Result {
	case resultDone:
		q := chk.Quote
		avg := res.Fill.AvgPrice
		if avg <= 0 {
			avg = q.Price
		}
		spent := res.Fill.Filled * avg
		tag := ""
		if res.Fill.Paper {
			tag = "[paper] "
		}
		if th {
			return Notice{Severity: "info", Silent: true, DedupKey: key,
				Title: fmt.Sprintf("%sแผน DCA %s: ซื้อ %s แล้ว", tag, planID, symbol),
				Body: fmt.Sprintf("ซื้อ %s %s ที่ราคา %s (รวม %s %s, ประมาณ $%s) ใช้ไปแล้ว $%s จากวงเงิน%s นี้",
					fg(res.Fill.Filled), symbol, fg(avg), fg(spent), q.QuoteCcy, f2(res.USD), f2(chk.Used+res.USD), chk.PeriodN)}
		}
		return Notice{Severity: "info", Silent: true, DedupKey: key,
			Title: fmt.Sprintf("%sDCA plan %s: bought %s", tag, planID, symbol),
			Body: fmt.Sprintf("Bought %s %s at %s (total %s %s, about $%s). Used $%s of this %s's limit so far.",
				fg(res.Fill.Filled), symbol, fg(avg), fg(spent), q.QuoteCcy, f2(res.USD), f2(chk.Used+res.USD), chk.PeriodN)}
	case resultUnproven:
		if th {
			return Notice{Severity: "warning", DedupKey: key,
				Title: fmt.Sprintf("แผน DCA %s: ยืนยันคำสั่งไม่ได้ - ตรวจที่ exchange", planID),
				Body: "คำสั่งซื้ออาจถูกส่งไปแล้วหรือไม่ก็ได้ (" + res.Reason + ") ระบบจะไม่ซื้อซ้ำ และรอบถัดไปจะถามคุณก่อนจนกว่าคุณจะตรวจและอนุมัติวงเงินใหม่"}
		}
		return Notice{Severity: "warning", DedupKey: key,
			Title: fmt.Sprintf("DCA plan %s: the order could not be proven - check the exchange", planID),
			Body: "The buy order MAY OR MAY NOT HAVE BEEN PLACED (" + res.Reason + "). Nothing will be bought again for this slot, " +
				"and the next due slot will ask you first until you have looked and approved a new mandate."}
	case resultPending:
		if th {
			return Notice{Severity: "warning", DedupKey: key,
				Title: fmt.Sprintf("แผน DCA %s: คำสั่งซื้อยังไม่เสร็จ - ตรวจที่ exchange", planID),
				Body: "คำสั่งซื้อถูกส่งแล้วแต่ยังไม่เสร็จสมบูรณ์: " + res.Reason + " (คิดเป็นประมาณ $" + f2(res.USD) +
					" ของวงเงิน) ระบบจะไม่ยกเลิกคำสั่งเอง และจะไม่ซื้อซ้ำ รอบถัดไปจะถามคุณก่อนจนกว่าคุณจะตรวจและอนุมัติวงเงินใหม่"}
		}
		return Notice{Severity: "warning", DedupKey: key,
			Title: fmt.Sprintf("DCA plan %s: the order is still open - check the exchange", planID),
			Body: "The buy order was placed but is not finished: " + res.Reason + " (about $" + f2(res.USD) + " of the limit). " +
				"It is not cancelled automatically, nothing will be bought again, and the next due slot will ask you first until you have looked and approved a new mandate."}
	case resultFailed:
		if th {
			return Notice{Severity: "notice", DedupKey: key,
				Title: fmt.Sprintf("แผน DCA %s: ไม่ได้ซื้อ", planID), Body: "ไม่มีการซื้อเกิดขึ้น: " + res.Reason}
		}
		return Notice{Severity: "notice", DedupKey: key,
			Title: fmt.Sprintf("DCA plan %s: nothing bought", planID), Body: "Nothing was bought: " + res.Reason}
	}
	// refused (and a question that could not be made)
	if th {
		return Notice{Severity: "notice", DedupKey: key,
			Title: fmt.Sprintf("แผน DCA %s: ไม่ได้ส่งคำสั่ง", planID), Body: "ไม่มีการซื้อเกิดขึ้น: " + res.Reason}
	}
	return Notice{Severity: "notice", DedupKey: key,
		Title: fmt.Sprintf("DCA plan %s: not executed", planID), Body: "Nothing was bought: " + res.Reason}
}

// ReminderNotice is the one reminder sent when a mandate is still pending after 24 h.
func ReminderNotice(thai bool, planID int64, code string) Notice {
	if thai {
		return Notice{Severity: "notice", DedupKey: "dca-mandate-reminder:" + strconv.FormatInt(planID, 10),
			Title: fmt.Sprintf("แผน DCA %d ยังรอการอนุมัติวงเงิน", planID),
			Body:  "แผนนี้ยังทำงานแบบเดิม (ผ่านงานตามเวลา) จนกว่าคุณจะอนุมัติวงเงิน (mandate) รหัส " + code + " ตอบ /approve " + code + " เพื่ออนุมัติ"}
	}
	return Notice{Severity: "notice", DedupKey: "dca-mandate-reminder:" + strconv.FormatInt(planID, 10),
		Title: fmt.Sprintf("DCA plan %d is still waiting for its mandate", planID),
		Body:  "The plan keeps running the old way (its scheduled job) until you approve the mandate with code " + code + ". Reply /approve " + code + " to approve it."}
}

// MutedSkipNotice tells the owner that a mute stopped a mandated buy. It is a template
// over ids, a symbol and times: no model text. slot is the skipped slot in the
// mandate's zone; earlier is how many other slots were skipped and not reported yet.
func MutedSkipNotice(thai bool, planID int64, symbol string, slot time.Time, zone string, earlier int, day string) Notice {
	at := slot.Format("2006-01-02 15:04") + " (" + zone + ")"
	key := "dca-muted-skip:" + strconv.FormatInt(planID, 10) + ":" + day
	if thai {
		body := fmt.Sprintf("รอบซื้อ %s ของแผน DCA %d (%s) ถูกข้าม เพราะคุณปิดเสียงการแจ้งเตือนนี้ไว้ ยังไม่มีการซื้อเกิดขึ้น "+
			"หากต้องการให้ซื้อตามปกติ ตอบ /unmute dca_due (หรือ /unmute dca_due:plan-%d ถ้าปิดเสียงเฉพาะแผนนี้) ส่วน /snooze จะหมดอายุเอง", at, planID, symbol, planID)
		if earlier > 0 {
			body += fmt.Sprintf(" ก่อนหน้านี้ยังมีอีก %d รอบที่ถูกข้ามโดยไม่ได้แจ้ง", earlier)
		}
		return Notice{Severity: "notice", DedupKey: key, Title: fmt.Sprintf("แผน DCA %d: ข้ามการซื้อเพราะปิดเสียงแจ้งเตือน", planID), Body: body}
	}
	body := fmt.Sprintf("The %s slot of DCA plan %d (%s) was skipped because you muted this alert. Nothing was bought. "+
		"To buy as usual, reply /unmute dca_due (or /unmute dca_due:plan-%d if you muted only this plan); a /snooze ends by itself.", at, planID, symbol, planID)
	if earlier > 0 {
		body += fmt.Sprintf(" %d earlier slot(s) were also skipped and not reported.", earlier)
	}
	return Notice{Severity: "notice", DedupKey: key, Title: fmt.Sprintf("DCA plan %d: buy skipped because of a mute", planID), Body: body}
}

// DialSkipNotice tells the owner that their autonomy setting for mandated buys (notify
// or off) stopped a plan's buy. It is a template over ids, a symbol, a fixed reason and
// a day: no model text. It is sent at most once a day per plan.
func DialSkipNotice(thai bool, planID int64, symbol, reason, day string) Notice {
	key := "dca-dial-skip:" + strconv.FormatInt(planID, 10) + ":" + day
	if thai {
		return Notice{Severity: "notice", DedupKey: key,
			Title: fmt.Sprintf("แผน DCA %d: ข้ามการซื้อตามที่คุณตั้งค่าความเป็นอิสระไว้", planID),
			Body: fmt.Sprintf("รอบซื้อของแผน DCA %d (%s) ถูกข้าม ยังไม่มีการซื้อเกิดขึ้น เพราะคุณตั้งค่าการซื้อตามวงเงิน (mandate) ให้ไม่ซื้อเอง (%s) "+
				"แจ้งเตือนนี้ส่งวันละครั้งต่อแผน", planID, symbol, reason)}
	}
	return Notice{Severity: "notice", DedupKey: key,
		Title: fmt.Sprintf("DCA plan %d: buy skipped by your autonomy setting", planID),
		Body: fmt.Sprintf("The buy of DCA plan %d (%s) was skipped and nothing was bought: %s. "+
			"You set mandated buys not to run on their own. This notice is sent once a day per plan.", planID, symbol, reason)}
}
