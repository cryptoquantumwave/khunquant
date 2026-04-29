package dca

import "fmt"

// ValidTimeframes is the set of supported candle timeframe strings.
var ValidTimeframes = map[string]bool{
	"1m": true, "5m": true, "15m": true, "30m": true,
	"1h": true, "2h": true, "4h": true, "6h": true, "12h": true,
	"1d": true, "1w": true,
}

// TimeframeToCron maps a candle timeframe to its natural cron expression.
// The cron fires at each candle close boundary, which is the right cadence for
// re-evaluating indicator conditions.
func TimeframeToCron(tf string) (string, error) {
	switch tf {
	case "1m":
		return "* * * * *", nil
	case "5m":
		return "*/5 * * * *", nil
	case "15m":
		return "*/15 * * * *", nil
	case "30m":
		return "*/30 * * * *", nil
	case "1h":
		return "0 * * * *", nil
	case "2h":
		return "0 */2 * * *", nil
	case "4h":
		return "0 */4 * * *", nil
	case "6h":
		return "0 */6 * * *", nil
	case "12h":
		return "0 */12 * * *", nil
	case "1d":
		return "0 0 * * *", nil
	case "1w":
		return "0 0 * * 1", nil
	default:
		return "", fmt.Errorf("unsupported timeframe %q", tf)
	}
}
