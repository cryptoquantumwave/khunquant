package devmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// registerSandboxTools registers all sandbox fixture management tools with the MCP server.
// These are write-capable tools, deliberately placed in a separate registration function
// to make their intentional addition visible. They are gated on sandbox being enabled.
func registerSandboxTools(s *mcp.Server, d Deps) {
	// Tool: sandbox_list_venues — no parameters, read-only
	s.AddTool(&mcp.Tool{
		Name:        "sandbox_list_venues",
		Description: "List all venues with fixtures, and how many fixture entries each has",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, sandboxListVenuesHandler(d))

	// Tool: sandbox_read_fixtures — venue parameter, read-only
	s.AddTool(&mcp.Tool{
		Name:        "sandbox_read_fixtures",
		Description: "Read all fixture entries for a venue (method, path prefix, status, body)",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"venue":{"type":"string","description":"Venue name (e.g., 'okx', 'binance')"}
			},
			"required":["venue"]
		}`),
	}, sandboxReadFixturesHandler(d))

	// Tool: sandbox_write_fixtures — venue and entries parameters, write (gated)
	s.AddTool(&mcp.Tool{
		Name:        "sandbox_write_fixtures",
		Description: "Replace all fixture entries for a venue. Validates JSON, persists to disk, applies in memory. Gated on sandbox being enabled.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"venue":{"type":"string","description":"Venue name (e.g., 'okx', 'binance')"},
				"entries":{"type":"string","description":"JSON array of fixture entries: [{\"method\":\"GET\",\"path_prefix\":\"/api/v5\",\"response\":{\"status\":200,\"body\":{...}}}]"}
			},
			"required":["venue","entries"]
		}`),
	}, sandboxWriteFixturesHandler(d))

	// Tool: sandbox_upsert_fixture — venue, method, path_prefix, and response parameters, write (gated)
	s.AddTool(&mcp.Tool{
		Name:        "sandbox_upsert_fixture",
		Description: "Upsert a single fixture entry (add or replace) without resending the whole venue file. Gated on sandbox being enabled.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"venue":{"type":"string","description":"Venue name (e.g., 'okx', 'binance')"},
				"method":{"type":"string","description":"HTTP method (GET, POST, etc.)"},
				"path_prefix":{"type":"string","description":"Path prefix to match (e.g., '/api/v5/account/balance')"},
				"status":{"type":"integer","description":"HTTP response status code"},
				"body":{"type":"string","description":"JSON response body as a string (will be parsed and preserved as-is)"},
				"headers":{"type":"string","description":"Optional JSON object of response headers"}
			},
			"required":["venue","method","path_prefix","status","body"]
		}`),
	}, sandboxUpsertFixtureHandler(d))

	// Tool: sandbox_reload_fixtures — venue parameter, write (gated)
	s.AddTool(&mcp.Tool{
		Name:        "sandbox_reload_fixtures",
		Description: "Reload fixture entries for a venue from disk, overwriting in-memory state. Gated on sandbox being enabled.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"venue":{"type":"string","description":"Venue name (e.g., 'okx', 'binance'). Empty string reloads all venues."}
			}
		}`),
	}, sandboxReloadFixturesHandler(d))

	// Tool: sandbox_reset_simulator — no parameters, write (gated)
	s.AddTool(&mcp.Tool{
		Name:        "sandbox_reset_simulator",
		Description: "Reset the stateful simulator (in-memory balances, positions, orders). Gated on sandbox being enabled.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, sandboxResetSimulatorHandler(d))
}

// Helper: check if sandbox is enabled
func isSandboxEnabled() bool {
	enabled, _ := sandbox.GlobalState()
	return enabled
}

// sandboxListVenuesHandler returns all venues and their fixture counts.
func sandboxListVenuesHandler(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// This is read-only, no gate needed
		srv := sandbox.GetInstance()
		if srv == nil {
			return errorResult("sandbox server not initialized"), nil
		}

		venues := srv.GetVenues()
		sort.Strings(venues)

		venueList := make([]map[string]interface{}, 0, len(venues))
		for _, venue := range venues {
			fixtures := srv.GetFixtures(venue)
			venueList = append(venueList, map[string]interface{}{
				"venue": venue,
				"count": len(fixtures),
			})
		}

		output := map[string]interface{}{
			"venues": venueList,
			"total":  len(venues),
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(output)}},
		}, nil
	}
}

// sandboxReadFixturesHandler returns all fixtures for a venue.
func sandboxReadFixturesHandler(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// This is read-only, no gate needed
		var input struct {
			Venue string `json:"venue"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return errorResult("invalid input: " + err.Error()), nil
		}
		if err := sandbox.ValidateVenueName(input.Venue); err != nil {
			return errorResult(err.Error()), nil
		}

		srv := sandbox.GetInstance()
		if srv == nil {
			return errorResult("sandbox server not initialized"), nil
		}

		fixtures := srv.GetFixtures(input.Venue)

		// Convert to output format, preserving body as raw JSON
		fixtureList := make([]map[string]interface{}, 0, len(fixtures))
		for _, f := range fixtures {
			entry := map[string]interface{}{
				"method":      f.Method,
				"path_prefix": f.PathPrefix,
				"status":      f.Response.Status,
			}
			if len(f.Response.Body) > 0 {
				// Preserve body as json.RawMessage (do not re-marshal)
				entry["body"] = f.Response.Body
			}
			if f.Response.BodyText != "" {
				entry["body_text"] = f.Response.BodyText
			}
			if len(f.Response.Headers) > 0 {
				entry["headers"] = f.Response.Headers
			}
			fixtureList = append(fixtureList, entry)
		}

		output := map[string]interface{}{
			"venue":    input.Venue,
			"count":    len(fixtures),
			"fixtures": fixtureList,
		}

		result := &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: mustJSON(output)},
			},
		}

		return result, nil
	}
}

// sandboxWriteFixturesHandler replaces all fixtures for a venue.
func sandboxWriteFixturesHandler(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Gate on sandbox enabled
		if !isSandboxEnabled() {
			return errorResult("sandbox is disabled; fixture writes are not permitted"), nil
		}

		var input struct {
			Venue   string `json:"venue"`
			Entries string `json:"entries"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return errorResult("invalid input: " + err.Error()), nil
		}
		if err := sandbox.ValidateVenueName(input.Venue); err != nil {
			return errorResult(err.Error()), nil
		}
		if input.Entries == "" {
			return errorResult("entries is required"), nil
		}

		// Parse entries JSON
		var entries []sandbox.FixtureEntry
		if err := json.Unmarshal([]byte(input.Entries), &entries); err != nil {
			return errorResult("invalid entries JSON: " + err.Error()), nil
		}

		// Validate all entries before applying or persisting
		for i, entry := range entries {
			if err := sandbox.ValidateFixtureEntry(entry); err != nil {
				return errorResult(fmt.Sprintf("entry %d (%s %s): %v", i, entry.Method, entry.PathPrefix, err)), nil
			}
		}

		srv := sandbox.GetInstance()
		if srv == nil {
			return errorResult("sandbox server not initialized"), nil
		}

		// Build warnings for shadowed fixtures
		var warnings []string
		for _, entry := range entries {
			if sandbox.SimulatorOwnedPath(input.Venue, entry.Method, entry.PathPrefix) {
				warnings = append(warnings, fmt.Sprintf("%s %s (shadowed by stateful simulator)", entry.Method, entry.PathPrefix))
			}
		}

		// Apply in memory
		srv.SetFixtures(input.Venue, entries)

		// Persist to disk
		fixturesDir := sandbox.ResolveFixturesDir(d.Cfg)
		if err := persistFixturesToDisk(fixturesDir, input.Venue, entries); err != nil {
			return errorResult("failed to persist fixtures: " + err.Error()), nil
		}

		output := map[string]interface{}{
			"venue":   input.Venue,
			"count":   len(entries),
			"message": "fixtures written and persisted",
		}
		if len(warnings) > 0 {
			output["warnings"] = warnings
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(output)}},
		}, nil
	}
}

// sandboxUpsertFixtureHandler upserts a single fixture entry.
func sandboxUpsertFixtureHandler(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Gate on sandbox enabled
		if !isSandboxEnabled() {
			return errorResult("sandbox is disabled; fixture writes are not permitted"), nil
		}

		var input struct {
			Venue      string `json:"venue"`
			Method     string `json:"method"`
			PathPrefix string `json:"path_prefix"`
			Status     int    `json:"status"`
			Body       string `json:"body"`
			Headers    string `json:"headers"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return errorResult("invalid input: " + err.Error()), nil
		}
		if err := sandbox.ValidateVenueName(input.Venue); err != nil {
			return errorResult(err.Error()), nil
		}
		if input.Method == "" || input.PathPrefix == "" {
			return errorResult("method and path_prefix are required"), nil
		}

		// Validate body as JSON
		var bodyRaw json.RawMessage
		if err := json.Unmarshal([]byte(input.Body), &bodyRaw); err != nil {
			return errorResult("invalid body JSON: " + err.Error()), nil
		}

		// Parse optional headers
		var headersMap map[string]string
		if input.Headers != "" {
			if err := json.Unmarshal([]byte(input.Headers), &headersMap); err != nil {
				return errorResult("invalid headers JSON: " + err.Error()), nil
			}
		}

		// Build the candidate entry and validate it before merging
		candidate := sandbox.FixtureEntry{
			Method:     input.Method,
			PathPrefix: input.PathPrefix,
			Response: sandbox.Response{
				Status:  input.Status,
				Body:    bodyRaw,
				Headers: headersMap,
			},
		}
		if err := sandbox.ValidateFixtureEntry(candidate); err != nil {
			return errorResult(fmt.Sprintf("entry (%s %s): %v", candidate.Method, candidate.PathPrefix, err)), nil
		}

		srv := sandbox.GetInstance()
		if srv == nil {
			return errorResult("sandbox server not initialized"), nil
		}

		// Get current fixtures, upsert, and set
		fixtures := srv.GetFixtures(input.Venue)

		// Find and replace or append
		found := false
		for i := range fixtures {
			if strings.EqualFold(fixtures[i].Method, input.Method) &&
				fixtures[i].PathPrefix == input.PathPrefix {
				fixtures[i].Response.Status = input.Status
				fixtures[i].Response.Body = bodyRaw
				fixtures[i].Response.Headers = headersMap
				found = true
				break
			}
		}

		if !found {
			fixtures = append(fixtures, candidate)
		}

		// Check if this fixture is shadowed by the simulator
		var warnings []string
		if sandbox.SimulatorOwnedPath(input.Venue, candidate.Method, candidate.PathPrefix) {
			warnings = append(warnings, fmt.Sprintf("%s %s (shadowed by stateful simulator)", candidate.Method, candidate.PathPrefix))
		}

		// Apply in memory
		srv.SetFixtures(input.Venue, fixtures)

		// Persist to disk
		fixturesDir := sandbox.ResolveFixturesDir(d.Cfg)
		if err := persistFixturesToDisk(fixturesDir, input.Venue, fixtures); err != nil {
			return errorResult("failed to persist fixtures: " + err.Error()), nil
		}

		action := "updated"
		if !found {
			action = "added"
		}

		output := map[string]interface{}{
			"venue":   input.Venue,
			"method":  input.Method,
			"path":    input.PathPrefix,
			"action":  action,
			"message": fmt.Sprintf("fixture %s and persisted", action),
		}
		if len(warnings) > 0 {
			output["warnings"] = warnings
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(output)}},
		}, nil
	}
}

// sandboxReloadFixturesHandler reloads fixtures from disk.
func sandboxReloadFixturesHandler(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Gate on sandbox enabled
		if !isSandboxEnabled() {
			return errorResult("sandbox is disabled; fixture operations are not permitted"), nil
		}

		var input struct {
			Venue string `json:"venue"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
			return errorResult("invalid input: " + err.Error()), nil
		}

		// Validate venue if specified (empty string means reload all)
		if input.Venue != "" {
			if err := sandbox.ValidateVenueName(input.Venue); err != nil {
				return errorResult(err.Error()), nil
			}
		}

		srv := sandbox.GetInstance()
		if srv == nil {
			return errorResult("sandbox server not initialized"), nil
		}

		fixturesDir := sandbox.ResolveFixturesDir(d.Cfg)

		var reloadedVenues []string

		if input.Venue == "" {
			// Reload all venues
			venues := srv.GetVenues()
			for _, v := range venues {
				entries, err := loadFixturesFromDisk(fixturesDir, v)
				if err != nil {
					return errorResult(fmt.Sprintf("failed to reload %s: %v", v, err)), nil
				}
				srv.SetFixtures(v, entries)
				reloadedVenues = append(reloadedVenues, v)
			}
		} else {
			// Reload specific venue
			entries, err := loadFixturesFromDisk(fixturesDir, input.Venue)
			if err != nil {
				return errorResult(fmt.Sprintf("failed to reload %s: %v", input.Venue, err)), nil
			}
			srv.SetFixtures(input.Venue, entries)
			reloadedVenues = []string{input.Venue}
		}

		output := map[string]interface{}{
			"venues": reloadedVenues,
			"count":  len(reloadedVenues),
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(output)}},
		}, nil
	}
}

// sandboxResetSimulatorHandler resets the stateful simulator.
func sandboxResetSimulatorHandler(d Deps) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Gate on sandbox enabled
		if !isSandboxEnabled() {
			return errorResult("sandbox is disabled; simulator operations are not permitted"), nil
		}

		srv := sandbox.GetInstance()
		if srv == nil {
			return errorResult("sandbox server not initialized"), nil
		}

		// Try to get responder and reset it
		responder := srv.GetResponder()
		if responder == nil {
			return errorResult("no stateful simulator configured"), nil
		}

		// Check if the responder has a Reset method (via duck typing)
		// The StatefulSimulator type is in pkg/sandbox/simulator.go
		// For now, we'll try to call Reset if it exists
		type Resetter interface {
			Reset()
		}

		if resetter, ok := responder.(Resetter); ok {
			resetter.Reset()
		} else {
			// Try to find the Reset method on the responder
			// This is a best-effort approach for now
			return errorResult("simulator does not support reset (likely not yet implemented)"), nil
		}

		output := map[string]interface{}{
			"message": "simulator state reset",
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: mustJSON(output)}},
		}, nil
	}
}

// persistFixturesToDisk writes fixture entries to disk in JSON format.
// This preserves the body bytes exactly as they were parsed.
func persistFixturesToDisk(fixturesDir string, venue string, entries []sandbox.FixtureEntry) error {
	// Validate venue name early to prevent path traversal
	if err := sandbox.ValidateVenueName(venue); err != nil {
		return err
	}

	if fixturesDir == "" {
		return fmt.Errorf("fixtures_dir not configured")
	}

	venueDir := filepath.Join(fixturesDir, venue)
	if err := os.MkdirAll(venueDir, 0755); err != nil {
		return fmt.Errorf("create venue directory: %w", err)
	}

	// Write to fixtures.json
	filePath := filepath.Join(venueDir, "fixtures.json")
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal entries: %w", err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	logger.Debugf("persisted %d fixtures for %s to %s", len(entries), venue, filePath)
	return nil
}

// loadFixturesFromDisk reads fixture entries from disk.
func loadFixturesFromDisk(fixturesDir string, venue string) ([]sandbox.FixtureEntry, error) {
	venueDir := filepath.Join(fixturesDir, venue)
	venueEntries, err := os.ReadDir(venueDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no fixtures for this venue
		}
		return nil, fmt.Errorf("read venue directory: %w", err)
	}

	var allEntries []sandbox.FixtureEntry
	for _, entry := range venueEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(venueDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read fixture file %s: %w", filePath, err)
		}

		var entries []sandbox.FixtureEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("unmarshal fixture file %s: %w", filePath, err)
		}

		allEntries = append(allEntries, entries...)
	}

	return allEntries, nil
}
