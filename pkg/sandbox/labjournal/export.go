package labjournal

// Lookup reads a dotted path ("event.payload.Candidate.TriggerID") out of a decoded JSON
// document: the rule Match.Fields uses.
func Lookup(doc any, path string) (any, bool) { return lookup(doc, path) }
