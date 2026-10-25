// Package trigger implements `khunquant trigger`, which asks the running gateway to
// notice something: a script, a hook or a cron job on this machine hands the
// assistant a short message, and the gateway puts it through attention like any
// other candidate. It talks to the gateway over loopback, with the gateway's
// management token (gateway.management_token in config.json, the one the other
// management routes take), and it is not a delivery: an accepted request is one the
// assistant will consider, nothing more.
//
// Exit codes: 0 accepted; 2 a usage or validation error (the command line, or the
// gateway refusing the content); 3 the gateway is not running or cannot be reached;
// 4 the gateway refused the request (credentials, rate limit, perception or
// triggers off, a busy pipeline); 1 anything else.
package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

// Exit codes.
const (
	ExitUsage   = 2
	ExitNoGate  = 3
	ExitRefused = 4
)

const (
	triggerPath    = "/api/proactive/trigger"
	requestTimeout = 10 * time.Second
	// maxReply bounds what is read of the gateway's answer.
	maxReply = 8 << 10
	// maxStdinBytes bounds --stdin: the message, at 4 bytes a character, and a byte
	// to tell that there is more.
	maxStdinBytes = perception.MaxExternalMessageRunes*4 + 1
)

// ExitError is the error a command returns to say how the process should end. The
// caller prints Error() (when it is not empty) on stderr and exits with Code.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// ExitCode is the process exit status.
func (e *ExitError) ExitCode() int { return e.Code }

func usageErr(format string, a ...any) error {
	return &ExitError{Code: ExitUsage, Msg: fmt.Sprintf(format, a...)}
}

// deps are the command's seams.
type deps struct {
	// home is the khunquant home, where the gateway's pid file is.
	home func() string
	// load reads the config (for the management token).
	load func() (*config.Config, error)
	// readPid returns the running gateway's pid file, nil when it is not running.
	readPid func(home string) *pid.PidFileData
	client  *http.Client
}

func defaultDeps() deps {
	return deps{
		home:    config.HomeDir,
		load:    internal.LoadConfig,
		readPid: pid.ReadPidFileWithCheck,
		client: &http.Client{
			Timeout: requestTimeout,
			// A redirect is never followed: the request carries the token.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// NewTriggerCommand returns `khunquant trigger`.
func NewTriggerCommand() *cobra.Command { return newTriggerCommand(defaultDeps()) }

func newTriggerCommand(d deps) *cobra.Command {
	var (
		severity string
		data     string
		stdin    bool
	)
	cmd := &cobra.Command{
		Use:   "trigger <name> [message]",
		Short: "Ask the running gateway to notice something",
		Long: "Hands the running gateway a short message under a name, for the assistant to consider.\n" +
			"It goes through attention like anything else the assistant notices: it may be told to you at once,\n" +
			"held for the daily digest, or dropped as a repeat. An accepted request is not a delivery.\n\n" +
			"The name is 1-41 characters of a-z, 0-9, '_', '.' and '-', starting with a letter or digit. The message is at most\n" +
			"2000 characters; --data is a JSON object of at most 4 KB. Severity is info, notice (the default) or warning.\n\n" +
			"Exit codes: 0 accepted, 2 usage or validation error, 3 the gateway is not running, 4 refused by the gateway\n" +
			"(credentials, rate limit, perception or triggers off).",
		Example: "  khunquant trigger backup-done \"NAS backup finished with 2 errors\" --severity warning\n" +
			"  echo \"disk at 95%\" | khunquant trigger disk-full --stdin",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErr("usage: khunquant trigger <name> [message] (got %d arguments)", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, d, args, severity, data, stdin)
		},
	}
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr("%v", err) })
	cmd.Flags().StringVar(&severity, "severity", "", "info, notice (default) or warning")
	cmd.Flags().StringVar(&data, "data", "", "a JSON object of at most 4 KB to keep with the request")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read the message from standard input instead of the command line")
	return cmd
}

type request struct {
	Name     string          `json:"name"`
	Message  string          `json:"message,omitempty"`
	Severity string          `json:"severity,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

func run(cmd *cobra.Command, d deps, args []string, severity, data string, useStdin bool) error {
	req := request{Name: args[0], Severity: strings.ToLower(strings.TrimSpace(severity))}
	if !perception.ValidExternalName(req.Name) {
		return usageErr("the name must match ^[a-z0-9][a-z0-9_.-]{0,40}$")
	}
	switch {
	case useStdin && len(args) == 2:
		return usageErr("give the message as an argument or with --stdin, not both")
	case useStdin:
		msg, err := readStdin(cmd.InOrStdin())
		if err != nil {
			return err
		}
		req.Message = msg
	case len(args) == 2:
		req.Message = args[1]
	}
	if err := checkMessage(req.Message); err != nil {
		return err
	}
	switch req.Severity {
	case "", "info", "notice", "warning":
	default:
		return usageErr("--severity must be info, notice or warning")
	}
	if data != "" {
		raw, err := checkData(data)
		if err != nil {
			return err
		}
		req.Data = raw
	}

	base, err := gatewayBase(d)
	if err != nil {
		return err
	}
	cfg, err := d.load()
	if err != nil {
		return &ExitError{Code: 1, Msg: "cannot read the configuration: " + clean(err.Error(), 200, "")}
	}
	token := cfg.Gateway.ManagementToken
	if token == "" {
		return &ExitError{Code: ExitRefused, Msg: "there is no management token in the configuration " +
			"(gateway.management_token): start the gateway once so that it creates one"}
	}
	return send(cmd, d, base, token, req)
}

// readStdin reads the message from r, at most the bound, and trims the line break
// that ends it.
func readStdin(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxStdinBytes))
	if err != nil {
		return "", &ExitError{Code: 1, Msg: "cannot read standard input: " + clean(err.Error(), 200, "")}
	}
	if len(b) >= maxStdinBytes {
		return "", usageErr("the message is over %d characters", perception.MaxExternalMessageRunes)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func checkMessage(msg string) error {
	if utf8.RuneCountInString(msg) > perception.MaxExternalMessageRunes {
		return usageErr("the message is over %d characters", perception.MaxExternalMessageRunes)
	}
	if !utf8.ValidString(msg) {
		return usageErr("the message is not valid UTF-8")
	}
	for _, r := range msg {
		switch {
		case perception.IsBidiControl(r):
			return usageErr("the message holds a bidirectional control character")
		case unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t':
			return usageErr("the message holds a control character")
		}
	}
	return nil
}

func checkData(data string) (json.RawMessage, error) {
	data = strings.TrimSpace(data)
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(data)); err != nil || !strings.HasPrefix(data, "{") {
		return nil, usageErr("--data must be a JSON object")
	}
	if buf.Len() > perception.MaxExternalDataBytes {
		return nil, usageErr("--data is over %d bytes", perception.MaxExternalDataBytes)
	}
	return buf.Bytes(), nil
}

// gatewayBase finds the running gateway: the pid file says whether it runs and where
// it listens. The token is only ever sent to a loopback address.
func gatewayBase(d deps) (string, error) {
	pf := d.readPid(d.home())
	if pf == nil {
		return "", &ExitError{Code: ExitNoGate, Msg: "the gateway is not running (no live pid file); start it with `khunquant gateway`"}
	}
	host := strings.Trim(strings.TrimSpace(pf.Host), "[]")
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
		return "", &ExitError{Code: ExitNoGate, Msg: "the gateway listens on " + clean(host, 64, "") +
			", which is not a loopback address; its management API answers on loopback only"}
	}
	if pf.Port <= 0 || pf.Port > 65535 {
		return "", &ExitError{Code: ExitNoGate, Msg: "the pid file holds no usable port"}
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(pf.Port))}).String(), nil
}

// reply is the part of the gateway's answer the command uses.
type reply struct {
	EventID  string `json:"event_id"`
	Severity string `json:"severity"`
	Mode     string `json:"mode"`
	Error    string `json:"error"`
}

func send(cmd *cobra.Command, d deps, base, token string, req request) error {
	body, err := json.Marshal(req)
	if err != nil {
		return &ExitError{Code: 1, Msg: "cannot encode the request"}
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, base+triggerPath, bytes.NewReader(body))
	if err != nil {
		return &ExitError{Code: 1, Msg: "cannot build the request"}
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+token)

	resp, err := d.client.Do(hr)
	if err != nil {
		// err can name the address; it never holds the token (it is a header).
		return &ExitError{Code: ExitNoGate, Msg: "the gateway did not answer at " + base + ": " + reason(err, token)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	var rep reply
	_ = json.Unmarshal(raw, &rep)

	switch {
	case resp.StatusCode == http.StatusAccepted:
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Accepted for consideration (event %s). This is not a delivery: the assistant decides whether, and when, to tell you.\n",
			clean(rep.EventID, 40, token))
		asked := req.Severity
		if asked == "" {
			asked = "notice"
		}
		if sev := onlyOneOf(rep.Severity, "info", "notice", "warning"); sev != "" && sev != asked {
			fmt.Fprintf(out, "Treated as %s: a request is never more urgent than a warning.\n", sev)
		}
		if onlyOneOf(rep.Mode, "shadow") != "" {
			fmt.Fprintln(out, "Triggers are in shadow mode: it will be recorded and never delivered.")
		}
		return nil
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusRequestEntityTooLarge:
		return usageErr("the gateway refused the request: %s", detail(rep, resp.StatusCode, token))
	default:
		msg := "the gateway refused the request: " + detail(rep, resp.StatusCode, token)
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			msg = "the gateway did not accept the management token (" + strconv.Itoa(resp.StatusCode) + ")"
		case http.StatusNotFound:
			msg = "the gateway has no trigger route: perception is not enabled " +
				"(proactive.enabled and proactive.perception.enabled), or the gateway is older than this command"
		case http.StatusTooManyRequests:
			msg = "too many trigger requests; try again in " + onlySeconds(resp.Header.Get("Retry-After")) + " seconds"
		}
		return &ExitError{Code: ExitRefused, Msg: msg}
	}
}

// detail is the gateway's reason, cleaned, or its status.
func detail(rep reply, status int, token string) string {
	if e := clean(rep.Error, 200, token); e != "" {
		return e
	}
	return "status " + strconv.Itoa(status)
}

// reason is why a request failed, without anything that could be the token.
func reason(err error, token string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return clean(err.Error(), 200, token)
}

// clean makes text from outside fit on one line of the terminal: control and
// invisible characters become spaces, it is cut to limit runes, and the token is
// blanked out of it.
func clean(s string, limit int, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > limit {
		s = string([]rune(s)[:limit-1]) + "…"
	}
	return s
}

func onlyOneOf(s string, allowed ...string) string {
	for _, a := range allowed {
		if s == a {
			return s
		}
	}
	return ""
}

func onlySeconds(s string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 && n < 1<<20 {
		return strconv.Itoa(n)
	}
	return "a few"
}
