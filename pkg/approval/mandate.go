package approval

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// KindMandate is a standing permission: "buy this, at most this much per run and per
// period, until this date". Approving it creates the mandate row (a MandateSink) and
// places nothing; the orders it later allows are placed by code, inside its bounds,
// by the dca.execute workflow.
const KindMandate Kind = "mandate"

// OriginMandate marks an order proposal built by CODE (the dca.execute workflow)
// because a run fell outside its mandate. It is not the origin of any model turn:
// propose_order only ever sets OriginOwner or OriginProactive, and a request carrying
// OriginMandate needs a mandate id, which no tool argument can supply. The rule that a
// background turn may propose only risk-reducing orders is about model turns; this
// proposal is the question "may I buy this once?", and it is placed only if the owner
// approves exactly that card.
const OriginMandate Origin = "mandate"

// MandateProposalExpiry is how long a mandate card can be decided: it is a standing
// permission, not an order that goes stale in minutes.
const MandateProposalExpiry = 72 * time.Hour

// MandateAskExpiry is how long an out-of-mandate order question can be answered.
const MandateAskExpiry = 6 * time.Hour

// MaxMandateMonths bounds a mandate's life.
const MaxMandateMonths = 12

// Mandate periods.
const (
	PeriodWeek  = "week"
	PeriodMonth = "month"
)

// MandateParams is the canonical, hashed description of a mandate. The caps are in
// the plan's quote currency (Quote) AND in USD, converted once at proposal time at
// RateUSDPerQuote (from RateSource); the USD figures are what execution enforces.
type MandateParams struct {
	Kind              string  `json:"kind"` // "dca"
	PlanID            int64   `json:"plan_id"`
	Provider          string  `json:"provider"`
	Account           string  `json:"account"`
	Symbol            string  `json:"symbol"`
	Side              string  `json:"side"` // buy only in this change
	Quote             string  `json:"quote"`
	MaxPerRunQuote    float64 `json:"max_per_run_quote"`
	MaxPerPeriodQuote float64 `json:"max_per_period_quote"`
	MaxPerRunUSD      float64 `json:"max_per_run_usd"`
	MaxPerPeriodUSD   float64 `json:"max_per_period_usd"`
	RateUSDPerQuote   float64 `json:"rate_usd_per_quote"`
	RateSource        string  `json:"rate_source"`
	Period            string  `json:"period"`
	Timezone          string  `json:"timezone"` // the owner's, for period boundaries
	ExpiresAtMS       int64   `json:"expires_at_ms"`
}

// ExpiresAt is the mandate's last instant.
func (m MandateParams) ExpiresAt() time.Time { return time.UnixMilli(m.ExpiresAtMS).UTC() }

func validateMandateParams(m MandateParams) error {
	switch {
	case m.Kind != "dca":
		return errors.New("mandate kind must be dca")
	case m.PlanID <= 0:
		return errors.New("a mandate belongs to a plan")
	case strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.Symbol) == "":
		return errors.New("provider and symbol are required")
	case m.Side != "buy":
		return errors.New("only a buy mandate exists in this change")
	case strings.TrimSpace(m.Quote) == "":
		return errors.New("the quote currency is required")
	case strings.Contains(m.Symbol, ":"):
		return errors.New("a mandate covers spot symbols; a contract symbol's amount is a contract count")
	case !finitePositive(m.MaxPerRunQuote) || !finitePositive(m.MaxPerPeriodQuote) ||
		!finitePositive(m.MaxPerRunUSD) || !finitePositive(m.MaxPerPeriodUSD) || !finitePositive(m.RateUSDPerQuote):
		return errors.New("every cap and the rate must be a positive, finite number")
	case m.MaxPerPeriodUSD < m.MaxPerRunUSD:
		return errors.New("the per-period cap is smaller than the per-run cap")
	case m.Period != PeriodWeek && m.Period != PeriodMonth:
		return errors.New("period must be week or month")
	case m.ExpiresAtMS <= 0:
		return errors.New("a mandate needs an expiry")
	}
	if _, err := time.LoadLocation(m.Timezone); err != nil || m.Timezone == "" {
		return errors.New("the owner's time zone is unknown")
	}
	return nil
}

func canonicalMandate(m MandateParams) (json.RawMessage, error) {
	if err := validateMandateParams(m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("approval: encode mandate: %w", err)
	}
	return b, nil
}

// VerifiedMandate decodes a stored mandate proposal's params and proves they are
// exactly what was hashed (the same proof decodeVerified gives an order).
func VerifiedMandate(p Proposal) (MandateParams, error) {
	if p.Kind != KindMandate {
		return MandateParams{}, fmt.Errorf("approval: proposal %s is not a mandate", p.Code)
	}
	want := hashOf(p.Kind, p.Params)
	if subtle.ConstantTimeCompare(want[:], p.Hash[:]) != 1 {
		return MandateParams{}, errHashMismatch
	}
	dec := json.NewDecoder(bytes.NewReader(p.Params))
	dec.DisallowUnknownFields()
	var m MandateParams
	if err := dec.Decode(&m); err != nil || dec.More() {
		return MandateParams{}, errHashMismatch
	}
	again, err := json.Marshal(m)
	if err != nil || !bytes.Equal(again, p.Params) {
		return MandateParams{}, errHashMismatch
	}
	if err := validateMandateParams(m); err != nil {
		return MandateParams{}, fmt.Errorf("%w: %v", errHashMismatch, err)
	}
	return m, nil
}

// verifyProposal proves a stored proposal of any kind matches its hash.
func verifyProposal(p Proposal) error {
	switch p.Kind {
	case KindMandate:
		_, err := VerifiedMandate(p)
		return err
	case KindMemory:
		_, err := VerifiedMemory(p)
		return err
	}
	_, err := decodeVerified(p)
	return err
}

// MandateSink creates the mandate row of an approved mandate proposal. It must be
// idempotent by proposal id (Recover calls it again after a crash) and must itself
// refuse a proposal that is not approved, not owner-decided, past its window or
// whose hash does not verify.
type MandateSink interface {
	CreateMandate(ctx context.Context, p Proposal) error
}

// MandateRequest is a mandate to propose. The params are computed by code from the
// plan, the rate and the owner's profile; nothing here comes from model prose.
type MandateRequest struct {
	Params MandateParams
}

// ProposeMandate stores a pending mandate proposal and sends the owner its card.
// Only the owner's own root turn calls it (the tools check that before they get
// here); it places and enables nothing.
func (s *Service) ProposeMandate(ctx context.Context, r MandateRequest) (Proposal, error) {
	if _, ok := s.owner(); !ok {
		return Proposal{}, fmt.Errorf("%w: no owner is configured to approve it", ErrUnavailable)
	}
	if s.o.Mandates == nil {
		return Proposal{}, fmt.Errorf("%w: mandates are not wired", ErrUnavailable)
	}
	if r.Params.Timezone == "" { // the owner's own time zone, from the profile
		if loc := s.locale(ctx); loc.Loc != nil {
			r.Params.Timezone = loc.Loc.String()
		}
	}
	params, err := canonicalMandate(r.Params)
	if err != nil {
		return Proposal{}, err
	}
	if r.Params.ExpiresAt().After(s.now().AddDate(0, MaxMandateMonths, 1)) {
		return Proposal{}, fmt.Errorf("approval: a mandate lasts at most %d months", MaxMandateMonths)
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout+30*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if codes, err := s.st.pendingCodes(ctx, now, classMandate); err != nil {
		return Proposal{}, err
	} else if len(codes) >= MaxPendingMandate {
		return Proposal{}, &PendingError{Codes: codes}
	}
	loc := s.locale(ctx)
	p := Proposal{
		ID: proactive.NewID(), Nonce: newNonce(), Kind: KindMandate, Params: params, Hash: hashOf(KindMandate, params),
		Origin: OriginOwner, Mode: ModeLive, Lang: langOf(loc.Lang), Status: StatusPending,
		CreatedAt: now, ExpiresAt: now.Add(MandateProposalExpiry),
	}
	var card Card
	for attempt := 0; ; attempt++ {
		p.Code = newCode()
		card = RenderMandateCard(p, r.Params, loc, "")
		p.Summary = card.Title
		err = s.st.insert(ctx, p, now)
		if errors.Is(err, errCodeTaken) && attempt < maxCodeAttempts {
			continue
		}
		break
	}
	var bounds errBounds
	switch {
	case errors.As(err, &bounds):
		if bounds.hourly {
			return Proposal{}, ErrHourlyLimit
		}
		codes, _ := s.st.pendingCodes(ctx, now, classMandate)
		return Proposal{}, &PendingError{Codes: codes}
	case err != nil:
		return Proposal{}, err
	}
	if err := s.deliverCard(ctx, &p, card); err != nil {
		return Proposal{}, err
	}
	s.emit(events.KindApprovalCreated, p, events.SeverityInfo)
	return p, nil
}

// executeMandate runs the executor of an approved mandate proposal: it creates the
// mandate row (idempotently) and settles the proposal. It places nothing. A failure
// leaves the proposal executing, and Recover tries again.
func (s *Service) executeMandate(ctx context.Context, p Proposal) error {
	if s.o.Mandates == nil {
		return errors.New("approval: no mandate sink attached")
	}
	if _, err := s.st.markExecuting(ctx, p.ID, "mandate"); err != nil {
		return err
	}
	cur, err := s.st.get(ctx, p.ID)
	if err != nil {
		return err
	}
	if cur.Status != StatusExecuting {
		return nil // already settled by another pass
	}
	if err := s.o.Mandates.CreateMandate(ctx, cur); err != nil {
		logger.ErrorCF(logComponent, "SECURITY: an approved mandate could not be created", map[string]any{"code": cur.Code, "error": err.Error()})
		return err
	}
	text := mandateOutcome(cur.Lang)
	if changed, err := s.st.finish(ctx, cur.ID, StatusExecuted, text, s.now()); err != nil {
		return err
	} else if changed {
		cur.Status, cur.Outcome = StatusExecuted, text
		_ = s.st.markNotified(context.WithoutCancel(ctx), cur.ID) // the card edit is the message
		s.editCard(ctx, cur, text)
	}
	return nil
}

func mandateOutcome(lang string) string {
	if langOf(lang) == "th" {
		return "อนุมัติแล้ว - สร้างวงเงินอนุมัติ (mandate) แล้ว ยังไม่มีการส่งคำสั่ง"
	}
	return "APPROVED - the mandate is active. Nothing was bought by this approval."
}

// RenderMandateCard renders the card of a mandate proposal from its params alone.
func RenderMandateCard(p Proposal, m MandateParams, loc Locale, outcome string) Card {
	lang := langOf(loc.Lang)
	w := wordsFor[lang]
	tz := loc.Loc
	if tz == nil {
		tz = time.UTC
	}
	account := accountLabel(m.Account)
	until := m.ExpiresAt().In(tz).Format("2006-01-02")
	period := periodWord(lang, m.Period)
	var title, body string
	if lang == "th" {
		title = fmt.Sprintf("อนุญาตให้ KhunQuant ซื้อ %s ที่ %s: ไม่เกิน %s %s (ประมาณ $%s) ต่อครั้ง และ %s %s (ประมาณ $%s) ต่อ%s จนถึง %s? [%s]",
			m.Symbol, m.Provider, num(m.MaxPerRunQuote), m.Quote, usd(m.MaxPerRunUSD),
			num(m.MaxPerPeriodQuote), m.Quote, usd(m.MaxPerPeriodUSD), period, until, p.Code)
		body = fmt.Sprintf("บัญชี: %s\nแผน DCA: %d\nวงเงินเป็น USD คำนวณที่ 1 %s = $%s (ที่มา: %s) ตอนสร้างการ์ดนี้ และระบบบังคับใช้เป็น USD\n"+
			"ช่วงเวลานับตามเขตเวลา %s\nดำเนินการโดยไม่ถามภายในวงเงินนี้ นอกวงเงินจะถามก่อนเสมอ\nหมดอายุ %s",
			account, m.PlanID, m.Quote, num(m.RateUSDPerQuote), m.RateSource, m.Timezone, until)
	} else {
		title = fmt.Sprintf("Allow KhunQuant to BUY %s on %s: up to %s %s (about $%s) per run and %s %s (about $%s) per %s, until %s? [%s]",
			m.Symbol, m.Provider, num(m.MaxPerRunQuote), m.Quote, usd(m.MaxPerRunUSD),
			num(m.MaxPerPeriodQuote), m.Quote, usd(m.MaxPerPeriodUSD), period, until, p.Code)
		body = fmt.Sprintf("Account: %s\nDCA plan: %d\nThe USD limits were converted at 1 %s = $%s (source: %s) when this card was made, and are enforced in USD.\n"+
			"Periods are counted in %s.\nExecutes without asking within these limits; anything outside them is asked first.\nExpires %s",
			account, m.PlanID, m.Quote, num(m.RateUSDPerQuote), m.RateSource, m.Timezone, until)
	}
	if outcome != "" {
		body += "\n\n" + outcome
	}
	session := title + "\n" + w.sessionLine(p.Code, p.ExpiresAt.In(tz).Format("01-02 15:04"))
	return Card{
		Title: title, Body: body, SessionBody: session,
		Actions: []bus.Action{
			{ID: "approve", Label: w.approve, Style: "primary", Payload: ButtonPayload(p.Code, p.Nonce), TextCommand: "/approve " + p.Code},
			{ID: "reject", Label: w.reject, Style: "danger", Payload: ButtonPayload(p.Code, p.Nonce), TextCommand: "/reject " + p.Code},
		},
	}
}

func periodWord(lang, period string) string {
	if lang == "th" {
		if period == PeriodWeek {
			return "สัปดาห์"
		}
		return "เดือน"
	}
	return period
}
