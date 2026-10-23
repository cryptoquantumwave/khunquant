#!/usr/bin/env bash
# Mix a recorded voice-over onto the promo.
# The voice is levelled to -16 LUFS; music (MUSIC_DB, default -6 dB) and SFX (SFX_DB, default -10 dB)
# are turned down and ducked further while the voice is speaking.
# The result is loudness-normalised for social platforms (-14 LUFS, -1.5 dBTP).
#
#   ./add-voiceover.sh vo.wav [out.mp4]
#
# vo.wav: any format ffmpeg reads; start it at 0:00 and follow the timing in khunquant-promo-60s.th.srt.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
vo="${1:?usage: $0 vo.wav [out.mp4]}"
out="${2:-$here/khunquant-promo-60s_with-vo.mp4}"
ffmpeg -y -hide_banner -loglevel error \
  -i "$here/khunquant-promo-60s_video-only.mp4" -i "$here/audio/music.wav" -i "$here/audio/sfx.wav" -i "$vo" \
  -filter_complex "\
[3:a]aformat=sample_rates=48000:channel_layouts=stereo,apad,atrim=0:60,highpass=f=80,acompressor=threshold=-24dB:ratio=3:attack=5:release=120:makeup=2,loudnorm=I=-16:TP=-2:LRA=7,asplit=3[vo][k1][k2];\
[1:a]volume=${MUSIC_DB:--6}dB[mus];[mus][k1]sidechaincompress=threshold=0.015:ratio=10:attack=20:release=500[duck];\
[2:a]volume=${SFX_DB:--10}dB[fx];[fx][k2]sidechaincompress=threshold=0.03:ratio=3:attack=10:release=300[fxd];\
[duck][fxd][vo]amix=inputs=3:normalize=0,alimiter=limit=0.89,loudnorm=I=-14:TP=-1.5:LRA=11,aresample=48000[a]" \
  -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 192k -shortest -movflags +faststart "$out"
echo "wrote $out"
