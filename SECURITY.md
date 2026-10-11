# Security Posture

This document describes khunquant's security architecture and known limitations. Our goal is transparency: we aim to help operators understand what risks are or are not mitigated by the current design.

## Subprocess Execution — No OS-Level Isolation

### The Isolation Gap

Upstream picoclaw implements OS-level process confinement for child processes via `pkg/isolation`:
- **Linux**: `bwrap` (Bubblewrap) creates a confined namespace with filesystem restrictions, capability dropping, resource limits, and network isolation.
- **Windows**: Restricted tokens, low-integrity level, and Job Objects limit what a child process can access or do.

**khunquant does not have OS-level isolation, and never did.** Our fork point (2026-03-15) predates upstream's addition of `pkg/isolation` (2026-04-08, commit `51eecde0`). Adopting it would require:
- 27 `exec.Command` call sites across 17 non-test files to route through an isolation layer.
- Linux deployment to require `bwrap` on the PATH — a poor fit for the $10-device / Raspberry Pi Zero target.
- A macOS stub (`platform_other.go`) providing no isolation, leaving that platform entirely unprotected.

**If subprocess confinement is operationally required, this fork is not suitable.**

### Our Defense: Regex-Based Deny-List

The sole defence against subprocess risks is a regex deny-list defined in `pkg/tools/shell.go` (`defaultDenyPatterns` and platform-specific `windowsDenyPatterns`). The guard function `guardCommand` validates user-supplied commands before execution.

This layer **structurally cannot provide**:
- **Filesystem confinement**: The regex inspects the command string only, and does not forbid writes to files outside the workspace. It is *not* purely textual any more — when `restrict_to_workspace` is set, `checkWorkspaceViolation` resolves symlinks, blocks `..` traversal, and expands `~` and `$HOME` against the child's own environment (`pkg/tools/shell_env.go`), with a restricted child's `HOME` set to the workspace so the guard and the shell agree on what `~` means. That closes the common escapes, not the category: this is still string analysis of one command, not a kernel-enforced boundary.
- **Resource limits**: No CPU time, memory, or I/O quotas. A command can exhaust system resources.
- **Capability dropping**: Process retains all capabilities of its parent. It can bind to privileged ports, modify kernel state, or load kernel modules if the parent can.
- **PID/namespace isolation**: The child process is visible in the same PID namespace; it can signal or inspect sibling processes.
- **Network-egress restriction**: There is no firewall or proxy; the child process can connect to any network address the parent can reach.

### What the Regex Layer Blocks

The deny-list catches common attack patterns in the command string:
- Recursive deletion (`rm -rf`, `del /f`), disk formatting (`format`, `mkfs`, `dd if=`), and writes to block devices.
- Shell metacharacters that would spawn sub-shells (`$()`, `` ` ``, `|`, heredocs).
- PowerShell encoding schemes (`-EncodedCommand`, `[Text.Encoding]`, `.GetString`, `FromBase64String`).
- Privilege escalation (`sudo`, `chmod`, `chown`, `kill`).
- Package manager operations (`apt`, `yum`, `npm -g`, `pip --user`), container ops, and git push.

**The regex layer can be bypassed** by:
- Obfuscating commands in files written by prior steps. (Environment variables are a weaker vector than they were: exec children get an allowlisted environment rather than the gateway's — so no API keys — and the workspace guard expands `~`/`$HOME` and unset variables the way the shell will.)
- Using lesser-known commands not on the deny-list (e.g., `shred`, `srm`, `dd bs=` with a different argument order).
- Running code through an interpreter: `python -c`, `perl -e`, `node -e`, and equivalents are **not** blocked, and deliberately so — they are ordinary development commands, and denying them would break far more legitimate work than it would prevent. An agent that can run an interpreter can do anything that interpreter can, which is the reason this layer is described as a deny-list and not a sandbox.
- Exploiting parser bugs in the regex engine or shell itself (e.g., Unicode normalization, locale-specific character classes).

## Launcher Dashboard — Network and Application-Level Access Control

### Access Control Model

The web launcher (`web/backend`) implements multi-layer access control:

1. **IP allowlist** (optional, network-level): A CIDR allowlist in `launcher-config.json` (field `allowed_cidrs`). Requests from IPs outside this range are rejected at the network level.
2. **Trusted-proxy X-Forwarded-For parsing**: If the launcher is behind a reverse proxy (e.g., nginx), it can extract the client IP from the `X-Forwarded-For` header, but only when the proxy's IP is in `trusted_proxy_cidrs`. That list decides who may claim to be any client address, so it is guarded: a `/0` entry (`0.0.0.0/0`, `::/0`) fails validation at startup, and the dashboard API accepts the field only unchanged — edit `launcher-config.json` to change it.
3. **Password authentication** (optional, application-level): A bcrypt-hashed password stored in `launcher-config.json` (field `dashboard_password_hash`). Verified via HTTP Basic authentication, a `POST /api/auth/login` session cookie, or the launcher token — see "Password Authentication Semantics" below. This layer is **independent of** the IP allowlist—both must pass (if configured).
4. **CSRF protection on state-changing endpoints** (application-level): Every mutating `/api/` route verifies that the request originates from the launcher's own origin, not from an attacker's page. The check uses the `Sec-Fetch-Site` header when present (modern browsers), falls back to comparing parsed `Origin` and then `Referer` headers against the request's own scheme and host, and rejects requests that cannot be identified.

### Password Authentication Semantics

- **Password is opt-in**: No password configured (empty `dashboard_password_hash`) means password authentication is disabled entirely. The dashboard behaves exactly as before (IP allowlist only, plus the loopback/launcher-token session rules described below). **This is deliberate backward compatibility.** Operators must actively set a password to enable this protection.
- **Setting the password**: `khunquant launcher set-password` prompts twice without echo, hashes with bcrypt (cost 12), and writes `dashboard_password_hash` to `launcher-config.json`; `--clear` removes it. The command refuses a non-TTY stdin, so a password cannot be piped in from a shell history or CI log. It derives the config path exactly as the launcher does, because writing the hash anywhere else would fail silently and look identical to the gate not working. **It cannot revoke live sessions** — those live in memory inside the running launcher process — so restart the launcher after setting a password. Until this command existed, nothing in the shipped binaries could write the hash, which meant no deployment could reach the gate described in this section.
- **Startup warning when exposed**: if no password is configured and the launcher is either listening on `0.0.0.0` (`-public`) or trusting proxy headers (`trusted_proxy_cidrs` non-empty), it logs a warning at startup naming the exposure and the command above. It warns rather than refusing, so existing deployments keep working.
- **Middleware order enforces the gate**: the launcher's HTTP handler wraps, outer to inner, `IPAllowlist → PasswordAuth → SessionAuth → mux`. `PasswordAuth` therefore always runs, and rejects, before `SessionAuth` gets a chance to do anything — this is what makes the next bullet true rather than aspirational.
- **Loopback bypass applies to IP allowlist only**: If `allow_localhost_bypass` is enabled, loopback addresses bypass the CIDR check. A configured password is still required from loopback callers: `SessionAuth`'s auto-issue-a-session-for-loopback-callers rule is disabled outright whenever a password is configured, so a loopback caller with no session, no Basic credentials, and no launcher token gets a 401 with **no** `Set-Cookie` header — the same as any other caller. The two checks (IP allowlist and password) remain independent of each other.
- **Credentials accepted**: on each request, in order — (1) a valid `kq_session` cookie from a prior login, (2) HTTP Basic authentication checked against the bcrypt hash, or (3) the launcher token (`Authorization: Bearer`, `X-Launcher-Token`, or `launcher_token` query parameter) — the launcher token is treated as an alternative launcher credential, since it is at least as privileged a secret as the dashboard password.
- **Sessions are opt-in, not automatic**: `POST /api/auth/login` exchanges the dashboard password for a session cookie; `POST /api/auth/logout` revokes it. Basic-authenticated and launcher-token-authenticated requests are **not** automatically upgraded into a session — each such request is checked again on its own. In particular, presenting the launcher token authorizes that one request but does not mint a persistent session; only a successful password login does.
- **Health checks exempt**: `/api/health` and `/api/ready` skip password authentication (and session/loopback checks) to allow external health monitors to function.
- **OAuth callback exempt**: `GET /oauth/callback` (exactly — not a prefix, not `POST`) skips password authentication. The provider redirects the browser there cross-site, and the session cookie is `SameSite=Strict`, so it is not sent; before the exemption every provider login stalled on a Basic prompt once a password was set. The handler authenticates the request itself: it requires the unguessable, single-use `state` of a flow an authenticated user started.
- **Failed attempts lock an address out**: five wrong passwords from one address within 15 minutes — through the login form or Basic auth, which share one budget — earn `429` with `Retry-After` until the window passes. The check runs before bcrypt, and a request that offers no password does not count. It keys on the TCP peer, never `X-Forwarded-For` (which the client writes), so behind a reverse proxy all clients share the proxy's budget: a lockout there applies to everyone, the conservative failure for a password check.
- **Session cookies**: `HttpOnly`, `SameSite=Strict`, expire with the 12-hour server-side session, and are `Secure` when the launcher serves TLS **or** `behind_tls_proxy: true` is set in `launcher-config.json` — needed behind a TLS-terminating proxy, where the launcher itself sees plain HTTP. The setting is explicit rather than inferred from `X-Forwarded-Proto`, which the client can write.

### Fail-Closed Guard

The launcher refuses to start in public mode (listening on `0.0.0.0`) without an explicit IP allowlist. The guard is in `web/backend/main.go` (search for "Refusing to start in public mode"):

```go
// Refuse to start in public mode without an explicit CIDR allowlist — the
// IP allowlist is the only network-level boundary for LAN access.
if effectivePublic && len(launcherCfg.AllowedCIDRs) == 0 {
	log.Fatalf(
		"Refusing to start in public mode without an IP allowlist.\n" +
			"Add allowed_cidrs to launcher-config.json (e.g. \"192.168.1.0/24\") or\n" +
			"remove the -public flag to restrict access to localhost only.",
	)
}
```

If no allowlist is provided, the process exits with an error.

**Note**: The IP allowlist guard applies regardless of whether a password is configured. Password authentication is application-level and does not replace network-level access control.

### CSRF Protection Semantics

Every state-changing request — `POST`, `PUT`, `PATCH` or `DELETE` to any `/api/` route — is protected against Cross-Site Request Forgery (CSRF) via origin verification. The check is applied centrally by the `CSRFMutations` middleware (`web/backend/middleware/csrf.go`), which sits inside the auth chain, so a route is covered by existing rather than by each handler remembering to ask.

Handlers that were individually protected before the check was centralised still call it inline as well. That redundancy is deliberate: a future refactor could reorder or drop the middleware, and those routes would remain protected.

The mechanism works as follows:

- **Sec-Fetch-Site header** (modern browsers): Checked first. Values `cross-site` and `same-site` are rejected immediately; `same-origin` is accepted immediately. The value `none` (top-level navigation) and absent headers fall through to `Origin` header checks.
- **Origin header** (older browsers): If present and non-empty, parsed as a URL and compared against the request's own scheme and host. Must match exactly. The special value `"null"` (used in sandboxed contexts) is rejected as unidentified. Malformed URLs or headers containing whitespace are rejected as well.
- **Referer header** (optional, very old browsers): If `Origin` is absent or empty, the `Referer` header (if present) is parsed and compared the same way.
- **Unidentified callers are rejected**: If none of the above headers identify the caller, the request is rejected with HTTP 403 Forbidden and the state-changing operation **does not proceed**. This is stricter than some upstream implementations and reflects a deliberate choice: a launcher setup request should come from the launcher's own UI, which will provide these headers. **A plain `curl` POST to `/api/pico/setup` without headers will return 403.**

**Why the IP allowlist does not substitute for CSRF protection**: A CSRF attack originates from the victim's own browser, on the same IP address (loopback) that the allowlist permits. The attack works *because* the browser is trusted at the IP level. CSRF defences exist precisely to distinguish between the user's intentional requests and an attacker's forged requests, both arriving from the same source IP.

### Known Limitations

- **Spoofed or missing X-Forwarded-For**: If the reverse proxy is misconfigured or omits the `X-Forwarded-For` header, the launcher may fall back to the proxy's own IP, allowing unintended clients to bypass the IP allowlist.
- **Dashboard sessions can be revoked**: `POST /api/auth/login` exchanges the dashboard password for a session; `POST /api/auth/logout` revokes it. Session IDs are 32 bytes of `crypto/rand`, held server-side, and independent of the launcher secret.

  This replaces a scheme in which the `kq_session` cookie's value **was** the launcher secret. Under that design a captured cookie disclosed the secret itself, and every session was byte-identical — so there was nothing to revoke short of rotating the secret for everyone. An existing test asserted that equality, which is how it stayed in place.

  **Limitations:** sessions live in memory only, so a launcher restart logs everyone out — a deliberate trade for a single local process, not persistence we intended and lost. Sessions expire after 12 hours. HTTP Basic still works as an alternative credential for non-browser clients, so this adds a revocable path rather than removing the unrevocable one; a leaked *password* still requires a hash rotation, and `SessionStore.DestroyAll` exists for that.

- **No per-user authentication**: Authentication is single-password (not per-user). Any client who provides the correct password can perform any operation (modify config, restart agents, etc.).
- **HTTPS not enforced**: The launcher does not require TLS. If a password is configured, credentials are transmitted via HTTP Basic auth in the request header. Over plain HTTP, these credentials can be intercepted. TLS is strongly recommended when exposing the launcher over a network.
- **No audit log**: Access is not logged by user or timestamp (though the launcher emits structured request logs). There is no record of who accessed the dashboard or what changes were made.
- **Password hash in config file**: The bcrypt-hashed password is stored in the plaintext `launcher-config.json` file on disk. File permissions are set to 0o600 (read/write for owner only), but the hash could be extracted and cracked if the file is compromised.
- **CSRF protection** covers every mutating `/api/` route; see "CSRF Protection Coverage & Special Cases" below. (An older list of 13 protected setup operations used to appear here; it described the pre-`CSRFMutations` state and is superseded.)
- **Pico `allow_origins` is empty by default, and that is not "allow anything"**: Setup no longer records the Origin of whichever request happened to trigger it. That pin locked the channel to a single host — a launcher configured from `localhost` then rejected the same user reaching it over the LAN or on another port. With the list empty the channel falls back to a same-origin check (`isSameOriginRequest`, `pkg/channels/pico/pico.go`), which is what actually protects a browser; the pin added nothing on top of it. An operator who wants specific origins can still set `allow_origins` explicitly, and a wildcard `*` is never written automatically.

- **Pico WebSocket token is never sent to the browser**: The Pico channel authenticates its WebSocket handshake with a `token.<value>` subprotocol. The launcher proxies `/pico/ws` and attaches that token server-side from config, so no API response carries it and no browser holds it. `GET /api/pico/info` returns only `ws_url` and `enabled`; `POST /api/pico/token` rotates the token and returns the same token-free payload. The proxy also discards any subprotocol the caller supplied, so a client cannot present a token of its own choosing to the gateway, and strips the gateway's echoed subprotocol from the handshake response.

  **Limitation:** this confines the token to the server; it is not itself an authentication boundary. `/pico/ws` is gated by the launcher session/password middleware (`apiRequiresAuth` in `web/backend/middleware/auth.go`), and that gate — not token confinement — is what stops an unauthenticated client from reaching the gateway. Anyone who can read `config.json` still has the token.
- **Credentials are withheld from `GET /api/config`**: Most credentials are `config.SecureString`, which withholds its own value on marshal and is restored from `.security.yml` on save. Three subtrees cannot use it — `Providers`, `Tools`, and `Debug` are tagged `yaml:"-"`, so a `SecureString` there would marshal as withheld and have nothing to be restored from, blanking the credential on the next save. Those fields are plain strings and are redacted at the API boundary instead: the response tree is walked and every credential-named key with a non-empty value is replaced with `[NOT_HERE]`. `PUT`/`PATCH /api/config` restore the real values before decoding, so a dashboard round-trip cannot overwrite a credential with the placeholder.

  Redaction is keyed on field name (`api_key`, `token`, `secret`, …), not on a list of known config paths, so a newly added provider or search backend is withheld the moment it is added rather than when someone remembers to update a list. Empty values are left as-is so "not configured" stays distinguishable from "configured but withheld".

  **Reachability:** `GET /api/config` sits behind the launcher session/password middleware and the IP allowlist, so this was never remote-unauthenticated. The exposure was that any dashboard-authenticated session — and anything that records its responses, such as browser devtools, extensions, or a HAR export — could read provider and tool API keys in full. Anyone who can read `config.json` on disk still has them.

- **Exec sessions are unreachable from the model**: the `exec` schema sets `additionalProperties: false` and names only `command` and `working_dir`, so registry validation rejects `action`, `background`, `pty` and every session argument before `Execute` runs; the only direct caller (cron) passes `action: run`. That unreachability is the control — sessions have an idle reaper but no hard lifetime or count cap — and `TestExecSessionArgumentsAreUnreachableThroughTheRegistry` pins it. Exposing sessions to the model must add those caps in the same change. The rest of this bullet describes the guard that applies if they are reached.

- **Exec sessions guard their input, not just their first command**: `exec` supports background and PTY sessions (`action: run` with `background`/`pty`, then `poll`/`read`/`write`/`send-keys`/`kill`). A session is a running shell, so anything written into it afterwards is executed too. `guardSessionInput` therefore applies the exec deny patterns to `write` data and to the literal (non-control-key) portion of `send-keys`.

  **This is a deliberate divergence from upstream picoclaw**, where only the starting command reaches the guard. Under upstream's design, starting a session with a permitted interpreter (`sh`, `bash`, `python`) and then writing into it bypasses every deny pattern — the PowerShell encoding guards, the `rm -rf` forms, and the deny-patterns-always-apply fix — all of which would then apply only to the first line.

  **Limitation:** the session guard checks deny patterns only, not the allowlist. Session input is a fragment (a bare `y`, a filename, a continuation line), and matching fragments against whole-command allow patterns would reject ordinary interactive use without adding safety. A command assembled across several `write` calls, each individually innocuous, is not detected — the guard inspects each write in isolation, not the reconstructed line.

- **web_fetch identifies itself when challenged**: outbound web requests present a browser User-Agent by default. When a WAF answers with `403` plus `Cf-Mitigated: challenge`, the request is retried **once** with `khunquant/<version> (+<repo>; AI assistant bot)` — a truthful identifier with a contact URL, which some operators allow-list. A challenge is a request to say who is calling, and the response is to answer it rather than to escalate the disguise.

  Deliberately narrow: only that exact status-and-header pair retries, so an ordinary `403` costs one request rather than two. The browser default itself is unchanged and remains a pre-existing posture choice, not something this behaviour introduces.

- **Non-browser clients can spoof CSRF headers**: A client that is not a web browser (e.g., a custom HTTP client or an attacker's tool) can arbitrarily set `Sec-Fetch-Site`, `Origin`, and `Referer` headers. CSRF protection assumes these headers are set by the browser and cannot be forged; this assumption breaks for non-browser clients. CSRF protection is part of the application-level defense but is **not a substitute for IP allowlist and password authentication**, which govern non-browser access.

### CSRF Protection Coverage & Special Cases

**Endpoints deliberately NOT protected (with reasons):**

- **`GET /oauth/callback`**: Top-level cross-site navigation from OAuth provider (Google, Anthropic). Naturally carries `Sec-Fetch-Site: cross-site`. Defense is state validation (checking `state` parameter against stored flows), not CSRF headers. This is correct and intentional.
- **Read-only endpoints (GET, HEAD)**: Do not mutate state. CSRF protection adds failure modes with no security benefit.
- **Test-only endpoints without persistence**: `POST /api/config/test-command-patterns` validates patterns and changes nothing. It is nonetheless covered by `CSRFMutations`, like every other mutating `/api/` route — a uniform rule is easier to reason about, and to keep correct as routes are added, than an exemption list, and protecting this one costs nothing. (`POST /api/models/fetch` was once listed here and is not test-only at all: it persists fetched results to `model_catalogs.json` — see `handleFetchModels`'s SSRF/CSRF policy comment in `web/backend/api/models.go`.)

**Coverage is now universal.** Earlier releases protected only the launcher setup endpoints, leaving agent configuration and memory writes, delta-neutral position management, cron scheduling, device pairing, the sandbox, system autostart and broker reconnection reachable from any page the operator merely visited — the session cookie rides along automatically, so a forged request from an attacker's page was indistinguishable from a real one at the network layer. `CSRFMutations` closes that gap by covering every mutating `/api/` route by default; a newly added route is protected without its author doing anything.

**Calling the API from a script**: because unidentified callers are rejected, a non-browser client must positively identify itself. Sending `Sec-Fetch-Site: none` does **not** work — that value falls through to the `Origin`/`Referer` branch, and a request with neither header is refused. Send an `Origin` matching the launcher's own scheme and host instead:

```bash
curl -X POST http://127.0.0.1:18800/api/gateway/restart \
  -H "Origin: http://127.0.0.1:18800" \
  -H "Authorization: Bearer $LAUNCHER_TOKEN"
```

No exemption exists for token-authenticated callers. Nothing shipped sends a token header to these routes — the dashboard authenticates by cookie and relies on the browser to supply `Sec-Fetch-Site` — so an exemption would be attack surface with no consumer.

**Note the opposite polarity of the gateway management API** (`/api/cron/*` on the gateway's own port, see "Gateway Management API" below): that guard *rejects* requests carrying browser markers, because its only legitimate caller is the launcher backend making a server-to-server call. The launcher's CSRF check demands proof of a same-origin browser; the gateway's demands the absence of a browser. Each matches its own caller, and a request forwarded from one to the other satisfies both: `proxyToGateway` forwards only `Content-Type` and `Authorization`, so the browser's origin headers do not reach the gateway.

## Gateway Management API

The gateway exposes a small management API (`/api/cron/*`) on its own port (default `127.0.0.1:18790`) for the launcher to manage scheduled jobs. Three guards apply to every route, installed by wrapping the mux so a later route cannot skip them:

- **Bearer token**: `gateway.management_token` is minted and persisted before any route registers (the ordering matters — the bearer middleware passes everything when the token is empty). The launcher forwards it as `Authorization`. It is a plain string rather than a `SecureString` because the `gateway` subtree is `yaml:"-"`; it is withheld from API responses and logs by name.
- **Loopback only**: requests must come from a loopback peer.
- **No browsers**: a request carrying `Origin` or a `Sec-Fetch-Site` other than `none` is rejected — the only legitimate caller is a server-to-server call from the launcher.

The launcher no longer forces the gateway onto `0.0.0.0` in `-public` mode. To expose the gateway (for example for webhooks), set `gateway.host` / `KHUNQUANT_GATEWAY_HOST` explicitly.

## Trading Safety

The agent can place real orders on the exchanges and brokers it is configured for. The LLM is treated as potentially attacker-controlled (prompt injection through fetched pages, messages, tool output), so the controls below do not rely on the model behaving.

### Per-tool policy

`pkg/tools/policy.go` holds a policy table keyed by tool-name constants, enforced inside `ToolRegistry.ExecuteWithContext` — the single funnel for every model-driven tool call — after argument validation. A tool gets its guarantees from the table, not from remembering to call a check. Refusals are shown to the operator as well as to the model.

- **Origins.** Each turn is tagged with its origin where it starts: `internal` (CLI), `remote` (a chat channel), `cron` (a scheduled job, tagged at its own call site) or `heartbeat` (tagged in `ProcessHeartbeat`; it borrows the user's last active channel, so without its own tag it would pass as `remote`). `cron` and `heartbeat` are the **autonomous** origins: the assistant started the turn by itself. Sub-turns (`spawn`, `subagent`, `delegate`) inherit their parent's tag, and so does the follow-up turn started by a `spawn` result: the origin (and the job scope below) travel on the `system` inbound message and are re-applied before the follow-up runs. They are honoured only on the `system` channel — which no channel adapter can publish on, since each stamps its own fixed name — and only if autonomous, so the field can restrict a turn but never grant anything.
- **Class ceiling for autonomous turns.** Every tool has a class in `pkg/tools/classification.go` (`ClassOf`): read, notify, local state, automation, propose, trade, funds, system. An autonomous turn may call **read, notify and local-state tools only** — market and portfolio reads, web search/fetch, `message`/`send_file`, workspace file writes and snapshots. Everything else is refused with a result naming the origin, and is hidden from the model's tool definitions by the same predicate (`turnMayCall`, shared by `checkToolPolicy` and `ToolRegistry.ToProviderDefsFor`), so the model is never shown a tool it cannot call. A tool without a class — including every MCP tool — is denied. Two exceptions, each bound to its job: a heartbeat may `spawn` (the child inherits the ceiling), and a scheduled `dca:` job may call `execute_dca_order` for **its own plan only** (the DCA handler puts the plan ID in the turn's automation scope; the tool refuses any other `plan_id`). A `dn:` monitor escalation gets no trade tools. New tools must be classified: `TestClassOf_AllRegisteredToolsClassified` fails otherwise. Like the rows below, the ceiling is inert until `SetPolicyConfig` arms it (`NewAgentLoop` and every reload do), and an untagged turn is not treated as autonomous — every production turn is tagged before any tool runs.
- **Exits are not available to autonomous turns — a deliberate change.** The ceiling covers `cancel_order`, `futures_close_position`, `futures_reduce_position`, `futures_cancel_orders`, `futures_emergency_flatten`, `emergency_stop` and `unwind_delta_neutral_position` as well. The consequence, stated plainly: at 3 am a margin alert reaches you (the alert and the `message` tool still work), but the assistant cannot unwind a position by itself until an approval flow exists. Turns you start are unaffected.
- **Autonomous file writes are fenced.** `write_file`, `edit_file` and `append_file` are local-state tools and stay available to autonomous turns, but several things the ceiling denies are driven by plain workspace files, so in an autonomous turn the target must resolve **inside the workspace** (`restrict_to_workspace=false` and allow-paths do not widen this) and must not be, or be under, a protected path: `cron/` (the scheduler store: a job with a command or a `dca:` name is automation and shell access), `state/` and `state.json` (e.g. `daily_loss.json`, which the loss brake reads), `sessions/`, `skills/` (a file there is `install_skill` by another route), `memory/dca/`, `memory/delta_neutral/`, `memory/snapshots/` (the plan and snapshot stores), `sandbox/` (Webull session), `whatsapp/` (session credentials), `pairing/` (pending channel pairing requests: access-control state), and the prompt files `AGENTS.md`, `IDENTITY.md`, `SOUL.md`, `USER.md`. The list lives in `pkg/tools/autonomous_paths.go` (`autonomousProtectedPaths`, one reason per entry); a new state directory or prompt file belongs there. The path is cleaned, resolved through symlinks (via the deepest existing ancestor) and compared case-insensitively, so `CRON/jobs.json`, `a/../cron/jobs.json` and a symlink into `cron/` are all refused, and the tool writes exactly the path that was checked. Turns a person started are unaffected. These three are the only tools that write a file at a model-chosen path; the others either hold a class the ceiling denies (`exec`, `install_skill`, `config_encrypt_keys`, MCP) or write only through their own store (`take_snapshot`) or a generated temp file (charts, media).
  - **Accepted residual:** an autonomous turn can still write `memory/*.md` (including `memory/MEMORY.md`, the heartbeat's alert ledger) and `HEARTBEAT.md`, and later turns read them. Content a heartbeat fetched from the web can therefore influence later turns through those notes. Also not covered: an MCP server's `env_file`, if the config points it at a file inside the workspace (the name is operator-chosen, so it cannot be listed); keep such files outside the workspace. Both are left to the later trust-propagation phase.
- **Reserved job names.** The scheduler routes a job to a code handler by the prefix of its name (`dca:`, `dn:`, `price_alert:`, `indicator_alert:`) and the handler trusts the name. The `cron` tool therefore refuses to create or rename a job to a name with a reserved prefix, and refuses to edit a job that already has one; only the alert, DCA and delta-neutral tools mint them (`pkg/tools/reserved_jobs.go`; the CLI `khunquant cron add` is a person at a terminal and is not restricted). In addition the DCA handler runs a `dca:` job only if it is the cron job the plan itself recorded (`Plan.CronJobID`); a job that merely carries the name is not executed. A plan with no recorded job id is not executed either.
- **Per-tool rows (below) apply to turns a person started**, and to whatever the ceiling lets through. Financial tools (spot/option order placement, transfers, futures leverage/open/protection, delta-neutral open/resize, earn, DCA and delta-neutral plan writes) and `install_skill` also refuse `cron` by row, and a turn with **no** tag is refused by those rows: "unknown" must not be the one value that slips past the gate.
- **Risk-reducing tools are never gated for a person**: closing, reducing, cancelling, flattening, emergency stop and delta-neutral unwind carry no policy row, so an operator cutting losses is never told to wait. (They are not available to autonomous turns; see above.)
- **Paper mode** (`trading_risk.paper_trading_mode`). With it on, no code path opens or enlarges a REAL position or moves real funds; exits of real positions stay possible. Per tool:
  - *Simulated:* `create_order` (spot). It runs every gate it runs live - validation, trade permission, order type, the USD order-value cap, the daily-loss check, market status, balance preflight and the dry-run - and only then places the order with a simulation wrapped around the real provider (`broker.TradingProviderFor`). Prices, balances and market status are read from the real exchange; the fill is simulated, takes no rate-limit token, and the result is prefixed `[PAPER]` with an order ID starting `paper-`. `execute_dca_order` handles paper mode itself (see its own documentation). `paper_trade` is the explicit simulator and is unchanged.
  - *Refused* (no simulation exists): `option_create_order`, `transfer_funds`, `futures_set_leverage`, `futures_open_position`, `futures_modify_protection`, `open_delta_neutral_position`, `resize_delta_neutral_position` (including a resize-down; unwind is the exit) and `manage_earn_position`. The policy table refuses them at the registry (`RefuseUnderPaperMode`); `option_create_order` also refuses in the tool (`broker.ErrPaperModeUnsupported`). `update_delta_neutral_plan` refuses, in the tool, only the edit that sets leverage on an ACTIVE plan (it would change live leverage); every other plan edit works.
  - *Real, deliberately:* `futures_close_position`, `futures_reduce_position`, `futures_emergency_flatten`, `unwind_delta_neutral_position` and the cancel tools act on the REAL exchange even in paper mode, so a real position can always be closed (from a turn you started; autonomous turns cannot call them at all). Their results are prefixed `[REAL — closing a real position while paper mode is on]`.
  - Reads (tickers, balances, positions) always come from the real exchange. The flag is re-read on every config reload, including a dashboard change.
- **Scheduled jobs are unattended agent turns**: with `deliver` false (the default), even a plain `message` job runs through the agent; `type: directive` only adds a prompt prefix. Any channel can schedule one. The origin tag above is what keeps such a job from trading; requiring confirmation for every agent-turn job would break ordinary reminders and is not done.

### Operator approval (opt-in)

A tool listed as `tools.policies.<tool>.require_approval: true` does not run on the model's say-so. The first call is held and a one-time 8-character code is sent to the chat the call came from (the terminal's stderr on the CLI). Replying `/approve <code>` lets the **identical** call run once; `/deny <code>` discards it; codes expire after 5 minutes.

- The code never enters model context: it is delivered by a notifier, not in the tool result, and `/approve` is consumed as a command before any LLM turn.
- An approval is bound to the channel, chat **and sender** that made the request, and to a hash of the exact arguments (`confirm` excluded, so preview and execute match). Changing the amount needs a new approval.
- `/approve` from the `system` or `subagent` channels, from `async:*` senders (async tool results are re-published inbound with model-influenced content) or from the `cron` sender outside the CLI is refused before the store is consulted.
- Off by default. Risk-reducing tools cannot be flagged. Approvals are in memory, so a restart drops pending ones (the model's retry simply asks again). A call inside a sub-turn cannot be approved — it has no chat to bind to — and is refused.

### What the registry does not cover

The funnel covers model-driven calls. Code that constructs a tool and calls `Execute` directly bypasses it; today that is the dashboard's delta-neutral force-unwind, which carries its own paper-mode refusal (`409`).

### Limits and loss tracking

`trading_risk` caps (`max_order_value_usd`, `daily_loss_limit_usd`, `max_dca_amount_per_order_usd`) are **zero by default, meaning unlimited**, and leverage is allowed by default; the shipped defaults do not restrict live trading. When set:

- `daily_loss_limit_usd` is an **equity-drawdown guard** (see "Daily loss guard" below), with realised losses from **futures closes, reduces and flattens, and delta-neutral unwinds and resizes** as a second signal. State is persisted to `<workspace>/state/daily_loss.json`. Two processes sharing a workspace can lose each other's recorded realised loss; the equity samples and `khunquant risk reset-daily-loss` go through the same file and are picked up by modification time.
- **The futures and delta-neutral exit tools are not blocked by the risk-adding brakes.** The leverage switch (`allow_leverage=false`) and the daily-loss limit stop a user from getting further *in*, not from getting *out*: delta-neutral unwind and resize-down, `futures_close_position`, `futures_reduce_position`, `futures_emergency_flatten` and `futures_cancel_orders` skip both. Opens, resize-up, `futures_open_position`, `futures_set_leverage`, `futures_modify_protection` (it cannot be classified before the existing orders are read, so it counts as risk-adding), `create_order` and `option_create_order` stay gated. For the daily-loss limit specifically, `create_order` gates buys, sells of a contract symbol (a `:` in the unified symbol, where a sell can open a short), and sells that carry margin, borrow or leverage params (`tdMode`, `sideEffectType`, `autoBorrow`, `leverage`, `posSide`, ... which can open or enlarge a short on a spot symbol); a **plain spot sell is never blocked** by a tripped limit. Permission checks (`CheckPermission`) apply to every path, exits included. The per-provider rate limit still applies to confirmed exits, but a dry-run no longer spends it.
- `max_order_value_usd` also applies to each leg of `open_delta_neutral_position` and of a delta-neutral resize-up (the leg's notional, or the resize delta); an unwind or resize-down is never blocked by it.
- Market orders are priced from a ticker so notional caps apply to them too, and an order that cannot be priced is refused while a cap is set. `create_order`, the futures tools, `transfer_funds`, `create_dca_plan` and `open_delta_neutral_position` reject NaN and infinite amounts (checked per tool, not by the generic validator). Exchange `params` on `create_order` and futures orders pass through an allowlist, with margin/leverage keys only when allowed.

### Daily loss guard

With `trading_risk.daily_loss_limit_usd > 0` the guard measures **equity drawdown since the start of the loss day**, unrealised losses included, so spot and options losses count as well as futures:

- In gateway mode a sampler (every `equity_sample_minutes`, default 5; first sample at start) values each configured exchange account in USDT. An account's first sample of the day is its baseline; its loss is baseline minus latest sample. The day total is `max(realised losses, sum of per-account drawdowns)`. An account that failed outright in a cycle (client error, wallet call failed entirely, an asset that cannot be priced or converted) is not sampled, so a partial valuation never reads as a loss.
- **Comparable samples:** every sample carries the list of wallet types that were read. A wallet type that fails inside an otherwise successful account (for example margin or earn without API permission) does not exclude the account; it is just absent from that list. The baseline remembers its list, and a later sample with a different list is **ignored** (one warning per account per day), so a wallet that drops out for a cycle cannot read as a loss. If the mismatch persists, the account's latest comparable sample goes stale and orders on it fail closed through the staleness rule; a persistent change in what can be measured therefore costs one staleness window of refused risk-increasing orders; the next sample then re-baselines the account and the loss measured so far is kept (carried, and never offset by later gains). A key that permanently lacks a wallet type has a consistent list and works, but its equity is consistently understated: losses held in the unreadable wallet are not seen.
- A risk-increasing order (`create_order` buy or contract sell, `option_create_order`, `futures_open_position`, delta-neutral open and resize-up, `execute_dca_order`) is refused when the total reaches the limit. Exits are never gated.
- **Fail closed:** when the sampler is running, an order on a sampled account with no baseline today, or whose latest sample is older than 3 sampling intervals, is refused with `account equity unknown`. Other accounts are unaffected, and a stale account still counts its last known drawdown. An account the sampler does not cover is checked against the total only (one warning per account in the log).
- `loss_day_timezone` (default `UTC`) sets where the loss day rolls over, at local midnight. An unknown zone is logged as an error and UTC is used. Changing the zone mid-day carries the current day over rather than clearing it.
- `khunquant risk status` shows the day, per-account baseline/last/drawdown/age, the total and whether the limit is tripped. `khunquant risk reset-daily-loss --reason "<text>"` re-baselines every account to its latest sample and zeroes realised losses; a running gateway picks it up on its next check.

Known limitations:

- **Withdrawals and outgoing transfers look like losses** — run `khunquant risk reset-daily-loss --reason "withdrawal"` afterwards. **Deposits mask losses** by raising equity.
- **Only gateway mode samples equity.** `khunquant agent` and other modes enforce realised losses only and never fail closed on equity.
- Samples are persisted. After a gateway restart a sample younger than 3 sampling intervals is honoured immediately; orders on a sampled account are refused with `account equity unknown` only when there is no baseline yet today (first start of the day, or a new account) or the last sample is older than that bound (the gateway was down longer), until the first sample lands (up to one sampling interval). A config reload does not discard samples, but the equity check is briefly disarmed while the sampler restarts. At the loss-day rollover each account's latest sample, if still fresh, is carried forward as the new baseline, so midnight has no blind window.
- Equity comes from the wallet balances the exchange adapters report, priced in USDT; how much unrealised PnL a given futures wallet includes depends on the exchange. Exchanges without a priced-wallet interface contribute 0 and are effectively unmonitored.

`option_create_order` is **disabled by default** (`tools.option_create_order.enabled`); the other option tools are read-only and enabled.

## Credentials and Secrets

- **At rest**: `enc://` values are AES-256-GCM encrypted under a key derived with HKDF-SHA256 from an SSH private key and a passphrase (`HMAC-SHA256(SHA256(sshKey), passphrase)` as input keying material). HKDF is chosen for speed on small devices; it is **not** a password-hardening KDF, so the strength rests on the SSH key being secret, not on the passphrase being hard to guess. A slower KDF (Argon2id) would need a new scheme prefix and is not implemented.
- **The passphrase file is a degraded mode**: `~/.khunquant/.passphrase` (or `$KHUNQUANT_HOME/.passphrase`) lets the process start unattended, at the cost of storing the passphrase beside the data it protects. It is written `0600`, and is **refused** — with a warning naming `chmod 600` — when group or others can read it. `KHUNQUANT_KEY_PASSPHRASE` takes precedence. `.passphrase` is in `.gitignore`.
- **Logs**: every credential in the loaded config is registered for redaction — every `SecureString`, plus every string under a credential-named field or map key (the same name list the dashboard API uses, `config.CredentialFieldNames`). Messages and fields are redacted in both the console and the file log. Tool calls log argument names and a short redacted preview at Info; full arguments only at Debug.
- **Child processes**: `exec` commands and stdio MCP servers start from an allowlisted environment (`PATH`, `HOME`, locale, temp dirs, …), not the gateway's, so they do not inherit API keys. Pass more with `tools.exec.env_passthrough` or a server's `inherit_env`; `inherit_all_env: true` restores full inheritance for an MCP server and logs a warning on every start. A server's explicit `env` and `env_file` are unaffected.

## Channels

- **`allow_from`**: an empty list currently admits everyone, and each such channel logs a warning at startup. **A future release will treat an empty `allow_from` as deny-all.** Add your IDs, or `"*"` to keep a channel open deliberately.
- **Matrix**: `join_on_invite` defaults to `false` for new configs (existing configs keep their saved value).
- **MaixCam**: devices must send `{"type":"auth","token":"..."}` first when `channels.maixcam.token` is set; the channel refuses to listen on a non-loopback address without a token. Only authenticated devices receive agent replies (the channel broadcasts to connected clients). Defaults: `127.0.0.1:18791`. Messages are capped at 64 KiB and the detector label is sanitised before it reaches the agent.
- **Skills**: installing from ClawHub fails closed when moderation metadata cannot be fetched (`tools.skills.registries.clawhub.allow_unverified` overrides), malware-flagged skills are refused before download, and extraction is staged so a half-extracted skill is never loaded.
- **Serial**: the serial tool accepts only device paths (`/dev/...`, `COMn`).
- **File access**: `allow_read_paths` / `allow_write_paths` entries are anchored at the start of the path; an entry without a leading `^` is anchored with a warning.

## Upgrade Notes (breaking changes)

- `option_create_order` is disabled by default — set `tools.option_create_order.enabled: true` to place option orders.
- `-public` no longer moves the gateway onto `0.0.0.0` — set `gateway.host` explicitly.
- CSRF protection covers every mutating `/api/` route — scripts must send a matching `Origin`.
- Stdio MCP servers no longer inherit the environment — use `inherit_env` or `env`.
- Skill installs fail when ClawHub moderation metadata is unavailable — or set `allow_unverified`.
- MaixCam on a network address needs a token, and devices must send it first.
- A `/0` in `trusted_proxy_cidrs` fails validation; the dashboard cannot change the list.
- A group- or world-readable `.passphrase` is ignored until `chmod 600`.
- Allow-path patterns without a leading `^` are anchored and can stop matching mid-path.
- Scheduled agent jobs and heartbeats run with read-and-notify tools only: they can no longer trade (exits included), change plans or schedules, run commands or use MCP tools; `execute_dca_order` works only in a plan's own `dca:` job. Each generic scheduled agent job that existed before the upgrade is told once, on the first start after it, in its own chat (jobs created afterwards are born under the ceiling and are not told). For recurring buys, create a DCA plan.
- Container images built from `docker/Dockerfile` and `Dockerfile.full` run as UID 1000.

## Reporting Security Issues

If you discover a security vulnerability in khunquant, please report it via a private GitHub security advisory on this repository. Do not open a public issue or pull request.

To file a security advisory:
1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue, impact, and recommended fix.

The maintainers will assess the report and coordinate a fix and disclosure timeline.
