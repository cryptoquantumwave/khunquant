package tools

// ExecResponse is the JSON payload the session actions return to the model.
//
// A structured shape rather than free text: the model has to decide whether a
// background job is still running, what its exit code was, and which session to
// poll next, and parsing that out of prose is exactly where it goes wrong.
type ExecResponse struct {
	SessionID string        `json:"sessionId,omitempty"`
	Status    string        `json:"status,omitempty"`
	ExitCode  int           `json:"exitCode,omitempty"`
	Output    string        `json:"output,omitempty"`
	Error     string        `json:"error,omitempty"`
	Sessions  []SessionInfo `json:"sessions,omitempty"`
}
