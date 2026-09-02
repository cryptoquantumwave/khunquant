package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/constants"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

type ExecTool struct {
	workingDir          string
	timeout             time.Duration
	denyPatterns        []*regexp.Regexp
	allowPatterns       []*regexp.Regexp
	customAllowPatterns []*regexp.Regexp
	allowedPathPatterns []*regexp.Regexp
	restrictToWorkspace bool
	allowRemote         bool
}

var (
	defaultDenyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`),
		regexp.MustCompile(`\bdel\s+/[fq]\b`),
		regexp.MustCompile(`\brmdir\s+/s\b`),
		// Match disk wiping commands (must be followed by space/args)
		regexp.MustCompile(
			`(^|[^-\w])\b(format|mkfs|diskpart)\b\s`,
		),
		regexp.MustCompile(`\bdd\s+if=`),
		// Block writes to block devices (all common naming schemes).
		regexp.MustCompile(
			`>\s*/dev/(sd[a-z]|hd[a-z]|vd[a-z]|xvd[a-z]|nvme\d|mmcblk\d|loop\d|dm-\d|md\d|sr\d|nbd\d)`,
		),
		regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`),
		regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),
		regexp.MustCompile(`\$\([^)]+\)`),
		regexp.MustCompile(`\$\{[^}]+\}`),
		regexp.MustCompile("`[^`]+`"),
		regexp.MustCompile(`\|\s*sh\b`),
		regexp.MustCompile(`\|\s*bash\b`),
		regexp.MustCompile(`;\s*rm\s+-[rf]`),
		regexp.MustCompile(`&&\s*rm\s+-[rf]`),
		regexp.MustCompile(`\|\|\s*rm\s+-[rf]`),
		regexp.MustCompile(`<<\s*EOF`),
		regexp.MustCompile(`\$\(\s*cat\s+`),
		regexp.MustCompile(`\$\(\s*curl\s+`),
		regexp.MustCompile(`\$\(\s*wget\s+`),
		regexp.MustCompile(`\$\(\s*which\s+`),
		regexp.MustCompile(`\bsudo\b`),
		regexp.MustCompile(`\bchmod\s+[0-7]{3,4}\b`),
		regexp.MustCompile(`\bchown\b`),
		regexp.MustCompile(`\bpkill\b`),
		regexp.MustCompile(`\bkillall\b`),
		regexp.MustCompile(`\bkill\b`),
		regexp.MustCompile(`\bcurl\b.*\|\s*(sh|bash)`),
		regexp.MustCompile(`\bwget\b.*\|\s*(sh|bash)`),
		regexp.MustCompile(`\bnpm\s+install\s+-g\b`),
		regexp.MustCompile(`\bpip\s+install\s+--user\b`),
		regexp.MustCompile(`\bapt\s+(install|remove|purge)\b`),
		regexp.MustCompile(`\byum\s+(install|remove)\b`),
		regexp.MustCompile(`\bdnf\s+(install|remove)\b`),
		regexp.MustCompile(`\bdocker\s+run\b`),
		regexp.MustCompile(`\bdocker\s+exec\b`),
		regexp.MustCompile(`\bgit\s+push\b`),
		regexp.MustCompile(`\bgit\s+force\b`),
		regexp.MustCompile(`\bssh\b.*@`),
		regexp.MustCompile(`\beval\b`),
		regexp.MustCompile(`\bsource\s+.*\.sh\b`),
		regexp.MustCompile(`\bfind\s+/(?:\s|$)`),  // find / - traverse entire filesystem
		regexp.MustCompile(`\bls\s+/(?:\s|$)`),   // ls / - list root directory
	}

	// windowsDenyPatterns contains PowerShell-specific deny patterns that only
	// apply on Windows, where commands are executed via powershell -Command.
	windowsDenyPatterns = []*regexp.Regexp{
		// [Text.Encoding] used to construct command strings at runtime.
		// Matches [Text.Encoding] and [System.Text.Encoding] variants.
		regexp.MustCompile(`\[(?:\w+\.)?text\.encoding\]`),
		// PowerShell -EncodedCommand flag (base64-encoded command) and all short forms.
		// Matches: -e, -ec, -enc, -en, -EncodedCommand (all with space prefix)
		regexp.MustCompile(` -e(?:$|\s)| -ec(?:$|\s)| -enc(?:$|\s)| -en(?:$|\s)| -encodedcommand\b`),
		// .GetString called on byte array to decode commands.
		regexp.MustCompile(`\.getstring\s*\(\s*\[byte\[\]`),
		// FromBase64String used in command construction chain.
		regexp.MustCompile(`frombase64string\(`),
		// PowerShell variable holding byte array used in GetString.
		regexp.MustCompile(`\$[a-zA-Z_]\w*\s*=\s*\[byte\[\]`),
		// Unicode escape sequences that could be used to construct commands.
		// Matches \uXXXX format used to represent characters like i = "i"
		regexp.MustCompile(`\\u[0-9a-fA-F]{4}`),
	}

	// absolutePathPattern matches absolute file paths in commands (Unix and Windows).
	absolutePathPattern = regexp.MustCompile(`[A-Za-z]:\\[^\\\"']+|/(?:[^\s\"']*)?`)

	// safePaths are kernel pseudo-devices that are always safe to reference in
	// commands, regardless of workspace restriction. They contain no user data
	// and cannot cause destructive writes.
	safePaths = map[string]bool{
		"/dev/null":    true,
		"/dev/zero":    true,
		"/dev/random":  true,
		"/dev/urandom": true,
		"/dev/stdin":   true,
		"/dev/stdout":  true,
		"/dev/stderr":  true,
	}
)

func NewExecTool(workingDir string, restrict bool, allowPaths ...[]*regexp.Regexp) (*ExecTool, error) {
	return NewExecToolWithConfig(workingDir, restrict, nil, allowPaths...)
}

func NewExecToolWithConfig(
	workingDir string,
	restrict bool,
	config *config.Config,
	allowPaths ...[]*regexp.Regexp,
) (*ExecTool, error) {
	denyPatterns := make([]*regexp.Regexp, 0)
	customAllowPatterns := make([]*regexp.Regexp, 0)
	var allowedPathPatterns []*regexp.Regexp
	allowRemote := true
	if len(allowPaths) > 0 {
		allowedPathPatterns = allowPaths[0]
	}

	if config != nil {
		execConfig := config.Tools.Exec
		enableDenyPatterns := execConfig.EnableDenyPatterns
		allowRemote = execConfig.AllowRemote
		if enableDenyPatterns {
			denyPatterns = append(denyPatterns, defaultDenyPatterns...)
			if runtime.GOOS == "windows" {
				denyPatterns = append(denyPatterns, windowsDenyPatterns...)
			}
			if len(execConfig.CustomDenyPatterns) > 0 {
				fmt.Printf("Using custom deny patterns: %v\n", execConfig.CustomDenyPatterns)
				for _, pattern := range execConfig.CustomDenyPatterns {
					re, err := regexp.Compile(pattern)
					if err != nil {
						return nil, fmt.Errorf("invalid custom deny pattern %q: %w", pattern, err)
					}
					denyPatterns = append(denyPatterns, re)
				}
			}
		} else {
			// If deny patterns are disabled, we won't add any patterns, allowing all commands.
			fmt.Println("Warning: deny patterns are disabled. All commands will be allowed.")
		}
		for _, pattern := range execConfig.CustomAllowPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid custom allow pattern %q: %w", pattern, err)
			}
			customAllowPatterns = append(customAllowPatterns, re)
		}
	} else {
		denyPatterns = append(denyPatterns, defaultDenyPatterns...)
		if runtime.GOOS == "windows" {
			denyPatterns = append(denyPatterns, windowsDenyPatterns...)
		}
	}

	timeout := 60 * time.Second
	if config != nil && config.Tools.Exec.TimeoutSeconds > 0 {
		timeout = time.Duration(config.Tools.Exec.TimeoutSeconds) * time.Second
	}

	return &ExecTool{
		workingDir:          workingDir,
		timeout:             timeout,
		denyPatterns:        denyPatterns,
		allowPatterns:       nil,
		customAllowPatterns: customAllowPatterns,
		allowedPathPatterns: allowedPathPatterns,
		restrictToWorkspace: restrict,
		allowRemote:         allowRemote,
	}, nil
}

func (t *ExecTool) Name() string {
	return NameExec
}

func (t *ExecTool) Description() string {
	return "Execute a shell command and return its output. Use with caution."
}

func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Optional working directory for the command",
			},
		},
		"required": []string{"command"},
		// Opt in to strict argument checking. The schema-validation default in
		// this tree is permissive about unnamed properties (see
		// allowsAdditional in validate.go), because models routinely add
		// harmless extras and rejecting them would break working calls across
		// every tool. exec is the one tool where an unexpected argument is
		// worth refusing outright rather than ignoring.
		"additionalProperties": false,
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	command, ok := args["command"].(string)
	if !ok {
		return ErrorResult("command is required")
	}

	// GHSA-pv8c-p6jf-3fpp: block exec from remote channels (e.g. Telegram webhooks)
	// unless explicitly opted-in via config. Fail-closed: empty channel = blocked.
	if !t.allowRemote {
		channel := ToolChannel(ctx)
		if channel == "" {
			channel, _ = args["__channel"].(string)
		}
		channel = strings.TrimSpace(channel)
		if channel == "" || !constants.IsInternalChannel(channel) {
			return ErrorResult("exec is restricted to internal channels")
		}
	}

	cwd := t.workingDir
	if wd, ok := args["working_dir"].(string); ok && wd != "" {
		if t.restrictToWorkspace && t.workingDir != "" {
			resolvedWD, err := validatePathWithAllowPaths(wd, t.workingDir, true, t.allowedPathPatterns)
			if err != nil {
				return ErrorResult("Command blocked by safety guard (" + err.Error() + ")")
			}
			cwd = resolvedWD
		} else {
			cwd = wd
		}
	}

	if cwd == "" {
		wd, err := os.Getwd()
		if err == nil {
			cwd = wd
		}
	}

	if guardError := t.guardCommand(command, cwd); guardError != "" {
		return ErrorResult(guardError)
	}

	// Re-resolve symlinks immediately before execution to shrink the TOCTOU window
	// between validation and cmd.Dir assignment.
	if t.restrictToWorkspace && t.workingDir != "" && cwd != t.workingDir {
		resolved, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return ErrorResult(fmt.Sprintf("Command blocked by safety guard (path resolution failed: %v)", err))
		}
		if isAllowedPath(resolved, t.allowedPathPatterns) {
			cwd = resolved
		} else {
			absWorkspace, _ := filepath.Abs(t.workingDir)
			wsResolved, _ := filepath.EvalSymlinks(absWorkspace)
			if wsResolved == "" {
				wsResolved = absWorkspace
			}
			rel, err := filepath.Rel(wsResolved, resolved)
			if err != nil || !filepath.IsLocal(rel) {
				return ErrorResult("Command blocked by safety guard (working directory escaped workspace)")
			}
			cwd = resolved
		}
	}

	// timeout == 0 means no timeout
	var cmdCtx context.Context
	var cancel context.CancelFunc
	if t.timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, t.timeout)
	} else {
		cmdCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(cmdCtx, "sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return ErrorResult(fmt.Sprintf("failed to start command: %v", err))
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorCF("shell", "cmd.Wait goroutine panic recovered",
					map[string]any{
						"panic": fmt.Sprintf("%v", r),
						"stack": string(debug.Stack()),
					})
				done <- fmt.Errorf("panic in cmd.Wait: %v", r)
			}
		}()
		done <- cmd.Wait()
	}()

	var err error
	select {
	case err = <-done:
	case <-cmdCtx.Done():
		_ = terminateProcessTree(cmd)
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			err = <-done
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR:\n" + stderr.String()
	}

	if err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			msg := fmt.Sprintf("Command timed out after %v", t.timeout)
			if output != "" {
				msg += "\n\nPartial output before timeout:\n" + output
			}
			return &ToolResult{
				ForLLM:  msg,
				ForUser: msg,
				IsError: true,
				Err:     fmt.Errorf("command timeout: %w", err),
			}
		}

		// Extract detailed exit information
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode := exitErr.ExitCode()
			output += fmt.Sprintf("\n\n[Command exited with code %d]", exitCode)

			// Add signal information if killed by signal (Unix)
			if exitCode == -1 {
				output += " (killed by signal)"
			}
		} else {
			output += fmt.Sprintf("\n\n[Command failed: %v]", err)
		}
	}

	if output == "" {
		output = "(no output)"
	}

	maxLen := 10000
	if len(output) > maxLen {
		output = output[:maxLen] + fmt.Sprintf("\n... (truncated, %d more chars)", len(output)-maxLen)
	}

	if err != nil {
		return &ToolResult{
			ForLLM:  output,
			ForUser: output,
			IsError: true,
		}
	}

	return &ToolResult{
		ForLLM:  output,
		ForUser: output,
		IsError: false,
	}
}

// expandPowerShellEnvVars expands environment variable syntax used by both
// PowerShell ($env:VAR) and CMD (%VAR%) to their actual values.
func expandPowerShellEnvVars(cmd string) string {
	// Handle PowerShell style: $env:VAR and ${env:VAR}
	rePs := regexp.MustCompile(`\$\{?env:(\w+)\}?`)
	cmd = rePs.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := rePs.FindStringSubmatch(match)[1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})

	// Handle CMD style: %VAR%
	reCmd := regexp.MustCompile(`%([^%]+)%`)
	return reCmd.ReplaceAllStringFunc(cmd, func(match string) string {
		varName := reCmd.FindStringSubmatch(match)[1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		return match
	})
}

// commandPathAbs resolves a command path to an absolute path, accounting for
// relative paths within the workspace.
func commandPathAbs(pathText, cwdPath string) (string, error) {
	if filepath.IsAbs(pathText) {
		return filepath.Abs(pathText)
	}
	return filepath.Abs(filepath.Join(cwdPath, pathText))
}

// commandPathTextFromMatch extracts the actual path text from a regex match,
// handling special cases like relative paths with slashes and option values.
func commandPathTextFromMatch(cmd string, start, end int) string {
	raw := cmd[start:end]
	if !strings.HasPrefix(raw, "/") || isUnixAbsolutePathMatchStart(cmd, start) {
		return raw
	}

	tokenStart, tokenEnd := shellTokenBounds(cmd, start)
	prefix := cmd[tokenStart:start]
	// For --flag=rel/path, validate the value. For ambiguous attached option
	// forms like -isystem/path, keep the slash-starting path conservative.
	if eq := strings.IndexByte(prefix, '='); eq >= 0 {
		return cmd[tokenStart+eq+1 : tokenEnd]
	}
	if strings.HasPrefix(prefix, "-") {
		return raw
	}
	return cmd[tokenStart:tokenEnd]
}

// shellTokenBounds returns the start and end position of the shell token
// containing the given index.
func shellTokenBounds(cmd string, idx int) (int, int) {
	start := idx
	for start > 0 && !isShellTokenBoundary(cmd[start-1]) {
		start--
	}
	end := idx
	for end < len(cmd) && !isShellTokenBoundary(cmd[end]) {
		end++
	}
	return start, end
}

// isUnixAbsolutePathMatchStart returns true when a regex match beginning with
// "/" is actually an absolute path token, not the separator inside a relative
// path such as "skills/foo.py".
func isUnixAbsolutePathMatchStart(cmd string, idx int) bool {
	if idx <= 0 {
		return true
	}

	prev := cmd[idx-1]
	if isShellTokenBoundary(prev) || prev == '=' || prev == ',' || prev == '(' || prev == '[' || prev == '{' {
		return true
	}

	j := idx - 1
	for j >= 0 && !isShellTokenBoundary(cmd[j]) {
		j--
	}
	prefix := cmd[j+1 : idx]

	return strings.HasPrefix(prefix, "-") && !strings.Contains(prefix, "=")
}

// isShellTokenBoundary returns true when b is a byte that separates
// tokens in a shell command (space, tab, colon, semicolon, pipe, etc.).
func isShellTokenBoundary(b byte) bool {
	switch b {
	case ' ', '\t', ':', ';', '|', '&', '<', '>', '\'', '"', '`', '\n', '\r':
		return true
	}
	return false
}

// looksLikeDomain returns true when s looks like a DNS domain name:
// it contains at least one dot, starts with an alphanumeric character,
// and does not end with a common file extension.
func looksLikeDomain(s string) bool {
	if len(s) < 3 || !strings.ContainsRune(s, '.') {
		return false
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	// Exclude tokens ending with common file/programming extensions,
	// e.g. "script.py", "main.go", "app.exe".
	if idx := strings.LastIndexByte(s, '.'); idx >= 0 {
		ext := strings.ToLower(s[idx+1:])
		if commonFileExtension(ext) {
			return false
		}
	}
	return true
}

// commonFileExtension returns true when ext is a file extension that
// strongly indicates a local file rather than a domain TLD.
func commonFileExtension(ext string) bool {
	switch ext {
	case "py", "js", "ts", "tsx", "jsx", "go", "rs", "rb", "php",
		"java", "c", "cpp", "h", "hpp", "cs", "swift", "kt", "scala",
		"sh", "bash", "zsh", "fish", "ps1", "bat", "cmd",
		"txt", "md", "rst", "log", "json", "yaml", "yml", "toml",
		"xml", "html", "css", "scss", "ini", "cfg", "conf", "env",
		"exe", "dll", "so", "dylib", "lib", "a", "o", "obj",
		"zip", "tar", "gz", "bz2", "xz", "7z", "rar",
		"png", "jpg", "jpeg", "gif", "svg", "ico", "bmp", "webp",
		"mp3", "mp4", "wav", "avi", "mov", "mkv", "flac",
		"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
		"pub", "pem", "key", "crt", "cer", "p12", "pfx",
		"bak", "tmp", "swp", "lock",
		"ttf", "otf", "woff", "woff2", "eot",
		"deb", "rpm", "apk", "msi", "dmg",
		"sql", "sqlite", "db":
		return true
	}
	return false
}

// localPathExists returns true when the given token resolves to an
// existing filesystem entry relative to cwd.
func localPathExists(cwd, token string) bool {
	info, err := os.Lstat(filepath.Join(cwd, token))
	return err == nil && info != nil
}

// commandMatchesAllowPattern checks if a command matches either the standard
// allow patterns or the custom allow patterns.
func (t *ExecTool) commandMatchesAllowPattern(lower string) bool {
	for _, pattern := range t.allowPatterns {
		if pattern.MatchString(lower) {
			return true
		}
	}
	for _, pattern := range t.customAllowPatterns {
		if pattern.MatchString(lower) {
			return true
		}
	}
	return false
}

func (t *ExecTool) guardCommand(command, cwd string) string {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)

	// Deny patterns always apply, even when a command matches a custom allow rule.
	// Custom allow rules can permit a command, but must not disable secret-safety
	// deny rules such as jq env access checks.
	for _, pattern := range t.denyPatterns {
		if pattern.MatchString(lower) {
			return "Command blocked by safety guard (dangerous pattern detected)"
		}
	}

	if len(t.allowPatterns) > 0 {
		if !t.commandMatchesAllowPattern(lower) {
			return "Command blocked by safety guard (not in allowlist)"
		}
	}

	if t.restrictToWorkspace {
		// Block path traversal patterns including .../.../ variants
		if regexp.MustCompile(`\.\.(?:[\\/]\.\.)*[\\/]`).MatchString(cmd) {
			return "Command blocked by safety guard (path traversal detected)"
		}

		cwdPath, err := filepath.Abs(cwd)
		if err != nil {
			return ""
		}

		// Resolve symlinks on the workspace path to enable proper comparison
		if resolved, err := filepath.EvalSymlinks(cwdPath); err == nil {
			cwdPath = resolved
		}

		// On Windows, expand ~ and PowerShell environment variables ($env:VAR) before path checking
		if runtime.GOOS == "windows" {
			// Expand PowerShell environment variables ($env:VAR and ${env:VAR})
			cmd = expandPowerShellEnvVars(cmd)
			// Also expand ~ for completeness
			if home, err := os.UserHomeDir(); err == nil {
				cmd = strings.ReplaceAll(cmd, "~", filepath.FromSlash(home))
			}
		}

		// Web URL schemes whose path components (starting with //) should be exempt
		// from workspace sandbox checks. file: is intentionally excluded so that
		// file:// URIs are still validated against the workspace boundary.
		webSchemes := []string{"http:", "https:", "ftp:", "ftps:", "sftp:", "ssh:", "git:"}

		matchIndices := absolutePathPattern.FindAllStringIndex(cmd, -1)

		for _, loc := range matchIndices {
			raw := cmd[loc[0]:loc[1]]

			// Skip URL path components that look like they're from web URLs.
			// When a URL like "https://github.com" is parsed, the regex captures
			// "//github.com" as a match (the path portion after "https:").
			// Use the exact match position (loc[0]) so that duplicate //path substrings
			// in the same command are each evaluated at their own position.
			if strings.HasPrefix(raw, "//") && loc[0] > 0 {
				before := cmd[:loc[0]]
				isWebURL := false

				for _, scheme := range webSchemes {
					if strings.HasSuffix(before, scheme) {
						isWebURL = true
						break
					}
				}

				if isWebURL {
					continue
				}
			}

			// Skip scheme-less URL paths like "wttr.in/Beijing".
			// When a /path is immediately preceded by a token that looks
			// like a domain name and that token does NOT exist as a local
			// filesystem entry, treat the path as part of a URL and skip
			// workspace sandbox validation.
			//
			// The local-path-exists guard prevents symlink bypass: if
			// "foo.bar" exists as a local symlink or directory, the path
			// still undergoes full workspace validation (see #2965).
			if loc[0] > 0 && raw[0] == '/' {
				// Find the token immediately before the "/".
				j := loc[0] - 1
				for j >= 0 && !isShellTokenBoundary(cmd[j]) {
					j--
				}
				token := cmd[j+1 : loc[0]]
				if looksLikeDomain(token) && !localPathExists(cwd, token) {
					continue
				}
			}

			p, err := commandPathAbs(commandPathTextFromMatch(cmd, loc[0], loc[1]), cwdPath)
			if err != nil {
				continue
			}

			// Windows-specific: normalize paths to block ADS and extended-length paths
			if runtime.GOOS == "windows" {
				// Strip \\?\ prefix (extended-length path)
				p = strings.TrimPrefix(p, `\\?\`)
				// Strip NTFS alternate data streams (only if colon is not at position 1 = drive letter)
				if idx := strings.Index(p, ":"); idx > 1 {
					p = p[:idx]
				}
			}

			// Check symlinks and junctions
			resolved, err := filepath.EvalSymlinks(p)
			if err == nil {
				p = resolved
			}

			if safePaths[p] {
				continue
			}
			if isAllowedPath(p, t.allowedPathPatterns) {
				continue
			}

			rel, err := filepath.Rel(cwdPath, p)
			if err != nil {
				continue
			}

			if strings.HasPrefix(rel, "..") {
				return "Command blocked by safety guard (path outside working dir)"
			}
		}
	}

	return ""
}

func (t *ExecTool) SetTimeout(timeout time.Duration) {
	t.timeout = timeout
}

func (t *ExecTool) SetRestrictToWorkspace(restrict bool) {
	t.restrictToWorkspace = restrict
}

func (t *ExecTool) SetAllowPatterns(patterns []string) error {
	t.allowPatterns = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("invalid allow pattern %q: %w", p, err)
		}
		t.allowPatterns = append(t.allowPatterns, re)
	}
	return nil
}
