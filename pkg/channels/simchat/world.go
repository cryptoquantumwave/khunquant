package simchat

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

const (
	maxButtonData = 64   // Telegram's callback_data limit, the strictest of the platforms
	maxOutbox     = 2000 // messages kept; the journal keeps all of them
	idPrefix      = "sc-"
)

// Journal is where the world records chat.*; *labjournal.Journal is one. If it also has
// Query (it does), NewWorld resumes the message counter from it.
type Journal interface{ Append(e labjournal.Entry) }

// Button is one button of an outbound message. Data is the encoded "kq1|..." callback
// data a client would send back.
type Button struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Data   string `json:"data"`
}

// OutMsg is a message the instance sent, as the outbox shows it. Severity, DedupKey and
// InterventionID are set only for a proactive message ("" otherwise).
type OutMsg struct {
	Seq            int64     `json:"seq"`
	ID             string    `json:"id,omitempty"` // "" when the send was refused
	To             string    `json:"to"`           // the chat id
	Text           string    `json:"text"`
	ReplyTo        string    `json:"reply_to,omitempty"`
	Severity       string    `json:"severity"`
	DedupKey       string    `json:"dedup_key"`
	InterventionID string    `json:"intervention_id"`
	Silent         bool      `json:"silent"`
	Buttons        []Button  `json:"buttons"`
	ButtonsLive    bool      `json:"buttons_live"` // false once the message was edited
	Delivered      bool      `json:"delivered"`
	Err            string    `json:"error,omitempty"`
	At             time.Time `json:"at"`
	SimAt          time.Time `json:"sim_at"`
}

// World is everything about the simulated chat that must outlive a config reload:
// the personas and groups, the outbox, the channel state and the message counters.
// Each generation's Channel attaches to it.
type World struct {
	j        Journal
	now      func() time.Time
	personas map[string]config.SimChatPersona
	groups   map[string][]string

	mu     sync.Mutex
	ch     *Channel
	out    []*OutMsg
	byID   map[string]*OutMsg
	seq    int64 // outbox sequence, refused sends included
	nextID int64 // last "sc-<n>" handed out
	inID   int64
	cbID   int64
	st     chanState
}

// NewWorld builds the world from the first config of the process; the personas are
// fixed from then on. now is read on every use.
func NewWorld(j Journal, now func() time.Time, cfg config.SimChatConfig) *World {
	w := &World{j: j, now: now, personas: map[string]config.SimChatPersona{}, groups: cfg.Groups,
		byID: map[string]*OutMsg{}, st: chanState{kind: stateUp}}
	ps := cfg.Personas
	if len(ps) == 0 {
		ps = []config.SimChatPersona{{Name: "owner"}, {Name: "stranger"}, {Name: "alice"}, {Name: "bob"}}
	}
	for _, p := range ps {
		w.personas[p.Name] = p
	}
	if len(w.groups) == 0 {
		w.groups = map[string][]string{"team": {"alice", "bob"}}
	}
	w.nextID = resumeCounter(j)
	return w
}

// resumeCounter returns the highest "sc-<n>" in the journal's chat.out entries, so a
// message after a restart never reuses the ID of a card stored before it (approval
// matches a card by ID). Refused sends have no ID, hence the scan.
func resumeCounter(j Journal) int64 {
	q, ok := j.(interface {
		Query(ctx context.Context, q labjournal.Query) ([]labjournal.Entry, error)
	})
	if !ok {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	es, err := q.Query(ctx, labjournal.Query{Kinds: []string{"chat.out"}, Desc: true, Limit: labjournal.MaxLimit})
	if err != nil {
		return 0
	}
	var top int64
	for _, e := range es {
		var p struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil {
			if n, err := strconv.ParseInt(strings.TrimPrefix(p.ID, idPrefix), 10, 64); err == nil {
				top = max(top, n)
			}
		}
	}
	return top
}

func (w *World) attach(c *Channel) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ch = c
}

func (w *World) detach(c *Channel) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == c {
		w.ch = nil
	}
}

// Attached is the channel of the running generation, nil when there is none or it is not running.
func (w *World) Attached() *Channel {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ch == nil || !w.ch.IsRunning() {
		return nil
	}
	return w.ch
}

// log appends a chat.* entry whose subject is the chat id (inspect's outbox filters on it).
func (w *World) log(kind, chatID string, payload any) {
	if w.j == nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte(`{}`)
	}
	w.j.Append(labjournal.Entry{Kind: kind, Source: channelName, Subject: chatID, Payload: b})
}

// sender is the identity a persona's messages carry: "simchat:<ID>".
func sender(p config.SimChatPersona) bus.SenderInfo {
	id := cmp.Or(p.ID, p.Name)
	return bus.SenderInfo{
		Platform: channelName, PlatformID: id, CanonicalID: identity.BuildCanonicalID(channelName, id),
		Username: p.Name, DisplayName: cmp.Or(p.DisplayName, p.Name),
	}
}

// route resolves who speaks and where. as is a persona name, or "name@group:g"; chat
// overrides the "@" part. A persona's own chat is dm:<name>; a group must exist, and
// anyone may speak in it (the owner in a group is a scenario of its own).
func (w *World) route(as, chat string) (p config.SimChatPersona, chatID, peerKind string, err error) {
	name, at, _ := strings.Cut(as, "@")
	p, ok := w.personas[name]
	if !ok {
		return p, "", "", fmt.Errorf("unknown persona %q", name)
	}
	switch chat = cmp.Or(chat, at); {
	case chat == "" || chat == "dm:"+name:
		return p, "dm:" + name, "direct", nil
	case strings.HasPrefix(chat, "group:") && w.groups[strings.TrimPrefix(chat, "group:")] != nil:
		return p, chat, "group", nil
	}
	return p, "", "", fmt.Errorf("persona %q cannot speak in chat %q (use dm:%s or an existing group:<name>)", name, chat, name)
}

// deliver is the channel's send: refuse as the state says, else capture the message.
func (w *World) deliver(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	w.mu.Lock()
	wait, gateErr := w.st.gate(w.now())
	w.mu.Unlock()
	if gateErr == nil && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			gateErr = ctx.Err()
		}
	}

	m := &OutMsg{
		To: msg.ChatID, Text: msg.Content, ReplyTo: msg.ReplyToMessageID, DedupKey: msg.DedupKey,
		InterventionID: msg.InterventionID, Silent: msg.DisableNotification, Buttons: []Button{},
		At: time.Now(), SimAt: w.now(),
	}
	if msg.IsProactive() {
		m.Severity = msg.Severity.String()
	}
	m.Buttons = encodeButtons(msg.Actions)
	w.mu.Lock()
	w.seq++
	m.Seq = w.seq
	if gateErr != nil {
		m.Err = gateErr.Error()
	} else {
		w.nextID++
		m.ID, m.Delivered, m.ButtonsLive = idPrefix+strconv.FormatInt(w.nextID, 10), true, len(m.Buttons) > 0
		w.byID[m.ID] = m
	}
	if w.out = append(w.out, m); len(w.out) > maxOutbox {
		delete(w.byID, w.out[0].ID)
		w.out = w.out[1:]
	}
	snap := *m
	w.mu.Unlock()

	w.log("chat.out", m.To, struct {
		OutMsg
		Sandbox bool `json:"sandbox"`
	}{snap, true})
	if gateErr != nil {
		return nil, gateErr
	}
	return []string{snap.ID}, nil
}

// encodeButtons encodes every action with the codec every channel shares. One that
// cannot be encoded drops all the buttons, as the Telegram adapter does.
func encodeButtons(actions []bus.Action) []Button {
	out := make([]Button, 0, len(actions))
	for _, a := range actions {
		data, err := channels.EncodeCallbackData(a, maxButtonData)
		if err != nil {
			return []Button{}
		}
		out = append(out, Button{Action: a.ID, Label: cmp.Or(strings.TrimSpace(a.Label), a.ID), Data: data})
	}
	return out
}

// edit replaces a sent message's text and takes its keyboard away, as Telegram's
// editCard does. An unknown message is an error, so a card editor sees the failure.
func (w *World) edit(chatID, id, text string) error {
	w.mu.Lock()
	m := w.byID[id]
	if m == nil || m.To != chatID {
		w.mu.Unlock()
		return errors.New("simchat: no such message " + id + " in " + chatID)
	}
	m.Text, m.ButtonsLive = text, false
	w.mu.Unlock()
	w.log("chat.edit", chatID, map[string]any{"message": id, "text": text, "buttons_removed": true, "sandbox": true})
	return nil
}

// setState switches the failure mode; the next send sees it.
func (w *World) setState(s chanState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.st = s
}
