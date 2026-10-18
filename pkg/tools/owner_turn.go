package tools

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

// ownerVerdict is the answer to "is the turn carried by ctx the owner, in a
// conversation?". It is a decision, not text: each tool words its own refusal.
type ownerVerdict int

const (
	// ownerYes: the owner, typing at the local console or sending from their
	// configured chat account.
	ownerYes ownerVerdict = iota
	// ownerBackground: an untagged turn, or one the assistant started itself (a
	// heartbeat, a scheduled job, a proactive turn).
	ownerBackground
	// ownerAutomated: an internal turn that was not typed at the console (a system
	// or subagent turn: it can carry text a web page put there).
	ownerAutomated
	// ownerSubTurn: a turn spawned by another turn. Only returned when the caller
	// asked for root turns only.
	ownerSubTurn
	// ownerNoConfig: a chat turn and no config to look the owner up in.
	ownerNoConfig
	// ownerNoOwner: a chat turn and no owner is configured.
	ownerNoOwner
	// ownerOther: a chat turn from somebody who is not the configured owner.
	ownerOther
	// ownerUnknownOrigin: an origin this function does not know.
	ownerUnknownOrigin
)

// checkOwnerTurn decides whether the turn carried by ctx is the owner in a
// conversation, and returns the name to record as the author ("cli" at the local
// console, the canonical sender in a chat). It is the one place that rule lives:
// update_profile, create_intent and update_intent all ask it.
//
//   - A background turn (untagged, heartbeat, cron, proactive) is never the owner.
//   - An internal turn is the owner only when it is typed at the local console
//     (channel "cli"); a system or subagent turn is not.
//   - A remote turn is the owner only when an owner is configured and the turn's
//     sender is that owner. With no owner configured nothing a chat sends counts.
//
// rootOnly additionally refuses a sub-turn: a context tagged by WithSubTurn, or a
// remote turn with no channel. A sub-turn (spawn, delegate, subagent) keeps the
// origin and sender of the turn that started it (see subturn.go), and its task
// text was written by a model that may have read a web page. Every caller asks
// for root turns only (update_profile, create_intent, update_intent), because
// what they change outlives the turn. (The SubTurnSpawner's children run without a channel, the
// SubagentManager's with their parent's; the tag covers both, and the empty
// channel stays as a second line.)
func checkOwnerTurn(ctx context.Context, cfg *config.Config, rootOnly bool) (by string, v ownerVerdict) {
	origin := ToolOrigin(ctx)
	switch {
	case origin == "" || IsAutonomousOrigin(origin):
		return "", ownerBackground
	case origin == OriginInternal:
		if ToolChannel(ctx) != cliChannel {
			return "", ownerAutomated
		}
		return cliChannel, ownerYes
	case origin == OriginRemote:
		if rootOnly && (IsSubTurn(ctx) || ToolChannel(ctx) == "") {
			return "", ownerSubTurn
		}
		sender := canonicalSender(ctx)
		if cfg == nil {
			return "", ownerNoConfig
		}
		route, ok := cfg.Proactive.OwnerRoute()
		if !ok {
			return "", ownerNoOwner
		}
		if !identity.SameSender(sender, route.SenderID) {
			return "", ownerOther
		}
		return sender, ownerYes
	}
	return "", ownerUnknownOrigin
}

// canonicalSender returns the turn's sender as "platform:id". Channels set the
// sender to that form already; a raw id is qualified with the turn's channel,
// the way the allow-list falls back.
func canonicalSender(ctx context.Context) string {
	return identity.CanonicalSender(ToolSender(ctx), ToolChannel(ctx))
}

// isOwnerRootTurn is checkOwnerTurn(rootOnly) plus the sub-turn test it lacks for an
// internal turn: a sub-turn started from the owner's console is a model's delegation,
// not the owner. Memory is written directly only here.
func isOwnerRootTurn(ctx context.Context, cfg *config.Config) bool {
	if IsSubTurn(ctx) {
		return false
	}
	_, v := checkOwnerTurn(ctx, cfg, true)
	return v == ownerYes
}
