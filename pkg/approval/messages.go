package approval

import (
	"fmt"
	"strings"
)

// msgText is the owner-facing wording of how an execution ended. Like the card it
// is built by code from the stored params; no model text is in it.
type msgText struct {
	executedGeneric      func(code string) string
	executedTitle        func(code string) string
	failedTitle          func(code string) string
	stoppedWhilePlacing  string
	stoppedBeforePlacing string
	notPlaced            func(code, reason string) string
	mayExist             func(code, reason, where string) string
	receipt              func(tag, code, side, amount, unit, where string, extra []string) string
	order, status        string
	gotPrice             func(avg, ref, pct string) string
	filled, fee, noFill  string
}

var msgs = map[string]msgText{
	"en": {
		executedGeneric: func(code string) string {
			return "Proposal " + code + " was executed. Check the exchange for the details."
		},
		executedTitle:        func(code string) string { return "Proposal " + code + ": executed" },
		failedTitle:          func(code string) string { return "Proposal " + code + ": NOT executed" },
		stoppedWhilePlacing:  "the process stopped while the order was being placed",
		stoppedBeforePlacing: "the execution was stopped before an order was placed",
		notPlaced: func(code, reason string) string {
			return "Proposal " + code + " failed and NOTHING was placed: " + reason
		},
		mayExist: func(code, reason, where string) string {
			return "Proposal " + code + ": the order MAY OR MAY NOT HAVE BEEN PLACED on " + where + " (" + reason + "). " +
				"Check " + where + " before doing anything else, and do not approve it again until you have."
		},
		receipt: func(tag, code, side, amount, unit, where string, extra []string) string {
			return tag + "Proposal " + code + " executed: " + side + " " + amount + " " + unit + " on " + where + ". " + strings.Join(extra, " ")
		},
		gotPrice: func(avg, ref, pct string) string {
			return "Got an average price of " + avg + " against the reference " + ref + " (" + pct + ")."
		},
		order: "Order ID:", status: "Status:", filled: "Filled", fee: "Fee:", noFill: "The fill could not be confirmed: check the exchange.",
	},
	"th": {
		executedGeneric: func(code string) string {
			return "ข้อเสนอ " + code + " ถูกดำเนินการแล้ว โปรดตรวจรายละเอียดที่ exchange"
		},
		executedTitle: func(code string) string {
			return "ข้อเสนอ " + code + ": ดำเนินการแล้ว"
		},
		failedTitle: func(code string) string {
			return "ข้อเสนอ " + code + ": ไม่ได้ดำเนินการ"
		},
		stoppedWhilePlacing:  "ระบบหยุดทำงานระหว่างส่งคำสั่ง",
		stoppedBeforePlacing: "การดำเนินการถูกหยุดก่อนส่งคำสั่ง",
		notPlaced: func(code, reason string) string {
			return "ข้อเสนอ " + code + " ล้มเหลวและไม่มีการส่งคำสั่งใด ๆ: " + reason
		},
		mayExist: func(code, reason, where string) string {
			return "ข้อเสนอ " + code + ": คำสั่งอาจถูกส่งไปที่ " + where + " แล้วหรือไม่ก็ได้ (" + reason + ") " +
				"โปรดตรวจที่ " + where + " ก่อนทำอย่างอื่น และอย่าอนุมัติซ้ำจนกว่าจะตรวจแล้ว"
		},
		receipt: func(tag, code, side, amount, unit, where string, extra []string) string {
			return tag + "ข้อเสนอ " + code + " ดำเนินการแล้ว: " + side + " " + amount + " " + unit + " ที่ " + where + " " + strings.Join(extra, " ")
		},
		gotPrice: func(avg, ref, pct string) string {
			return "ได้ราคาเฉลี่ย " + avg + " เทียบกับราคาอ้างอิง " + ref + " (" + pct + ")"
		},
		order: "หมายเลขคำสั่ง:", status: "สถานะ:", filled: "จับคู่แล้ว", fee: "ค่าธรรมเนียม:", noFill: "ยืนยันการจับคู่ไม่ได้ โปรดตรวจที่ exchange",
	},
}

func sideWord(lang, side string) string {
	w := wordsFor[langOf(lang)]
	if side == "sell" {
		return w.sell
	}
	return w.buy
}

func modeTag(lang, mode string) string {
	w := wordsFor[langOf(lang)]
	switch mode {
	case ModePaper:
		return w.paper
	case ModeRealExit:
		return w.realExit
	}
	return ""
}

// receiptText is the message for an order that was placed.
func receiptText(p Proposal, op OrderParams, rc Rechecked, placed Placed, fill Fill) string {
	lang := langOf(p.Lang)
	m := msgs[lang]
	mode := rc.Mode
	if mode == "" {
		mode = p.Mode
	}
	unit := baseOf(op.Symbol)
	if op.Market == MarketFutures {
		unit = wordsFor[lang].contracts + " " + op.Symbol
	}
	var extra []string
	if placed.OrderID != "" {
		extra = append(extra, m.order+" "+placed.OrderID+".")
	}
	if fill.Known {
		if fill.Status != "" {
			extra = append(extra, m.status+" "+fill.Status+".")
		}
		line := m.filled + " " + num(fill.Filled)
		if fill.AvgPrice > 0 {
			line += " @ " + num(fill.AvgPrice)
		}
		extra = append(extra, line+".")
		if fill.AvgPrice > 0 && op.RefPrice > 0 {
			extra = append(extra, m.gotPrice(num(fill.AvgPrice), num(op.RefPrice), fmt.Sprintf("%+.2f%%", (fill.AvgPrice-op.RefPrice)/op.RefPrice*100)))
		}
		if fill.FeeCost > 0 {
			extra = append(extra, strings.TrimSpace(fmt.Sprintf("%s %s %s.", m.fee, num(fill.FeeCost), fill.FeeCcy)))
		}
	} else {
		extra = append(extra, m.noFill)
	}
	return m.receipt(modeTag(lang, mode), p.Code, sideWord(lang, op.Side), num(op.Amount), unit,
		op.Provider+"/"+accountLabel(op.Account), extra)
}

// failureText is the message for an execution that ended without a completed order.
func failureText(p Proposal, op OrderParams, f failure) string {
	m := msgs[langOf(p.Lang)]
	if f.mayExist {
		where := op.Provider
		if where == "" {
			where = "the exchange"
		}
		return m.mayExist(p.Code, f.reason, where)
	}
	return m.notPlaced(p.Code, f.reason)
}
