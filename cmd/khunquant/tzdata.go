package main

// Embed the IANA time zone database in the binary (about 450 KB).
//
// The owner's profile names a time zone ("Asia/Bangkok") that quiet hours, the
// daily budget and the digest are judged in, and cron jobs and the loss guard
// load named zones too. time.LoadLocation reads them from the host, and a
// minimal container or a small device has no zoneinfo database: without this
// import a zone that is perfectly valid fails to load and quiet hours silently
// fall back to UTC. With it, LoadLocation falls back to the embedded copy when
// the host has none (the host's own database still wins when it has one).
//
// TestTZDataIsEmbedded fails if this import is removed.
import _ "time/tzdata"
