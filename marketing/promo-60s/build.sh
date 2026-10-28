#!/usr/bin/env bash
# Full rebuild: frames -> video, cue sheet -> music + SFX, then mux.
set -euo pipefail
cd "$(dirname "$0")"
node render.mjs                      # build/video-only.mp4 + audio/cues.json
python3 audio/make_audio.py          # audio/music.wav, sfx.wav, music_sfx.wav
cp build/video-only.mp4 khunquant-promo-60s_video-only.mp4
ffmpeg -y -v error -i khunquant-promo-60s_video-only.mp4 -i audio/music_sfx.wav \
  -map 0:v -map 1:a -c:v copy -c:a aac -b:a 192k -shortest -movflags +faststart khunquant-promo-60s.mp4
echo "wrote khunquant-promo-60s.mp4"
if [ -f audio/vo.wav ]; then
  ./add-voiceover.sh audio/vo.wav khunquant-promo-60s_with-vo.mp4
fi
