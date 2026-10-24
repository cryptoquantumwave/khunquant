package config

import (
	"reflect"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// CredentialFieldNames are the JSON keys whose values are credentials.
//
// Most credentials in this config are config.SecureString, which withholds its
// own value on marshal and is restored from .security.yml on save. But three
// large subtrees are tagged yaml:"-" — Providers, Tools, and Debug — so their
// fields cannot be SecureString: they would marshal as withheld and then have
// nothing to be restored from, silently blanking the credential on next save.
// Those subtrees hold plain strings, and this set is what keeps them out of API
// responses (web/backend/api) and out of logs (registerSecretsForRedaction).
//
// Matching is by exact key name, so "max_tokens" is unaffected by "token".
var CredentialFieldNames = map[string]bool{
	"api_key":              true,
	"api_keys":             true,
	"token":                true,
	"secret":               true,
	"password":             true,
	"passphrase":           true,
	"app_secret":           true,
	"client_secret":        true,
	"bot_token":            true,
	"app_token":            true,
	"access_token":         true,
	"channel_access_token": true,
	"channel_secret":       true,
	"verification_token":   true,
	"encoding_aes_key":     true,
	"webhook_secret":       true,
	"webhook_url":          true,
	"private_key":          true,
	"signing_secret":       true,
	// Exact-key matching means the "token" entry above does not cover this one.
	"management_token": true,
}

// minRedactableSecretLen keeps short values out of the redaction set. Redact
// is a plain substring replace, so registering "1" or "abc" would mangle
// unrelated log text while protecting nothing.
const minRedactableSecretLen = 8

// registerSecretsForRedaction registers every credential in cfg with the
// logger, so it is replaced in log output wherever it appears.
//
// H-9: registration used to happen only inside individual exchange adapters,
// and only once one was constructed. LLM provider keys, channel tokens and the
// gateway management token were never registered, so any error or debug line
// that echoed one - a provider's error body, a URL with a token query - wrote
// it to the log verbatim. Registering at load covers every SecureString the
// config holds, including ones added later, without a per-field list.
func registerSecretsForRedaction(cfg *Config) {
	walkCredentials(reflect.ValueOf(cfg), false, func(s string) {
		if len(s) >= minRedactableSecretLen {
			logger.RegisterSecret(s)
		}
	})
}

var secureStringType = reflect.TypeOf(SecureString{})

// walkCredentials visits every credential value under v: every SecureString,
// and every string or []string held in a field whose JSON name is in
// CredentialFieldNames. named reports whether v itself sits under such a
// field. Both are needed - the yaml:"-" subtrees (Providers, Tools, Debug)
// cannot use SecureString, so their keys are plain strings found by name.
func walkCredentials(v reflect.Value, named bool, visit func(string)) {
	if !v.IsValid() {
		return
	}
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return
		}
		if v.Kind() == reflect.Ptr && v.Type().Elem() == secureStringType {
			visit(v.Interface().(*SecureString).String())
			return
		}
		v = v.Elem()
	}

	if v.Type() == secureStringType {
		s := v.Interface().(SecureString)
		visit(s.String())
		return
	}

	switch v.Kind() {
	case reflect.String:
		if named {
			visit(v.String())
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			walkCredentials(v.Field(i), CredentialFieldNames[strings.ToLower(name)], visit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkCredentials(v.Index(i), named, visit)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			// A map keyed by credential name, e.g. an MCP server's env.
			keyNamed := named
			if key.Kind() == reflect.String {
				keyNamed = keyNamed || CredentialFieldNames[strings.ToLower(key.String())]
			}
			walkCredentials(v.MapIndex(key), keyNamed, visit)
		}
	}
}
