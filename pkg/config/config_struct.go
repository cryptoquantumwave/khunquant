package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/khunquant/khunquant/pkg/credential"
	"github.com/khunquant/khunquant/pkg/logger"
)

const (
	notHere = `"[NOT_HERE]"`
)

// SecureStrings is a slice of SecureString.
//
//nolint:recvcheck
type SecureStrings []*SecureString

// IsZero returns true if the SecureStrings is nil or empty.
// When called from a non-YAML context (e.g. JSON marshal via omitempty), it
// always returns true so the field is omitted — secrets must not appear in JSON.
func (s SecureStrings) IsZero() bool {
	if !callerFromYaml() {
		return true
	}
	return len(s) == 0
}

// Values returns the decrypted/resolved values.
func (s *SecureStrings) Values() []string {
	if s == nil {
		return nil
	}
	keys := make([]string, len(*s))
	for i, k := range *s {
		keys[i] = k.String()
	}
	return unique(keys)
}

// SimpleSecureStrings creates a SecureStrings from plain string values.
func SimpleSecureStrings(val ...string) SecureStrings {
	val = unique(val)
	vv := make(SecureStrings, len(val))
	for i, s := range val {
		vv[i] = NewSecureString(s)
	}
	return vv
}

// unique returns a new slice with duplicate elements removed.
func unique[T comparable](input []T) []T {
	m := make(map[T]struct{})
	var result []T
	for _, v := range input {
		if _, ok := m[v]; !ok {
			m[v] = struct{}{}
			result = append(result, v)
		}
	}
	return result
}

func (s SecureStrings) MarshalJSON() ([]byte, error) {
	return []byte(notHere), nil
}

func (s *SecureStrings) UnmarshalJSON(value []byte) error {
	if string(value) == notHere {
		return nil
	}
	var v []*SecureString
	if err := json.Unmarshal(value, &v); err != nil {
		return err
	}
	*s = v
	return nil
}

// SecureString is a string value that can be decrypted or resolved from
// file:// or enc:// references. Secrets are stored in .security.yml and
// never serialized to config.json (JSON marshaling returns "[NOT_HERE]").
//
//nolint:recvcheck
type SecureString struct {
	resolved string // Decrypted/resolved value returned by String()
	raw      string // Persisted raw value (enc://, file://, or plaintext)
}

// callerFromYaml returns true if the immediate caller is NOT from a yaml.v package.
// Used by IsZero to suppress JSON marshaling of SecureString fields.
func callerFromYaml() bool {
	_, file, _, ok := runtime.Caller(2)
	if ok {
		d := filepath.Dir(file)
		if !strings.Contains(d, "yaml.v") {
			return true
		}
	}
	return false
}

// IsZero returns true if the SecureString is empty.
// If the caller is not yaml, it always returns true to prevent JSON marshaling.
func (s SecureString) IsZero() bool {
	if callerFromYaml() {
		return true
	}
	return s.resolved == ""
}

// NewSecureString creates a SecureString from a raw value (plaintext, file://, or enc://).
func NewSecureString(value string) *SecureString {
	s := &SecureString{}
	if err := s.fromRaw(value); err != nil {
		logger.Warn(fmt.Sprintf("NewSecureString.fromRaw error: %s", err))
	}
	return s
}

// String returns the resolved (decrypted/read) credential value.
func (s *SecureString) String() string {
	if s == nil {
		return ""
	}
	return s.resolved
}

// Set sets the resolved value directly (bypassing raw resolution).
func (s *SecureString) Set(value string) *SecureString {
	s.resolved = value
	s.raw = ""
	return s
}

func (s SecureString) MarshalJSON() ([]byte, error) {
	return []byte(notHere), nil
}

func (s *SecureString) UnmarshalJSON(value []byte) error {
	if string(value) == notHere {
		return nil
	}
	var v string
	if err := json.Unmarshal(value, &v); err != nil {
		return err
	}
	return s.fromRaw(v)
}

func (s SecureString) MarshalYAML() (any, error) {
	// Preserve raw value if it is already a reference (enc:// or file://)
	if strings.HasPrefix(s.raw, credential.EncScheme) || strings.HasPrefix(s.raw, credential.FileScheme) {
		return s.raw, nil
	}
	// If resolved is a reference format (e.g. set via Set), copy back to raw
	if strings.HasPrefix(s.resolved, credential.EncScheme) || strings.HasPrefix(s.resolved, credential.FileScheme) {
		s.raw = s.resolved
		return s.raw, nil
	}
	// Try to encrypt the resolved value
	if passphrase := credential.PassphraseProvider(); passphrase != "" {
		encrypted, err := credential.Encrypt(passphrase, "", s.resolved)
		if err != nil {
			logger.Errorf("Encrypt error: %v", err)
			return nil, err
		}
		s.raw = encrypted
	} else {
		s.raw = s.resolved
	}
	return s.raw, nil
}

func (s *SecureString) UnmarshalYAML(value *yaml.Node) error {
	return s.fromRaw(value.Value)
}

func (s *SecureString) fromRaw(v string) error {
	s.raw = v
	vv, err := resolveKey(v)
	if err != nil {
		return err
	}
	s.resolved = vv
	return nil
}

// UnmarshalText implements encoding.TextUnmarshaler for env variable parsing.
func (s *SecureString) UnmarshalText(text []byte) error {
	return s.fromRaw(string(text))
}

var (
	secResolverMu sync.RWMutex
	secResolver   *credential.Resolver
)

func updateResolver(path string) {
	secResolverMu.Lock()
	defer secResolverMu.Unlock()
	secResolver = credential.NewResolver(path)
}

func resolveKey(v string) (string, error) {
	secResolverMu.RLock()
	resolver := secResolver
	secResolverMu.RUnlock()
	if resolver == nil {
		resolver = credential.NewResolver("")
	}
	if strings.HasPrefix(v, "enc://") || strings.HasPrefix(v, "file://") {
		decrypted, err := resolver.Resolve(v)
		if err != nil {
			logger.Errorf("Resolve error: %v", err)
			return "", err
		}
		return decrypted, nil
	}
	return v, nil
}

// SecureModelList is a []ModelConfig whose api_key fields are persisted in
// .security.yml rather than config.json.
type SecureModelList []ModelConfig

// toNameIndex builds a list of "modelName:index" keys for each entry in the list.
// Duplicate model names are disambiguated by their occurrence index.
func toNameIndex(list []ModelConfig) []string {
	nameList := make([]string, 0, len(list))
	countMap := make(map[string]int)
	for _, model := range list {
		name := model.ModelName
		index := countMap[name]
		nameList = append(nameList, fmt.Sprintf("%s:%d", name, index))
		countMap[name]++
	}
	return nameList
}

// UnmarshalYAML overlays api_key values from the security YAML onto the existing list.
// The YAML structure is a map of "modelName:index" -> {api_key: value}.
func (v *SecureModelList) UnmarshalYAML(value *yaml.Node) error {
	type secEntry struct {
		APIKey SecureString `yaml:"api_key"`
	}
	mm := make(map[string]*secEntry)
	if err := value.Decode(&mm); err != nil {
		logger.Errorf("SecureModelList.UnmarshalYAML Decode error: %v", err)
		return err
	}
	nameList := toNameIndex(*v)
	for i := range *v {
		m := &(*v)[i]
		sec := mm[nameList[i]]
		if sec == nil {
			sec = mm[m.ModelName]
		}
		if sec != nil {
			m.APIKey = sec.APIKey
		}
	}
	return nil
}

// MarshalYAML serializes only api_key fields into the security YAML,
// keyed by "modelName:index".
func (v SecureModelList) MarshalYAML() (any, error) {
	type secEntry struct {
		APIKey SecureString `yaml:"api_key,omitempty"`
	}
	mm := make(map[string]secEntry)
	nameList := toNameIndex(v)
	for i, m := range v {
		mm[nameList[i]] = secEntry{APIKey: m.APIKey}
	}
	return mm, nil
}
