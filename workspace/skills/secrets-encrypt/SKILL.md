---
name: secrets-encrypt
description: Encrypt exchange API keys and credentials stored in .security.yml using AES-256-GCM. Triggered when the user asks to encrypt their keys, secure credentials, or migrate from plaintext storage.
---

# Credential Encryption

Use this skill when the user says things like:
- "encrypt my API keys"
- "secure my credentials"
- "my .security.yml is plaintext, fix it"
- "set up encryption for my exchange keys"
- "migrate to enc://"

## How it works

Credentials in `.security.yml` are stored as `enc://<base64>` blobs. Decryption requires **both**:
1. An SSH private key at `~/.ssh/khunquant_ed25519.key` (generated automatically)
2. A passphrase stored at `$KHUNQUANT_HOME/.passphrase` (or `KHUNQUANT_KEY_PASSPHRASE` env var)

The encryption scheme is AES-256-GCM with HKDF-SHA256 key derivation — the open-source code does not weaken it; both the SSH key and passphrase are required to decrypt.

## Workflow

1. **Warn the user** that the passphrase will be stored at `~/.khunquant/.passphrase`. They should also keep a copy somewhere safe — if both the passphrase file and their backup are lost, the credentials cannot be recovered.

2. **Ask for the passphrase** — remind them not to share it in public. 

3. **Confirm** before running — this overwrites `.security.yml`.

4. **Call `config_encrypt_keys`** with the passphrase and `rotate=false` (or `true` if re-encrypting).

5. **After success**, tell the user:
   - Their credentials are now encrypted.
   - `khunquant` will auto-decrypt on startup via `~/.khunquant/.passphrase`.
   - They can override by setting `export KHUNQUANT_KEY_PASSPHRASE=<passphrase>`.
   - To rotate the passphrase later, run `khunquant auth encrypt` again.

## Tools

### config_encrypt_keys
Encrypts all SecureString fields in `.security.yml`.

Parameters:
- `passphrase` (required) — the encryption passphrase. **Never echo this back to the user or include it in a visible response.**
- `rotate` (optional, default false) — set to `true` when re-encrypting with a new passphrase.

## FAQ the agent should be able to answer

**Q: What if I forget my passphrase?**
There is no recovery mechanism. Keep a secure backup (password manager, etc.). You can reset by re-running `khunquant onboard` (which regenerates a new SSH key and passphrase) then re-entering all exchange credentials.

**Q: Is it safe that the passphrase file is on disk?**
The passphrase file (`~/.khunquant/.passphrase`, 0600) is one factor. Decryption also requires the SSH key (`~/.ssh/khunquant_ed25519.key`, 0600) which is a separate file. Both files together are needed to decrypt — the same security model as SSH-protected private keys.

**Q: Does encryption work with the web launcher / TUI?**
Yes. Any credentials entered via `khunquant-launcher-tui` or `khunquant auth login` are encrypted automatically when the passphrase is set.
