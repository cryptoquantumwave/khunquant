package broker

// NewTestResolver returns an empty resolver of the same type as
// DefaultResolver, for tests that must not touch the package-level one.
//
// A catch-all registration is the reason this exists: it has no removal path,
// and once DefaultResolver holds one, every later Resolve succeeds - including
// the ones asserting ErrSymbolNotFound. Registering it on the global made
// `go test -count=2` fail, since the second pass inherits the first pass's
// catch-all.
//
// Defined in an internal test file so the external broker_test package can use
// it without widening the production API.
func NewTestResolver() SymbolResolver {
	return &defaultResolver{exact: make(map[string][]symbolEntry)}
}
