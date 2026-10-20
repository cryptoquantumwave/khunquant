# KhunQuant promo — 30s, 9:16

A 30-second vertical (1080×1920, 30 fps) motion-graphic promo with no audio. It shows KhunQuant in use over LINE and Telegram chats.

| File | What it is |
|---|---|
| `khunquant-promo-30s.mp4` | Final video. H.264, no audio track |
| `khunquant-promo-30s.th.srt` | Thai voice-over / subtitle timing |
| `khunquant-promo-30s.en.srt` | English version, same timing |
| `khunquant-promo-30s_preview-th-subs.mp4` | Timing check only: the Thai SRT burned in |
| `promo.html` | Animation source. Every frame is a pure function of `render(t)` |
| `render.mjs` | Steps the timeline in headless Chromium and pipes frames into ffmpeg |
| `fonts/` | Kanit + IBM Plex Sans Thai (SIL OFL 1.1, from Fontsource) |

## Storyboard and voice-over

| Time | Scene | Voice-over (TH) |
|---|---|---|
| 0.0–3.0 | Hook: notifications flood a lock screen | ตลาดวิ่งเร็วทุกวินาที… คุณตามทันไหม? |
| 3.0–6.0 | Logo reveal, คุณควอนต์ | รู้จัก "คุณควอนต์" ผู้ช่วยดูแลพอร์ตส่วนตัว ที่อยู่ในแชทของคุณ |
| 6.0–11.0 | LINE: set a BTC price/RSI alert → alert fires → morning news digest | แค่พิมพ์ใน LINE หรือ Telegram ให้ช่วยเตือนราคา / หรือสรุปข่าว หุ้นเทค หุ้นไทย คริปโต ส่งถึงมือทันที |
| 11.0–15.5 | Telegram: one summary of every portfolio (SET / Webull / crypto) + rebalance tip | ถามคำเดียว ได้ภาพรวมครบทุกพอร์ต ทุกโบรกเกอร์ พร้อมคำแนะนำ rebalance |
| 15.5–19.5 | LINE: RSI-gated DCA in on BTC, laddered DCA out on ETH | ตั้ง DCA ทยอยซื้อ ตามเงื่อนไข RSI / หรือทยอยขายทำกำไร อัตโนมัติ |
| 19.5–23.0 | Telegram: SOL delta-neutral (spot long + perp short), net Δ 0, funding APR | สั่งเปิด Delta-Neutral เก็บค่า Funding โดยไม่ต้องลุ้นทิศทางตลาด |
| 23.0–26.5 | LINE: NVDA chart analysis + monthly trade journal | วิเคราะห์กราฟให้ แถมจดบันทึกกำไรขาดทุนทุกไม้ |
| 26.5–30.0 | CTA: logo, channels, open-source/local, khunquant.com | KhunQuant คุยง่าย จัดการครบ ทุกพอร์ตของคุณ / เริ่มเลยที่ khunquant.com |

Voice direction: friendly and confident, like a smart friend rather than a broker. Read คุณควอนต์ as "khun-quant". Read DCA as "ดี-ซี-เอ".
Each cue in the SRT fits a normal Thai speaking pace (about 6 syllables/s). If a take runs long, the scene that has the most room to absorb it is 11.0–15.5.

All prices, balances and news in the video are made up for the demo. A footer on screen says so ("ภาพจำลองเพื่อการสาธิต · ตัวเลขสมมติ · ไม่ใช่คำแนะนำการลงทุน").

## Adding the voice-over

```bash
# mux a recorded VO (and optional music bed) onto the video
ffmpeg -i khunquant-promo-30s.mp4 -i vo.wav -c:v copy -c:a aac -b:a 192k -shortest out-with-vo.mp4

# burn Thai subtitles in (needs a Thai-capable font, e.g. the bundled Kanit)
ffmpeg -i out-with-vo.mp4 -vf "subtitles=khunquant-promo-30s.th.srt:fontsdir=fonts:force_style='FontName=Kanit,FontSize=13,MarginV=70'" -c:a copy out-subbed.mp4
```

## Re-rendering

```bash
node render.mjs                         # ~3 min → khunquant-promo-30s.mp4
node render.mjs --stills 1.5,10.8,29.5  # PNG stills in stills/ for quick checks
open promo.html                         # live looping preview in a browser
```

You need `playwright` (with Chromium) and `ffmpeg`. Timings are set in the `data-t`, `data-s` and `data-e` attributes in `promo.html`. The SRTs line up with the scene windows, so if you move a scene, move its cues too.
