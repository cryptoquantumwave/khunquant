# KhunQuant promo — 30s, 9:16

A 30-second vertical (1080×1920, 30 fps) motion-graphic promo with a minimal, light look. It is shot as one continuous camera move over a realistic iPhone running a LINE chat with KhunQuant. The camera pushes in on the input box while the user types, with a Thai keyboard and key sounds. The pixel mascot carries the opening and middle of the clip, and the logo appears only at the end.

| File | What it is |
|---|---|
| `khunquant-promo-30s.mp4` | Final video with background music and SFX, **no voice**. Ready to drop a VO on top |
| `khunquant-promo-30s_video-only.mp4` | Same picture, silent, for editors who build their own audio |
| `khunquant-promo-30s_preview-th-subs.mp4` | Timing check only: the Thai SRT burned in |
| `khunquant-promo-30s.th.srt` / `.en.srt` | Voice-over script and subtitle timing (TH / EN) |
| `audio/music.wav` | Music bed only, about -24 LUFS |
| `audio/sfx.wav` | Sound effects only (keys, send, receive, notification chimes, taps, whooshes, mascot blips) |
| `audio/music_sfx.wav` | The two above mixed (the audio in the final MP4) |
| `add-voiceover.sh` | One command to add a recorded VO: ducks the music under the voice and normalises to -14 LUFS |
| `promo.html` | Animation source. Every frame is a pure function of `render(t)` |
| `render.mjs` | Steps the timeline in headless Chromium, pipes frames to ffmpeg, writes `audio/cues.json` |
| `audio/make_audio.py` | Synthesises the music and SFX from the cue sheet with numpy |
| `build.sh` | Full rebuild (render → audio → mux) |

**Audio licensing.** The music and every sound effect are generated procedurally by `audio/make_audio.py`. There are no samples or third-party recordings, so there is nothing to license or credit. The fonts (Kanit, IBM Plex Sans Thai) are SIL OFL 1.1.

## Storyboard and voice-over

| Time | Shot | Voice-over (TH) |
|---|---|---|
| 0.0–2.9 | Mascot pops in, waves: "ตลาดวิ่งเร็วทุกวินาที… / ให้ผมช่วยดูพอร์ตให้นะ!" | ตลาดวิ่งเร็วทุกวินาที… ให้คุณควอนต์ช่วยดูพอร์ตให้สิ |
| 2.9–3.6 | Mascot hops into the chat avatar as the phone rises | — |
| 3.6–8.2 | Camera pushes in on the input box; user types an alert request on the Thai keyboard, sends; bot confirms | แค่พิมพ์สั่งในแชท LINE ว่าอยากให้เตือนอะไร / แล้วผมจะเฝ้าตลาดให้ 24 ชั่วโมง |
| 8.2–12.5 | Phone locks; KhunQuant push notifications land on the lock screen (BTC alert, NVDA, SET, DCA filled); mascot peeks out; tap opens the chat | ราคาถึงเป้า ข่าวหุ้นเทค หุ้นไทย คริปโต… เด้งเตือนถึงมือทันที |
| 12.5–16.2 | Types "สรุปพอร์ตทั้งหมดให้หน่อย" → portfolio card (SET / Webull / crypto) | อยากรู้ภาพรวม ถามคำเดียว สรุปครบทุกพอร์ต ทุกโบรกเกอร์ |
| 16.2–20.5 | Types a DCA request → DCA In (RSI-gated weeks) + DCA Out (take-profit ladder) | ตั้ง DCA ทยอยซื้อ ทยอยขาย อัตโนมัติ ตามสัญญาณอินดิเคเตอร์ |
| 20.5–23.5 | Camera pushes onto the card button, taps "เปิด Delta-Neutral" → hedge card; the balance beam levels to Δ 0 | เปิด Delta-Neutral เก็บค่า Funding โดยไม่ต้องลุ้นทิศทางตลาด |
| 23.5–26.6 | Taps "วิเคราะห์กราฟ NVDA" → candlestick chart draws in; P&L journal card | วิเคราะห์กราฟให้ แถมจดบันทึกกำไรขาดทุนทุกไม้ |
| 26.6–30.0 | Phone drops away, mascot jumps to centre and flashes into the KhunQuant logo; channels + khunquant.com | KhunQuant ผู้ช่วยดูแลพอร์ตส่วนตัวของคุณ / เริ่มเลยที่ khunquant.com |

Voice direction: friendly and confident, like a smart friend rather than a broker. Read คุณควอนต์ as "khun-quant" and DCA as "ดี-ซี-เอ". Each cue fits a normal Thai speaking pace. Scenes 4–6 have the most room if a take runs long.

All prices, balances and news are made up for the demo. A footer on screen says so.

## Adding the voice-over

```bash
./add-voiceover.sh vo.wav                 # -> khunquant-promo-30s_with-vo.mp4
```

The script mixes `music.wav` (sidechain-ducked by the voice), `sfx.wav` and your VO onto the silent video, then normalises to -14 LUFS / -1.5 dBTP. The VO should start at 0:00 and follow `khunquant-promo-30s.th.srt`.
In CapCut or Premiere you can instead put `khunquant-promo-30s.mp4` on the timeline and add the VO track. The bed is already quiet enough to talk over.

To burn Thai subtitles in:

```bash
ffmpeg -i khunquant-promo-30s_with-vo.mp4 -vf "subtitles=khunquant-promo-30s.th.srt:force_style='FontName=Kanit,FontSize=10,Bold=1,Outline=2.5,MarginV=60'" -c:a copy out-subbed.mp4
```

## Re-rendering

```bash
./build.sh                               # ~3 min, needs playwright (Chromium), ffmpeg, python3 + numpy
node render.mjs --stills 1.2,9.6,29.5    # PNG stills in stills/ for quick checks
open promo.html                          # live looping preview in a browser (silent)
```

Timings live in `promo.html`:
- `ITEMS`: chat messages, their appear and typing windows
- `NOTIFS`: lock-screen pushes
- `TAPS`, `PEEKS` and `TITLES`
- `CAM`: camera keyframes, inside `setup()`

Sound cues are derived from the same timeline, so a re-render keeps them in sync. If you move a scene, move its SRT cues too.
