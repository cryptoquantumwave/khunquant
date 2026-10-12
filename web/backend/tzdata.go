package main

// Embed the IANA time zone database in the binary (about 450 KB).
//
// The launcher validates and saves the owner's profile, whose time zone
// ("Asia/Bangkok") is checked with time.LoadLocation, and cron schedules carry
// named zones. LoadLocation reads them from the host, and a minimal container
// or a small device has no zoneinfo database: without this import a perfectly
// valid zone is refused when the profile is saved. With it, LoadLocation falls
// back to the embedded copy when the host has none (the host's own database
// still wins when it has one).
//
// TestTZDataIsEmbedded fails if this import is removed.
import _ "time/tzdata"
