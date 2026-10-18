package proactive

// MemoryTriggerPrefix starts the TriggerID of the digest candidate that announces a
// memory suggestion ("memory:<proposal code>"). The digest renderer recognises such an
// item by it, shows the suggested fact (the candidate's Quote) in full to the owner, and
// keeps it out of the owner's session.
const MemoryTriggerPrefix = "memory:"
