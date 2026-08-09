package sandbox

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
)

// RewriteExchangeURLs recursively rewrites all URL-shaped strings in a CCXT exchange's
// Urls field to point to the sandbox server. It handles nested maps and slices generically
// using reflection to cover all possible structures (map[string]any, []any, etc).
//
// The exchange parameter should be a CCXT exchange instance (*ccxt.Binance, *ccxt.Okx, etc).
// The function extracts the Urls and Hostname fields from the exchange using reflection,
// rewrites all URLs to sandbox addresses, and writes them back.
//
// The resulting sandbox address follows:
//
//	http://127.0.0.1:<port>/__sbx__/<venue>/<real-host>/<original-path>
//
// Returns an error if any URL parsing fails or if Hostname cannot be extracted from the exchange.
// After a successful rewrite, call VerifyExchangeURLsSandboxed to ensure all URLs point to loopback.
func RewriteExchangeURLs(venue string, exchange interface{}, baseURL string) error {
	if baseURL == "" {
		return fmt.Errorf("sandbox: baseURL is empty")
	}

	// Extract Hostname and Urls from the exchange using reflection
	hostname, err := extractHostname(exchange)
	if err != nil {
		return fmt.Errorf("extract hostname: %w", err)
	}

	urls, err := extractURLs(exchange)
	if err != nil {
		return fmt.Errorf("extract URLs: %w", err)
	}

	if urls == nil {
		return nil // No URLs to rewrite
	}

	// Rewrite the URLs
	if err := walkAndRewriteURLs(venue, urls, baseURL, hostname); err != nil {
		return err
	}

	// Write the modified URLs back to the exchange
	if err := writeURLs(exchange, urls); err != nil {
		return fmt.Errorf("write URLs back: %w", err)
	}

	return nil
}

// VerifyExchangeURLsSandboxed walks a CCXT exchange's Urls field and asserts that every
// URL-shaped string points to loopback (127.0.0.1, ::1, or localhost). If any non-loopback
// URL is found, it returns an error with the key path and the offending URL.
//
// The exchange parameter should be a CCXT exchange instance. The function extracts the
// Urls field from the exchange using reflection.
//
// This guard must be called after RewriteExchangeURLs to ensure all URLs are sandboxed.
// If this check fails, the exchange client must not be used.
func VerifyExchangeURLsSandboxed(exchange interface{}) error {
	urls, err := extractURLs(exchange)
	if err != nil {
		return fmt.Errorf("extract URLs: %w", err)
	}

	if urls == nil {
		return nil
	}

	return walkAndVerifyURLs(urls, "urls")
}

// extractHostname extracts the Hostname field from a CCXT exchange instance using reflection.
func extractHostname(exchange interface{}) (string, error) {
	v := reflect.ValueOf(exchange)

	// Dereference pointer
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return "", fmt.Errorf("exchange is nil")
		}
		v = v.Elem()
	}

	// Try to find the Hostname field (may be embedded in a struct)
	if field := v.FieldByName("Hostname"); field.IsValid() && field.Kind() == reflect.String {
		return field.String(), nil
	}

	// Try to find it in an embedded Exchange struct
	if exchangeField := v.FieldByName("Exchange"); exchangeField.IsValid() {
		return extractHostname(exchangeField.Interface())
	}

	// Try to find it in a Core field
	if coreField := v.FieldByName("Core"); coreField.IsValid() {
		return extractHostname(coreField.Interface())
	}

	return "", fmt.Errorf("could not extract Hostname from exchange")
}

// extractURLs extracts the Urls field from a CCXT exchange instance using reflection.
// It returns a deep copy so modifications don't affect the original.
func extractURLs(exchange interface{}) (interface{}, error) {
	v := reflect.ValueOf(exchange)

	// Dereference pointer
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, fmt.Errorf("exchange is nil")
		}
		v = v.Elem()
	}

	// Try to find the Urls field
	if field := v.FieldByName("Urls"); field.IsValid() {
		return field.Interface(), nil
	}

	// Try to find it in an embedded Exchange struct
	if exchangeField := v.FieldByName("Exchange"); exchangeField.IsValid() {
		return extractURLs(exchangeField.Interface())
	}

	// Try to find it in a Core field
	if coreField := v.FieldByName("Core"); coreField.IsValid() {
		return extractURLs(coreField.Interface())
	}

	return nil, fmt.Errorf("could not extract Urls from exchange")
}

// writeURLs writes the modified Urls back to a CCXT exchange instance using reflection.
func writeURLs(exchange interface{}, urls interface{}) error {
	v := reflect.ValueOf(exchange)

	// Dereference pointer
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return fmt.Errorf("exchange is nil")
		}
		v = v.Elem()
	}

	// Try to find the Urls field and write to it
	if field := v.FieldByName("Urls"); field.IsValid() && field.CanSet() {
		field.Set(reflect.ValueOf(urls))
		return nil
	}

	// Try to write through an embedded Exchange struct
	if exchangeField := v.FieldByName("Exchange"); exchangeField.IsValid() {
		return writeURLs(exchangeField.Interface(), urls)
	}

	// Try to write through a Core field
	if coreField := v.FieldByName("Core"); coreField.IsValid() {
		return writeURLs(coreField.Interface(), urls)
	}

	return fmt.Errorf("could not write Urls to exchange")
}

// walkAndRewriteURLs recursively walks a nested structure (maps, slices, strings) and
// rewrites any URL-shaped strings to sandbox addresses. Uses reflection to handle
// arbitrary nesting and types. Works directly with the map reference passed in.
func walkAndRewriteURLs(venue string, obj interface{}, baseURL, exchangeHostname string) error {
	if obj == nil {
		return nil
	}

	// Extract the actual map and walk it
	m, ok := obj.(map[string]interface{})
	if !ok {
		return fmt.Errorf("urls object is not map[string]interface{}: %T", obj)
	}

	return walkAndRewriteMap(m, venue, baseURL, exchangeHostname)
}

// walkAndRewriteMap directly walks a map[string]interface{} and rewrites URLs in-place.
// Works with the actual map reference to ensure modifications persist.
func walkAndRewriteMap(m map[string]interface{}, venue, baseURL, exchangeHostname string) error {
	for key, val := range m {
		// Handle string URLs
		if urlStr, ok := val.(string); ok {
			if isURLLike(urlStr) {
				newURL, err := rewriteURL(venue, urlStr, baseURL, exchangeHostname)
				if err != nil {
					return fmt.Errorf("rewrite URL at key %s: %w", key, err)
				}
				m[key] = newURL // Modify in-place
			}
		} else if nestedMap, ok := val.(map[string]interface{}); ok {
			// Recursively handle nested maps
			if err := walkAndRewriteMap(nestedMap, venue, baseURL, exchangeHostname); err != nil {
				return err
			}
		} else if sliceVal, ok := val.([]interface{}); ok {
			// Handle slices
			for i, item := range sliceVal {
				if urlStr, ok := item.(string); ok {
					if isURLLike(urlStr) {
						newURL, err := rewriteURL(venue, urlStr, baseURL, exchangeHostname)
						if err != nil {
							return fmt.Errorf("rewrite URL at key %s[%d]: %w", key, i, err)
						}
						sliceVal[i] = newURL
					}
				} else if nestedMap, ok := item.(map[string]interface{}); ok {
					if err := walkAndRewriteMap(nestedMap, venue, baseURL, exchangeHostname); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// walkValue recursively walks a reflect.Value and rewrites URLs in-place.
func walkValue(v reflect.Value, venue, baseURL, exchangeHostname, keyPath string) error {
	// Dereference pointers
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.String:
		// If this is a URL, replace it in the parent map (we need context to do this)
		// This is handled by the map/slice cases below.
		return nil

	case reflect.Map:
		// Walk map entries and rewrite URLs in-place
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			val := iter.Value()
			newKeyPath := keyPath
			if keyPath != "" {
				newKeyPath = keyPath + "." + key.String()
			} else {
				newKeyPath = key.String()
			}

			// If value is a string and looks like a URL, rewrite it
			if val.Kind() == reflect.String {
				urlStr := val.String()
				if isURLLike(urlStr) {
					newURL, err := rewriteURL(venue, urlStr, baseURL, exchangeHostname)
					if err != nil {
						return fmt.Errorf("rewrite URL at %s: %w", newKeyPath, err)
					}
					v.SetMapIndex(key, reflect.ValueOf(newURL))
				}
			} else if val.Kind() == reflect.Map || val.Kind() == reflect.Slice || val.Kind() == reflect.Ptr {
				// Recursively walk complex types
				if err := walkValue(val, venue, baseURL, exchangeHostname, newKeyPath); err != nil {
					return err
				}
			}
		}
		return nil

	case reflect.Slice:
		// Walk slice entries and rewrite URLs in-place
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			newKeyPath := fmt.Sprintf("%s[%d]", keyPath, i)

			// If element is a string and looks like a URL, rewrite it
			if elem.Kind() == reflect.String {
				urlStr := elem.String()
				if isURLLike(urlStr) {
					newURL, err := rewriteURL(venue, urlStr, baseURL, exchangeHostname)
					if err != nil {
						return fmt.Errorf("rewrite URL at %s: %w", newKeyPath, err)
					}
					elem.SetString(newURL)
				}
			} else if elem.Kind() == reflect.Map || elem.Kind() == reflect.Slice || elem.Kind() == reflect.Ptr {
				// Recursively walk complex types
				if err := walkValue(elem, venue, baseURL, exchangeHostname, newKeyPath); err != nil {
					return err
				}
			}
		}
		return nil
	}

	return nil
}

// rewriteURL takes an original URL string and rewrites it to a sandbox address.
// It resolves {hostname} placeholders to the actual exchange hostname.
func rewriteURL(venue, originalURLStr, baseURL, exchangeHostname string) (string, error) {
	// Replace {hostname} placeholder with the actual hostname
	urlStr := strings.ReplaceAll(originalURLStr, "{hostname}", exchangeHostname)

	// Parse the URL
	originalURL, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("parse URL %q: %w", originalURLStr, err)
	}

	// Use sandbox.BuildURL to rewrite to sandbox address
	sandboxURL, err := BuildURL(venue, originalURL, baseURL)
	if err != nil {
		return "", fmt.Errorf("build sandbox URL: %w", err)
	}

	return sandboxURL.String(), nil
}

// isURLLike returns true if a string looks like a URL (starts with http:// or https://).
func isURLLike(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// walkAndVerifyURLs recursively walks a nested structure and verifies that every
// URL-shaped string points to loopback. Returns an error with the key path if a
// non-loopback URL is found.
func walkAndVerifyURLs(obj interface{}, rootKeyPath string) error {
	if obj == nil {
		return nil
	}

	v := reflect.ValueOf(obj)
	return verifyValue(v, rootKeyPath)
}

// verifyValue recursively verifies that all URL strings point to loopback.
func verifyValue(v reflect.Value, keyPath string) error {
	// Dereference pointers
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.String:
		// If this is a URL, verify it points to loopback
		urlStr := v.String()
		if isURLLike(urlStr) {
			u, err := url.Parse(urlStr)
			if err != nil {
				return fmt.Errorf("parse URL at %s: %w", keyPath, err)
			}
			if !isLoopback(u.Host) {
				return fmt.Errorf("URL at %s is not loopback: %s", keyPath, urlStr)
			}
		}
		return nil

	case reflect.Map:
		// Walk map entries
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			val := iter.Value()
			newKeyPath := keyPath
			if keyPath != "" {
				newKeyPath = keyPath + "." + key.String()
			} else {
				newKeyPath = key.String()
			}

			if err := verifyValue(val, newKeyPath); err != nil {
				return err
			}
		}
		return nil

	case reflect.Slice:
		// Walk slice entries
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			newKeyPath := fmt.Sprintf("%s[%d]", keyPath, i)

			if err := verifyValue(elem, newKeyPath); err != nil {
				return err
			}
		}
		return nil
	}

	return nil
}
