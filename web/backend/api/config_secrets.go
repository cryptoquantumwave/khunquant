package api

import (
	"encoding/json"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// redactedSentinel is the placeholder written in place of a secret value in
// GET /api/config responses. It matches the marker config.SecureString already
// marshals itself as, so the dashboard sees one consistent "value withheld"
// token regardless of whether a field is SecureString-backed.
const redactedSentinel = "[NOT_HERE]"

// secretFieldNames are the JSON keys whose values are credentials. The set
// lives in pkg/config so log redaction uses the same definition; see
// config.CredentialFieldNames for why it is keyed by name.
var secretFieldNames = config.CredentialFieldNames

// redactSecrets walks a marshaled config tree and replaces every non-empty
// credential value with redactedSentinel.
//
// Deliberately keyed on field name rather than an allowlist of known config
// paths: a newly added provider or search backend is redacted the moment it is
// added, without anyone remembering to update this file. That is the property
// worth having — the failure mode of a path list is a silently leaking field.
//
// Empty values are left alone so the dashboard can still tell "not configured"
// from "configured but withheld".
func redactSecrets(node any) {
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			if secretFieldNames[strings.ToLower(key)] && !isEmptySecret(val) {
				v[key] = withheldValue(val)
				continue
			}
			redactSecrets(val)
		}
	case []any:
		for _, item := range v {
			redactSecrets(item)
		}
	}
}

// withheldValue returns the redacted stand-in for a credential, preserving the
// JSON type of the original. A list-valued credential such as api_keys becomes
// a list of sentinels of the same length rather than a bare string: collapsing
// it to a scalar changes the field's type, which breaks both the dashboard's
// parsing and the round-trip back through PUT /api/config (the decoder rejects
// a string where it expects []string).
func withheldValue(val any) any {
	if list, ok := val.([]any); ok {
		out := make([]any, len(list))
		for i := range list {
			out[i] = redactedSentinel
		}
		return out
	}
	return redactedSentinel
}

// isEmptySecret reports whether a secret-named value carries nothing worth
// hiding, so that an unset credential stays visibly unset.
func isEmptySecret(val any) bool {
	switch v := val.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	}
	return false
}

// restoreRedactedSecrets copies values from onDisk into incoming wherever the
// caller echoed back redactedSentinel.
//
// The dashboard reads config, edits one field, and writes the whole document
// back — so every credential it never touched arrives as the sentinel. Without
// this, the first save from the UI would overwrite every provider and tool
// credential with the literal string "[NOT_HERE]".
//
// config.SecureString handles this case itself on unmarshal, and Config.
// SecurityCopyFrom restores anything held in .security.yml. This covers the
// yaml:"-" subtrees that neither of those reach.
func restoreRedactedSecrets(incoming, onDisk any) {
	inMap, ok := incoming.(map[string]any)
	if !ok {
		return
	}
	diskMap, ok := onDisk.(map[string]any)
	if !ok {
		return
	}
	for key, val := range inMap {
		diskVal, present := diskMap[key]
		if !present {
			continue
		}
		if isWithheld(val) {
			inMap[key] = diskVal
			continue
		}
		restoreRedactedSecrets(val, diskVal)
	}
}

// isWithheld reports whether an incoming value is the redaction stand-in —
// either the scalar sentinel or a list composed entirely of it.
func isWithheld(val any) bool {
	switch v := val.(type) {
	case string:
		return v == redactedSentinel
	case []any:
		if len(v) == 0 {
			return false
		}
		for _, item := range v {
			if s, ok := item.(string); !ok || s != redactedSentinel {
				return false
			}
		}
		return true
	}
	return false
}

// configAsTree loads the config from disk and returns it as a generic tree,
// with credentials intact. Used to restore values a caller echoed back as the
// redaction sentinel.
func (h *Handler) configAsTree() (any, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return tree, nil
}
