package proactive

import (
	"fmt"
	"strings"
)

// ModelTier names which of an agent's models a proactive turn runs on. It is
// the vocabulary the configuration, the gateway and the agent loop share, so
// that none of them has to know how the loop picks a model.
//
// The zero value is ModelTierPrimary: a caller that says nothing gets the
// normal model, never the cheap one.
type ModelTier string

const (
	// ModelTierPrimary is the agent's normal model.
	ModelTierPrimary ModelTier = "primary"
	// ModelTierLight is the agent's light model when it has one, else the
	// normal model.
	ModelTierLight ModelTier = "light"
)

// Valid reports whether t is a known tier. The zero value is valid: it means
// primary.
func (t ModelTier) Valid() bool {
	return t == "" || t == ModelTierPrimary || t == ModelTierLight
}

// String returns "primary" or "light"; the zero value is "primary". An unknown
// tier gets a placeholder that never parses back, so it cannot be mistaken for
// data.
func (t ModelTier) String() string {
	switch t {
	case "", ModelTierPrimary:
		return string(ModelTierPrimary)
	case ModelTierLight:
		return string(ModelTierLight)
	}
	return fmt.Sprintf("model_tier(%q)", string(t))
}

// ParseModelTier parses a tier name, case-insensitively. Empty text is
// ModelTierPrimary; unknown text is an error, never a silent default.
func ParseModelTier(text string) (ModelTier, error) {
	switch t := ModelTier(strings.ToLower(strings.TrimSpace(text))); t {
	case "", ModelTierPrimary:
		return ModelTierPrimary, nil
	case ModelTierLight:
		return ModelTierLight, nil
	}
	return ModelTierPrimary, fmt.Errorf("proactive: unknown model tier %q", text)
}

// MarshalText implements encoding.TextMarshaler. The zero value encodes as
// "primary"; an unknown tier is an error.
func (t ModelTier) MarshalText() ([]byte, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("proactive: cannot marshal %s", t)
	}
	return []byte(t.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *ModelTier) UnmarshalText(text []byte) error {
	v, err := ParseModelTier(string(text))
	if err != nil {
		return err
	}
	*t = v
	return nil
}
