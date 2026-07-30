# recover — Cashu Token Recovery CLI

Recovers value from rejected Cashu tokens that may or may not have been spent at the mint.

## Background

The old TollGate binary (NDS gate-open bug #88) logged Cashu tokens as "rejected" when it
couldn't open the gate. The binary may have already swapped them at the mint (consuming
them) but couldn't complete the gate-open flow. This tool distinguishes **spent** vs
**unspent** tokens using NUT-07 checkstate and recovers the unspent ones via `Wallet.Receive`.

## Log File Format

Each line in the log file must follow this format (pipe-space delimited):

```
2024-01-15T10:30:00Z | https://mint.example.com | cashuBeyJ0Ij... | gate open failed: NDS timeout
```

Fields: `timestamp | mint_url | cashuB_token | error_message`

- `mint_url` and `error_message` are optional for parsing (the token itself encodes its mint URL)
- Lines starting with `#` are treated as comments and skipped
- Blank lines are skipped

## Usage

```bash
# Build
go build ./cmd/recover/

# Dry-run (checkstate only, no wallet needed, no recovery)
./recover -log rejected_tokens.log -dry-run

# Full recovery: swap unspent tokens to your trusted/default mint
./recover -log rejected_tokens.log -mint https://your-trusted-mint.com

# Full recovery: keep proofs at original mint (no swap)
./recover -log rejected_tokens.log -mint https://your-trusted-mint.com -no-swap

# Custom wallet path and rate limiting
./recover -log rejected_tokens.log -wallet ~/.gonuts/wallet -mint https://mint.com -delay 1s
```

## Flags

| Flag       | Description                                      | Default               |
|------------|--------------------------------------------------|-----------------------|
| `-log`    | Path to log file with rejected tokens (required) | —                     |
| `-wallet` | Wallet directory path                             | `~/.gonuts/wallet` or `WALLET_PATH` env |
| `-mint`   | Trusted mint URL to swap recovered tokens to     | `MINT_URL` env        |
| `-delay`  | Delay between mint API calls (avoids 429s)      | `500ms`               |
| `-dry-run`| Check state only; do not receive/recover        | `false`               |
| `-no-swap`| Receive without swapping to trusted mint        | `false`               |

## Environment Variables

The tool reads `.env` from the current directory or `~/.gonuts/wallet/.env`:

- `WALLET_PATH` — wallet directory (overrides default)
- `MINT_URL` — trusted/default mint URL

## How It Works

1. **Parse** each log line to extract the Cashu token string
2. **Decode** the token (supports both V3 `cashuA` and V4 `cashuB` formats)
3. **CheckState** (NUT-07): compute `Y = HashToCurve(secret)` for each proof, send to mint's `/v1/checkstate` endpoint
4. **If all proofs spent**: skip, log "ALREADY SPENT"
5. **If unspent**: call `Wallet.Receive(token, swapToTrusted=true)` to swap proofs to the trusted mint and store them
6. **Summary**: print count of recovered / spent / failed tokens and total sats recovered

## Rate Limiting

A configurable delay (default 500ms) is inserted before each checkstate call and after each
receive to avoid hitting mint rate limits (HTTP 429). Adjust with `-delay`.