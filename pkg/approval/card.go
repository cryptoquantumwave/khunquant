package approval

import (
	"bytes"
	"embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

//go:embed templates/order.en.tmpl templates/order.th.tmpl
var templateFS embed.FS

var cardTemplates = map[string]*template.Template{
	"en": template.Must(template.ParseFS(templateFS, "templates/order.en.tmpl")),
	"th": template.Must(template.ParseFS(templateFS, "templates/order.th.tmpl")),
}

// Card is the owner-facing rendering of a proposal. Everything in it comes from
// the stored params and the proposal's own fields through a template; the model's
// rationale appears only under the "Why" label and nothing is parsed out of it.
type Card struct {
	// Title is the headline (the template's first line).
	Title string
	// Body is the rest of the rendering.
	Body string
	// SessionBody is the copy written into the owner's session: the headline, the
	// code and the expiry. No nonce is anywhere in the card (it is only in the
	// buttons' payloads), and the rationale is left out of the session copy.
	SessionBody string
	Actions     []bus.Action
}

// Text is the full card as one string, for editing the delivered message.
func (c Card) Text() string {
	if c.Body == "" {
		return c.Title
	}
	return c.Title + "\n" + c.Body
}

type cardView struct {
	ModeTag, Code, SideWord, Amount, Unit, USD, TypeWord, PriceText string
	Provider, Account, RefPrice, Collar, Why, Expires, Outcome      string
	PriceLine                                                       string
	Minutes                                                         int
	ReduceOnly                                                      bool
}

type words struct {
	buy, sell, market, limit, contracts string
	paper, realExit                     string
	approve, reject                     string
	approved, rejected, expired         string
	// priceAbort, priceExitMarket and priceExitLimit are the card's price line: format
	// strings taking the reference price (and the collar for the first).
	priceAbort, priceExitMarket, priceExitLimit string
	cancelled, executing                        string
	// planClosed is the card line of a recovery proposal withdrawn because an unwind closed its plan.
	planClosed string
}

var wordsFor = map[string]words{
	"en": {
		buy: "BUY", sell: "SELL", market: "market", limit: "limit", contracts: "contracts",
		priceAbort:      "Price ref %s - aborts if the price moves against the order by more than %s%%",
		priceExitMarket: "Price ref %s - this order reduces risk, so it executes at the MARKET price even if the price has moved since this card was sent",
		priceExitLimit:  "Price ref %s - this order reduces risk, so it is not abandoned for a price move; it fills at its limit price or better",
		paper:           "PAPER (simulated, nothing real) - ",
		realExit:        "REAL exit while paper mode is on - ",
		approve:         "Approve", reject: "Reject",
		approved:  "APPROVED - the order is being checked and placed.",
		rejected:  "REJECTED - nothing was placed.",
		expired:   "EXPIRED - nothing was placed.",
		cancelled: "CANCELLED - nothing was placed.",
		planClosed: "CANCELLED - the plan was closed, so this close is no longer needed. Nothing was placed.",
		executing: "APPROVED - the order is being checked and placed.",
	},
	"th": {
		buy: "ซื้อ", sell: "ขาย", market: "ตลาด", limit: "ลิมิต", contracts: "สัญญา",
		priceAbort:      "ราคาอ้างอิง %s - ยกเลิกถ้าราคาเคลื่อนที่สวนทางกับคำสั่งเกิน %s%%",
		priceExitMarket: "ราคาอ้างอิง %s - คำสั่งนี้ลดความเสี่ยง จึงส่งที่ราคาตลาดแม้ราคาจะเปลี่ยนไปหลังจากส่งการ์ดนี้",
		priceExitLimit:  "ราคาอ้างอิง %s - คำสั่งนี้ลดความเสี่ยง จึงไม่ถูกยกเลิกเพราะราคาเปลี่ยน และจับคู่ที่ราคาลิมิตหรือดีกว่า",
		paper:           "PAPER (จำลอง ไม่ใช่ของจริง) - ",
		realExit:        "ออกจริง (REAL) ขณะเปิดโหมด paper - ",
		approve:         "อนุมัติ", reject: "ปฏิเสธ",
		approved:  "อนุมัติแล้ว - กำลังตรวจสอบและส่งคำสั่ง",
		rejected:  "ปฏิเสธแล้ว - ไม่มีการส่งคำสั่ง",
		expired:   "หมดอายุ - ไม่มีการส่งคำสั่ง",
		cancelled: "ยกเลิก - ไม่มีการส่งคำสั่ง",
		planClosed: "ยกเลิก - แผนถูกปิดแล้ว จึงไม่ต้องปิดสถานะนี้อีก ไม่มีการส่งคำสั่ง",
		executing: "อนุมัติแล้ว - กำลังตรวจสอบและส่งคำสั่ง",
	},
}

func langOf(l string) string {
	if _, ok := wordsFor[l]; ok {
		return l
	}
	return "en"
}

// OutcomeText is the line a card shows for a status, or "" for one that has no
// text of its own (pending). Terminal results of an execution carry their own
// text (Proposal.Outcome).
func OutcomeText(lang string, s Status) string {
	w := wordsFor[langOf(lang)]
	switch s {
	case StatusApproved, StatusExecuting:
		return w.approved
	case StatusRejected:
		return w.rejected
	case StatusExpired:
		return w.expired
	case StatusCancelled:
		return w.cancelled
	}
	return ""
}

// num formats a quantity the way the exchange takes it: shortest exact decimal.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// usd formats a USD value to cents.
func usd(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// baseOf returns the base asset of a unified symbol ("BTC/USDT" -> "BTC").
func baseOf(symbol string) string {
	if i := strings.Index(symbol, "/"); i > 0 {
		return symbol[:i]
	}
	return symbol
}

// RenderOrderCard renders the card of an order proposal. outcome, when set, is
// appended (an edited card); expires is shown in loc.
func RenderOrderCard(p Proposal, op OrderParams, loc Locale, outcome string) (Card, error) {
	lang := langOf(loc.Lang)
	w := wordsFor[lang]
	tz := loc.Loc
	if tz == nil {
		tz = time.UTC
	}

	view := cardView{
		Code:       p.Code,
		Amount:     num(op.Amount),
		Unit:       baseOf(op.Symbol),
		USD:        usd(op.NotionalUSD),
		SideWord:   w.buy,
		TypeWord:   w.market,
		Provider:   op.Provider,
		Account:    accountLabel(op.Account),
		RefPrice:   num(op.RefPrice),
		Collar:     num(op.PriceCollarPct),
		Why:        p.Rationale,
		Expires:    p.ExpiresAt.In(tz).Format("15:04"),
		Minutes:    int(p.ExpiresAt.Sub(p.CreatedAt).Round(time.Minute) / time.Minute),
		ReduceOnly: op.ReduceOnly,
		Outcome:    outcome,
	}
	switch {
	case p.Reducing && op.Type == "limit":
		view.PriceLine = fmt.Sprintf(w.priceExitLimit, view.RefPrice)
	case p.Reducing:
		view.PriceLine = fmt.Sprintf(w.priceExitMarket, view.RefPrice)
	default:
		view.PriceLine = fmt.Sprintf(w.priceAbort, view.RefPrice, view.Collar)
	}
	if op.Side == "sell" {
		view.SideWord = w.sell
	}
	if op.Type == "limit" {
		view.TypeWord = w.limit
		if op.Price != nil {
			view.PriceText = num(*op.Price)
		}
	}
	if op.Market == MarketFutures {
		view.Unit = w.contracts + " " + op.Symbol
	}
	switch p.Mode {
	case ModePaper:
		view.ModeTag = w.paper
	case ModeRealExit:
		view.ModeTag = w.realExit
	}

	var buf bytes.Buffer
	if err := cardTemplates[lang].Execute(&buf, view); err != nil {
		return Card{}, fmt.Errorf("approval: render card: %w", err)
	}
	text := strings.TrimRight(buf.String(), "\n")
	title, body, _ := strings.Cut(text, "\n")

	session := title + "\n" + wordsFor[lang].sessionLine(p.Code, view.Expires)
	return Card{
		Title: title, Body: strings.TrimSpace(body), SessionBody: session,
		Actions: []bus.Action{
			{ID: "approve", Label: w.approve, Style: "primary", Payload: ButtonPayload(p.Code, p.Nonce), TextCommand: "/approve " + p.Code},
			{ID: "reject", Label: w.reject, Style: "danger", Payload: ButtonPayload(p.Code, p.Nonce), TextCommand: "/reject " + p.Code},
		},
	}, nil
}

func (w words) sessionLine(code, expires string) string {
	if w.buy == "BUY" {
		return "Waiting for the owner's decision on proposal " + code + " (expires " + expires + ")."
	}
	return "รอเจ้าของตัดสินใจข้อเสนอ " + code + " (หมดอายุ " + expires + ")"
}

func accountLabel(account string) string {
	if strings.TrimSpace(account) == "" {
		return "default"
	}
	return account
}

// Headline renders just the first line, for the Summary column and the logs.
func Headline(p Proposal, op OrderParams, loc Locale) string {
	c, err := RenderOrderCard(p, op, loc, "")
	if err != nil {
		return p.Code
	}
	return c.Title
}
