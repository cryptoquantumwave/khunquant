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
2. **Trusted-proxy X-Forwarded-For parsing**: If the launcher is behind a reverse proxy (e.g., nginx), it can extract the client IP from the `X-Forwarded-For` header, but only when the proxy's IP is in `trusted_proxy_cidrs`.
3. **Password authentication** (optional, application-level): A bcrypt-hashed password stored in `launcher-config.json` (field `dashboard_password_hash`). Verified via HTTP Basic authentication, a `POST /api/auth/login` session cookie, or the launcher token — see "Password Authentication Semantics" below. This layer is **independent of** the IP allowlist—both must pass (if configured).
4. **CSRF protection on state-changing endpoints** (application-level): State-changing launcher setup endpoints (e.g., `POST /api/pico/setup`) verify that requests originate from the launcher's own origin, not from an attacker's page. The check uses the `Sec-Fetch-Site` header when present (modern browsers), falls back to comparing parsed `Origin` and then `Referer` headers against the request's own scheme and host, and rejects requests that cannot be identified.

### Password Authentication Semantics

- **Password is opt-in**: No password configured (empty `dashboard_password_hash`) means password authentication is disabled entirely. The dashboard behaves exactly as before (IP allowlist only, plus the loopback/launcher-token session rules described below). **This is deliberate backward compatibility.** Operators must actively set a password to enable this protection.
- **Setting the password**: `khunquant launcher set-password` prompts twice without echo, hashes with bcrypt (cost 12), and writes `dashboard_password_hash` to `launcher-config.json`; `--clear` removes it. The command refuses a non-TTY stdin, so a password cannot be piped in from a shell history or CI log. It derives the config path exactly as the launcher does, because writing the hash anywhere else would fail silently and look identical to the gate not working. **It cannot revoke live sessions** — those live in memory inside the running launcher process — so restart the launcher after setting a password. Until this command existed, nothing in the shipped binaries could write the hash, which meant no deployment could reach the gate described in this section.
- **Startup warning when exposed**: if no password is configured and the launcher is either listening on `0.0.0.0` (`-public`) or trusting proxy headers (`trusted_proxy_cidrs` non-empty), it logs a warning at startup naming the exposure and the command above. It warns rather than refusing, so existing deployments keep working.
- **Middleware order enforces the gate**: the launcher's HTTP handler wraps, outer to inner, `IPAllowlist → PasswordAuth → SessionAuth → mux`. `PasswordAuth` therefore always runs, and rejects, before `SessionAuth` gets a chance to do anything — this is what makes the next bullet true rather than aspirational.
- **Loopback bypass applies to IP allowlist only**: If `allow_localhost_bypass` is enabled, loopback addresses bypass the CIDR check. A configured password is still required from loopback callers: `SessionAuth`'s auto-issue-a-session-for-loopback-callers rule is disabled outright whenever a password is configured, so a loopback caller with no session, no Basic credentials, and no launcher token gets a 401 with **no** `Set-Cookie` header — the same as any other caller. The two checks (IP allowlist and password) remain independent of each other.
- **Credentials accepted**: on each request, in order — (1) a valid `kq_session` cookie from a prior login, (2) HTTP Basic authentication checked against the bcrypt hash, or (3) the launcher token (`Authorization: Bearer`, `X-Launcher-Token`, or `launcher_token` query parameter) — the launcher token is treated as an alternative launcher credential, since it is at least as privileged a secret as the dashboard password.
- **Sessions are opt-in, not automatic**: `POST /api/auth/login` exchanges the dashboard password for a session cookie; `POST /api/auth/logout` revokes it. Basic-authenticated and launcher-token-authenticated requests are **not** automatically upgraded into a session — each such request is checked again on its own. In particular, presenting the launcher token authorizes that one request but does not mint a persistent session; only a successful password login does.
- **Health checks exempt**: `/api/health` and `/api/ready` skip password authentication (and session/loopback checks) to allow external health monitors to function.

### Fail-Closed Guard

The launcher refuses to start in public mode (listening on `0.0.0.0`) without an explicit IP allowlist. The guard is in `web/backend/main.go`, lines 113–119:

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

State-changing launcher endpoints (currently `/api/pico/setup` only) are protected against Cross-Site Request Forgery (CSRF) attacks via origin verification. The mechanism works as follows:

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
- **CSRF protection**: Protects the following 13 launcher setup operations with `Sec-Fetch-Site: same-origin` validation (modern browsers) and Origin/Referer fallback (older browsers):
  1. **Configuration** (`PUT /api/config`, `PATCH /api/config`, `POST /api/config/reset`)
  2. **Pico channel** (`POST /api/pico/setup`, `POST /api/pico/token`)
  3. **Gateway lifecycle** (`POST /api/gateway/start`, `POST /api/gateway/stop`, `POST /api/gateway/restart`, `POST /api/gateway/logs/clear`)
  4. **Model management** (`POST /api/models`, `PUT /api/models/{index}`, `DELETE /api/models/{index}`, `POST /api/models/default`)
  5. **OAuth** (`POST /api/oauth/login`, `POST /api/oauth/logout`, `POST /api/oauth/flows/{id}/poll`, `POST /api/oauth/providers/{provider}/select-model`)
  6. **Session** (`DELETE /api/sessions/{id}`)
  7. **Skills** (`POST /api/skills/import`, `DELETE /api/skills/{name}`)
  8. **Tools** (`PUT /api/tools/{name}/state`)
  9. **System** (`PUT /api/system/launcher-config`)
  10. **Updates** (`POST /api/update/apply`)
  
  Read-only endpoints (GET requests) are not protected. Certain operations deliberately excluded (see below).
- **Pico `allow_origins` is empty by default, and that is not "allow anything"**: Setup no longer records the Origin of whichever request happened to trigger it. That pin locked the channel to a single host — a launcher configured from `localhost` then rejected the same user reaching it over the LAN or on another port. With the list empty the channel falls back to a same-origin check (`isSameOriginRequest`, `pkg/channels/pico/pico.go`), which is what actually protects a browser; the pin added nothing on top of it. An operator who wants specific origins can still set `allow_origins` explicitly, and a wildcard `*` is never written automatically.

- **Pico WebSocket token is never sent to the browser**: The Pico channel authenticates its WebSocket handshake with a `token.<value>` subprotocol. The launcher proxies `/pico/ws` and attaches that token server-side from config, so no API response carries it and no browser holds it. `GET /api/pico/info` returns only `ws_url` and `enabled`; `POST /api/pico/token` rotates the token and returns the same token-free payload. The proxy also discards any subprotocol the caller supplied, so a client cannot present a token of its own choosing to the gateway, and strips the gateway's echoed subprotocol from the handshake response.

  **Limitation:** this confines the token to the server; it is not itself an authentication boundary. `/pico/ws` is gated by the launcher session/password middleware (`apiRequiresAuth` in `web/backend/middleware/auth.go`), and that gate — not token confinement — is what stops an unauthenticated client from reaching the gateway. Anyone who can read `config.json` still has the token.
- **Credentials are withheld from `GET /api/config`**: Most credentials are `config.SecureString`, which withholds its own value on marshal and is restored from `.security.yml` on save. Three subtrees cannot use it — `Providers`, `Tools`, and `Debug` are tagged `yaml:"-"`, so a `SecureString` there would marshal as withheld and have nothing to be restored from, blanking the credential on the next save. Those fields are plain strings and are redacted at the API boundary instead: the response tree is walked and every credential-named key with a non-empty value is replaced with `[NOT_HERE]`. `PUT`/`PATCH /api/config` restore the real values before decoding, so a dashboard round-trip cannot overwrite a credential with the placeholder.

  Redaction is keyed on field name (`api_key`, `token`, `secret`, …), not on a list of known config paths, so a newly added provider or search backend is withheld the moment it is added rather than when someone remembers to update a list. Empty values are left as-is so "not configured" stays distinguishable from "configured but withheld".

  **Reachability:** `GET /api/config` sits behind the launcher session/password middleware and the IP allowlist, so this was never remote-unauthenticated. The exposure was that any dashboard-authenticated session — and anything that records its responses, such as browser devtools, extensions, or a HAR export — could read provider and tool API keys in full. Anyone who can read `config.json` on disk still has them.

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
- **Test-only endpoints without persistence**: `POST /api/config/test-command-patterns` — validation only, no state changes. (`POST /api/models/fetch` is no longer in this category: it persists fetched results to `model_catalogs.json` and is CSRF-protected like the other mutating model endpoints — see `handleFetchModels`'s SSRF/CSRF policy comment in `web/backend/api/models.go`.)

**Endpoints NOT yet protected (out of scope for this change):**

Agent operations are not yet CSRF-protected:
- `/api/agent/config/files/*` (agent-specific configuration file operations)
- `/api/agent/memory/files/*` (agent memory snapshots and queries)
- `/api/agent/snapshots/*` (agent state snapshots)
- `/api/agent/delta-neutral/*` (delta-neutral position management)
- `/api/cron/jobs/*` (cron job scheduling)
- `/api/pairing/*` (device pairing operations)
- `/api/sandbox/*` (isolated code execution environment)
- `/api/system/autostart` (system autostart configuration)
- `/api/exchanges/webull/*/connect` (Webull broker reconnection)

These are **deliberately out of scope** for this CSRF sweep, which focuses on launcher setup operations. Agent-specific operations should be reviewed and protected in a separate task.

## Reporting Security Issues

If you discover a security vulnerability in khunquant, please report it via a private GitHub security advisory on this repository. Do not open a public issue or pull request.

To file a security advisory:
1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue, impact, and recommended fix.

The maintainers will assess the report and coordinate a fix and disclosure timeline.
