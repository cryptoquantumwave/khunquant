package gateway

import (
	"database/sql"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// proactiveDBResource is the name the shared database is registered under.
const proactiveDBResource = "proactive-db"

// proactiveDB is the ONE place the gateway opens the shared proactive database
// (<default agent workspace>/memory/proactive/proactive.db). Every service that
// needs it takes the handle from here.
//
// sqlitex.Shared returns the same handle for the same workspace, so calling this
// on every build and reload is free and never reopens anything. The first call
// registers sqlitex.CloseShared with d.Shared, exactly once per sharedResources,
// so the handle is closed at gateway shutdown and never on a reload. The
// handle itself must not be closed by the caller.
//
// Tools that need the database but are built inside the agent loop (the profile
// tools, which the CLI agent has as well) open it through the same
// sqlitex.Shared call; this helper is what puts the close on the gateway's
// shutdown path.
func proactiveDB(d assemblyDeps) (*sql.DB, error) {
	db, err := sqlitex.Shared(agent.DefaultAgentWorkspace(d.Cfg))
	if err != nil {
		return nil, err
	}
	if d.Shared != nil {
		d.Shared.addOnce(proactiveDBResource, sharedCloserFunc(sqlitex.CloseShared))
	}
	return db, nil
}

// sharedCloserFunc adapts a function to io.Closer.
type sharedCloserFunc func() error

func (f sharedCloserFunc) Close() error { return f() }

// addOnce registers c under name unless a resource of that name is already
// registered. It is add with a guard, for resources every build asks for.
func (r *sharedResources) addOnce(name string, c interface{ Close() error }) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, nc := range r.closers {
		if nc.name == name {
			return
		}
	}
	r.closers = append(r.closers, namedCloser{name: name, c: c})
}
