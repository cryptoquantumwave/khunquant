package tools

// Background and PTY exec sessions.
//
// Kept out of shell.go on purpose: these actions operate on already-running
// processes and accept caller-supplied input, which is a different and larger
// attack surface than the one-shot command path. Keeping them separate makes
// that boundary visible.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/creack/pty"
)

func (t *ExecTool) runBackground(ctx context.Context, command, cwd string, ptyEnabled bool) *ToolResult {
	sessionID := generateSessionID()
	session := &ProcessSession{
		ID:         sessionID,
		Command:    command,
		PTY:        ptyEnabled,
		Background: true,
		StartTime:  time.Now().Unix(),
		Status:     "running",
		ptyKeyMode: PtyKeyModeCSI,
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdoutReader io.ReadCloser
	var stderrReader io.ReadCloser
	var stdinWriter io.WriteCloser

	if ptyEnabled {
		ptmx, tty, err := pty.Open()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create PTY: %v", err))
		}

		cmd.Stdin = tty
		cmd.Stdout = tty
		cmd.Stderr = tty

		// For PTY, we need Setsid to create a new session.
		// Note: Setsid and Setpgid conflict, so we must replace SysProcAttr entirely.
		setSysProcAttrForPty(cmd)

		session.ptyMaster = ptmx
	} else {
		var err error
		stdoutReader, err = cmd.StdoutPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create stdout pipe: %v", err))
		}
		stderrReader, err = cmd.StderrPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create stderr pipe: %v", err))
		}
		stdinWriter, err = cmd.StdinPipe()
		if err != nil {
			return ErrorResult(fmt.Sprintf("failed to create stdin pipe: %v", err))
		}
		session.stdoutPipe = io.MultiReader(stdoutReader, stderrReader)
		session.stdinWriter = stdinWriter
	}

	if err := cmd.Start(); err != nil {
		if session.ptyMaster != nil {
			session.ptyMaster.Close()
		}
		return ErrorResult(fmt.Sprintf("failed to start command: %v", err))
	}

	session.PID = cmd.Process.Pid
	t.sessionManager.Add(session)

	session.outputBuffer = &bytes.Buffer{}

	// PTY mode: read from ptyMaster and wait for process
	// Note: On Linux, closing ptyMaster doesn't interrupt blocking Read() calls,
	// so we need cmd.Wait() in a separate goroutine to detect process exit.
	if session.PTY && session.ptyMaster != nil {
		go func() {
			cmd.Wait() // Wait for process to exit
			session.mu.Lock()
			if cmd.ProcessState != nil {
				session.ExitCode = cmd.ProcessState.ExitCode()
			}
			session.Status = "done"
			session.mu.Unlock()
		}()

		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := session.ptyMaster.Read(buf)
				if n > 0 {
					raw := string(buf[:n])
					if mode := detectPtyKeyMode(raw); mode != PtyKeyModeNotFound && mode != session.GetPtyKeyMode() {
						session.SetPtyKeyMode(mode)
					}

					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}
		}()
	} else {
		// Non-PTY mode: single goroutine reads pipes.
		// When Read() returns EOF (pipe closed), we break.
		// When process exits, OS closes pipe write end → Read() returns EOF → we exit.
		go func() {
			buf := make([]byte, 4096)

			// Read stdout
			for {
				n, err := stdoutReader.Read(buf)
				if n > 0 {
					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}

			// Read stderr
			for {
				n, err := stderrReader.Read(buf)
				if n > 0 {
					session.mu.Lock()
					if session.outputBuffer.Len() >= maxOutputBufferSize {
						if !session.outputTruncated {
							session.outputBuffer.WriteString(outputTruncateMarker)
							session.outputTruncated = true
						}
					} else {
						session.outputBuffer.Write(buf[:n])
					}
					session.mu.Unlock()
				}
				if err != nil {
					break
				}
			}

			// All pipes closed, get exit status
			if stdinWriter != nil {
				stdinWriter.Close()
			}
			cmd.Wait()

			session.mu.Lock()
			if cmd.ProcessState != nil {
				session.ExitCode = cmd.ProcessState.ExitCode()
			}
			session.Status = "done"
			session.mu.Unlock()
		}()
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "running",
	}
	data, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("Session %s started", sessionID),
		IsError: false,
	}
}

func (t *ExecTool) executeList() *ToolResult {
	sessions := t.sessionManager.List()
	resp := ExecResponse{
		Sessions: sessions,
	}
	data, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("%d active sessions", len(sessions)),
		IsError: false,
	}
}

func (t *ExecTool) executePoll(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    session.GetStatus(),
		ExitCode:  session.GetExitCode(),
	}
	data, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(data),
		IsError: false,
	}
}

func (t *ExecTool) executeRead(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	output := session.Read()

	resp := ExecResponse{
		SessionID: sessionID,
		Output:    output,
		Status:    session.GetStatus(),
	}
	data, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(data),
		IsError: false,
	}
}

func (t *ExecTool) executeWrite(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	data, ok := args["data"].(string)
	if !ok {
		return ErrorResult("data is required")
	}

	// DIVERGENCE FROM UPSTREAM — the reason this feature is safe to carry.
	//
	// Upstream guards only the command that starts a session. Anything written
	// afterwards goes straight to the process's stdin, so starting a session
	// with a permitted interpreter (sh, bash, python) and then writing into it
	// executes arbitrary input with none of the deny patterns applied. Every
	// protection in this file's guardCommand would be reachable only for the
	// first line typed.
	//
	// Written input is executed by the shell already running in the session, so
	// it is a command by any useful definition and is guarded as one.
	if guardError := t.guardSessionInput(data); guardError != "" {
		return ErrorResult(guardError)
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
	}

	if err := session.Write(data); err != nil {
		if errors.Is(err, ErrSessionDone) {
			return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
		}
		return ErrorResult(fmt.Sprintf("failed to write to session: %v", err))
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    session.GetStatus(),
	}
	respData, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(respData),
		IsError: false,
	}
}

func (t *ExecTool) executeKill(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
	}

	if err := session.Kill(); err != nil {
		return ErrorResult(fmt.Sprintf("failed to kill session: %v", err))
	}

	t.sessionManager.Remove(sessionID)

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "done",
	}
	data, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(data),
		ForUser: fmt.Sprintf("Session %s killed", sessionID),
		IsError: false,
	}
}

var keyMap = map[string]string{
	"enter":     "\r",
	"return":    "\r",
	"tab":       "\t",
	"escape":    "\x1b",
	"esc":       "\x1b",
	"space":     " ",
	"backspace": "\x7f",
	"bspace":    "\x7f",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"home":      "\x1b[1~",
	"end":       "\x1b[4~",
	"pageup":    "\x1b[5~",
	"pagedown":  "\x1b[6~",
	"pgup":      "\x1b[5~",
	"pgdn":      "\x1b[6~",
	"insert":    "\x1b[2~",
	"ic":        "\x1b[2~",
	"delete":    "\x1b[3~",
	"del":       "\x1b[3~",
	"dc":        "\x1b[3~",
	"btab":      "\x1b[Z",
	"f1":        "\x1bOP",
	"f2":        "\x1bOQ",
	"f3":        "\x1bOR",
	"f4":        "\x1bOS",
	"f5":        "\x1b[15~",
	"f6":        "\x1b[17~",
	"f7":        "\x1b[18~",
	"f8":        "\x1b[19~",
	"f9":        "\x1b[20~",
	"f10":       "\x1b[21~",
	"f11":       "\x1b[23~",
	"f12":       "\x1b[24~",
}

var ss3KeysMap = map[string]string{
	"up":    "\x1bOA",
	"down":  "\x1bOB",
	"right": "\x1bOC",
	"left":  "\x1bOD",
	"home":  "\x1bOH",
	"end":   "\x1bOF",
}

func detectPtyKeyMode(raw string) PtyKeyMode {
	const SMKX = "\x1b[?1h"
	const RMKX = "\x1b[?1l"

	lastSmkx := strings.LastIndex(raw, SMKX)
	lastRmkx := strings.LastIndex(raw, RMKX)

	if lastSmkx == -1 && lastRmkx == -1 {
		return PtyKeyModeNotFound
	}

	if lastSmkx > lastRmkx {
		return PtyKeyModeSS3
	}
	return PtyKeyModeCSI
}

func encodeKeyToken(token string, ptyKeyMode PtyKeyMode) (string, error) {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return "", nil
	}

	// Handle ctrl-X format (c-x)
	if strings.HasPrefix(token, "c-") {
		char := token[2]
		if char >= 'a' && char <= 'z' {
			return string(rune(char) & 0x1f), nil // ctrl-a through ctrl-z
		}
		return "", fmt.Errorf("invalid ctrl key: %s", token)
	}

	// Handle ctrl-X format (ctrl-x)
	if strings.HasPrefix(token, "ctrl-") {
		char := token[5]
		if char >= 'a' && char <= 'z' {
			return string(rune(char) & 0x1f), nil
		}
		return "", fmt.Errorf("invalid ctrl key: %s", token)
	}

	// Handle alt-X format (m-x or alt-x)
	if strings.HasPrefix(token, "m-") || strings.HasPrefix(token, "alt-") {
		var char string
		if strings.HasPrefix(token, "m-") {
			char = token[2:]
		} else {
			char = token[4:]
		}
		if len(char) == 1 {
			return "\x1b" + char, nil
		}
		return "", fmt.Errorf("invalid alt key: %s", token)
	}

	// Handle shift modifier for special keys (shift-up, shift-down, etc.)
	if strings.HasPrefix(token, "s-") || strings.HasPrefix(token, "shift-") {
		var key string
		if strings.HasPrefix(token, "s-") {
			key = token[2:]
		} else {
			key = token[6:]
		}
		// Apply shift modifier: for single-char keys, return uppercase
		if seq, ok := keyMap[key]; ok {
			// For escape sequences, we can't easily add shift
			// For single-char keys (letters), return uppercase
			if len(seq) == 1 {
				return strings.ToUpper(seq), nil
			}
			return seq, nil
		}
		return "", fmt.Errorf("unknown key with shift: %s", key)
	}

	if ptyKeyMode == PtyKeyModeSS3 {
		if seq, ok := ss3KeysMap[token]; ok {
			return seq, nil
		}
	}

	if seq, ok := keyMap[token]; ok {
		return seq, nil
	}

	return "", fmt.Errorf("unknown key: %s (use write action for text input)", token)
}

func encodeKeySequence(tokens []string, ptyKeyMode PtyKeyMode) (string, error) {
	var result string
	for _, token := range tokens {
		seq, err := encodeKeyToken(token, ptyKeyMode)
		if err != nil {
			return "", err
		}
		result += seq
	}
	return result, nil
}

func (t *ExecTool) executeSendKeys(args map[string]any) *ToolResult {
	sessionID, ok := args["sessionId"].(string)
	if !ok {
		return ErrorResult("sessionId is required")
	}

	keysStr, ok := args["keys"].(string)
	if !ok {
		return ErrorResult("keys must be a string")
	}

	if keysStr == "" {
		return ErrorResult("keys cannot be empty")
	}

	// Parse comma-separated key names
	keyNames := strings.Split(keysStr, ",")
	var keys []string
	for _, k := range keyNames {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}

	if len(keys) == 0 {
		return ErrorResult("keys cannot be empty")
	}

	// Guarded for the same reason as executeWrite. Named keys ("enter", "up")
	// are control sequences and harmless, but any token that is not a known key
	// name is sent as literal text, so send-keys is a second path for typing a
	// command into a live session. guardSessionInput ignores the control
	// sequences and inspects the literal remainder.
	if guardError := t.guardSessionInput(literalSendKeysText(keys)); guardError != "" {
		return ErrorResult(guardError)
	}

	session, err := t.sessionManager.Get(sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return ErrorResult(fmt.Sprintf("session not found: %s", sessionID))
		}
		return ErrorResult(err.Error())
	}

	ptyKeyMode := session.GetPtyKeyMode()

	data, err := encodeKeySequence(keys, ptyKeyMode)
	if err != nil {
		return ErrorResult(fmt.Sprintf("invalid key: %v", err))
	}

	if session.IsDone() {
		return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
	}

	if err := session.Write(data); err != nil {
		if errors.Is(err, ErrSessionDone) {
			return ErrorResult(fmt.Sprintf("process already exited with code %d", session.GetExitCode()))
		}
		return ErrorResult(fmt.Sprintf("failed to send keys: %v", err))
	}

	resp := ExecResponse{
		SessionID: sessionID,
		Status:    "running",
		Output:    fmt.Sprintf("Sent keys: %v", keys),
	}
	respData, _ := json.Marshal(resp)
	return &ToolResult{
		ForLLM:  string(respData),
		IsError: false,
	}
}
