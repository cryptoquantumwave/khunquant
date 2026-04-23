package api

import (
	"encoding/json"
	"net/http"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

type channelCatalogItem struct {
	Name      string `json:"name"`
	ConfigKey string `json:"config_key"`
	Variant   string `json:"variant,omitempty"`
	Enabled   bool   `json:"enabled"`
}

var channelCatalog = []channelCatalogItem{
	{Name: "telegram", ConfigKey: "telegram"},
	{Name: "discord", ConfigKey: "discord"},
	{Name: "slack", ConfigKey: "slack"},
	{Name: "feishu", ConfigKey: "feishu"},
	{Name: "dingtalk", ConfigKey: "dingtalk"},
	{Name: "line", ConfigKey: "line"},
	{Name: "qq", ConfigKey: "qq"},
	{Name: "onebot", ConfigKey: "onebot"},
	{Name: "wecom", ConfigKey: "wecom"},
	{Name: "wecom_app", ConfigKey: "wecom_app"},
	{Name: "wecom_aibot", ConfigKey: "wecom_aibot"},
	{Name: "whatsapp", ConfigKey: "whatsapp", Variant: "bridge"},
	{Name: "whatsapp_native", ConfigKey: "whatsapp", Variant: "native"},
	{Name: "pico", ConfigKey: "pico"},
	{Name: "maixcam", ConfigKey: "maixcam"},
	{Name: "matrix", ConfigKey: "matrix"},
	{Name: "irc", ConfigKey: "irc"},
}

// registerChannelRoutes binds read-only channel catalog endpoints to the ServeMux.
func (h *Handler) registerChannelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/channels/catalog", h.handleListChannelCatalog)
}

// handleListChannelCatalog returns all supported channels with their enabled status.
//
//	GET /api/channels/catalog
func (h *Handler) handleListChannelCatalog(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, "failed to load config", http.StatusInternalServerError)
		return
	}

	enabledByConfigKey := map[string]bool{
		"telegram":    cfg.Channels.Telegram.Enabled,
		"discord":     cfg.Channels.Discord.Enabled,
		"slack":       cfg.Channels.Slack.Enabled,
		"feishu":      cfg.Channels.Feishu.Enabled,
		"dingtalk":    cfg.Channels.DingTalk.Enabled,
		"line":        cfg.Channels.LINE.Enabled,
		"qq":          cfg.Channels.QQ.Enabled,
		"onebot":      cfg.Channels.OneBot.Enabled,
		"wecom":       cfg.Channels.WeCom.Enabled,
		"wecom_app":   cfg.Channels.WeComApp.Enabled,
		"wecom_aibot": cfg.Channels.WeComAIBot.Enabled,
		"whatsapp":    cfg.Channels.WhatsApp.Enabled,
		"pico":        cfg.Channels.Pico.Enabled,
		"maixcam":     cfg.Channels.MaixCam.Enabled,
		"matrix":      cfg.Channels.Matrix.Enabled,
		"irc":         cfg.Channels.IRC.Enabled,
	}

	result := make([]channelCatalogItem, len(channelCatalog))
	for i, ch := range channelCatalog {
		result[i] = ch
		result[i].Enabled = enabledByConfigKey[ch.ConfigKey]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"channels": result,
	})
}
