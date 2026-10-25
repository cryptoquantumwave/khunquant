package triggers

import (
	"encoding/json"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// What a crossing kind does differently when its spec was compiled from an intent
// (pkg/triggers/intents): the intent's title, which the owner wrote or a model
// restated, travels ONLY in the candidate's Quote. It is not in the title, the
// subject, the detail, the evidence or the dedup key, so nothing a composer reads
// holds it. The dedup key is built from the kind and the intent's id, so two
// intents on the same line never silence each other and neither silences an alert
// the owner set with the alert tools. A standing order's firing is a Notice that
// says so in its title and does nothing else.

// intentRef is the intent a spec was compiled from. The zero value is a spec that
// was not.
type intentRef struct {
	id    string
	order bool // a standing order
}

// intentOf reads the intent out of a spec: its IntentID, and from its origin whether
// it is a standing order.
func intentOf(spec Spec) intentRef {
	if spec.IntentID == "" {
		return intentRef{}
	}
	_, order, _ := IntentOfOrigin(spec.Origin)
	return intentRef{id: spec.IntentID, order: order}
}

// shape rewrites the candidate of a crossing for the intent. symbol is the
// validated symbol of the line.
func (r intentRef) shape(kind, symbol string, thai bool, c proactive.Candidate) proactive.Candidate {
	if r.id == "" {
		return c
	}
	c.DedupKey = IntentCrossKey(kind, r.id, symbol)
	if len(c.Evidence) > 0 {
		ev := make(map[string]any, len(c.Evidence))
		for k, v := range c.Evidence {
			if k != "message" {
				ev[k] = v
			}
		}
		c.Evidence = ev
	}
	if r.order {
		if thai {
			c.Title = "คำสั่งประจำ: " + c.Title
		} else {
			c.Title = "Standing order: " + c.Title
		}
	}
	return c
}

// IntentCrossKey is <kind>:<intent id>:<symbol>, the dedup key of a crossing that
// was compiled from an intent.
func IntentCrossKey(kind, intentID, symbol string) string {
	return kind + ":" + intentID + ":" + symbol
}

// ParsePriceCross decodes and validates the parameters of a price_cross spec with
// the kind's own rules, for a caller that must refuse early (the intent tools).
func ParsePriceCross(raw json.RawMessage) (PriceCrossParams, error) {
	var p PriceCrossParams
	if err := decodeStrict(raw, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// ParseIndicatorCross is ParsePriceCross for indicator_cross.
func ParseIndicatorCross(raw json.RawMessage) (IndicatorCrossParams, error) {
	var p IndicatorCrossParams
	if err := decodeStrict(raw, &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// MaxCooldownMinutes is the longest cooldown_minutes a crossing accepts.
const MaxCooldownMinutes = maxCooldownMin
