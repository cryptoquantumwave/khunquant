#!/usr/bin/env bash
# Mix a recorded voice-over onto the promo.
# The music is ducked automatically under the voice; SFX stay as they are.
# The result is loudness-normalised for social platforms (-14 LUFS, -1.5 dBTP).
#
#   ./add-voiceover.sh vo.wav [out.mp4]
#
# vo.wav: any format ffmpeg reads; start it at 0:00 and follow the timing in khunquant-promo-30s.th.srt.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
vo="${1:?usage: $0 vo.wav [out.mp4]}"
out="${2:-$here/khunquant-promo-30s_with-vo.mp4}"
ffmpeg -y -hide_banner -loglevel error \
  -i "$here/khunquant-promo-30s_video-only.mp4" -i "$here/audio/music.wav" -i "$here/audio/sfx.wav" -i "$vo" \
  -filter_complex "\
[3:a]aformat=sample_rates=48000:channel_layouts=stereo,apad,atrim=0:30,asplit=2[vo][key];\
[1:a][key]sidechaincompress=threshold=0.02:ratio=6:attack=30:release=450:makeup=1[duck];\
[duck][2:a][vo]amix=inputs=3:normalize=0,loudnorm=I=-14:TP=-1.5:LRA=11,aresample=48000[a]" \
  -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 192k -shortest -movflags +faststart "$out"
echo "wrote $out"
