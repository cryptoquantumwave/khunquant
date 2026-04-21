package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// registerConfigRoutes binds configuration management endpoints to the ServeMux.
func (h *Handler) registerConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config", h.handleGetConfig)
	mux.HandleFunc("PUT /api/config", h.handleUpdateConfig)
	mux.HandleFunc("PATCH /api/config", h.handlePatchConfig)
}

// handleGetConfig returns the complete system configuration.
//
//	GET /api/config
func (h *Handler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// handleUpdateConfig updates the complete system configuration.
//
//	PUT /api/config
func (h *Handler) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var raw map[string]any
	if err = json.Unmarshal(body, &raw); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	if err = normalizeChannelArrayFields(raw); err != nil {
		http.Error(w, fmt.Sprintf("Invalid channel array field: %v", err), http.StatusBadRequest)
		return
	}
	normalizedBody, err := json.Marshal(raw)
	if err != nil {
		http.Error(w, "Failed to normalize config payload", http.StatusBadRequest)
		return
	}
	var cfg config.Config
	if err = json.Unmarshal(normalizedBody, &cfg); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	// Restore secrets that the UI sent back as "[NOT_HERE]" from .security.yml.
	if err := cfg.SecurityCopyFrom(h.configPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to load security config: %v", err), http.StatusInternalServerError)
		return
	}
	if execAllowRemoteOmitted(body) {
		cfg.Tools.Exec.AllowRemote = config.DefaultConfig().Tools.Exec.AllowRemote
	}

	if errs := validateConfig(&cfg); len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "validation_error",
			"errors": errs,
		})
		return
	}

	if err := config.SaveConfig(h.configPath, &cfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func execAllowRemoteOmitted(body []byte) bool {
	var raw struct {
		Tools *struct {
			Exec *struct {
				AllowRemote *bool `json:"allow_remote"`
			} `json:"exec"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	return raw.Tools == nil || raw.Tools.Exec == nil || raw.Tools.Exec.AllowRemote == nil
}

// handlePatchConfig partially updates the system configuration using JSON Merge Patch (RFC 7396).
// Only the fields present in the request body will be updated; all other fields remain unchanged.
//
//	PATCH /api/config
func (h *Handler) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	patchBody, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Validate the patch is valid JSON
	var patch map[string]any
	if err = json.Unmarshal(patchBody, &patch); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Load existing config and marshal to a map for merging
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load config: %v", err), http.StatusInternalServerError)
		return
	}

	existing, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "Failed to serialize current config", http.StatusInternalServerError)
		return
	}

	var base map[string]any
	if err = json.Unmarshal(existing, &base); err != nil {
		http.Error(w, "Failed to parse current config", http.StatusInternalServerError)
		return
	}

	// Recursively merge patch into base
	mergeMap(base, patch)
	if err = normalizeChannelArrayFields(base); err != nil {
		http.Error(w, fmt.Sprintf("Invalid channel array field: %v", err), http.StatusBadRequest)
		return
	}

	// Convert merged map back to Config struct
	merged, err := json.Marshal(base)
	if err != nil {
		http.Error(w, "Failed to serialize merged config", http.StatusInternalServerError)
		return
	}

	var newCfg config.Config
	if err := json.Unmarshal(merged, &newCfg); err != nil {
		http.Error(w, fmt.Sprintf("Merged config is invalid: %v", err), http.StatusBadRequest)
		return
	}
	// Restore secrets that round-tripped through JSON as "[NOT_HERE]".
	if err := newCfg.SecurityCopyFrom(h.configPath); err != nil {
		http.Error(w, fmt.Sprintf("Failed to load security config: %v", err), http.StatusInternalServerError)
		return
	}

	if errs := validateConfig(&newCfg); len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "validation_error",
			"errors": errs,
		})
		return
	}

	if err := config.SaveConfig(h.configPath, &newCfg); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// validateConfig checks the config for common errors before saving.
// Returns a list of human-readable error strings; empty means valid.
func validateConfig(cfg *config.Config) []string {
	var errs []string

	// Validate model_list entries
	if err := cfg.ValidateModelList(); err != nil {
		errs = append(errs, err.Error())
	}

	// Gateway port range
	if cfg.Gateway.Port != 0 && (cfg.Gateway.Port < 1 || cfg.Gateway.Port > 65535) {
		errs = append(errs, fmt.Sprintf("gateway.port %d is out of valid range (1-65535)", cfg.Gateway.Port))
	}

	// Pico channel: token required when enabled
	if cfg.Channels.Pico.Enabled && cfg.Channels.Pico.Token.String() == "" {
		errs = append(errs, "channels.pico.token is required when pico channel is enabled")
	}

	// Telegram: token required when enabled
	if cfg.Channels.Telegram.Enabled && cfg.Channels.Telegram.Token.String() == "" {
		errs = append(errs, "channels.telegram.token is required when telegram channel is enabled")
	}

	// Discord: token required when enabled
	if cfg.Channels.Discord.Enabled && cfg.Channels.Discord.Token.String() == "" {
		errs = append(errs, "channels.discord.token is required when discord channel is enabled")
	}

	// Binance: at least one account required when enabled
	if cfg.Exchanges.Binance.Enabled {
		if acc, ok := cfg.Exchanges.Binance.ResolveAccount(""); !ok || acc.APIKey.String() == "" || acc.Secret.String() == "" {
			errs = append(errs, "exchanges.binance: at least one account with api_key and secret is required when Binance is enabled")
		}
	}

	return errs
}

// mergeMap recursively merges src into dst (JSON Merge Patch semantics).
// - If a key in src has a null value, it is deleted from dst.
// - If both dst and src have a nested object for the same key, merge recursively.
// - Otherwise the value from src overwrites dst.
func mergeMap(dst, src map[string]any) {
	for key, srcVal := range src {
		if srcVal == nil {
			delete(dst, key)
			continue
		}
		srcMap, srcIsMap := srcVal.(map[string]any)
		dstMap, dstIsMap := dst[key].(map[string]any)
		if srcIsMap && dstIsMap {
			mergeMap(dstMap, srcMap)
		} else {
			dst[key] = srcVal
		}
	}
}

func asMapField(value map[string]any, key string) (map[string]any, bool) {
	raw, exists := value[key]
	if !exists {
		return nil, false
	}
	m, isMap := raw.(map[string]any)
	return m, isMap
}

var (
	allowFromHiddenCharsRe = regexp.MustCompile(`[\x{200B}\x{200C}\x{200D}\x{200E}\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2069}\x{FEFF}]`)
	allowFromSplitRe       = regexp.MustCompile(`[,\x{FF0C}\x{3001};\x{FF1B}\r\n\t]+`)
	conservativeSplitRe    = regexp.MustCompile(`[,\x{FF0C}\r\n\t]+`)
)

type stringArrayParserOptions struct {
	stripHiddenChars bool
}

func normalizeChannelArrayFields(raw map[string]any) error {
	channelsMap, hasChannels := asMapField(raw, "channel_list")
	if !hasChannels {
		return nil
	}

	defaultCfg := config.DefaultConfig()
	for channelName, rawChannel := range channelsMap {
		chMap, ok := rawChannel.(map[string]any)
		if !ok {
			continue
		}

		if rawAllowFrom, exists := chMap["allow_from"]; exists {
			normalized, err := normalizeStringArrayValue(rawAllowFrom, stringArrayParserOptions{
				stripHiddenChars: true,
			})
			if err != nil {
				return fmt.Errorf("channel_list.%s.allow_from: %w", channelName, err)
			}
			chMap["allow_from"] = normalized
		}

		if groupTrigger, ok := asMapField(chMap, "group_trigger"); ok {
			if rawPrefixes, exists := groupTrigger["prefixes"]; exists {
				normalized, err := normalizeStringArrayValue(rawPrefixes, stringArrayParserOptions{})
				if err != nil {
					return fmt.Errorf("channel_list.%s.group_trigger.prefixes: %w", channelName, err)
				}
				groupTrigger["prefixes"] = normalized
			}
		}

		settingsMap, hasSettings := asMapField(chMap, "settings")
		if !hasSettings {
			continue
		}

		settingsType := channelSettingsType(defaultCfg, channelName, chMap)
		if settingsType == nil {
			continue
		}

		for i := range settingsType.NumField() {
			field := settingsType.Field(i)
			if !field.IsExported() || !isStringSliceType(field.Type) {
				continue
			}
			jsonKey := strings.Split(field.Tag.Get("json"), ",")[0]
			if jsonKey == "" || jsonKey == "-" {
				continue
			}
			rawValue, exists := settingsMap[jsonKey]
			if !exists {
				continue
			}

			options := stringArrayParserOptions{}
			if jsonKey == "allow_from" {
				options.stripHiddenChars = true
			}
			normalized, err := normalizeStringArrayValue(rawValue, options)
			if err != nil {
				return fmt.Errorf("channel_list.%s.settings.%s: %w", channelName, jsonKey, err)
			}
			settingsMap[jsonKey] = normalized
		}
	}
	return nil
}

func channelSettingsType(
	_ *config.Config,
	_ string,
	_ map[string]any,
) reflect.Type {
	return nil // struct-based channels config doesn't support dynamic type lookup
}

func derefType(typ reflect.Type) reflect.Type {
	for typ != nil && typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return typ
}

func isStringSliceType(typ reflect.Type) bool {
	typ = derefType(typ)
	return typ != nil && typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.String
}

func normalizeStringArrayValue(value any, options stringArrayParserOptions) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return parseStringArrayValue(typed, options), nil
	case float64:
		return normalizeStringArrayItems([]string{fmt.Sprintf("%.0f", typed)}, options), nil
	case []string:
		return normalizeStringArrayItems(typed, options), nil
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			switch raw := item.(type) {
			case string:
				items = append(items, raw)
			case float64:
				items = append(items, fmt.Sprintf("%.0f", raw))
			default:
				return nil, fmt.Errorf("unsupported list item type %T", item)
			}
		}
		return normalizeStringArrayItems(items, options), nil
	default:
		return nil, fmt.Errorf("unsupported list field type %T", value)
	}
}

func parseStringArrayValue(raw string, options stringArrayParserOptions) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	splitRe := conservativeSplitRe
	if options.stripHiddenChars {
		splitRe = allowFromSplitRe
	}
	return normalizeStringArrayItems(splitRe.Split(raw, -1), options)
}

func normalizeStringArrayItems(items []string, options stringArrayParserOptions) []string {
	result := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		normalized := item
		if options.stripHiddenChars {
			normalized = allowFromHiddenCharsRe.ReplaceAllString(normalized, "")
		}
		normalized = strings.TrimSpace(normalized)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	if len(result) == 0 {
		return []string{}
	}
	return result
}

func getSecretString(m map[string]any, key string) (string, bool) {
	if raw, exists := m[key]; exists {
		s, isString := raw.(string)
		if isString {
			return s, true
		}
	}
	if raw, exists := m["_"+key]; exists {
		s, isString := raw.(string)
		if isString {
			return s, true
		}
	}
	return "", false
}

func applyConfigSecretsFromMap(_ *config.Config, _ map[string]any) {
	// No-op: khunquant uses struct-based ChannelsConfig and doesn't support
	// dynamic channel_list secret injection from the API payload.
	// Secrets are restored via SecurityCopyFrom after each PATCH/PUT.
}

// applySecureStringsToStruct walks a struct and applies SecureString fields
// from the matching keys in rawMap. It recurses into nested maps and slices.
func applySecureStringsToStruct(rv reflect.Value, rawMap map[string]any) {
	rt := rv.Type()
	for jsonKey, rawVal := range rawMap {
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name != jsonKey {
				continue
			}
			sf := rv.Field(i)
			if !sf.CanSet() {
				continue
			}
			// Direct SecureString field
			if s, ok := rawVal.(string); ok {
				if f.Type == reflect.TypeOf(config.SecureString{}) {
					sf.Set(reflect.ValueOf(*config.NewSecureString(s)))
				} else if f.Type == reflect.TypeOf(&config.SecureString{}) {
					sf.Set(reflect.ValueOf(config.NewSecureString(s)))
				}
				continue
			}
			// Recurse into nested struct
			if sf.Kind() == reflect.Struct {
				if nested, ok := rawVal.(map[string]any); ok {
					applySecureStringsToStruct(sf, nested)
				}
				continue
			}
			// Recurse into map fields (e.g., map[string]SomeStruct)
			if sf.Kind() == reflect.Map && sf.Type().Elem().Kind() == reflect.Struct {
				if nestedMap, ok := rawVal.(map[string]any); ok {
					for mapKey, mapVal := range nestedMap {
						nested, ok := mapVal.(map[string]any)
						if !ok {
							continue
						}
						elemType := sf.Type().Elem()
						// Get existing element or create a new zero value
						var elem reflect.Value
						existing := sf.MapIndex(reflect.ValueOf(mapKey))
						if existing.IsValid() {
							if existing.Kind() == reflect.Interface {
								existing = existing.Elem()
							}
							if existing.Kind() == reflect.Ptr && !existing.IsNil() {
								elem = reflect.New(elemType)
								elem.Elem().Set(existing.Elem())
							} else if existing.Kind() == reflect.Struct {
								elem = reflect.New(elemType)
								elem.Elem().Set(existing)
							}
						}
						if !elem.IsValid() {
							elem = reflect.New(elemType)
						}
						applySecureStringsToStruct(elem.Elem(), nested)
						sf.SetMapIndex(reflect.ValueOf(mapKey), elem.Elem())
					}
				}
				continue
			}
			// Recurse into slice elements that are structs
			if sf.Kind() == reflect.Slice && sf.Type().Elem().Kind() == reflect.Struct {
				if sliceRaw, ok := rawVal.([]any); ok {
					for idx, elemRaw := range sliceRaw {
						if nested, ok := elemRaw.(map[string]any); ok {
							if idx < sf.Len() {
								applySecureStringsToStruct(sf.Index(idx), nested)
							}
						}
					}
				}
			}
		}
	}
}
