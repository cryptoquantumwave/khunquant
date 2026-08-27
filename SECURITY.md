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

### Known Limitations

- **Spoofed or missing X-Forwarded-For**: If the reverse proxy is misconfigured or omits the `X-Forwarded-For` header, the launcher may fall back to the proxy's own IP, allowing unintended clients to bypass the IP allowlist.
- **No per-user authentication**: Authentication is single-password (not per-user). Any client who provides the correct password can perform any operation (modify config, restart agents, etc.).
- **HTTPS not enforced**: The launcher does not require TLS. If a password is configured, credentials are transmitted via HTTP Basic auth in the request header. Over plain HTTP, these credentials can be intercepted. TLS is strongly recommended when exposing the launcher over a network.
- **No audit log**: Access is not logged by user or timestamp (though the launcher emits structured request logs). There is no record of who accessed the dashboard or what changes were made.
- **Password hash in config file**: The bcrypt-hashed password is stored in the plaintext `launcher-config.json` file on disk. File permissions are set to 0o600 (read/write for owner only), but the hash could be extracted and cracked if the file is compromised.

### Upstream's Deliberate Non-Port

The upstream codebase includes a `POST /api/config/reset` endpoint that clears the entire configuration. **We intentionally did not port this endpoint.** While password authentication is now implemented and would provide additional protection, the `reset` endpoint's destructive nature warrants a separate, deliberate decision and review. It remains un-ported and is not on the roadmap without explicit approval.

## Reporting Security Issues

If you discover a security vulnerability in khunquant, please report it via a private GitHub security advisory on this repository. Do not open a public issue or pull request.

To file a security advisory:
1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue, impact, and recommended fix.

The maintainers will assess the report and coordinate a fix and disclosure timeline.
