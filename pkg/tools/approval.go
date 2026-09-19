package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// Operator approval (H-8).
//
// A tool flagged RequiresApproval does not run on the model's say-so alone. The
// first call is parked and the operator is sent a one-time code; the call runs
// only once the operator replies "/approve <code>" and the model repeats the
// identical call.
//
// The property that matters is that the model cannot satisfy the gate itself:
//
//   - The code is delivered by the approval notifier straight to the operator's
//     chat (or terminal), never through the tool result. ForLLM is written to
//     session history, so it says approval is pending but does not carry the
//     code. ForUser is not used either: processMessage runs turns with
//     SendResponse=false, which drops ForUser, so relying on it would leave the
//     operator with nothing to approve.
//   - /approve is a command, consumed by handleCommand before any LLM turn, so
//     the operator's reply never enters model context either.
//   - An approval is bound to the channel, chat AND sender that made the request.
//     A code leaked some other way is still useless from any other identity -
//     including the "system" channel that async tool results are re-published
//     on, whose content the model influences.
//   - A grant is bound to a hash of the exact arguments and is consumed by the
//     first matching call, so changing the amount produces a fresh request.

const (
	// DefaultApprovalTTL bounds both how long a request waits for the operator
	// and how long a granted approval waits for the model to repeat the call.
	DefaultApprovalTTL = 5 * time.Minute

	// approvalCodeAlphabet omits 0/O and 1/I/L so a person can retype the code
	// from a phone screen without guessing.
	approvalCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	approvalCodeLength   = 8

	// maxPendingApprovals caps the store so a model looping on a gated tool
	// cannot grow it without bound. Expired entries are pruned first.
	maxPendingApprovals = 64
)

var (
	ErrApprovalNotFound  = errors.New("no pending approval matches that code")
	ErrApprovalWrongUser = errors.New("that approval was requested by a different chat or user")
	ErrApprovalExpired   = errors.New("that approval has expired")
)

// ApprovalKey identifies one exact tool call by one requester.
type ApprovalKey struct {
	Channel  string
	ChatID   string
	SenderID string
	Tool     string
	ArgsHash string
}

type approvalEntry struct {
	key      ApprovalKey
	code     string
	expires  time.Time
	approved bool
}

// ApprovalStore holds pending and granted approvals in memory. Restarting the
// process drops them, which fails closed: the model's retry simply asks again.
type ApprovalStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]*approvalEntry // by code
}

// NewApprovalStore returns an empty store. A non-positive ttl uses
// DefaultApprovalTTL.
func NewApprovalStore(ttl time.Duration) *ApprovalStore {
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	return &ApprovalStore{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*approvalEntry),
	}
}

// Consume reports whether an unexpired, granted approval exists for key, and
// removes it if so. A grant authorises exactly one call.
func (s *ApprovalStore) Consume(key ApprovalKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()

	for code, e := range s.entries {
		if e.approved && e.key == key {
			delete(s.entries, code)
			return true
		}
	}
	return false
}

// Request returns the code for a pending approval of key, creating one if none
// is outstanding. Re-requesting the same call reuses the existing code rather
// than flooding the chat with new ones.
func (s *ApprovalStore) Request(key ApprovalKey) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()

	for code, e := range s.entries {
		if !e.approved && e.key == key {
			e.expires = s.now().Add(s.ttl)
			return code, nil
		}
	}

	if len(s.entries) >= maxPendingApprovals {
		return "", fmt.Errorf("too many approvals outstanding (%d); deny or let some expire first",
			maxPendingApprovals)
	}

	code, err := s.newCodeLocked()
	if err != nil {
		return "", err
	}
	s.entries[code] = &approvalEntry{key: key, code: code, expires: s.now().Add(s.ttl)}
	return code, nil
}

// Approve grants the pending approval named by code, provided the approver is
// the same channel, chat and sender that requested it. It returns the tool name
// for the confirmation message.
func (s *ApprovalStore) Approve(code, channel, chatID, senderID string) (string, error) {
	return s.resolve(code, channel, chatID, senderID, true)
}

// Deny discards the pending approval named by code, under the same identity
// check as Approve.
func (s *ApprovalStore) Deny(code, channel, chatID, senderID string) (string, error) {
	return s.resolve(code, channel, chatID, senderID, false)
}

func (s *ApprovalStore) resolve(code, channel, chatID, senderID string, grant bool) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[code]
	if !ok || e.approved {
		// An already-granted entry is not re-approvable or deniable by code:
		// its only remaining transition is being consumed by the call.
		return "", ErrApprovalNotFound
	}
	if !s.now().Before(e.expires) {
		delete(s.entries, code)
		return "", ErrApprovalExpired
	}
	// Checked after existence so a wrong identity learns nothing beyond "not
	// yours", and the entry survives for its rightful owner.
	if e.key.Channel != channel || e.key.ChatID != chatID || e.key.SenderID != senderID {
		return "", ErrApprovalWrongUser
	}

	if !grant {
		delete(s.entries, code)
		return e.key.Tool, nil
	}
	e.approved = true
	e.expires = s.now().Add(s.ttl)
	return e.key.Tool, nil
}

func (s *ApprovalStore) discard(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, code)
}

func (s *ApprovalStore) pruneLocked() {
	now := s.now()
	for code, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, code)
		}
	}
}

func (s *ApprovalStore) newCodeLocked() (string, error) {
	alphabetLen := big.NewInt(int64(len(approvalCodeAlphabet)))
	for attempt := 0; attempt < 8; attempt++ {
		var b strings.Builder
		for i := 0; i < approvalCodeLength; i++ {
			n, err := rand.Int(rand.Reader, alphabetLen)
			if err != nil {
				return "", fmt.Errorf("generate approval code: %w", err)
			}
			b.WriteByte(approvalCodeAlphabet[n.Int64()])
		}
		if _, taken := s.entries[b.String()]; !taken {
			return b.String(), nil
		}
	}
	return "", errors.New("generate approval code: collision")
}

// ApprovalArgsHash hashes a call's arguments for approval matching.
//
// "confirm" is excluded: the preview call (confirm=false) and the executing call
// (confirm=true) would otherwise hash differently, and an approval could never
// match the call it was granted for. encoding/json sorts map keys, so the
// encoding is canonical for the map[string]any the registry receives.
//
// The raw arguments are hashed. Parameter sanitisation happens inside each tool,
// downstream of this check, so the operator approves what the model sent.
func ApprovalArgsHash(args map[string]any) (string, error) {
	filtered := make(map[string]any, len(args))
	for k, v := range args {
		if k == "confirm" {
			continue
		}
		filtered[k] = v
	}
	raw, err := json.Marshal(filtered)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// isPreviewCall reports whether a call explicitly asked for a dry run. Those
// place nothing, so they are not gated - gating them would force two approvals
// for one order.
func isPreviewCall(args map[string]any) bool {
	confirm, present := args["confirm"]
	if !present {
		return false
	}
	b, ok := confirm.(bool)
	return ok && !b
}

// ApprovalNotifier delivers an approval request to the operator on the chat the
// call came from.
type ApprovalNotifier func(ctx context.Context, channel, chatID, text string) error

// Package-level store, following the policy table. The command handlers and the
// registry hook must share one instance, and the registry has no field that
// every clone would reliably carry (see the note on defaultToolPolicies).
var (
	approvalMu       sync.RWMutex
	approvalStore    = NewApprovalStore(DefaultApprovalTTL)
	approvalNotifier ApprovalNotifier
)

// SetApprovalNotifier installs how approval codes reach the operator and
// returns the previous notifier. With none installed, gated calls are refused
// outright: a code nobody receives can only expire.
func SetApprovalNotifier(n ApprovalNotifier) ApprovalNotifier {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	prev := approvalNotifier
	approvalNotifier = n
	return prev
}

// Approvals returns the shared approval store.
func Approvals() *ApprovalStore {
	approvalMu.RLock()
	defer approvalMu.RUnlock()
	return approvalStore
}

// SetApprovalStore replaces the shared store and returns the previous one.
// Intended for tests, which must restore it.
func SetApprovalStore(s *ApprovalStore) *ApprovalStore {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	prev := approvalStore
	approvalStore = s
	return prev
}

// checkApproval gates a call to a RequiresApproval tool. It returns nil to let
// the call run, or a result telling the model to wait and the operator how to
// approve.
func checkApproval(ctx context.Context, name string, args map[string]any) *ToolResult {
	if isPreviewCall(args) {
		return nil
	}

	channel, chatID, sender := ToolChannel(ctx), ToolChatID(ctx), ToolSender(ctx)
	if channel == "" || chatID == "" || sender == "" {
		// No identity to bind the approval to means nobody could ever approve
		// it. Refuse plainly rather than park a request that can only expire.
		return policyRefusal(name, fmt.Sprintf(
			"%s requires operator approval, but this turn has no identifiable chat and sender "+
				"to approve it from.", name))
	}

	hash, err := ApprovalArgsHash(args)
	if err != nil {
		return policyRefusal(name, fmt.Sprintf("%s requires operator approval, but its arguments "+
			"could not be hashed: %v", name, err))
	}
	key := ApprovalKey{Channel: channel, ChatID: chatID, SenderID: sender, Tool: name, ArgsHash: hash}

	approvalMu.RLock()
	store, notify := approvalStore, approvalNotifier
	approvalMu.RUnlock()

	if store.Consume(key) {
		return nil
	}
	if notify == nil {
		return policyRefusal(name, fmt.Sprintf(
			"%s requires operator approval, but no way to reach the operator is configured.", name))
	}

	code, err := store.Request(key)
	if err != nil {
		return policyRefusal(name, fmt.Sprintf("%s requires operator approval: %v", name, err))
	}

	text := fmt.Sprintf(
		"🔐 Approval needed for %s\n%s\n\nReply /approve %s to allow this exact call once, or "+
			"/deny %s. Expires in %s.",
		name, describeArgsForApproval(args), code, code, DefaultApprovalTTL)
	if err := notify(ctx, channel, chatID, text); err != nil {
		// Nobody received the code, so nobody can redeem it. Leaving the
		// entry would only occupy a slot until it expires.
		store.discard(code)
		return policyRefusal(name, fmt.Sprintf(
			"%s requires operator approval, but the request could not be delivered: %v", name, err))
	}

	// Deliberately no code here and no ForUser: see the note at the top of
	// this file.
	return &ToolResult{
		ForLLM: fmt.Sprintf(
			"%s is waiting for operator approval and did NOT run. The operator has been sent a "+
				"one-time code. Do not retry until the operator says they have approved it; then "+
				"repeat exactly the same call with the same arguments. Changing any argument "+
				"requires a new approval.", name),
		IsError: true,
	}
}

// describeArgsForApproval renders the arguments the operator is approving.
// Keys are sorted by encoding/json; the output is capped so one oversized
// argument cannot bury the code off-screen.
func describeArgsForApproval(args map[string]any) string {
	filtered := make(map[string]any, len(args))
	for k, v := range args {
		if k != "confirm" {
			filtered[k] = v
		}
	}
	raw, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return "(arguments could not be displayed)"
	}
	const maxLen = 1200
	s := string(raw)
	if len(s) > maxLen {
		s = s[:maxLen] + "\n… (truncated)"
	}
	return s
}
