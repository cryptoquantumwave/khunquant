package config

import (
	"strings"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

// The web launcher chat as a second owner route.
//
// With proactive.owner set, the configured route (a Telegram DM, say) is the owner.
// The owner's own messages typed into the web launcher's dashboard chat (the pico
// channel) arrive as "pico:pico-user", which is not that route, so without this they
// were "not the owner". With proactive.owner.web_is_owner on (the default) the
// dashboard chat counts as the owner as well, but only while ALL of these hold, each
// checked when it is asked and none cached from startup:
//
//	(a) an owner is configured and valid (OwnerRoute);
//	(b) the message arrived on a pico connection authenticated with the pico token in
//	    a header or subprotocol, never a URL query (bus.SenderInfo.WebAuthenticated,
//	    set by the pico channel alone);
//	(c) the launcher that proxies the token requires its dashboard authentication:
//	    a dashboard password is set, or the launcher and the gateway's pico endpoint
//	    both listen on loopback only. The gateway arms the check (SetWebLauncherGate);
//	    until it does, and whenever it cannot tell, the answer is no.
//
// Everything that asks "is this the owner" goes through here, so every consumer
// (tools, control path, approvals, memory, policy) gets the same answer.

// WebChannel is the channel name of the web launcher chat.
const WebChannel = "pico"

// WebLauncherGate answers condition (c) for a gateway bound to gatewayHost. When it
// is not met, missing names what is absent (it goes to the one WARN at start).
type WebLauncherGate func(gatewayHost string) (ok bool, missing string)

var webLauncherGate atomic.Pointer[WebLauncherGate]

// SetWebLauncherGate arms condition (c); nil disarms it. Package state, but it holds
// no configuration (the gate reads the launcher's own file when asked), so a config
// reload needs no re-arming.
func SetWebLauncherGate(g WebLauncherGate) {
	if g == nil {
		webLauncherGate.Store(nil)
		return
	}
	webLauncherGate.Store(&g)
}

// WebIsOwnerEnabled is proactive.owner.web_is_owner, true when unset.
func (o OwnerConfig) WebIsOwnerEnabled() bool { return o.WebIsOwner == nil || *o.WebIsOwner }

// WebOwnerStatus reports whether the web chat can be the owner under this
// configuration (conditions (a) and (c); (b) is a fact about each message). When
// active is false, missing says why, or is "" when the switch is simply off.
func (c *Config) WebOwnerStatus() (active bool, missing string) {
	if c == nil {
		return false, "no configuration"
	}
	if _, ok := c.Proactive.OwnerRoute(); !ok {
		return false, "no valid proactive.owner"
	}
	if !c.Proactive.Owner.WebIsOwnerEnabled() {
		return false, ""
	}
	g := webLauncherGate.Load()
	if g == nil {
		return false, "the launcher's authentication could not be established"
	}
	ok, missing := (*g)(strings.TrimSpace(c.Gateway.Host))
	return ok, missing
}

// WebOwnerActive is WebOwnerStatus without the reason.
func (c *Config) WebOwnerActive() bool {
	active, _ := c.WebOwnerStatus()
	return active
}

// IsWebOwnerTurn reports whether a message on channel, from a connection the channel
// marked authenticated, is the owner by the web route. A caller still tries the
// configured route first: this is the second route.
func (c *Config) IsWebOwnerTurn(channel string, authenticated bool) bool {
	return authenticated && channel == WebChannel && c.WebOwnerActive()
}

// OwnerDecider returns the identity a stored decision must carry to count as the
// owner's now: the configured owner's sender, or identity.WebOwner when the
// decision was recorded as the web's and the web route is still active. An
// approved proposal is re-proven against it before anything acts on it, so turning
// the web route off between the approval and the action stops the action.
func (c *Config) OwnerDecider(decidedBy string) (string, bool) {
	if c == nil {
		return "", false
	}
	route, ok := c.Proactive.OwnerRoute()
	if !ok {
		return "", false
	}
	if decidedBy == identity.WebOwner && c.WebOwnerActive() {
		return identity.WebOwner, true
	}
	return route.SenderID, true
}
