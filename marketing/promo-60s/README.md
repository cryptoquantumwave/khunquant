# KhunQuant promo — 60s, 9:16

A 60-second vertical (1080×1920, 30 fps) motion-graphic promo with a minimal, light look. It opens on the pain point, introduces the pixel mascot and the supported exchanges/brokers, then follows one continuous camera over a realistic iPhone. The phone shows KhunQuant at work in LINE, then switches to Telegram and Discord. The logo appears only at the end.

| File | What it is |
|---|---|
| `khunquant-promo-60s.mp4` | Final video with background music and SFX, **no voice**. Ready to drop a VO on top |
| `khunquant-promo-60s_video-only.mp4` | Same picture, silent, for editors who build their own audio |
| `khunquant-promo-60s_preview-th-subs.mp4` | Timing check only: the Thai SRT burned in |
| `khunquant-promo-60s.th.srt` / `.en.srt` | Voice-over script and subtitle timing (TH / EN) |
| `audio/music.wav`, `audio/sfx.wav`, `audio/music_sfx.wav` | Music bed (about -24 LUFS), SFX only, and the two mixed (the audio in the final MP4) |
| `add-voiceover.sh` | One command to add a recorded VO: ducks the music under the voice and normalises to -14 LUFS |
| `promo.html` | Animation source. Every frame is a pure function of `render(t)` |
| `render.mjs` | Steps the timeline in headless Chromium, pipes frames to ffmpeg, writes `audio/cues.json` |
| `audio/make_audio.py` | Synthesises the music and SFX from the cue sheet with numpy |
| `build.sh` | Full rebuild (render → audio → mux) |
| `logos/` | Exchange / chat-app marks used in the video (see below) |

**Audio licensing.** The music and every sound effect are generated procedurally by `audio/make_audio.py`. There are no samples or third-party recordings, so there is nothing to license or credit. The fonts (Kanit, IBM Plex Sans Thai) are SIL OFL 1.1.

## Logos

`logos/binance.svg`, `okx.svg`, `line.svg`, `telegram.svg` and `discord.svg` are the brand marks from [Simple Icons](https://simpleicons.org) (CC0 icon data; the marks stay trademarks of their owners).
Simple Icons has no marks for **Bitkub, SET/Settrade or Webull**, so the video draws a plain placeholder tile with the name for those.
To use the official artwork, drop it in as `logos/bitkub.svg|png`, `logos/set.svg|png`, `logos/webull.svg|png` (and optionally `logos/binanceth.svg|png`; without it, Binance TH is the Binance mark with a "TH" badge) and run `./build.sh`. The page picks the files up automatically.

## Storyboard and voice-over

| Time | Shot | Voice-over (TH) |
|---|---|---|
| 0.0–8.4 | Pain point: app screens (Bitkub, Binance, Settrade, Webull, OKX), a broken spreadsheet, news feeds per market and a sticky note pile up with red badges. Camera pushes into the spreadsheet/news. Mascot pops in: "ให้ผมช่วยเอง!" and vacuums the clutter away | หุ้นไทย หุ้นนอก คริปโต… พอร์ตกระจายอยู่หลายแอป ดูแลยาก จัดการยาก / ต้องคอยจดบันทึกเอง ตามข่าวทีละตลาด ยิ่งเหนื่อย |
| 8.4–15.0 | Mascot says hi; Bitkub, Binance, Binance TH, OKX, SET, Webull tiles connect to it one by one; they merge into a single portfolio card. The mascot hops into the chat avatar as the phone rises | รู้จักคุณควอนต์ เชื่อมได้ทั้ง Bitkub, Binance, Binance TH, OKX, SET และ Webull / ทุกพอร์ตรวมเป็นหนึ่งเดียว คุยผ่าน agent ตัวเดียวจบ |
| 15.2–19.8 | LINE: camera pushes in on the input box; the user types an alert on the Thai keyboard; bot confirms | แค่พิมพ์สั่งในแชท ว่าอยากให้เตือนอะไร / แล้วผมจะเฝ้าตลาดให้ 24 ชั่วโมง |
| 19.8–24.1 | Phone locks; KhunQuant push notifications land on the lock screen; tap opens the chat | ราคาถึงเป้า ข่าวหุ้นเทค หุ้นไทย คริปโต… เด้งเตือนถึงมือทันที |
| 24.1–31.6 | **Proactive**: KhunQuant messages first about the AI-bubble risk. AI/tech 46% vs 30% target, energy 18% vs 12%, with a suggested trim. User taps "Rebalance ตามแผน" → 4 orders filled, allocation back on target | พอหุ้น AI กับพลังงานร้อนแรงจนพอร์ตเอียง คุณควอนต์ทักมาเตือนเอง / ให้คุณพิจารณา rebalance ได้ด้วยปุ่มเดียว |
| 31.6–35.3 | Types "สรุปพอร์ตทั้งหมดให้หน่อย" → portfolio card | อยากรู้ภาพรวม ถามคำเดียว สรุปครบทุกพอร์ต |
| 35.3–39.5 | DCA In (RSI-gated weeks) + DCA Out (take-profit ladder) | ตั้ง DCA ทยอยซื้อ ทยอยขาย อัตโนมัติ ตามสัญญาณอินดิเคเตอร์ |
| 39.5–42.6 | Tap "เปิด Delta-Neutral" → hedge card, beam levels to Δ 0 | เปิด Delta-Neutral เก็บค่า Funding โดยไม่ต้องลุ้นทิศทาง |
| 42.6–45.6 | Tap "วิเคราะห์กราฟ NVDA" → chart draws in; P&L journal | วิเคราะห์กราฟให้ แถมจดบันทึกกำไรขาดทุนทุกไม้ |
| 45.7–53.3 | Swipe up to the iOS app switcher: LINE → Telegram (asks for the portfolio) → Discord (`!pnl`) | จะคุยผ่าน LINE, Telegram หรือ Discord ก็ได้หมด / แค่สลับแอปในมือถือ คุณควอนต์ก็ตามไปดูแลทุกที่ |
| 53.3–60.0 | Mascot jumps to centre and flashes into the logo; exchange strip, chat apps, khunquant.com | KhunQuant ทุกพอร์ตรวมเป็นหนึ่งเดียว / เริ่มเลยที่ khunquant.com |

Voice direction: friendly and confident, like a smart friend rather than a broker. Read คุณควอนต์ as "khun-quant" and DCA as "ดี-ซี-เอ". Each cue fits a normal Thai speaking pace.

All prices, balances, orders and news are made up for the demo. A footer on screen says so ("ภาพจำลองเพื่อการสาธิต · ตัวเลขสมมติ · ไม่ใช่คำแนะนำการลงทุน").

## Adding the voice-over

```bash
./add-voiceover.sh vo.wav                 # -> khunquant-promo-60s_with-vo.mp4
```

The script mixes `music.wav` (sidechain-ducked by the voice), `sfx.wav` and your VO onto the silent video, then normalises to -14 LUFS / -1.5 dBTP. The VO should start at 0:00 and follow `khunquant-promo-60s.th.srt`.
In CapCut or Premiere you can instead put `khunquant-promo-60s.mp4` on the timeline and add the VO track. The bed is already quiet enough to talk over.

## Re-rendering

```bash
./build.sh                               # ~6 min, needs playwright (Chromium), ffmpeg, python3 + numpy
node render.mjs --stills 3,12.4,47       # PNG stills in stills/ for quick checks
open promo.html                          # live looping preview in a browser (silent)
```

Timings live in `promo.html`:
- `CHAOS` and `CAPS`: the pain-point scene
- `EX`: exchanges
- `ITEMS`: LINE chat
- `SIMPLE`: Telegram/Discord
- `NOTIFS`, `TAPS`, `PEEKS`, `TITLES`, `SW_WIN` and `TOUCH`
- `CAM`: camera keyframes, inside `setup()`

Sound cues are derived from the same timeline. If you move a scene, move its SRT cues too.
