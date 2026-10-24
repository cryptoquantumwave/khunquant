package sweep

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func topic(key, asset string) Topic {
	return Topic{Key: key, Kind: KindOf(key), Asset: asset, Severity: proactive.SeverityWarning}
}

var (
	topicMoveBTC = topic("move_vs_vol:BTC:1h:tier1", "BTC")
	topicDD      = topic("drawdown:portfolio:day:tier1", "")
	topicDrift   = topic("allocation_drift:crypto:over", "")
	topicLiqETH  = topic("liquidation_distance:okx/main:ETH/USDT_USDT:long:critical", "ETH")
	topicStale   = topic("data_stale:binance:stale1", "")
	topicStock   = topic("move_vs_vol:AAPL:1h:tier1", "") // a stock: no base asset in the log
	topicExt     = topic("external:deploy-42:1a2b3c4d", "")
	topicDN3     = topic("dn_health:3:margin_critical", "")
	topicDCA3    = topic("dca_due:3:1773118800000", "")
	topicLevel   = topic("price_cross:binance:BTC/USDT:above:70000", "BTC")
)

// The cases of the rule, each named for what it shows. A decision is dropped by its
// words only when it speaks of the topic's class and subject and of nothing the
// triggers do not report; it is dropped whatever its words when it cites the key.
func TestMatch(t *testing.T) {
	all := []Topic{topicMoveBTC, topicDD, topicDrift, topicLiqETH, topicStale, topicStock, topicExt, topicDN3, topicDCA3, topicLevel}
	for name, tc := range map[string]struct {
		key, text string
		topics    []Topic
		want      string // the matched topic's key, "" for none
	}{
		// ---- repeats that are dropped
		"the spec's volatile day: same asset, same class": {
			"btc:drop", "BTC dropped 8% today\nIt is your largest holding.", all, topicMoveBTC.Key},
		"a move topic for BTC and BTC fell 8% in an hour": {
			"", "BTC fell 8% in an hour", []Topic{topicMoveBTC}, topicMoveBTC.Key},
		"a drawdown said as the portfolio being down": {
			"portfolio:loss", "Your portfolio is down 6% from its peak", all, topicDD.Key},
		"a drawdown said as being below its high": {
			"", "portfolio is 12% below its high", []Topic{topicDD}, topicDD.Key},
		"a drawdown word alone": {
			"", "Drawdown deepened today", []Topic{topicDD}, topicDD.Key},
		"drift by class alone": {
			"alloc:drift", "Crypto weight drifted 12 points over target; consider a rebalance", all, topicDrift.Key},
		"liquidation, same asset": {
			"eth:liq", "ETH perp: liquidation is 9% away", all, topicLiqETH.Key},
		"a stale feed": {
			"data:stale", "Binance data has gone stale", all, topicStale.Key},
		"a stock named by its key, the log having no base asset": {
			"aapl:drop", "AAPL fell 6% at the open", []Topic{topicStock}, topicStock.Key},
		"a delta-neutral plan said in words": {
			"", "plan 3 hedge is drifting", []Topic{topicDN3}, topicDN3.Key},

		// ---- kept: another event, class, subject or topic
		"a move topic for BTC and a limit buy that filled": {
			"btc:fill", "BTC limit buy filled", []Topic{topicMoveBTC}, ""},
		"a fill that mentions the price": {
			"btc:fill", "Your BTC limit buy filled at price 60,000", all, ""},
		"a move topic for BTC and a holding that appeared": {
			"btc:holding", "BTC holding appeared at binance", []Topic{topicMoveBTC}, ""},
		"a holding that vanished on an asset that moved": {
			"btc:gone", "BTC position closed after the fall", []Topic{topicMoveBTC}, ""},
		"a source that went down, on an asset that also moved": {
			"binance:down", "Binance feed degraded; BTC prices may lag", []Topic{topicMoveBTC}, ""},
		"a plan that was paused": {
			"dca:paused", "DCA plan 3 was paused", []Topic{topicDCA3}, ""},
		"a hedge plan that was paused": {
			"dn:paused", "Hedge plan 3 was paused", []Topic{topicDN3}, ""},
		"a plan state on a plan with a hedge topic": {
			"", "Plan 3 created", []Topic{topicDN3}, ""},
		"a deposit while the portfolio is down": {
			"cash:deposit", "Deposit of 5,000 USDT arrived; the portfolio is down 4% so it is a good moment", []Topic{topicDD}, ""},
		"a new holding on a portfolio with a drawdown topic": {
			"", "portfolio: a new holding ETH appeared, price 3,000", []Topic{topicDD}, ""},
		"another asset, same class": {
			"sol:drop", "SOL dropped 9% today", []Topic{topicMoveBTC}, ""},
		"same asset, another class": {
			"btc:rsi", "BTC RSI is 85, extended", []Topic{topicMoveBTC}, ""},
		"another plan": {
			"", "plan 4 hedge is drifting", []Topic{topicDN3}, ""},
		"liquidation, other asset": {
			"btc:liq", "BTC perp: liquidation is 9% away", []Topic{topicLiqETH}, ""},
		"drift is not a drawdown": {
			"alloc:drift", "Allocation is off target, rebalance when convenient", []Topic{topicDD}, ""},
		"a sentence about the portfolio is not a drawdown": {
			"", "Your portfolio is well diversified and the book looks balanced", []Topic{topicDD}, ""},
		"a funding change on an asset that has a move topic": {
			"btc:funding", "BTC perp funding turned sharply negative", []Topic{topicMoveBTC}, ""},
		"words that only look like a class (price, margin, down, cross)": {
			"", "BTC price crossed 70,000, margin looks fine, the feed is delayed, due soon", all, ""},
		"an alert line is matched by its key only": {
			"", "BTC crossed above 70000", []Topic{topicLevel}, ""},
		"an external trigger is matched by its key only": {
			"deploy:42", "The deploy finished with warnings", []Topic{topicExt}, ""},
		"an unknown kind is matched by its key only": {
			"btc:drop", "BTC dropped", []Topic{topic("newkind:BTC", "BTC")}, ""},
		"no topics": {
			"btc:drop", "BTC dropped", nil, ""},
		"unrelated": {
			"news:fed", "The Fed holds rates; relevant to your bond ETF", all, ""},

		// ---- a citation is always a repeat, whatever the words
		"citing the key as the decision's own key": {
			"move_vs_vol:btc:1h:tier1", "BTC limit buy filled", []Topic{topicMoveBTC}, topicMoveBTC.Key},
		"citing the key as the start of the decision's own key": {
			"drawdown:portfolio:day:tier1:update", "a new holding appeared", []Topic{topicDD}, topicDD.Key},
		"citing the key in the text": {
			"x:y", "Follow-up on dn_health:3:margin_critical: the plan was paused", []Topic{topicDN3}, topicDN3.Key},
	} {
		t.Run(name, func(t *testing.T) {
			got, hit := Match(tc.key, tc.text, tc.topics)
			if tc.want == "" {
				if hit {
					t.Fatalf("dropped as a repeat of %q, want it kept", got.Key)
				}
				return
			}
			if !hit || got.Key != tc.want {
				t.Fatalf("matched %q (%v), want %q", got.Key, hit, tc.want)
			}
		})
	}
}

// Each case of an event no trigger reports has a guard word list, and a decision
// with one of its words is kept even when it also speaks of a move in the asset.
func TestMatch_EveryUncoveredEventIsGuarded(t *testing.T) {
	cases := map[string]string{
		"fill":    "BTC fell and your limit order filled",
		"holding": "BTC fell; a new holding appeared",
		"plan":    "BTC fell; the DCA plan was paused",
		"cash":    "BTC fell; you made a deposit",
		"source":  "BTC fell; the market source recovered",
	}
	for guard := range guardWords {
		text, ok := cases[guard]
		if !ok {
			t.Errorf("guard list %q has no test", guard)
			continue
		}
		if _, hit := Match("", text, []Topic{topicMoveBTC}); hit {
			t.Errorf("%s: %q was dropped by its words", guard, text)
		}
		// Without the guard word the same sentence is the move.
		if _, hit := Match("", "BTC fell", []Topic{topicMoveBTC}); !hit {
			t.Fatal("control: BTC fell is the move")
		}
	}
}

// Filter keeps silent decisions and everything that is not a repeat, and says what
// it dropped and why.
func TestFilter(t *testing.T) {
	ds := []proactive.Decision{
		{Kind: proactive.DecideSilent},
		notify("btc:drop", "BTC dropped 8%"),
		notify("eth:funding", "ETH funding turned negative"),
		{Kind: proactive.DecideAsk, DedupKey: "p:dd", Text: "Portfolio drawdown", Question: "Reduce risk?"},
		notify("btc:fill", "BTC limit buy filled"),
	}
	kept, dropped := Filter(ds, []Topic{topicMoveBTC, topicDD})
	if len(kept) != 3 || kept[0].Kind != proactive.DecideSilent || kept[1].DedupKey != "eth:funding" || kept[2].DedupKey != "btc:fill" {
		t.Fatalf("kept %+v", kept)
	}
	if len(dropped) != 2 || dropped[0].Topic.Key != topicMoveBTC.Key || dropped[1].Topic.Key != topicDD.Key {
		t.Fatalf("dropped %+v", dropped)
	}
	if kept, dropped := Filter(ds, nil); len(kept) != 5 || len(dropped) != 0 {
		t.Fatalf("with no topics: kept %d, dropped %d", len(kept), len(dropped))
	}
}

func TestKnownKindsAndKindOf(t *testing.T) {
	if len(KnownKinds()) != 10 {
		t.Errorf("%d kinds: %v", len(KnownKinds()), KnownKinds())
	}
	for _, k := range KnownKinds() {
		info := kinds[k]
		if info.class == "" {
			t.Errorf("kind %s has no class", k)
		}
		if len(classWords[info.class]) == 0 && info.class != classExternal && info.class != classLevel && info.class != classDrawdown {
			t.Errorf("class %s (kind %s) has no words", info.class, k)
		}
	}
	if got := KindOf("Move_Vs_Vol:BTC"); got != "move_vs_vol" {
		t.Errorf("KindOf = %q", got)
	}
}

// Text is read up to a cap, so a model that writes a page cannot make matching slow
// or put a topic far below the cut.
func TestMatch_ReadsABoundedPrefix(t *testing.T) {
	long := strings.Repeat("filler words about nothing ", 200) + " BTC dropped"
	if _, hit := Match("", long, []Topic{topicMoveBTC}); hit {
		t.Fatal("text beyond the cap was read")
	}
}

// symbolBase reads a base asset out of the symbol formats the keys carry, and
// nothing else.
func TestSymbolBase(t *testing.T) {
	for in, want := range map[string]string{
		"BTC/USDT": "BTC", "ETH/USDT_USDT": "ETH", "BTC-USDT-SWAP": "BTC", "AAPL": "AAPL", "sol": "SOL",
		"": "", "margin critical": "", "x$y": "", strings.Repeat("A", 21): "",
	} {
		if got := symbolBase(in); got != want {
			t.Errorf("symbolBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TopicSubject: the log's base asset wins; the key's words are used only where the
// kind's format puts a symbol or a plan number.
func TestTopicSubject(t *testing.T) {
	for name, tc := range map[string]struct {
		t           Topic
		asset, plan string
	}{
		"base asset from the log wins": {topic("move_vs_vol:Bitcoin:1h:tier1", "BTC"), "BTC", ""},
		"asset from the key":           {topic("move_vs_vol:AAPL:1h:tier1", ""), "AAPL", ""},
		"plan number":                  {topic("dn_health:3:margin_critical", ""), "", "3"},
		"a plan key with no number":    {topic("dn_health:plan:margin_critical", ""), "", ""},
		"portfolio-wide":               {topic("drawdown:portfolio:day:tier1", ""), "", ""},
		"unknown kind":                 {topic("newkind:BTC", "BTC"), "", ""},
	} {
		if a, p := TopicSubject(tc.t); a != tc.asset || p != tc.plan {
			t.Errorf("%s: (%q, %q), want (%q, %q)", name, a, p, tc.asset, tc.plan)
		}
	}
}
