package heartbeat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The sent-alert ledger. A legacy heartbeat reports through `message`, and with
// no guardrail in code it would say the same thing every interval. The v1
// template kept a dedup ledger in memory/MEMORY.md, written by the model; that is
// not an option any more (autonomous writes to memory/ are blocked), and a ledger
// the model keeps is only as good as its habit of keeping it. So code keeps it:
// every message a heartbeat turn really delivers is appended here, and the next
// legacy prompt shows the last day of it.
const (
	ledgerDir  = "heartbeat"
	ledgerFile = "ledger.jsonl"

	// ledgerShow is how far back the prompt shows; ledgerKeep is how long an
	// entry is kept (the prune happens on write).
	ledgerShow = 24 * time.Hour
	ledgerKeep = 48 * time.Hour

	// ledgerTextMax caps the text kept per entry, in runes.
	ledgerTextMax = 300
)

// SentAlert is one message a legacy heartbeat turn sent to the owner.
type SentAlert struct {
	Time    time.Time `json:"time"`
	Channel string    `json:"channel"`
	ChatID  string    `json:"chat_id"`
	Text    string    `json:"text"`
}

// ledgerMu serialises the read-modify-write of the ledger file: a message-tool
// send and sendResponse can both write, and the heartbeat of a reloaded service
// may overlap the old one's last write.
var ledgerMu sync.Mutex

func ledgerPath(workspace string) string {
	return filepath.Join(workspace, ledgerDir, ledgerFile)
}

// readLedger returns the entries in the file, oldest first. A missing or
// unreadable file is no entries; a line that does not parse is skipped, so a
// half-written or hand-edited file costs only the bad line. The caller holds
// ledgerMu.
func readLedger(workspace string) []SentAlert {
	data, err := os.ReadFile(ledgerPath(workspace))
	if err != nil {
		return nil
	}
	var out []SentAlert
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e SentAlert
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Time.IsZero() {
			continue
		}
		out = append(out, e)
	}
	return out
}

// oneLine collapses every run of whitespace (newlines included) to one space
// and truncates to limit runes.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "..."
	}
	return s
}

// recordSent appends a message to the ledger and prunes what is older than
// ledgerKeep. The write is atomic (temp file, rename). It never fails the
// heartbeat: an error is logged, and the message has been delivered regardless.
func recordSent(workspace string, now time.Time, channel, chatID, text string) {
	text = oneLine(text, ledgerTextMax)
	if text == "" {
		return
	}

	ledgerMu.Lock()
	defer ledgerMu.Unlock()

	cutoff := now.Add(-ledgerKeep)
	entries := []SentAlert{}
	for _, e := range readLedger(workspace) {
		if !e.Time.Before(cutoff) {
			entries = append(entries, e)
		}
	}
	entries = append(entries, SentAlert{Time: now, Channel: channel, ChatID: chatID, Text: text})

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			logger.WarnCF("heartbeat", "Could not encode a sent-alert ledger entry", map[string]any{"error": err.Error()})
			return
		}
	}
	if err := fileutil.WriteFileAtomic(ledgerPath(workspace), buf.Bytes(), 0o644); err != nil {
		logger.WarnCF("heartbeat", "Could not write the sent-alert ledger; the next heartbeat will not see this message",
			map[string]any{"error": err.Error()})
	}
}

// recentSent returns the entries from the last ledgerShow, oldest first.
func recentSent(workspace string, now time.Time) []SentAlert {
	ledgerMu.Lock()
	all := readLedger(workspace)
	ledgerMu.Unlock()

	cutoff := now.Add(-ledgerShow)
	var out []SentAlert
	for _, e := range all {
		if e.Time.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sentSection is the part of a legacy prompt that lists what was already sent.
func sentSection(sent []SentAlert) string {
	var b strings.Builder
	b.WriteString("Alerts you already sent in the last 24h:\n")
	if len(sent) == 0 {
		b.WriteString("(none)\n")
		return b.String()
	}
	for _, e := range sent {
		fmt.Fprintf(&b, "- %s [%s] %s\n", e.Time.Format("2006-01-02 15:04 -07:00"), e.Channel, e.Text)
	}
	return b.String()
}

// RecordSent records a message that a heartbeat turn delivered to the owner. The
// service's clock decides the time.
func (hs *HeartbeatService) RecordSent(channel, chatID, text string) {
	recordSent(hs.workspace, hs.clock.Now(), channel, chatID, text)
}

// ObserveMessageSends makes every message mt delivers during a heartbeat turn
// land in the ledger. A heartbeat message is recognised by the origin its turn
// carries (tools.OriginHeartbeat), so a cron job's or a user's message through
// the same tool is not recorded. This is the one place a `message` send can be
// seen whole: its target and text are known, it has just succeeded, and the
// turn's context is at hand, whereas the service only gets the turn's final text
// back. Call it again after a reload; it replaces the earlier observer.
func (hs *HeartbeatService) ObserveMessageSends(mt *tools.MessageTool) {
	if mt == nil {
		return
	}
	mt.SetSendObserver(func(ctx context.Context, channel, chatID, content string) {
		if tools.ToolOrigin(ctx) != tools.OriginHeartbeat {
			return
		}
		hs.RecordSent(channel, chatID, content)
	})
}
