package sandbox

import (
	"fmt"
	"regexp"
)

// ValidateStatus checks that a status code is a legal HTTP status (100..999).
// HTTP status codes outside this range will panic at http.ResponseWriter.WriteHeader.
func ValidateStatus(status int) error {
	if status < 100 || status > 999 {
		return fmt.Errorf("status code must be 100..999, got %d", status)
	}
	return nil
}

// ValidateFixtureEntry validates a FixtureEntry for correctness on write.
// It checks:
// - Status is a legal HTTP status (100..999)
// - Method is non-empty
// - PathPrefix starts with /
// - At most one of Body and BodyText is set (Body wins if both are set)
func ValidateFixtureEntry(e FixtureEntry) error {
	if err := ValidateStatus(e.Response.Status); err != nil {
		return fmt.Errorf("fixture response status: %w", err)
	}

	if e.Method == "" {
		return fmt.Errorf("fixture method must not be empty")
	}

	if e.PathPrefix == "" {
		return fmt.Errorf("fixture path_prefix must not be empty")
	}

	if e.PathPrefix[0] != '/' {
		return fmt.Errorf("fixture path_prefix must start with /, got %q", e.PathPrefix)
	}

	// Body takes precedence over BodyText; document this but allow both to be set
	// (do not reject — it is not a broken combination, just unusual)

	return nil
}

var venueRegex = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidateVenueName validates that a venue name is safe to join to a filesystem path.
// It checks that the name is non-empty and matches [a-z0-9_-]+ to prevent path traversal
// attacks (e.g., "../../escaped"). BOTH the gateway API (sandbox_api.go) and the MCP
// tools (devmcp/sandbox_tools.go) must call this before joining venue to a path.
func ValidateVenueName(venue string) error {
	if venue == "" {
		return fmt.Errorf("venue must not be empty")
	}

	if !venueRegex.MatchString(venue) {
		return fmt.Errorf("venue name must match [a-z0-9_-]+, got %q", venue)
	}

	return nil
}
