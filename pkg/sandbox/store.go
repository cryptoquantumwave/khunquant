package sandbox

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Store holds all fixtures, keyed by venue. It is concurrency-safe.
type Store struct {
	mu       sync.RWMutex
	fixtures map[string][]FixtureEntry // venue → list of fixtures
}

// NewStore creates an empty fixture store.
func NewStore() *Store {
	return &Store{
		fixtures: make(map[string][]FixtureEntry),
	}
}

// Load reads all fixture files from the given directory. The directory structure
// should be: <dir>/<venue>/fixtures.json (or other .json files). Each JSON file
// should contain a JSON array of FixtureEntry objects.
//
// If the directory does not exist, Load returns silently (no error).
// Malformed JSON in any file causes an error.
func (s *Store) Load(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if the directory exists.
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		logger.Debugf("fixtures directory does not exist: %s", dir)
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat fixtures directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("fixtures path is not a directory: %s", dir)
	}

	// List all venue directories.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read fixtures directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue // skip files in the root
		}

		venue := entry.Name()
		venueDir := filepath.Join(dir, venue)

		// Load all .json files from the venue directory.
		venueEntries, err := os.ReadDir(venueDir)
		if err != nil {
			return fmt.Errorf("read venue directory %s: %w", venueDir, err)
		}

		var fixtures []FixtureEntry
		for _, venueEntry := range venueEntries {
			if venueEntry.IsDir() {
				continue // skip subdirectories
			}
			if !strings.HasSuffix(venueEntry.Name(), ".json") {
				continue // skip non-JSON files
			}

			filePath := filepath.Join(venueDir, venueEntry.Name())
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("read fixture file %s: %w", filePath, err)
			}

			var entries []FixtureEntry
			if err := json.Unmarshal(data, &entries); err != nil {
				return fmt.Errorf("unmarshal fixture file %s: %w", filePath, err)
			}

			fixtures = append(fixtures, entries...)
		}

		if len(fixtures) > 0 {
			s.fixtures[venue] = fixtures
			logger.Debugf("loaded %d fixtures for venue %s from %s", len(fixtures), venue, venueDir)

			// Warn about fixtures that are shadowed by the stateful simulator.
			for _, fixture := range fixtures {
				if SimulatorOwnedPath(venue, fixture.Method, fixture.PathPrefix) {
					logger.WarnF(
						fmt.Sprintf("fixture for %s %s (venue=%s) is shadowed by the stateful simulator and will not be used",
							fixture.Method, fixture.PathPrefix, venue),
						map[string]any{"venue": venue, "method": fixture.Method, "path": fixture.PathPrefix},
					)
				}
			}
		}
	}

	return nil
}

// FindFixture returns the best-matching fixture for the given venue, method, path, and query.
// Matching is done by: (1) method and path prefix, (2) optional query constraints.
// If multiple fixtures match, the most specific (highest query constraint count, then longest path) wins.
// Method comparison is case-insensitive. If no fixture is found, returns nil.
func (s *Store) FindFixture(venue, method, path string, query url.Values) *FixtureEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fixtures := s.fixtures[venue]
	method = strings.ToUpper(method)

	var best *FixtureEntry
	bestQueryCount := -1
	bestPathLength := -1

	for i := range fixtures {
		f := &fixtures[i]

		// Check method and path prefix first
		if !strings.EqualFold(f.Method, method) || !strings.HasPrefix(path, f.PathPrefix) {
			continue
		}

		// If fixture has no query constraints, it matches any query
		// If it does have constraints, check that all are present and match
		if !queryMatches(f, query) {
			continue
		}

		// Calculate specificity score: (query constraint count, path length)
		queryCount := len(f.Query)
		pathLength := len(f.PathPrefix)

		// Keep the best match (highest query count, then longest path)
		if queryCount > bestQueryCount ||
			(queryCount == bestQueryCount && pathLength > bestPathLength) {
			best = f
			bestQueryCount = queryCount
			bestPathLength = pathLength
		}
	}

	return best
}

// HasPathMatch returns true if any fixture exists for the given venue, method, and path.
// Used to distinguish between "no fixture for this path" vs "fixture exists but query didn't match".
func (s *Store) HasPathMatch(venue, method, path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fixtures := s.fixtures[venue]
	method = strings.ToUpper(method)

	for i := range fixtures {
		f := &fixtures[i]
		if strings.EqualFold(f.Method, method) && strings.HasPrefix(path, f.PathPrefix) {
			return true
		}
	}

	return false
}

// queryMatches returns true if the request's query parameters satisfy the fixture's query constraints.
// A fixture with no query constraints matches any request.
// A fixture with constraints requires all constraint keys to be present with matching values.
func queryMatches(f *FixtureEntry, requestQuery url.Values) bool {
	// If fixture has no query constraints, it matches anything
	if len(f.Query) == 0 {
		return true
	}

	// Check that all fixture constraints are present in request with matching values
	for key, expectedValue := range f.Query {
		actualValues, ok := requestQuery[key]
		if !ok {
			// Required key not present in request
			return false
		}

		// Check that the expected value is in the request's values for this key
		found := false
		for _, actualVal := range actualValues {
			if actualVal == expectedValue {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

// SetFixtures replaces all fixtures for a given venue. Used by the web UI and MCP
// tools to update fixtures at runtime. Panics if venue is empty.
func (s *Store) SetFixtures(venue string, entries []FixtureEntry) {
	if venue == "" {
		panic("venue must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(entries) == 0 {
		delete(s.fixtures, venue)
	} else {
		s.fixtures[venue] = entries
	}
}

// GetFixtures returns a copy of all fixtures for a given venue.
func (s *Store) GetFixtures(venue string) []FixtureEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if entries, ok := s.fixtures[venue]; ok {
		result := make([]FixtureEntry, len(entries))
		copy(result, entries)
		return result
	}

	return nil
}

// Venues returns a sorted list of all venues with fixtures.
func (s *Store) Venues() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	venues := make([]string, 0, len(s.fixtures))
	for v := range s.fixtures {
		venues = append(venues, v)
	}

	return venues
}
