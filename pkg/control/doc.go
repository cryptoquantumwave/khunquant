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
//	/useful, /notuseful           as a reply to an alert: recorded in pkg/feedback with
//	                                proactive.learning on, else only logged
//	[Mute] button                 mutes exactly the dedup keys of the alert it is on (30 days)
//	/whoami                       the IDs proactive.owner needs; any allow-listed sender
//	/stop, /resume                /stop: the kill flag and everything in "/stop" below;
//	                                /resume clears the flag and restores nothing
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
//
// # Feedback
//
// With Deps.Feedback set (proactive.learning on) the owner's responses to a proactive
// message are recorded in pkg/feedback: the buttons and the typed /useful, /notuseful,
// /snooze and /mute (inside their handlers, after the owner check), a thumbs reaction
// (ObserveReaction) and a reply that is not a command (ObserveReply, which the channel
// calls beside publishing the message to the agent, and which classifies the text and
// drops it). All of them go through the same owner rule as the commands (standing), so
// the authenticated web chat counts and records with source "web:...", and nobody else's
// response is recorded. Nothing here changes what a command does or what the agent gets.
//
// # /stop
//
// Kill.Stop is the whole of /stop, also behind POST /api/control/kill. In this
// order, each step bounded and none stopped by another's failure: (1) engage the
// flag; (2) cancel the running model turns; (3) cancel the non-protective workflows
// at their next step boundary; (4) revoke every mandate and cancel every pending
// proposal, editing the cards; (5) withdraw the open orders the agent placed in the
// last 7 days; (6) reply with a summary built by code. What it leaves running, and
// why that is safe:
//
//   - protective workflows (journal.Definition.Protective, and an approved exit,
//     which approval.Service.FlowOrders marks): they only reduce risk;
//   - a workflow whose place step has finished, and any dn.open once a place step has
//     begun, is left to settle: cancelling it would skip the steps that record the
//     order, tell the owner or start the recovery of a filled leg. Every place step
//     reads the kill flag on every attempt, so it places nothing more;
//   - nothing scheduled keeps buying: execute_dca_order reads the same flag (armed by
//     the gateway through tools.SetKillFlag) and refuses in a turn the assistant
//     started itself (tools.IsAutonomousOrigin, a legacy "dca:" cron job above all);
//     the owner's own turn is never held back by the switch;
//   - a reduce-only exit the agent placed is not withdrawn (it is getting the owner
//     out), and the owner's own orders are never looked at: only orders a journal
//     step names are.
//
// An order a workflow was cut on, inside its place step, is looked up read-only
// (broker.FindOrder: its client order ID where the adapter was established to take
// one, else its shape) and withdrawn if found open; one that cannot be found or
// looked up is named "may exist: check the exchange". The reply is sent within
// DefaultReplyBudget; steps still running go on in the background and their outcome
// is sent as a second message. A second /stop while one runs answers "already
// running"; a /stop while already stopped runs again and may find new things.
package control
