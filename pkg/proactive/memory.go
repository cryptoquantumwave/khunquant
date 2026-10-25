package proactive

// MemoryTriggerPrefix starts the TriggerID of the digest candidate that announces a
// memory suggestion ("memory:<proposal code>"). The digest renderer recognises such an
// item by it, shows the suggested fact (the candidate's Quote) in full to the owner, and
// keeps it out of the owner's session.
const MemoryTriggerPrefix = "memory:"

// ReflectionTriggerPrefix starts the TriggerID of the digest candidate that announces a
// change the nightly reflection suggests ("reflection:<proposal code>"). The digest
// renders such an item apart, with a "Why" line, and keeps it out of the owner's session.
const ReflectionTriggerPrefix = "reflection:"
