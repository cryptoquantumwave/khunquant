package sandbox

import (
	"net/http"
	"sync"
)

// Stage names a slot of the Chain, in the order a request visits them. Later PRs fill a
// slot with Set; none adds or reorders one.
type Stage int

const (
	StageFault    Stage = iota // injected failures (SBX-13)
	StageMarket                // simulated prices (SimMarket)
	StageOrders                // the stateful simulator
	StageWeb                   // reserved (SBX-15)
	StageFixtures              // the recorded fixtures: always last
	numStages
)

var stageNames = [numStages]string{"fault", "market", "orders", "web", "fixtures"}

// StageNone is the label of a request no stage answered (the router's 404 did).
const StageNone = "none"

func (s Stage) String() string {
	if s < 0 || s >= numStages {
		return StageNone
	}
	return stageNames[s]
}

// Middleware answers a request itself or hands it to next, and may post-process what
// next returned.
type Middleware func(next Responder) Responder

// ReqInfo describes one request the chain saw, for the journal's exchange.req entries.
type ReqInfo struct {
	Venue, Host, Method, Path, Stage string
	Status                           int
}

// Chain is an ordered middleware chain behind the Server's single responder slot.
type Chain struct {
	mu      sync.RWMutex
	stages  [numStages]Middleware
	observe func(ReqInfo)
}

// NewChain returns a chain whose last stage serves store's fixtures.
func NewChain(store *Store) *Chain {
	c := &Chain{}
	c.stages[StageFixtures] = AsMiddleware(StoreResponder(store))
	return c
}

// Set installs mw in a stage; nil clears it. Safe to call while requests are served.
func (c *Chain) Set(s Stage, mw Middleware) {
	if s < 0 || s >= numStages {
		return
	}
	c.mu.Lock()
	c.stages[s] = mw
	c.mu.Unlock()
}

// Observe sets the function called once after every request. It must not block: it runs
// on the request's goroutine.
func (c *Chain) Observe(f func(ReqInfo)) {
	c.mu.Lock()
	c.observe = f
	c.mu.Unlock()
}

// Respond runs the request through the stages. The stage reported is the innermost one
// that produced the response, so a stage that post-processes next's answer does not
// take credit for it; StageNone means nobody did.
func (c *Chain) Respond(venue, method, path string, r *http.Request) (*Response, bool) {
	c.mu.RLock()
	stages, observe := c.stages, c.observe
	c.mu.RUnlock() // never call a stage with the lock held: a stage may Set or call next

	answered := Stage(-1)
	var next Responder = ResponderFunc(func(string, string, string, *http.Request) (*Response, bool) { return nil, false })
	for i := numStages - 1; i >= 0; i-- {
		if stages[i] == nil {
			continue
		}
		inner, st := stages[i](next), i
		next = ResponderFunc(func(v, m, p string, req *http.Request) (*Response, bool) {
			resp, ok := inner.Respond(v, m, p, req)
			if ok && answered < 0 {
				answered = st
			}
			return resp, ok
		})
	}
	resp, ok := next.Respond(venue, method, path, r)
	if observe != nil {
		info := ReqInfo{Venue: venue, Method: method, Path: path, Stage: StageNone, Status: http.StatusNotFound}
		if r != nil && r.URL != nil {
			if p, err := ParseRequest(r.URL.EscapedPath()); err == nil {
				info.Host = p.Host
			}
		}
		if ok {
			info.Stage, info.Status = answered.String(), resp.Status
		}
		observe(info)
	}
	return resp, ok
}

// AsMiddleware lets a plain Responder be a stage: it answers first, next only when it
// declines.
func AsMiddleware(r Responder) Middleware {
	return func(next Responder) Responder {
		return ResponderFunc(func(venue, method, path string, req *http.Request) (*Response, bool) {
			if resp, ok := r.Respond(venue, method, path, req); ok {
				return resp, true
			}
			return next.Respond(venue, method, path, req)
		})
	}
}

// StoreResponder serves the store's fixtures as a Responder (what the router does after
// its responders decline).
func StoreResponder(store *Store) Responder {
	return ResponderFunc(func(venue, method, path string, r *http.Request) (*Response, bool) {
		if r == nil || r.URL == nil {
			return nil, false
		}
		if f := store.FindFixture(venue, method, path, r.URL.Query()); f != nil {
			return &f.Response, true
		}
		return nil, false
	})
}
