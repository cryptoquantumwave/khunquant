package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Server is a loopback HTTP server that serves mocked exchange responses.
// It listens on 127.0.0.1:<auto-assigned port>.
type Server struct {
	mu        sync.RWMutex
	running   bool
	listener  net.Listener
	server    *http.Server
	store     *Store
	respMu    sync.RWMutex
	responder Responder

	// Global state accessor (singleton pattern for accessing current sandbox state).
	// Guarded by instanceMu.
}

var (
	instanceMu sync.RWMutex
	instance   *Server

	// globalStateMu guards the live sandbox state (enabled flag and base URL).
	// This is separate from the server instance because the state can change
	// at runtime (e.g., via the web UI), and all in-flight requests must reflect
	// the current state, not the state when they were constructed.
	globalStateMu sync.RWMutex
	globalEnabled bool
	globalBaseURL string
)

// SetInstance sets the global sandbox server instance for later retrieval via GetInstance.
func SetInstance(srv *Server) {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	instance = srv
}

// GetInstance returns the global sandbox server instance, or nil if not set.
func GetInstance() *Server {
	instanceMu.RLock()
	defer instanceMu.RUnlock()
	return instance
}

// SetGlobalState updates the global sandbox state (enabled flag and base URL).
// Called by Server.Start() and Server.Stop() to keep the live state in sync.
func SetGlobalState(enabled bool, baseURL string) {
	globalStateMu.Lock()
	defer globalStateMu.Unlock()
	globalEnabled = enabled
	globalBaseURL = baseURL
}

// GlobalState returns the current global sandbox state (enabled, baseURL).
// Used by RoundTripper to determine whether and where to rewrite each request.
func GlobalState() (enabled bool, baseURL string) {
	globalStateMu.RLock()
	defer globalStateMu.RUnlock()
	return globalEnabled, globalBaseURL
}

// liveResponder is an adapter that reads the current responder dynamically at request time,
// allowing SetResponder to work before or after Start().
type liveResponder struct {
	s *Server
}

func (lr liveResponder) Respond(venue, method, path string, r *http.Request) (*Response, bool) {
	lr.s.respMu.RLock()
	resp := lr.s.responder
	lr.s.respMu.RUnlock()
	if resp == nil {
		return nil, false
	}
	return resp.Respond(venue, method, path, r)
}

// NewServer creates a new sandbox server with the given fixture store.
func NewServer(store *Store) *Server {
	return &Server{
		store: store,
	}
}

// SetResponder registers a custom responder that will be consulted before the
// fixture store. Can be called before or after Start(). Thread-safe.
func (s *Server) SetResponder(r Responder) {
	s.respMu.Lock()
	defer s.respMu.Unlock()
	s.responder = r
}

// Start begins listening for requests. It is safe to call multiple times; if
// already running, Start returns the address without error.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	// Listen on any available port on loopback.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}

	s.listener = listener
	addr := listener.Addr().String()

	// Build the router with the live responder adapter.
	// This allows SetResponder to be called before or after Start().
	handler := BuildRouter(s.store, liveResponder{s})

	// Create and start the HTTP server.
	s.server = &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Start server in a goroutine so Start() returns immediately.
	go func() {
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Errorf("sandbox server error: %v", err)
		}
	}()

	s.running = true
	baseURL := fmt.Sprintf("http://%s", addr)
	SetGlobalState(true, baseURL)
	logger.Debugf("sandbox server started on %s", addr)

	return nil
}

// Stop gracefully shuts down the server. It is safe to call multiple times;
// if not running, Stop returns nil without error.
func (s *Server) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	if s.server != nil {
		// Graceful shutdown with a reasonable timeout.
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := s.server.Shutdown(ctx); err != nil {
			logger.Errorf("sandbox server shutdown error: %v", err)
		}
	}

	if s.listener != nil {
		s.listener.Close()
	}

	s.running = false
	SetGlobalState(false, "")
	logger.Debugf("sandbox server stopped")

	return nil
}

// Addr returns the server's listening address (e.g., "127.0.0.1:12345"), or an
// empty string if the server is not running.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.running || s.listener == nil {
		return ""
	}

	return s.listener.Addr().String()
}

// BaseURL returns the base URL for sandbox requests (e.g., "http://127.0.0.1:12345"),
// or an empty string if the server is not running.
func (s *Server) BaseURL() string {
	addr := s.Addr()
	if addr == "" {
		return ""
	}

	return fmt.Sprintf("http://%s", addr)
}

// IsRunning returns whether the server is currently running.
func (s *Server) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// GetVenues returns a list of all venues with fixtures.
func (s *Server) GetVenues() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store.Venues()
}

// GetFixtures returns a copy of all fixtures for a given venue.
func (s *Server) GetFixtures(venue string) []FixtureEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store.GetFixtures(venue)
}

// SetFixtures replaces all fixtures for a given venue.
func (s *Server) SetFixtures(venue string, entries []FixtureEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.SetFixtures(venue, entries)
}

// GetResponder returns the currently set custom responder, or nil.
func (s *Server) GetResponder() Responder {
	s.respMu.RLock()
	defer s.respMu.RUnlock()
	return s.responder
}

const shutdownTimeout = 5 * time.Second
