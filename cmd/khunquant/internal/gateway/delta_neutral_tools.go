package gateway

import "github.com/cryptoquantumwave/khunquant/pkg/config"

// dnToolNames lists every delta-neutral tool that is registered inside the shared
// "delta-neutral enabled" block of setupCronTool. The block is skipped entirely unless at
// least one of them is enabled, so a tool missing from this list is silently never
// registered when it is the only one switched on.
var dnToolNames = []string{
	"create_delta_neutral_plan",
	"list_delta_neutral_plans",
	"get_delta_neutral_plan",
	"update_delta_neutral_plan",
	"delete_delta_neutral_plan",
	"get_delta_neutral_summary",
	"get_delta_neutral_history",
	"prepare_delta_neutral_plan",
	"open_delta_neutral_position",
	"unwind_delta_neutral_position",
	"resize_delta_neutral_position",
	"render_delta_neutral_yield_chart",
}

// dnToolsEnabled reports whether any delta-neutral tool is enabled in cfg.
func dnToolsEnabled(cfg *config.Config) bool {
	for _, name := range dnToolNames {
		if cfg.Tools.IsToolEnabled(name) {
			return true
		}
	}
	return false
}
