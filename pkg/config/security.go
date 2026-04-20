// KhunQuant - Ultra-lightweight personal AI agent
// License: MIT

package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
)

const (
	SecurityConfigFile = ".security.yml"
)

// securityPath returns the path to .security.yml relative to the config file directory.
func securityPath(configPath string) string {
	configDir := filepath.Dir(configPath)
	return filepath.Join(configDir, SecurityConfigFile)
}

// loadSecurityConfig loads the security configuration from .security.yml and
// overlays sensitive field values onto cfg. Returns nil if the file doesn't exist.
func loadSecurityConfig(cfg *Config, securityPath string) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	data, err := os.ReadFile(securityPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read security config: %w", err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("failed to parse security config: %w", err)
	}

	return nil
}

// saveSecurityConfig saves the security configuration to .security.yml with 0o600 permissions.
func saveSecurityConfig(securityPath string, sec *Config) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(sec); err != nil {
		return fmt.Errorf("failed to marshal security config: %w", err)
	}
	return fileutil.WriteFileAtomic(securityPath, buf.Bytes(), 0o600)
}

// SensitiveDataCache caches the strings.Replacer for filtering sensitive data.
// Computed once on first access via sync.Once.
type SensitiveDataCache struct {
	replacer *strings.Replacer
	once     sync.Once
}

// SensitiveDataReplacer returns a strings.Replacer that replaces all sensitive
// credential values with "[FILTERED]". Computed once and cached.
func (cfg *Config) SensitiveDataReplacer() *strings.Replacer {
	cfg.initSensitiveCache()
	return cfg.sensitiveCache.replacer
}

// FilterSensitiveData replaces all sensitive credential values in content with "[FILTERED]".
// Returns content unchanged if filtering is disabled or content is shorter than FilterMinLength.
func (cfg *Config) FilterSensitiveData(content string) string {
	if !cfg.Tools.IsFilterSensitiveDataEnabled() {
		return content
	}
	if len(content) < cfg.Tools.GetFilterMinLength() {
		return content
	}
	return cfg.SensitiveDataReplacer().Replace(content)
}

// SecurityCopyFrom loads the security config from configPath and overlays its
// values onto this Config. Used by web handlers to restore secrets after a
// JSON round-trip that strips "[NOT_HERE]" fields.
func (cfg *Config) SecurityCopyFrom(configPath string) error {
	return loadSecurityConfig(cfg, securityPath(configPath))
}

// initSensitiveCache initializes the sensitive data cache if not already done.
func (cfg *Config) initSensitiveCache() {
	if cfg.sensitiveCache == nil {
		cfg.sensitiveCache = &SensitiveDataCache{}
	}
	cfg.sensitiveCache.once.Do(func() {
		values := cfg.collectSensitiveValues()
		if len(values) == 0 {
			cfg.sensitiveCache.replacer = strings.NewReplacer()
			return
		}

		// Build old/new pairs for strings.Replacer
		var pairs []string
		for _, v := range values {
			if len(v) > 3 {
				pairs = append(pairs, v, "[FILTERED]")
			}
		}
		if len(pairs) == 0 {
			cfg.sensitiveCache.replacer = strings.NewReplacer()
			return
		}
		cfg.sensitiveCache.replacer = strings.NewReplacer(pairs...)
	})
}

// collectSensitiveValues collects all resolved SecureString values from the Config.
func (cfg *Config) collectSensitiveValues() []string {
	var values []string
	collectSensitive(reflect.ValueOf(cfg), &values)
	return values
}

// collectSensitive recursively traverses v and collects SecureString/SecureStrings values.
func collectSensitive(v reflect.Value, values *[]string) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}

	t := v.Type()

	// SecureString: collect via String() method (defined on *SecureString)
	if t == reflect.TypeOf(SecureString{}) {
		result := v.Addr().MethodByName("String").Call(nil)
		if len(result) > 0 {
			if s := result[0].String(); s != "" {
				*values = append(*values, s)
			}
		}
		return
	}

	// SecureStrings ([]*SecureString): iterate and collect each element
	if t == reflect.TypeOf(SecureStrings{}) {
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			for elem.Kind() == reflect.Ptr || elem.Kind() == reflect.Interface {
				if elem.IsNil() {
					elem = reflect.Value{}
					break
				}
				elem = elem.Elem()
			}
			if elem.IsValid() && elem.Type() == reflect.TypeOf(SecureString{}) {
				result := elem.Addr().MethodByName("String").Call(nil)
				if len(result) > 0 {
					if s := result[0].String(); s != "" {
						*values = append(*values, s)
					}
				}
			}
		}
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			collectSensitive(v.Field(i), values)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			collectSensitive(v.Index(i), values)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			collectSensitive(v.MapIndex(key), values)
		}
	}
}
