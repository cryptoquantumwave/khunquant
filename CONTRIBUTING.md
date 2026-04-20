# Contributing to KhunQuant

ขอบคุณที่สนใจมีส่วนร่วมกับ KhunQuant / Thank you for your interest in contributing to KhunQuant.

KhunQuant is a community project aimed at democratizing quantitative portfolio management for the Thai market. Contributions that move us toward that goal are most welcome.

---

## Where to contribute

### High-priority areas (aligned with the roadmap)

**Phase 1 — Exchange Adapters**
- Bitkub REST/WebSocket adapter
- Binance adapter (spot trading)
- Settrade / Streaming integration
- InnovestX adapter
- Dime adapter (US stocks, Thai mutual funds)

**Phase 2 — Trading Tools**
- Agent tools: `trade_execute`, `portfolio_read`, `market_data`, `rebalance`
- TradingView webhook receiver
- Technical indicator library (RSI, MACD, Bollinger Bands, etc.)
- Risk controls: position limits, daily loss limits, kill-switch

**Phase 3 — Community SDK**
- Adapter conformance test harness
- Documentation for building new exchange adapters
- Support for additional Thai financial products (ThaiBMA bonds, LTF/SSF/RMF funds)

### Always welcome
- Bug reports and fixes
- Test coverage improvements
- Documentation (EN or TH)

---

## Development setup

```bash
git clone https://github.com/cryptoquantumwave/khunquant.git
cd khunquant
make deps
make build
make check   # fmt + vet + test — run before every PR
```

Run a single test:
```bash
go test ./pkg/<package>/... -run TestName
```

---

## Pull request process

1. Fork the repo and create a branch from `main`
2. Run `make check` — all checks must pass
3. Keep PRs focused; one feature or fix per PR
4. For new exchange adapters: include at minimum a mock-based unit test that validates the adapter interface
5. For trading tools: document any side effects (network calls, order placement) clearly in the PR description

---

## Financial data and API keys

- **Never** commit real API keys, tokens, or account credentials
- Test adapters against sandbox/testnet environments where available (Binance Testnet, paper trading)
- Mock real exchange responses in unit tests; reserve live integration tests for opt-in CI jobs

---

## Security

Exchange adapters handle real money. If you discover a vulnerability, please report it privately via GitHub Security Advisories rather than opening a public issue.

---

## Code of Conduct

Be respectful. Focus feedback on code, not people. Discussions may happen in Thai or English.
