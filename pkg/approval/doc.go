// Package approval turns a suggestion into an order only through the owner.
//
// The property it exists for: no order is placed from a proposal unless the
// owner, and only the owner, approved exactly that order, before it expired, and
// every gate still holds at the moment of placing. The model may suggest; code
// builds what the owner sees and what is executed, from the same stored
// parameters.
//
// # Lifecycle
//
//	Propose  -> pending   a canonical OrderParams, its hash, a code and a nonce are
//	                      stored; the card is rendered by code from those params and
//	                      delivered to the owner
//	Decide   -> approved | rejected   one conditional UPDATE, owner only, before expiry
//	         -> executing the journal workflow "approval.order" runs
//	         -> executed | failed
//	pending past ExpiresAt -> expired (the sweeper, every 30 s; fails closed)
//
// # Workflow approval.order
//
//	recheck   reload the row, verify the hash, refuse an approval older than the
//	          proposal's own window, run the order gates again, fetch a fresh ticker
//	throttle  spend the provider's rate-limiter token (retried, bounded)
//	place     broker.PlaceOrderIdempotent, once, with the workflow's idempotency key
//	verify    fetch the order, bounded poll
//	record    the perception order feed and the daily-loss tracker
//	receipt   the final status and a templated message to the owner
//
// The workflow id is derived from the proposal id and started with
// journal.StartWithID, so starting it twice is a no-op. Approving and starting
// are two components, not one transaction: at gateway start and on every sweep
// the service starts every approved proposal that has no workflow, and settles
// every executing one whose workflow has ended.
//
// # Cards are a second delivery path
//
// A card is delivered through notify.Service.Deliver directly, NOT through the
// attention outbox that Phase 2 made the only path for what the assistant says on
// its own. It bypasses grouping, digests, quiet hours and the daily budget on
// purpose: a card held for a digest would expire unseen. The only proposals that
// can be created by a background turn come from a candidate attention has already
// decided to deliver now, so quiet hours were respected upstream.
//
// # What is stored is what is executed
//
// The params are hashed as canonical JSON together with the kind; the executor
// re-reads the row, decodes strictly, re-encodes and compares with the stored
// hash. No statement in this package updates the params, amount, price or hash of
// a proposal after it is inserted.
//
// The package does not know exchanges. The gates, the pricing and the placing are
// an OrderBackend (implemented in pkg/tools, which owns the order helpers).
package approval
