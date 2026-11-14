---
name: promo-video
description: Make a narrated motion-graphic promo / product / explainer video end to end (vertical 9:16 by default) with the owner's house stack — HyperFrames to render and mix, ElevenLabs MCP for the voice-over in the owner's cloned voice "Arm", ffmpeg-skill for loudness, subtitle burn-in and checks. Use whenever the user asks to make, re-voice, retime or polish a promo video, motion graphic, product demo, feature reel, explainer, ad or social clip ("ทำวิดีโอ", "วิดีโอโปรโมท", "motion graphic", "พากย์เสียง", "ใส่ซับ"), even if they do not name any tool.
---

# Promo video: house workflow

The user has already chosen the tools and the style. Do not ask which tools to use; set them up and go.
The reference build is `marketing/promo/` in `cryptoquantumwave/khunquant` (the KhunQuant 74 s promo, [PR #117](https://github.com/cryptoquantumwave/khunquant/pull/117)). `templates/` holds copies of its working files:
- `example-index.html`: a full HyperFrames composition with phone, chat, keyboard, lock screen, app switcher, mascot, camera and `WARP`.
- `render.mjs`: writes the cue sheet and stills.
- `make_audio.py`: the synthesised music and SFX.
- `build.sh`: the full pipeline.

Start a new film by copying and adapting these rather than writing from scratch.

## 0. Tools (install once per machine/container)

```bash
npx -y hyperframes@0.9.1 skills update     # installs /hyperframes, /hyperframes-cli, /hyperframes-registry, /media-use
npx -y ffmpeg-skill && npx -y ffmpeg-skill doctor
```

- Then read the `hyperframes` skill (contract, `references/types/vertical.md`, `sound.md`, `format.md`, `runtimes.md`) and use `ffmpeg-skill` for any file-level audio/video job.
- **ElevenLabs MCP** (`mcp__ElevenLabs__*`) makes the voice-over. Load the tools with ToolSearch.

## 1. Brief → script → SRT

- **The user supplies an SRT/script:** use its text verbatim. **Never re-transcribe it or rewrite it.** Speech recognition is only ever used to *locate* lines in time.
- **No script supplied:** draft one per scene. Each cue fits normal Thai pace (about 6 syllables/s). Show it before generating the voice.
- **Always add disclaimers** for finance content:
  - Footer during the demo: "ภาพจำลองเพื่อการสาธิต · ตัวเลขสมมติ · ไม่ใช่คำแนะนำการลงทุน".
  - End-card line: "… เป็นเพียง AI agent ผู้ช่วย ไม่ใช่ผู้แนะนำการลงทุน · การลงทุนมีความเสี่ยง ผู้ลงทุนควรศึกษาข้อมูลก่อนตัดสินใจลงทุน".
  - Give the end-card line ≥ 5 s of hold before the film ends.

## 2. Voice-over (ElevenLabs)

- **Default voice:** the owner's clone **"Arm"**, `voice_id ehCAqFhUY5Wc79SLaGyn` (Thai, male). Confirm it still exists with `creative_list_voices`. **Model:** `eleven_v4` (speaks Thai, takes `[warmly]` / `[excited]` tags). Keep the default 2 takes; never call again to "retry" (it spends credits).
- **Prompt:** the cue texts in order, one paragraph per cue, as written (including the user's phonetic spellings such as "คุณคว้อน"), with a few delivery tags where the mood changes.
- **Poll and download:** poll `creative_get_flow_run_status`, then `curl` both `content_url`s into a scratch dir.
- **Pick a take** by measurement:
  - `ffmpeg ebur128` loudness and peak;
  - silence-gap pattern;
  - all words present (local ASR spot check).
  - Say which take was picked and why.
- **The VO length sets the film length.** If it overruns the requested length, say so and offer to trim the script.
- **Line timings:**
  - `silencedetect=n=-33dB:d=0.12` gives the candidate gaps. This VO voice rarely pauses between lines.
  - To map gaps to lines, transcribe each gap-to-gap chunk locally with **sherpa-onnx + whisper-turbo**: `pip install sherpa-onnx`; model from `https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-whisper-turbo.tar.bz2` (Hugging Face is blocked in the cloud sandbox; GitHub works).
  - Pad each chunk with 0.3 s of silence before and 2 s after, plus `tail_paddings=2000`, or the decoder truncates.
  - Write the SRT with the user's text and the measured start/end times. Keep an EN SRT on the same times.

## 3. Picture (HyperFrames project)

- **Layout:**
  - `index.html` is the root: `#root` `data-composition-id="main"`, `data-duration`, 1080×1920.
  - Everything lives under `assets/` (fonts, logos, `vendor/gsap.min.js` from `npm pack gsap@3.15.0`; the CDN is blocked in the cloud).
  - `<audio>` tags for `vo`, `music`, `sfx`.
- **Deterministic frames:**
  - One `renderScene(t)` draws everything from time alone.
  - Drive it from a paused GSAP timeline `onUpdate` and from `hf-seek`.
  - Register `window.__timelines.main` at the end of the async init.
  - No rAF or timers.
  - Name the scene function `renderScene`: a global `function render` collides with `window.render`.
- **Sync to voice:**
  - Scenes keep their own clock. A `WARP` table of `[videoTime, sceneTime]` anchors lands each beat on its word (logos on their names, typing on "พิมพ์", the tap on "ปุ่มเดียว"…).
  - Keep segment rates within about 0.6–1.6×.
  - Map sound cues through the same table.
- **House style (the user loved it; keep it unless asked otherwise):**
  - **Look:** minimal light theme: bg `#f3f4f8` with a soft radial; ink `#16173a`; accent `#5c6bc0` / `#3e4da0` / `#e8eaf6`. No busy backgrounds or tickers.
  - **Camera:** one continuous camera, no cuts. It pushes in on the input box while typing and on a button before a tap, and pulls back for cards.
  - **Phone:** realistic iPhone with a titanium frame, side buttons, Dynamic Island, glare, real status-bar icons and a home bar.
  - **Chat:** LINE-style chat with read receipts, timestamps and Flex-style card buttons. A Thai Kedmanee keyboard slides up, keys press per grapheme (`Intl.Segmenter`) and each press has a key sound.
  - **Notifications:** lock-screen push notifications from the product, plus a proactive banner.
  - **Mascot:** a cartoon mascot carries the opening and middle. For KhunQuant it is the 13×11 pixel mascot from `web/frontend/public/khunquant-logo.png`, drawn as an SVG grid with blink and wave. The logo appears only on the end card (mascot → flash → logo).
  - **Story:** opens on the pain point, then the solution, the use cases, an app switcher (LINE → Telegram → Discord) and the end card (logo, partner logos, channels, URL, disclaimer).
  - **Fonts:** Kanit (display) + IBM Plex Sans Thai (UI) from `@fontsource` (npm). Split thai/latin `@font-face` with `unicode-range`, otherwise Latin falls back to a serif.
  - **Brand logos:** real marks only. Simple Icons (npm `simple-icons`) has Binance, OKX, LINE, Telegram, Discord. Ask the user for the rest (Bitkub, SET, Webull were supplied as images). Crop to the mark, make the white background transparent, list the files explicitly (no 404 probing).
- **Review:**
  - `node render.mjs --stills a,b,c`, montage the stills, look at them, fix overlaps and zoom framing.
  - Then `npx hyperframes check --json`: fix every error (contrast AA, first frame not empty, 404s); weigh the warnings.

## 4. Sound

- **Music and SFX:** `audio/make_audio.py` synthesises the music bed (darker intro, groove when the hero appears, resolve on the logo) and the UI SFX from `audio/cues.json`. Offer the HeyGen music catalog once (`npx hyperframes auth login`, typed by the user).
- **Mix:** set it on the `<audio>` tags.
  - **VO:** highpass 80 → peaking 3 kHz +2.5 → compressor → +5 dB → limiter.
  - **Music:** `data-automation` volume 0.32 under speech, up for the end card, `data-fade-out`.
  - **SFX:** 0.3.
- **The voice must always be clearly on top.** The user rejected a mix where effects masked the speech. Keep the SFX about 10 dB under the voice and the bed about 20 dB under.
- **Final loudness:** `python3 ~/.claude/skills/ffmpeg-skill/scripts/loudness.py in.mp4 -I -14 --tp -1 -o out.mp4`. Report the measured numbers and say the mix was measured, not listened to.

## 5. Render and deliver

```bash
npx -y hyperframes@0.9.1 render -o build/hf-render.mp4 -q delivery --skill hyperframes --quiet   # ~4 min for 74 s
python3 $FFS/loudness.py build/hf-render.mp4 -I -14 --tp -1 -o <name>.mp4 --overwrite
python3 $FFS/caption.py <name>.mp4 --srt <name>.th.srt:th --font Loma --size 13 --outline 2.5 -o <name>_preview-th-subs.mp4
ffmpeg -i <name>.mp4 -an -c:v copy <name>_video-only.mp4
```

- Verify with `ffprobe` (duration, streams) and frames grabbed from the final file.
- Send the final, the subtitle preview, the SRT and the VO with `SendUserFile`.
- When working in a repo, commit the source plus the deliverables and push to the working branch.

## Pitfalls already hit

- The cloud proxy blocks Hugging Face, jsdelivr and unpkg. npm, PyPI and GitHub releases work.
- `npx hyperframes init` needs `--non-interactive --example blank --resolution portrait`.
- `caption.py` re-chunks long cues into shorter lines. That is fine for a preview.
- An `<audio>` without an `id` drops out of the HyperFrames mix.
- `pw()` passes negative scene times straight through. Skip cues with t ≤ 0, or the SFX placement breaks.
