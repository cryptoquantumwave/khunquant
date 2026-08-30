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
- **Filesystem confinement**: The regex inspects the command string only; it does not resolve symlinks, does not prevent path traversal via relative paths or environment variables, and does not forbid writes to files outside the workspace.
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
- Obfuscating commands in environment variables or files written by prior steps.
- Using lesser-known commands not on the deny-list (e.g., `shred`, `srm`, `dd bs=` with a different argument order).
- Exploiting parser bugs in the regex engine or shell itself (e.g., Unicode normalization, locale-specific character classes).

## Launcher Dashboard — Network and Application-Level Access Control

### Access Control Model

The web launcher (`web/backend`) implements multi-layer access control:

1. **IP allowlist** (optional, network-level): A CIDR allowlist in `launcher-config.json` (field `allowed_cidrs`). Requests from IPs outside this range are rejected at the network level.
2. **Trusted-proxy X-Forwarded-For parsing**: If the launcher is behind a reverse proxy (e.g., nginx), it can extract the client IP from the `X-Forwarded-For` header, but only when the proxy's IP is in `trusted_proxy_cidrs`.
3. **Password authentication** (optional, application-level): A bcrypt-hashed password stored in `launcher-config.json` (field `dashboard_password_hash`). Credentials are verified via HTTP Basic authentication per request. This layer is **independent of** the IP allowlist—both must pass (if configured).
4. **CSRF protection on state-changing endpoints** (application-level): State-changing launcher setup endpoints (e.g., `POST /api/pico/setup`) verify that requests originate from the launcher's own origin, not from an attacker's page. The check uses the `Sec-Fetch-Site` header when present (modern browsers), falls back to comparing parsed `Origin` and then `Referer` headers against the request's own scheme and host, and rejects requests that cannot be identified.

### Password Authentication Semantics

- **Password is opt-in**: No password configured (empty `dashboard_password_hash`) means password authentication is disabled entirely. The dashboard behaves exactly as before (IP allowlist only). **This is deliberate backward compatibility.** Operators must actively set a password to enable this protection.
- **Loopback bypass applies to IP allowlist only**: If `allow_localhost_bypass` is enabled, loopback addresses bypass the CIDR check. However, a configured password is still required from loopback callers. The two checks are independent.
- **Credentials verified per request**: HTTP Basic authentication is checked on every request. No session cookie is minted; each request independently supplies and verifies credentials.
- **Health checks exempt**: `/api/health` and `/api/ready` skip password authentication to allow external health monitors to function.

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
- **No per-user authentication**: Authentication is single-password (not per-user). Any client who provides the correct password can perform any operation (modify config, restart agents, etc.).
- **HTTPS not enforced**: The launcher does not require TLS. If a password is configured, credentials are transmitted via HTTP Basic auth in the request header. Over plain HTTP, these credentials can be intercepted. TLS is strongly recommended when exposing the launcher over a network.
- **No audit log**: Access is not logged by user or timestamp (though the launcher emits structured request logs). There is no record of who accessed the dashboard or what changes were made.
- **Password hash in config file**: The bcrypt-hashed password is stored in the plaintext `launcher-config.json` file on disk. File permissions are set to 0o600 (read/write for owner only), but the hash could be extracted and cracked if the file is compromised.
- **CSRF protection covers all state-changing launcher endpoints**: Configuration updates (`PUT/PATCH /api/config`, `POST /api/config/reset`), Pico setup and token regen (`POST /api/pico/setup`, `POST /api/pico/token`), gateway lifecycle (`POST /api/gateway/{start,stop,restart,logs/clear}`), model management, OAuth operations, skills import/delete, tool state updates, and other state-changing operations. Read-only endpoints (GET requests) are not protected, as CSRF applies only to state-changing operations.
- **Non-browser clients can spoof CSRF headers**: A client that is not a web browser (e.g., a custom HTTP client or an attacker's tool) can arbitrarily set `Sec-Fetch-Site`, `Origin`, and `Referer` headers. CSRF protection assumes these headers are set by the browser and cannot be forged; this assumption breaks for non-browser clients. CSRF protection is part of the application-level defense but is **not a substitute for IP allowlist and password authentication**, which govern non-browser access.

### CSRF Protection Special Cases

The following endpoints are **not** protected by CSRF checks, for documented reasons:

- **`GET /oauth/callback`**: This is a top-level cross-site navigation initiated by the OAuth provider (e.g., Google, Anthropic) as part of the authorization flow. The request naturally has `Sec-Fetch-Site: cross-site`. Defense is state validation via the `state` parameter (checked against `h.oauthState` in the handler), not CSRF headers. This design is correct and intentional.
- **Read-only endpoints (GET, HEAD)**: Endpoints that only read state and do not mutate configuration or process state are not protected. CSRF protection adds failure modes for no benefit on idempotent requests.
- **Test-only endpoints without persistence**: `POST /api/config/test-command-patterns`, `POST /api/models/fetch`, and similar test/validation endpoints that do not persist changes are not protected.

## Reporting Security Issues

If you discover a security vulnerability in khunquant, please report it via a private GitHub security advisory on this repository. Do not open a public issue or pull request.

To file a security advisory:
1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue, impact, and recommended fix.

The maintainers will assess the report and coordinate a fix and disclosure timeline.
