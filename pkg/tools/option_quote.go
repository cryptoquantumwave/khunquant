package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OptionQuoteTool fetches option quotes with greeks.
type OptionQuoteTool struct {
	cfg *config.Config
}

func NewOptionQuoteTool(cfg *config.Config) *OptionQuoteTool {
	return &OptionQuoteTool{cfg: cfg}
}

func (t *OptionQuoteTool) Name() string { return NameOptionQuote }

func (t *OptionQuoteTool) Description() string {
	return "Fetch market data for options contracts including price, greeks (delta, gamma, theta, vega, rho), implied volatility, and open interest. Requires underlying symbol, expiry date (yyyy-MM-dd), strike price, and option type (CALL/PUT)."
}

func (t *OptionQuoteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":   map[string]any{"type": "string", "description": "Provider/exchange name (e.g. 'webull')."},
			"account":    map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"underlying": map[string]any{"type": "string", "description": "Underlying asset symbol (e.g. 'AAPL')."},
			"expiry":     map[string]any{"type": "string", "description": "Expiration date in yyyy-MM-dd format."},
			"strike":     map[string]any{"type": "number", "description": "Strike price."},
			"option_type": map[string]any{
				"type":        "string",
				"enum":        []string{"CALL", "PUT"},
				"description": "Option type.",
			},
		},
		"required": []string{"provider", "underlying", "expiry", "strike", "option_type"},
	}
}

func (t *OptionQuoteTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	underlying, _ := args["underlying"].(string)
	expiry, _ := args["expiry"].(string)
	strike, _ := args["strike"].(float64)
	optionType, _ := args["option_type"].(string)

	// --- Validation ---
	if providerID == "" || underlying == "" || expiry == "" || optionType == "" {
		return ErrorResult("provider, underlying, expiry, and option_type are required")
	}
	if strike <= 0 {
		return ErrorResult("strike must be positive")
	}

	// Validate option_type
	optionTypeUpper := strings.ToUpper(optionType)
	if optionTypeUpper != "CALL" && optionTypeUpper != "PUT" {
		return ErrorResult("option_type must be CALL or PUT")
	}

	// Create provider
	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	// Type assert to OptionMarketDataProvider
	omp, ok := p.(broker.OptionMarketDataProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support option market data", providerID))
	}

	// Build contract
	contract := broker.OptionContract{
		Underlying: strings.ToUpper(underlying),
		Expiry:     expiry,
		Strike:     strike,
		OptionType: optionTypeUpper,
	}

	// Fetch snapshot
	quotes, err := omp.FetchOptionSnapshot(ctx, []broker.OptionContract{contract})
	if err != nil {
		if hint := reauthHint(err, providerID, account); hint != nil {
			return hint
		}
		// Check for subscription error
		if strings.Contains(err.Error(), "Insufficient permission") || strings.Contains(err.Error(), "subscription") {
			return ErrorResult(fmt.Sprintf("option market data requires a US_OPTION subscription: %v", err))
		}
		return ErrorResult(fmt.Sprintf("FetchOptionSnapshot failed: %v", err))
	}

	if len(quotes) == 0 {
		return ErrorResult(fmt.Sprintf("no quote data for %s %s %.2f %s", underlying, expiry, strike, optionType))
	}

	quote := quotes[0]

	// Format output
	out := fmt.Sprintf(`Option Quote: %s %s %.2f %s
  Symbol:         %s
  Price:          %.4f
  Bid/Ask:        %.4f / %.4f
  Open/Close:     %.4f / %.4f
  High/Low:       %.4f / %.4f
  Change:         %.4f (%.2f%%)
  Greeks:
    Delta:        %.4f
    Gamma:        %.4f
    Theta:        %.4f (per day)
    Vega:         %.4f (per 1%% change in IV)
    Rho:          %.4f (per 1%% change in rates)
  Implied Vol:    %.2f%%
  Open Interest:  %.0f
  Volume:         %.0f
  Updated:        %d ms
`,
		underlying, expiry, strike, optionType,
		quote.Symbol,
		quote.Price,
		quote.Bid, quote.Ask,
		quote.Open, quote.PreClose,
		quote.High, quote.Low,
		quote.Change, quote.ChangeRatio*100,
		quote.Delta,
		quote.Gamma,
		quote.Theta,
		quote.Vega,
		quote.Rho,
		quote.ImpVol*100,
		quote.OpenInterest,
		quote.Volume,
		quote.Timestamp,
	)

	return UserResult(out)
}
