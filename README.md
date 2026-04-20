# KhunQuant

**EN** | [TH](#thai)

> Open-source agentic framework for quantitative portfolio management — built for the Thai community.

KhunQuant bridges Thai equity markets (SET) and global digital assets through a single AI-powered orchestrator. It runs locally on your machine, keeping your API keys and strategy parameters under your full control.

Built on [khunquant](https://github.com/cryptoquantumwave/khunquant) — an ultra-lightweight Go AI assistant.

---

## Features

- **Unified execution** — Trade on Bitkub and Binance while monitoring SET via Settrade from a single agent
- **Thai market native** — Built-in support for Thai brokerages: Streaming (Settrade), InnovestX, Dime
- **Privacy-first** — All logic runs locally; no strategy data leaves your machine
- **LLM-powered** — Natural language commands like *"Rebalance my crypto into Thai high-dividend stocks if BTC RSI hits 30"*
- **TradingView integration** — Webhook receiver maps alerts directly to trade execution
- **Multi-provider LLM** — Anthropic Claude, OpenAI, Azure, Ollama, local models

---

## Quick Start

**Requirements:** Go 1.25+, make

```bash
git clone https://github.com/cryptoquantumwave/khunquant.git
cd khunquant
make deps
make install       # installs to ~/.local/bin/khunquant
khunquant onboard  # interactive setup wizard
```

### Web Console

```bash
cd web
make dev           # starts frontend (localhost:5173) + backend (localhost:18800)
```

---

## Architecture

```
Channels (Telegram, Discord, Web, …)
          │
          ▼
    Agent Orchestrator  ◄──  LLM Provider (Claude, GPT, …)
          │
          ▼
      Tools Layer
    ┌─────┴──────────────────────────┐
    │  Exchange Adapters             │
    │  ┌──────────┐  ┌───────────┐  │
    │  │  Bitkub  │  │  Binance  │  │
    │  └──────────┘  └───────────┘  │
    │  ┌──────────┐  ┌───────────┐  │
    │  │ Settrade │  │InnovestX  │  │
    │  └──────────┘  └───────────┘  │
    └────────────────────────────────┘
```

- `pkg/providers/` — LLM provider abstraction
- `pkg/channels/` — Chat platform adapters
- `pkg/tools/` — Agent tools (filesystem, shell, search, exchange adapters)
- `pkg/agent/` — Core agent loop and context management
- `cmd/khunquant/` — CLI entry point (`onboard`, `agent`, `gateway`, `status`, …)

---

## Roadmap

| Phase | Focus | Status |
|-------|-------|--------|
| 1 | Exchange adapter layer (Bitkub, Binance, Settrade, InnovestX, Dime) | 🔨 In progress |
| 2 | LLM orchestration — natural language trading commands | 📋 Planned |
| 3 | Open-source adapter SDK for community contributions | 📋 Planned |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

MIT — see [LICENSE](LICENSE).
KhunQuant — [github.com/cryptoquantumwave/khunquant](https://github.com/cryptoquantumwave/khunquant).

---

<a name="thai"></a>

# KhunQuant (ภาษาไทย)

> เฟรมเวิร์ก open-source สำหรับการบริหารพอร์ตเชิงปริมาณ — สร้างสำหรับชุมชนคนไทย

KhunQuant เชื่อมตลาดหุ้นไทย (SET) กับสินทรัพย์ดิจิทัลทั่วโลกผ่าน AI agent ตัวเดียว ทำงานบนเครื่องของคุณเอง — API key และกลยุทธ์การลงทุนอยู่ในมือคุณตลอดเวลา

---

## คุณสมบัติหลัก

- **ครบในที่เดียว** — เทรด Bitkub และ Binance พร้อมติดตาม SET ผ่าน Settrade จาก agent เดียว
- **รองรับตลาดไทย** — รองรับโบรกเกอร์ไทยโดยตรง: Streaming (Settrade), InnovestX, Dime
- **ความเป็นส่วนตัว** — ระบบทำงานในเครื่องของคุณ ไม่มีข้อมูลกลยุทธ์ส่งออกไปภายนอก
- **สั่งงานด้วยภาษาธรรมชาติ** — เช่น *"ปรับพอร์ตคริปโตไปหุ้นปันผลสูงไทย ถ้า RSI ของ BTC ถึง 30"*
- **รองรับ TradingView** — รับ webhook alert แล้วส่งคำสั่งเทรดต่อทันที

---

## เริ่มต้นใช้งาน

```bash
git clone https://github.com/cryptoquantumwave/khunquant.git
cd khunquant
make deps
make install
khunquant onboard
```

### เว็บคอนโซล

```bash
cd web
make dev   # เปิด frontend ที่ localhost:5173 และ backend ที่ localhost:18800
```

---

## โรดแมป

| เฟส | เป้าหมาย | สถานะ |
|-----|----------|-------|
| 1 | ชั้น adapter สำหรับ exchange ต่าง ๆ (Bitkub, Binance, Settrade, InnovestX, Dime) | 🔨 กำลังพัฒนา |
| 2 | LLM orchestration — สั่งงานด้วยภาษาธรรมชาติ | 📋 วางแผน |
| 3 | Open-source adapter SDK สำหรับชุมชน | 📋 วางแผน |

---

## การมีส่วนร่วม

ดู [CONTRIBUTING.md](CONTRIBUTING.md)

---

## สัญญาอนุญาต

MIT — ดู [LICENSE](LICENSE)
KhunQuant — [github.com/cryptoquantumwave/khunquant](https://github.com/cryptoquantumwave/khunquant)
