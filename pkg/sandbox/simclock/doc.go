// Package simclock is the one clock of a Sandbox Lab instance. In production the
// gateway holds Real(); a locked instance holds a *Sim, which moves only when told
// (Advance), or follows the wall clock scaled (scaled, realtime).
//
// Sim time versus wall time: everything that decides WHEN domain work happens (poll
// intervals, workflow waits, proposal expiry, schedules, cooldowns) reads the Clock
// and so follows sim time. Everything that bounds how long the machine may take
// (HTTP and LLM timeouts, context deadlines, SQLite busy timeouts, the settle poll)
// stays on the wall clock, or a sim jump of two hours would time every one of them out.
//
// The package imports only the standard library, so any package may import it.
package simclock
