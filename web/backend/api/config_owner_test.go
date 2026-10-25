package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

const (
	cfgOwnerChatID   = "5551112223334"
	cfgOwnerSenderID = "9876543210123"
	cfgOwnerSender   = "telegram:" + cfgOwnerSenderID

	cfgOwnerChatMask   = "***334"
	cfgOwnerSenderMask = "telegram:***123"
)

// cfgAllowFrom is a channel allow-list the dashboard must keep showing and
// editing; the owner masking must not reach it.
var cfgAllowFrom = []string{"7770001112223", "alice_the_trader"}

// ownerConfigEnv is a config with planted credentials, a configured owner and
// a channel allow-list.
func ownerConfigEnv(t *testing.T) (*Handler, string) {
	t.Helper()
	path := writeConfigWithSecrets(t)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: cfgOwnerChatID, SenderID: cfgOwnerSender}
	cfg.Proactive.Attention.DailyBudget = 5
	cfg.Channels.Telegram.AllowFrom = config.FlexibleStringSlice(cfgAllowFrom)
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	return NewHandler(path), path
}

// sendConfig drives the PUT or PATCH handler directly with the headers the CSRF
// check needs.
func sendConfig(t *testing.T, h *Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/config", bytes.NewBufferString(body))
	req.Header.Set("Origin", "http://launcher.local")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Host = "launcher.local"
	rec := httptest.NewRecorder()
	switch method {
	case http.MethodPut:
		h.handleUpdateConfig(rec, req)
	case http.MethodPatch:
		h.handlePatchConfig(rec, req)
	default:
		t.Fatalf("unsupported method %s", method)
	}
	return rec
}

func mustSend(t *testing.T, h *Handler, method, body string) {
	t.Helper()
	if rec := sendConfig(t, h, method, body); rec.Code != http.StatusOK {
		t.Fatalf("%s /api/config = %d, want 200 (body: %s)", method, rec.Code, rec.Body.String())
	}
}

func mustReject(t *testing.T, h *Handler, method, body string) string {
	t.Helper()
	rec := sendConfig(t, h, method, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%s /api/config = %d, want 400 (body: %s)", method, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// proactiveSection returns the "proactive" section of config.json exactly as it
// is on disk, re-indented only so two reads compare byte for byte.
func proactiveSection(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	section, ok := doc["proactive"]
	if !ok {
		t.Fatal("config.json has no proactive section")
	}
	return string(section)
}

func storedOwner(t *testing.T, path string) config.OwnerConfig {
	t.Helper()
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg.Proactive.Owner
}

func wantOwner(t *testing.T, path string, want config.OwnerConfig) {
	t.Helper()
	if got := storedOwner(t, path); got != want {
		t.Fatalf("stored owner changed: got %+v, want %+v", got, want)
	}
}

var originalOwner = config.OwnerConfig{Channel: "telegram", ChatID: cfgOwnerChatID, SenderID: cfgOwnerSender}

// configDoc is GET /api/config parsed, for tests that edit the body.
func configDoc(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(getConfigBody(t, h)), &doc); err != nil {
		t.Fatalf("unmarshal GET body: %v", err)
	}
	return doc
}

func ownerOf(doc map[string]any) map[string]any {
	return doc["proactive"].(map[string]any)["owner"].(map[string]any)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestHandleGetConfig_MasksTheOwnerIDs(t *testing.T) {
	h, _ := ownerConfigEnv(t)
	body := getConfigBody(t, h)

	for _, secret := range []string{cfgOwnerChatID, cfgOwnerSenderID} {
		if strings.Contains(body, secret) {
			t.Fatalf("GET /api/config contains the owner ID %q in the clear", secret)
		}
	}
	owner := ownerOf(configDoc(t, h))
	want := map[string]any{"channel": "telegram", "chat_id": cfgOwnerChatMask, "sender_id": cfgOwnerSenderMask}
	if !reflect.DeepEqual(owner, want) {
		t.Errorf("proactive.owner = %v, want %v", owner, want)
	}
	// The rest of the proactive section is untouched.
	pro := configDoc(t, h)["proactive"].(map[string]any)
	if pro["attention"].(map[string]any)["daily_budget"] != float64(5) {
		t.Errorf("proactive.attention.daily_budget = %v, want 5", pro["attention"])
	}
	for _, key := range []string{"enabled", "heartbeat", "triggers", "policy", "approvals", "learning"} {
		if _, ok := pro[key]; !ok {
			t.Errorf("proactive.%s is missing from GET /api/config", key)
		}
	}
}

func TestHandleGetConfig_MaskedOwnerMatchesTheProactivePage(t *testing.T) {
	h, path := ownerConfigEnv(t)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	status := buildProactiveStatus(cfg.Proactive)
	owner := ownerOf(configDoc(t, h))
	if owner["chat_id"] != status.Owner.ChatIDMasked || owner["sender_id"] != status.Owner.SenderIDMasked {
		t.Errorf("config owner %v differs from the proactive page's %+v", owner, status.Owner)
	}
}

func TestHandleGetConfig_KeepsChannelListsAndCredentialsAsBefore(t *testing.T) {
	h, _ := ownerConfigEnv(t)
	body := getConfigBody(t, h)
	for _, v := range cfgAllowFrom {
		if !strings.Contains(body, v) {
			t.Errorf("allow_from entry %q vanished from GET /api/config", v)
		}
	}
	if strings.Contains(body, secretProbes["provider"]) {
		t.Error("a credential leaked")
	}
	if !strings.Contains(body, redactedSentinel) {
		t.Error("credentials are no longer withheld")
	}
}

func TestHandleGetConfig_UnconfiguredOwnerStaysEmpty(t *testing.T) {
	path := writeConfigWithSecrets(t)
	owner := ownerOf(configDoc(t, NewHandler(path)))
	if owner["chat_id"] != "" || owner["sender_id"] != "" {
		t.Errorf("an unset owner must stay empty, got %v", owner)
	}
}

// The destructive case: the dashboard loads the (masked) config and saves it
// back untouched. The proactive section on disk must not change at all.
func TestHandleUpdateConfig_RoundTripKeepsTheOwner(t *testing.T) {
	h, path := ownerConfigEnv(t)
	before := proactiveSection(t, path)

	for i := 0; i < 2; i++ {
		mustSend(t, h, http.MethodPut, mustJSON(t, configDoc(t, h)))
		if got := proactiveSection(t, path); got != before {
			t.Fatalf("round trip %d changed the proactive section:\nbefore: %s\nafter:  %s", i+1, before, got)
		}
		wantOwner(t, path, originalOwner)
	}
	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Providers.Anthropic.APIKey != secretProbes["provider"] {
		t.Errorf("a credential did not survive the round trip: %q", saved.Providers.Anthropic.APIKey)
	}
	if !reflect.DeepEqual([]string(saved.Channels.Telegram.AllowFrom), cfgAllowFrom) {
		t.Errorf("allow_from = %v, want %v", saved.Channels.Telegram.AllowFrom, cfgAllowFrom)
	}
}

func TestHandlePatchConfig_RoundTripKeepsTheOwner(t *testing.T) {
	h, path := ownerConfigEnv(t)
	before := proactiveSection(t, path)

	for i := 0; i < 2; i++ {
		mustSend(t, h, http.MethodPatch, mustJSON(t, map[string]any{"proactive": configDoc(t, h)["proactive"]}))
		if got := proactiveSection(t, path); got != before {
			t.Fatalf("PATCH round trip %d changed the proactive section:\nbefore: %s\nafter:  %s", i+1, before, got)
		}
	}
	// The whole masked document, too.
	mustSend(t, h, http.MethodPatch, mustJSON(t, configDoc(t, h)))
	if got := proactiveSection(t, path); got != before {
		t.Fatalf("PATCH of the whole document changed the proactive section:\nbefore: %s\nafter:  %s", before, got)
	}
}

func TestHandleUpdateConfig_UnrelatedChangeKeepsTheOwner(t *testing.T) {
	h, path := ownerConfigEnv(t)
	doc := configDoc(t, h)
	doc["proactive"].(map[string]any)["attention"].(map[string]any)["daily_budget"] = 9
	doc["gateway"].(map[string]any)["port"] = 18999
	mustSend(t, h, http.MethodPut, mustJSON(t, doc))

	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Proactive.Owner != originalOwner {
		t.Errorf("owner = %+v, want unchanged", saved.Proactive.Owner)
	}
	if saved.Proactive.Attention.DailyBudget != 9 || saved.Gateway.Port != 18999 {
		t.Errorf("the unrelated edits were not saved: budget=%d port=%d",
			saved.Proactive.Attention.DailyBudget, saved.Gateway.Port)
	}
}

func TestHandlePatchConfig_UnrelatedChangeKeepsTheOwner(t *testing.T) {
	h, path := ownerConfigEnv(t)
	mustSend(t, h, http.MethodPatch, `{"proactive":{"attention":{"daily_budget":9}}}`)
	wantOwner(t, path, originalOwner)
	if saved, _ := config.LoadConfig(path); saved.Proactive.Attention.DailyBudget != 9 {
		t.Error("the patched field was not saved")
	}
}

func TestHandleUpdateConfig_NewOwnerIsStoredAndNormalised(t *testing.T) {
	h, path := ownerConfigEnv(t)
	doc := configDoc(t, h)
	owner := ownerOf(doc)
	owner["channel"] = "Telegram"
	owner["chat_id"] = "4440001112223"
	owner["sender_id"] = "Telegram:8880001112223"
	mustSend(t, h, http.MethodPut, mustJSON(t, doc))
	wantOwner(t, path, config.OwnerConfig{
		Channel: "telegram", ChatID: "4440001112223", SenderID: "telegram:8880001112223",
	})
	// A credential and the allow-list are intact after an owner change.
	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Providers.Anthropic.APIKey != secretProbes["provider"] ||
		saved.Tools.Web.Brave.APIKey != secretProbes["brave"] ||
		saved.Channels.Telegram.Token.String() != secretProbes["channel"] {
		t.Error("a credential was damaged by an owner change")
	}
	if !reflect.DeepEqual([]string(saved.Channels.Telegram.AllowFrom), cfgAllowFrom) {
		t.Errorf("allow_from = %v, want %v", saved.Channels.Telegram.AllowFrom, cfgAllowFrom)
	}
}

func TestHandlePatchConfig_NewOwnerIsStored(t *testing.T) {
	h, path := ownerConfigEnv(t)
	mustSend(t, h, http.MethodPatch,
		`{"proactive":{"owner":{"channel":"telegram","chat_id":"4440001112223","sender_id":"telegram:8880001112223"}}}`)
	wantOwner(t, path, config.OwnerConfig{
		Channel: "telegram", ChatID: "4440001112223", SenderID: "telegram:8880001112223",
	})
}

func TestHandleConfig_RefusesWhatTheOwnerRouteRefuses(t *testing.T) {
	cases := map[string]map[string]any{
		"group chat":           {"channel": "telegram", "chat_id": "-1001234567890", "sender_id": cfgOwnerSender},
		"non-canonical":        {"channel": "telegram", "chat_id": cfgOwnerChatID, "sender_id": "9876543210123"},
		"platform mismatch":    {"channel": "discord", "chat_id": cfgOwnerChatID, "sender_id": cfgOwnerSender},
		"username as sender":   {"channel": "telegram", "chat_id": cfgOwnerChatID, "sender_id": "telegram:alice"},
		"only chat id cleared": {"channel": "telegram", "chat_id": "", "sender_id": cfgOwnerSender},
		"only sender cleared":  {"channel": "telegram", "chat_id": cfgOwnerChatID, "sender_id": ""},
		"channel only":         {"channel": "telegram", "chat_id": "", "sender_id": ""},
	}
	for name, owner := range cases {
		for _, method := range []string{http.MethodPut, http.MethodPatch} {
			t.Run(name+"/"+method, func(t *testing.T) {
				h, path := ownerConfigEnv(t)
				before := proactiveSection(t, path)
				var body string
				if method == http.MethodPut {
					doc := configDoc(t, h)
					doc["proactive"].(map[string]any)["owner"] = owner
					body = mustJSON(t, doc)
				} else {
					body = mustJSON(t, map[string]any{"proactive": map[string]any{"owner": owner}})
				}
				msg := mustReject(t, h, method, body)
				if strings.Contains(msg, cfgOwnerChatID) || strings.Contains(msg, cfgOwnerSenderID) {
					t.Errorf("the error leaks a stored ID: %s", msg)
				}
				if got := proactiveSection(t, path); got != before {
					t.Errorf("a refused save changed the proactive section:\n%s", got)
				}
			})
		}
	}
}

func TestHandleConfig_MaskShapedValueThatIsNotTheStoredMaskIsRefused(t *testing.T) {
	cases := map[string]map[string]any{
		"another chat mask":     {"chat_id": "***999"},
		"another sender mask":   {"sender_id": "telegram:***999"},
		"bare star prefix":      {"chat_id": "***"},
		"mask of a short id":    {"sender_id": "***"},
		"sender masked as chat": {"chat_id": cfgOwnerSenderMask},
		"embedded":              {"chat_id": "55***334"},
	}
	for name, change := range cases {
		for _, method := range []string{http.MethodPut, http.MethodPatch} {
			t.Run(name+"/"+method, func(t *testing.T) {
				h, path := ownerConfigEnv(t)
				before := proactiveSection(t, path)
				var body string
				if method == http.MethodPut {
					doc := configDoc(t, h)
					for k, v := range change {
						ownerOf(doc)[k] = v
					}
					body = mustJSON(t, doc)
				} else {
					body = mustJSON(t, map[string]any{"proactive": map[string]any{"owner": change}})
				}
				msg := mustReject(t, h, method, body)
				for _, v := range change {
					if strings.Contains(msg, v.(string)) {
						t.Errorf("the error echoes the rejected value: %s", msg)
					}
				}
				if got := proactiveSection(t, path); got != before {
					t.Errorf("a refused save changed the proactive section:\n%s", got)
				}
			})
		}
	}
}

func TestHandleConfig_MaskIsRefusedWhenNoOwnerIsStored(t *testing.T) {
	path := writeConfigWithSecrets(t)
	h := NewHandler(path)
	doc := configDoc(t, h)
	ownerOf(doc)["chat_id"] = cfgOwnerChatMask
	mustReject(t, h, http.MethodPut, mustJSON(t, doc))
	wantOwner(t, path, config.OwnerConfig{})
}

// An omitted field is how an older client says "not mine to change": the stored
// value stays. Clearing needs an explicit empty value for all three fields.
func TestHandleUpdateConfig_OmittedOwnerKeepsIt(t *testing.T) {
	t.Run("proactive section omitted", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		doc := configDoc(t, h)
		delete(doc, "proactive")
		mustSend(t, h, http.MethodPut, mustJSON(t, doc))
		wantOwner(t, path, originalOwner)
	})
	t.Run("owner omitted", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		doc := configDoc(t, h)
		delete(doc["proactive"].(map[string]any), "owner")
		mustSend(t, h, http.MethodPut, mustJSON(t, doc))
		wantOwner(t, path, originalOwner)
	})
	t.Run("ids omitted", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		doc := configDoc(t, h)
		owner := ownerOf(doc)
		delete(owner, "chat_id")
		delete(owner, "sender_id")
		mustSend(t, h, http.MethodPut, mustJSON(t, doc))
		wantOwner(t, path, originalOwner)
	})
	t.Run("channel and ids omitted", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		doc := configDoc(t, h)
		doc["proactive"].(map[string]any)["owner"] = map[string]any{}
		mustSend(t, h, http.MethodPut, mustJSON(t, doc))
		wantOwner(t, path, originalOwner)
	})
}

func TestHandleConfig_ExplicitEmptyOwnerClearsIt(t *testing.T) {
	t.Run("PUT", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		doc := configDoc(t, h)
		doc["proactive"].(map[string]any)["owner"] = map[string]any{"channel": "", "chat_id": "", "sender_id": ""}
		mustSend(t, h, http.MethodPut, mustJSON(t, doc))
		wantOwner(t, path, config.OwnerConfig{})
	})
	t.Run("PATCH", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		mustSend(t, h, http.MethodPatch, `{"proactive":{"owner":{"channel":"","chat_id":"","sender_id":""}}}`)
		wantOwner(t, path, config.OwnerConfig{})
	})
	t.Run("PATCH null", func(t *testing.T) {
		h, path := ownerConfigEnv(t)
		mustSend(t, h, http.MethodPatch, `{"proactive":{"owner":null}}`)
		wantOwner(t, path, config.OwnerConfig{})
	})
}

// A hand-edited, incomplete owner in config.json must not make every dashboard
// save fail: an unchanged owner is never re-validated.
func TestHandleUpdateConfig_UnchangedIncompleteOwnerStillSaves(t *testing.T) {
	path := writeConfigWithSecrets(t)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: "-1001234567890"}
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(path)
	before := proactiveSection(t, path)
	mustSend(t, h, http.MethodPut, mustJSON(t, configDoc(t, h)))
	if got := proactiveSection(t, path); got != before {
		t.Errorf("proactive section changed:\n%s", got)
	}
}

// Channel configs have allow-lists of their own; they are shown and saved
// exactly as before, including when the owner changes in the same save.
func TestHandleUpdateConfig_ChannelAllowListsAreEditable(t *testing.T) {
	h, path := ownerConfigEnv(t)
	doc := configDoc(t, h)
	tg := doc["channels"].(map[string]any)["telegram"].(map[string]any)
	tg["allow_from"] = []any{"7770001112223", "bob", "1239998887776"}
	mustSend(t, h, http.MethodPut, mustJSON(t, doc))

	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"7770001112223", "bob", "1239998887776"}
	if !reflect.DeepEqual([]string(saved.Channels.Telegram.AllowFrom), want) {
		t.Errorf("allow_from = %v, want %v", saved.Channels.Telegram.AllowFrom, want)
	}
	if saved.Proactive.Owner != originalOwner {
		t.Errorf("owner = %+v, want unchanged", saved.Proactive.Owner)
	}
	if body := getConfigBody(t, h); !strings.Contains(body, "1239998887776") {
		t.Error("an allow_from entry is no longer returned by GET /api/config")
	}
}

// The mask must be applied to the owner path only, not by field name.
func TestMaskOwnerInTree_TouchesOnlyTheOwner(t *testing.T) {
	tree := map[string]any{
		"proactive": map[string]any{"owner": map[string]any{
			"channel": "telegram", "chat_id": cfgOwnerChatID, "sender_id": cfgOwnerSender,
		}},
		"somewhere": map[string]any{"chat_id": cfgOwnerChatID, "sender_id": cfgOwnerSender},
		"channels":  map[string]any{"x": map[string]any{"allow_from": []any{cfgOwnerChatID}}},
	}
	maskOwnerInTree(tree)
	owner := tree["proactive"].(map[string]any)["owner"].(map[string]any)
	if owner["chat_id"] != cfgOwnerChatMask || owner["sender_id"] != cfgOwnerSenderMask || owner["channel"] != "telegram" {
		t.Errorf("owner = %v", owner)
	}
	other := tree["somewhere"].(map[string]any)
	if other["chat_id"] != cfgOwnerChatID || other["sender_id"] != cfgOwnerSender {
		t.Errorf("a same-named field elsewhere was masked: %v", other)
	}
	if tree["channels"].(map[string]any)["x"].(map[string]any)["allow_from"].([]any)[0] != cfgOwnerChatID {
		t.Error("an allow-list entry was masked")
	}
	maskOwnerInTree(nil)
	maskOwnerInTree(map[string]any{"proactive": "x"})
}

// No launcher response other than GET /api/config carries the config; the
// write routes answer with a status only, even for an owner change.
func TestConfigWriteRoutes_DoNotEchoTheOwner(t *testing.T) {
	h, _ := ownerConfigEnv(t)
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		body := mustJSON(t, configDoc(t, h))
		rec := sendConfig(t, h, method, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", method, rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
			t.Errorf("%s response = %s", method, got)
		}
	}
}

// The route is behind the same CSRF check as before.
func TestConfigOwnerSave_StillRequiresSameOrigin(t *testing.T) {
	h, path := ownerConfigEnv(t)
	before := proactiveSection(t, path)
	req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewBufferString(`{}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.handleUpdateConfig(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("a cross-site PUT was accepted")
	}
	if proactiveSection(t, path) != before {
		t.Error("a refused request changed the config")
	}
}
