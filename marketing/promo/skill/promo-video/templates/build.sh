#!/usr/bin/env bash
# Full rebuild: sound cues -> music + SFX -> HyperFrames render (picture + VO/music/SFX mix)
# -> loudness to -14 LUFS (ffmpeg-skill) -> Thai-subtitle preview and a silent copy.
set -euo pipefail
cd "$(dirname "$0")"
FFS="${FFMPEG_SKILL:-$HOME/.claude/skills/ffmpeg-skill/scripts}"
ln -sfn "$(npm root -g)" node_modules 2>/dev/null || true
node render.mjs --cues                       # audio/cues.json from the timeline in index.html
rm -f node_modules
python3 audio/make_audio.py                  # assets/audio/music.wav, assets/audio/sfx.wav
npx -y hyperframes@0.9.1 render -o build/hf-render.mp4 -q delivery --skill hyperframes --quiet
python3 "$FFS/loudness.py" build/hf-render.mp4 -I -14 --tp -1 -o khunquant-promo.mp4 --overwrite
python3 "$FFS/caption.py" khunquant-promo.mp4 --srt khunquant-promo.th.srt:th --font Loma --size 13 --outline 2.5 \
  -o khunquant-promo_preview-th-subs.mp4 --overwrite
ffmpeg -v error -y -i khunquant-promo.mp4 -an -c:v copy khunquant-promo_video-only.mp4
echo "wrote khunquant-promo.mp4"
