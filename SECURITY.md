# Security Posture

This document describes khunquant's security architecture and known limitations. Our goal is transparency: we aim to help operators understand what risks are or are not mitigated by the current design.

## Subprocess Execution — No OS-Level Isolation

### The Isolation Gap

Upstream picoclaw implements OS-level process confinement for child processes via `pkg/isolation`:
- **Linux**: `bwrap` (Bubblewrap) creates a confined namespace with filesystem restrictions, capability dropping, resource limits, and network isolation.
- **Windows**: Restricted tokens, low-integrity level, and Job Objects limit what a child process can access or do.

**khunquant does not have OS-level isolation, and never did.** Our fork point (2026-03-15) predates upstream's addition of `pkg/isolation` (2026-04-08, commit `51eecde0`). Adopting it would require:
- 27 call sites across `exec.Command` across 17 non-test files to route through an isolation layer.
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

## Launcher Dashboard — Network-Level Access Control Only

### Access Control Model

The web launcher (`web/backend`) implements **IP-address-based access control only**. There is **no password authentication**.

Protection consists of:
1. **IP allowlist**: A CIDR allowlist in `launcher-config.json` (field `allowed_cidrs`).
2. **Trusted-proxy X-Forwarded-For parsing**: If the launcher is behind a reverse proxy (e.g., nginx), it can extract the client IP from the `X-Forwarded-For` header if the proxy's IP is in `trusted_proxy_cidrs`.

### Fail-Closed Guard

The launcher refuses to start in public mode (listening on `0.0.0.0`) without an explicit allowlist. The guard is in `web/backend/main.go`, lines 113–119:

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

### Known Limitations

- **Spoofed or missing X-Forwarded-For**: If the reverse proxy is misconfigured or omits the header, the launcher may fall back to the proxy's own IP, allowing unintended clients to access the dashboard.
- **No per-user authentication**: Any client whose IP matches the allowlist can perform any operation (modify config, restart agents, etc.). There is no credential prompt.
- **No audit log**: Access is not logged by user or timestamp (though the launcher does emit structured logs).
- **HTTPS not enforced**: The launcher does not require TLS for connections. Credentials or session tokens would be transmitted in plaintext over HTTP.

### Upstream's Deliberate Non-Port

The upstream codebase includes a `POST /api/config/reset` endpoint that clears the entire configuration. **We intentionally did not port this endpoint**, because without password authentication it would be a configuration wipe protected by IP address alone — an unacceptable risk for a shared network.

## Reporting Security Issues

If you discover a security vulnerability in khunquant, please report it via a private GitHub security advisory on this repository. Do not open a public issue or pull request.

To file a security advisory:
1. Go to the repository's **Security** tab.
2. Click **Report a vulnerability**.
3. Describe the issue, impact, and recommended fix.

The maintainers will assess the report and coordinate a fix and disclosure timeline.
