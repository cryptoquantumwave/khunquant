package agent

import (
	"context"
	"testing"
	"time"
)

// fakeContributor is a test double for FinancialContributor.
type fakeContributor struct {
	name     string
	outputs  []string // each call pops one; last is repeated
	callN    int
	closeErr error
}

func (f *fakeContributor) Name() string { return f.name }

func (f *fakeContributor) Section(ctx context.Context) (string, error) {
	if len(f.outputs) == 0 {
		return "", nil
	}
	idx := f.callN
	if idx >= len(f.outputs) {
		idx = len(f.outputs) - 1
	}
	f.callN++
	return f.outputs[idx], nil
}

func (f *fakeContributor) Close() error { return f.closeErr }

// TestFinancialContextCollectorEmpty tests that an empty collector returns empty string.
func TestFinancialContextCollectorEmpty(t *testing.T) {
	collector := NewFinancialContextCollector([]FinancialContributor{}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	if summary != "" {
		t.Errorf("expected empty string, got %q", summary)
	}
}

// TestFinancialContextCollectorSingleContributor tests a single working contributor.
func TestFinancialContextCollectorSingleContributor(t *testing.T) {
	contrib := &fakeContributor{
		name:    "test",
		outputs: []string{"Test Section Content"},
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	expected := "## Financial Context\nTest Section Content"
	if summary != expected {
		t.Errorf("expected %q, got %q", expected, summary)
	}
}

// TestFinancialContextCollectorMultipleContributors tests multiple contributors.
func TestFinancialContextCollectorMultipleContributors(t *testing.T) {
	contrib1 := &fakeContributor{
		name:    "first",
		outputs: []string{"First Section"},
	}
	contrib2 := &fakeContributor{
		name:    "second",
		outputs: []string{"Second Section"},
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib1, contrib2}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	expected := "## Financial Context\nFirst Section\nSecond Section"
	if summary != expected {
		t.Errorf("expected %q, got %q", expected, summary)
	}
}

// TestFinancialContextCollectorEmptyContributor tests that empty contributor output is omitted.
func TestFinancialContextCollectorEmptyContributor(t *testing.T) {
	contrib1 := &fakeContributor{
		name:    "first",
		outputs: []string{"First Section"},
	}
	contrib2 := &fakeContributor{
		name:    "empty",
		outputs: []string{""}, // empty output
	}
	contrib3 := &fakeContributor{
		name:    "third",
		outputs: []string{"Third Section"},
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib1, contrib2, contrib3}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	// Empty contributor should be skipped
	expected := "## Financial Context\nFirst Section\nThird Section"
	if summary != expected {
		t.Errorf("expected %q, got %q", expected, summary)
	}
}

// TestFinancialContextCollectorErrorContributor tests that erroring contributor is silently skipped.
func TestFinancialContextCollectorErrorContributor(t *testing.T) {
	contrib1 := &fakeContributor{
		name:    "good",
		outputs: []string{"Good Section"},
	}
	// fakeContributor with no outputs doesn't error, so create a special one for testing errors
	contrib2 := &errorContributor{name: "bad"}
	contrib3 := &fakeContributor{
		name:    "good2",
		outputs: []string{"Good Section 2"},
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib1, contrib2, contrib3}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	// Erroring contributor should be skipped
	expected := "## Financial Context\nGood Section\nGood Section 2"
	if summary != expected {
		t.Errorf("expected %q, got %q", expected, summary)
	}
}

// TestFinancialContextCollectorCacheTTL tests TTL-based caching.
func TestFinancialContextCollectorCacheTTL(t *testing.T) {
	contrib := &fakeContributor{
		name:    "stateful",
		outputs: []string{"First", "Second", "Third"},
	}
	ttl := 100 * time.Millisecond
	collector := NewFinancialContextCollector([]FinancialContributor{contrib}, ttl)
	defer collector.Close()

	// First call should return "First"
	summary1 := collector.GetSummary()
	expected1 := "## Financial Context\nFirst"
	if summary1 != expected1 {
		t.Errorf("call 1: expected %q, got %q", expected1, summary1)
	}

	// Second call within TTL should return the same cached value "First"
	summary2 := collector.GetSummary()
	if summary2 != summary1 {
		t.Errorf("call 2 (within TTL): expected %q, got %q (should be cached)", summary1, summary2)
	}

	// Check that contributor.callN is still 1 (only called once)
	if contrib.callN != 1 {
		t.Errorf("contributor should only be called once within TTL, but callN=%d", contrib.callN)
	}

	// Wait for TTL to expire
	time.Sleep(ttl + 10*time.Millisecond)

	// Third call after TTL expiry should rebuild and return "Second"
	summary3 := collector.GetSummary()
	expected3 := "## Financial Context\nSecond"
	if summary3 != expected3 {
		t.Errorf("call 3 (after TTL): expected %q, got %q", expected3, summary3)
	}

	// Check that contributor.callN is now 2
	if contrib.callN != 2 {
		t.Errorf("contributor should be called twice after TTL expiry, but callN=%d", contrib.callN)
	}
}

// TestFinancialContextCollectorNoCacheTTL tests that ttl=0 disables caching.
func TestFinancialContextCollectorNoCacheTTL(t *testing.T) {
	contrib := &fakeContributor{
		name:    "stateful",
		outputs: []string{"First", "Second", "Third"},
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib}, 0)
	defer collector.Close()

	// First call should return "First"
	summary1 := collector.GetSummary()
	expected1 := "## Financial Context\nFirst"
	if summary1 != expected1 {
		t.Errorf("call 1: expected %q, got %q", expected1, summary1)
	}

	// Second call with ttl=0 should rebuild and return "Second" (not cached)
	summary2 := collector.GetSummary()
	expected2 := "## Financial Context\nSecond"
	if summary2 != expected2 {
		t.Errorf("call 2 (no cache): expected %q, got %q", expected2, summary2)
	}

	// Third call should return "Third"
	summary3 := collector.GetSummary()
	expected3 := "## Financial Context\nThird"
	if summary3 != expected3 {
		t.Errorf("call 3 (no cache): expected %q, got %q", expected3, summary3)
	}

	// Check that contributor was called 3 times
	if contrib.callN != 3 {
		t.Errorf("contributor should be called 3 times with ttl=0, but callN=%d", contrib.callN)
	}
}

// TestFinancialContextCollectorAllEmpty tests that all-empty contributors returns empty string.
func TestFinancialContextCollectorAllEmpty(t *testing.T) {
	contrib1 := &fakeContributor{
		name:    "empty1",
		outputs: []string{""}, // empty
	}
	contrib2 := &fakeContributor{
		name:    "empty2",
		outputs: []string{""}, // empty
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib1, contrib2}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	if summary != "" {
		t.Errorf("expected empty string when all contributors are empty, got %q", summary)
	}
}

// errorContributor is a test double that always returns an error.
type errorContributor struct {
	name string
}

func (e *errorContributor) Name() string { return e.name }

func (e *errorContributor) Section(ctx context.Context) (string, error) {
	return "", &testError{msg: "simulated error"}
}

func (e *errorContributor) Close() error { return nil }

// testError is a simple error type for testing.
type testError struct {
	msg string
}

func (te *testError) Error() string { return te.msg }

// TestFinancialContextCollectorMixedContent tests mix of valid, empty, and error contributors.
func TestFinancialContextCollectorMixedContent(t *testing.T) {
	contrib1 := &fakeContributor{
		name:    "valid1",
		outputs: []string{"Valid Section 1"},
	}
	contrib2 := &errorContributor{
		name: "error",
	}
	contrib3 := &fakeContributor{
		name:    "empty",
		outputs: []string{""}, // empty
	}
	contrib4 := &fakeContributor{
		name:    "valid2",
		outputs: []string{"Valid Section 2"},
	}
	collector := NewFinancialContextCollector(
		[]FinancialContributor{contrib1, contrib2, contrib3, contrib4},
		0,
	)
	defer collector.Close()

	summary := collector.GetSummary()
	// Only valid sections should be included
	expected := "## Financial Context\nValid Section 1\nValid Section 2"
	if summary != expected {
		t.Errorf("expected %q, got %q", expected, summary)
	}
}

// TestFinancialContextCollectorContextTimeout tests that context timeout is handled.
func TestFinancialContextCollectorContextTimeout(t *testing.T) {
	contrib := &slowContributor{
		name:     "slow",
		duration: 5 * time.Second, // longer than the 2-second timeout
	}
	collector := NewFinancialContextCollector([]FinancialContributor{contrib}, 0)
	defer collector.Close()

	summary := collector.GetSummary()
	// Should return empty because the contributor times out
	if summary != "" {
		t.Errorf("expected empty string due to context timeout, got %q", summary)
	}
}

// slowContributor is a test double that sleeps before returning.
type slowContributor struct {
	name     string
	duration time.Duration
}

func (s *slowContributor) Name() string { return s.name }

func (s *slowContributor) Section(ctx context.Context) (string, error) {
	select {
	case <-time.After(s.duration):
		return "Slow Result", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *slowContributor) Close() error { return nil }
