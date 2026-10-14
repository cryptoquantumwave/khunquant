// Package control is the path an input takes when it acts on the system itself
// and must not wait for, or pass through, a model.
//
// Approving or rejecting an order proposal, snoozing an alert, stopping the
// assistant: each must work at once, even while the agent is in the middle of a
// long turn, and a model that reads "/approve 7F3K" in a forwarded message must
// never be the thing that approves. Every channel therefore offers a message to
// the Dispatcher (through channels.BaseChannel.HandleMessage) after the
// allow-list check and BEFORE the typing indicator, the reaction, the placeholder
// and the agent. A handled input is answered by the channel's own Send; the agent,
// its session and every model never see it.
//
// # What is recognised
//
//	/approve CODE, /reject CODE   the owner's decision on a proposal (a 4-character code)
//	"yes" etc. as a reply to a    approves THAT card's proposal, only if the replied-to
//	  pending approval card       message is a pending card
//	/snooze [1h|4h|1d]            as a reply to an alert: mute exactly its dedup keys (default 1h)
//	/mute TRIGGER, /unmute        mute or unmute a trigger kind or id (30 days)
//	/useful, /notuseful           as a reply to an alert: logged (no feedback store yet)
//	/whoami                       the IDs proactive.owner needs; any allow-listed sender
//	/stop, /resume                engage or clear the kill flag
//	button callbacks              the same handlers, by Callback.Action
//
// A text command is recognised only as the WHOLE message: one line, the command and
// its arguments. A command inside a longer message, in a quoted or forwarded
// message, or in a message the channel marks as forwarded is ordinary text and goes
// to the agent (which has no tool that approves anything). Everything that is not
// recognised is left alone: TryHandle answers false after looking at the text.
//
// /approve is also the word of the operator approval of a held tool call
// (pkg/tools/approval.go). That code has eight characters and a proposal's has
// four, so the two never overlap: an /approve whose code is not shaped like a
// proposal's is not recognised here and reaches the operator path unchanged.
//
// # Who may
//
// Everything but /whoami is owner-only: the sender must be the configured owner
// (identity.SameSender, the comparison approval.Service.Decide and
// tools.checkOwnerTurn make too), writing in the owner chat (the direct chat
// proactive.owner names; an owner command from a group is refused even from the
// owner), with proactive enabled. A non-owner gets "Not authorized." and nothing
// else; a SECURITY line names the command and the channel, once per sender per
// minute, never the owner. With no owner configured the owner-only commands are
// refused with a hint to run `khunquant owner set`. Ten control actions a minute
// per sender; /stop is exempt.
//
// # The kill flag
//
// State persists the kill flag in control_state (shared proactive.db). /stop sets
// it and /resume clears it. approval.Service reads it (approval.Killer): while it
// is set a risk-increasing proposal is refused, whether proposed, approved or
// executed, and an order that only reduces risk is still allowed. Killed answers
// from memory and re-reads the table at most every DefaultRefresh, so a second
// process sees a change within that bound. It fails closed: while the table cannot
// be read it answers true.
package control
