# KhunQuant promo — 74s, 9:16, narrated

A vertical (1080×1920, 30 fps) motion-graphic promo narrated in Thai. It opens on the pain point, introduces the pixel mascot and the supported exchanges and brokers, then follows one continuous camera over a realistic iPhone. The phone shows KhunQuant at work in LINE, then switches to Telegram and Discord. The logo appears only on the end card.

It is a [HyperFrames](https://github.com/heygen-com/hyperframes) project: `index.html` is the root composition, rendered and mixed by `npx hyperframes render`. Loudness and the subtitle preview are handled with [ffmpeg-skill](https://github.com/kajisho5/ffmpeg-skill).

| File | What it is |
|---|---|
| `khunquant-promo.mp4` | **Final cut**: picture, voice-over, music and SFX, -14 LUFS |
| `khunquant-promo_preview-th-subs.mp4` | Same, with the Thai subtitles burned in |
| `khunquant-promo_video-only.mp4` | Same picture, silent |
| `khunquant-promo.th.srt` / `.en.srt` | Script and subtitle timing (TH / EN), timed to the voice-over |
| `index.html` | HyperFrames root composition. Every frame is a pure function of time, driven by the timeline and `hf-seek` |
| `assets/audio/vo.wav` | Voice-over: ElevenLabs `eleven_v4`, the owner's cloned voice "Arm", read from the TH SRT |
| `assets/audio/music.wav`, `assets/audio/sfx.wav` | Music bed and UI sounds, synthesised by `audio/make_audio.py` from `audio/cues.json` |
| `assets/fonts`, `assets/logos`, `assets/vendor` | Kanit / IBM Plex Sans Thai (OFL), brand marks, GSAP |
| `render.mjs` | `--cues` writes the sound-cue sheet from the timeline; `--stills 3,12.4` writes PNG stills to `stills/` |
| `build.sh` | Full rebuild |

## Voice-over and sync

- The narration was generated with the ElevenLabs MCP. It used the owner's cloned voice "Arm" and `eleven_v4`, reading the TH SRT text verbatim with light `[warmly]` / `[excited]` tags. Of the two takes, take 1 was kept. It runs about 70.5 s.
- The SRT text is the supplied script. The only change is that the TTS spelling "คุณคว้อน" is shown as "คุณควอนต์". Cue times are the speech boundaries measured in `vo.wav`. Local speech recognition was used only to find where each line falls, never to write text.
- Scenes are authored on their own clock. The `WARP` table in `index.html` maps video time to scene time, so each beat lands on its line: exchange tiles on their names, typing on "แค่พิมพ์สั่ง", the Rebalance tap on "แค่ปุ่มเดียว", the app switcher on "แค่สลับแอป". The sound cues go through the same map.
- If the VO is regenerated, re-measure the line boundaries, update `WARP` and the SRT times, then run `./build.sh`.

## Mix

The mix is set on the `<audio>` tags in `index.html` (HyperFrames Web Audio graph):

- **Voice:** highpass 80 Hz → +2.5 dB at 3 kHz → compressor → +5 dB → limiter.
- **Music:** volume lane at 0.32 under the voice, rising to 0.8 for the end card, then fading out.
- **SFX:** volume 0.3, which keeps the effects about 10 dB below the voice.

The render is then normalised to -14 LUFS / -1 dBTP with `ffmpeg-skill/scripts/loudness.py`. Levels were measured, not listened to.

## Logos

- `assets/logos/bitkub.png`, `set.png` and `webull.png` come from the artwork the project owner supplied.
- `binance.svg`, `okx.svg`, `line.svg`, `telegram.svg` and `discord.svg` are from [Simple Icons](https://simpleicons.org) (CC0 icon data).
- All marks remain trademarks of their owners.
- Binance TH is the Binance mark with a "TH" badge. Add a file and an entry in `LOGO_FILES` to give it its own.

## Disclaimers

- A footer runs during the demo: "ภาพจำลองเพื่อการสาธิต · ตัวเลขสมมติ · ไม่ใช่คำแนะนำการลงทุน".
- The end card carries "KhunQuant เป็นเพียง AI agent ผู้ช่วย ไม่ใช่ผู้แนะนำการลงทุน · การลงทุนมีความเสี่ยง ผู้ลงทุนควรศึกษาข้อมูลก่อนตัดสินใจลงทุน".
- All prices, balances, orders and news are made up.

## Rebuild

```bash
./build.sh                                # needs node 22+, ffmpeg, python3 + numpy, playwright, ffmpeg-skill
npx hyperframes check --json              # lint / layout / contrast / audio checks
npx hyperframes preview                   # live preview in the browser
```

`npx hyperframes check` reports no errors. It still warns that some lines are on screen for less time than its reading-time rule wants. That comes from the voice-led pace of a 74 s promo.
