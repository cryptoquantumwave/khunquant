#!/usr/bin/env python3
"""Synthesize the promo's background music and sound effects from scratch.

Everything here is generated procedurally with numpy (no samples, no third-party
recordings), so the audio is free of third-party copyright.

    python3 audio/make_audio.py            # reads audio/cues.json (written by render.mjs)

Writes (48 kHz, 16-bit stereo WAV):
    audio/music.wav          soft lo-fi bed at about -24 LUFS, leaves room for a voice-over
    audio/sfx.wav            UI sounds synced to the animation cue sheet
    audio/music_sfx.wav      the two above mixed
"""
import json
import os
import wave

import numpy as np

SR = 48000
DUR = 30.0
N = int(SR * DUR)
HERE = os.path.dirname(os.path.abspath(__file__))
rng = np.random.default_rng(7)


# ---------------------------------------------------------------- helpers
def t_axis(d):
    return np.arange(int(SR * d)) / SR


def filt(x, lo=None, hi=None, order=2):
    """Zero-phase band filter in the frequency domain (Butterworth-shaped magnitude)."""
    X = np.fft.rfft(x)
    f = np.fft.rfftfreq(len(x), 1 / SR) + 1e-9
    H = np.ones_like(f)
    if hi:
        H /= np.sqrt(1 + (f / hi) ** (2 * order))
    if lo:
        H /= np.sqrt(1 + (lo / f) ** (2 * order))
    return np.fft.irfft(X * H, len(x))


def env(d, a=0.005, tau=0.2):
    t = t_axis(d)
    e = np.exp(-t / tau)
    na = max(1, int(a * SR))
    e[:na] *= np.linspace(0, 1, na)
    return e


def sweep(f0, f1, d, shape="exp"):
    t = t_axis(d)
    f = f0 * (f1 / f0) ** (t / d) if shape == "exp" else np.linspace(f0, f1, len(t))
    return np.sin(2 * np.pi * np.cumsum(f) / SR)


def square(f, d, duty=0.5):
    t = t_axis(d)
    return np.where((t * f) % 1 < duty, 1.0, -1.0)


def noise(d):
    return rng.standard_normal(int(SR * d))


def place(buf, x, at, gain=1.0, pan=0.0):
    i = int(at * SR)
    if i >= buf.shape[1]:
        return
    x = x[: buf.shape[1] - i]
    l, r = np.cos((pan + 1) * np.pi / 4), np.sin((pan + 1) * np.pi / 4)
    buf[0, i:i + len(x)] += x * gain * l * 1.414
    buf[1, i:i + len(x)] += x * gain * r * 1.414


def midi(n):
    return 440.0 * 2 ** ((n - 69) / 12)


def write(path, buf):
    pcm = (np.clip(buf, -1, 1) * 32767).astype("<i2").T.copy()
    with wave.open(path, "wb") as w:
        w.setnchannels(2)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def rms_db(x):
    return 20 * np.log10(np.sqrt(np.mean(x ** 2)) + 1e-12)


# ---------------------------------------------------------------- music
BPM = 96
BEAT = 60 / BPM
BAR = BEAT * 4
# Fmaj7 - Em7 - Dm7 - Cmaj7, voiced around middle C
CHORDS = [[53, 57, 60, 64], [52, 55, 59, 62], [50, 53, 57, 60], [48, 52, 55, 59]]
ROOTS = [41, 40, 38, 36]


def ep_note(f, d, vel):
    t = t_axis(d)
    tone = np.sin(2 * np.pi * f * t) + 0.28 * np.sin(4 * np.pi * f * t) + 0.08 * np.sin(6 * np.pi * f * t)
    trem = 1 + 0.12 * np.sin(2 * np.pi * 4.5 * t)
    return tone * trem * env(d, 0.006, 0.9) * vel


def pad_chord(notes, d):
    t = t_axis(d)
    x = np.zeros(len(t))
    for n in notes:
        f = midi(n + 12)
        for det in (-0.12, 0.12):
            ff = f * 2 ** (det / 12)
            for h in range(1, 6):
                x += np.sin(2 * np.pi * ff * h * t + rng.random() * 6.28) / h
    a = np.minimum(1, t / 0.6) * np.minimum(1, (d - t) / 0.5)
    return filt(x * a, hi=1400) * 0.05


def kick(d=0.35):
    return sweep(120, 42, d) * env(d, 0.002, 0.11)


def rim(d=0.12):
    return filt(noise(d), lo=900, hi=4000) * env(d, 0.001, 0.035) * 0.6 + np.sin(2 * np.pi * 1700 * t_axis(d)) * env(d, 0.001, 0.012) * 0.3


def hat(d=0.05):
    return filt(noise(d), lo=6000) * env(d, 0.001, 0.012)


def music(cues):
    buf = np.zeros((2, N))
    groove_from = 2.9           # drums enter with the phone
    end_hit = next((c["t"] for c in cues if c["type"] == "sparkle"), 27.3)
    nbars = int(np.ceil(DUR / BAR))
    for b in range(nbars):
        t0 = b * BAR
        ch = CHORDS[b % 4]
        last = t0 >= end_hit - BAR * 0.5
        if last:
            ch = [48, 52, 55, 59, 62]  # Cmaj9 to resolve on the logo
            t0 = end_hit
        d = BAR if not last else DUR - t0
        place(buf, pad_chord(ch, d + 0.4), t0, 1.0)
        # electric-piano stabs: beat 1 and the "and" of 2
        for off, vel in ((0, 0.11), (BEAT * 1.5, 0.07)) if not last else ((0, 0.13),):
            for k, n in enumerate(ch):
                place(buf, ep_note(midi(n), 2.2 if not last else d, vel), t0 + off + k * 0.012, 1.0, pan=-0.3 + 0.2 * k)
        # bass
        if t0 >= groove_from - 0.01 or last:
            place(buf, np.sin(2 * np.pi * midi(ROOTS[b % 4] if not last else 36) * t_axis(1.2)) * env(1.2, 0.01, 0.5), t0, 0.16)
            if not last:
                place(buf, np.sin(2 * np.pi * midi(ROOTS[b % 4]) * t_axis(0.8)) * env(0.8, 0.01, 0.35), t0 + BEAT * 2.5, 0.11)
        if last:
            break
        # drums
        for beat in range(4):
            tb = t0 + beat * BEAT
            if tb < groove_from:
                continue
            if beat in (0, 2):
                place(buf, kick(), tb, 0.32)
            if beat in (1, 3):
                place(buf, rim(), tb, 0.10, pan=0.15)
            for e8 in (0, 1):
                swing = 0.03 if e8 else 0
                place(buf, hat(), tb + e8 * BEAT / 2 + swing, 0.045 if e8 else 0.06, pan=0.35)
    # gentle lo-fi tone + fade out
    for c in range(2):
        buf[c] = filt(buf[c], lo=35, hi=7000)
    fade = np.ones(N)
    nf = int(1.2 * SR)
    fade[-nf:] = np.linspace(1, 0, nf) ** 1.5
    fade[: int(0.15 * SR)] = np.linspace(0, 1, int(0.15 * SR))
    buf *= fade
    buf *= 10 ** ((-26 - rms_db(buf)) / 20)   # quiet bed (~-24 LUFS) so a voice-over sits on top
    return buf


# ---------------------------------------------------------------- sfx
def sfx_sound(kind):
    if kind == "key":
        d = 0.03
        s = filt(noise(d), lo=1800, hi=6000) * env(d, 0.0005, 0.004)
        s += np.sin(2 * np.pi * rng.uniform(1500, 2100) * t_axis(d)) * env(d, 0.0005, 0.006) * 0.35
        return s * 0.32
    if kind == "kbup":
        d = 0.22
        return filt(noise(d), lo=2500, hi=9000) * np.sin(np.linspace(0, np.pi, int(SR * d))) * 0.12
    if kind == "send":
        d = 0.16
        return sweep(520, 1500, d) * env(d, 0.004, 0.06) * 0.45 + filt(noise(d), lo=3000) * env(d, 0.002, 0.04) * 0.08
    if kind == "recv":
        d = 0.18
        a = sweep(680, 1080, 0.07) * env(0.07, 0.002, 0.03)
        b = sweep(1050, 1250, 0.11) * env(0.11, 0.002, 0.05)
        s = np.zeros(int(SR * d))
        s[: len(a)] += a
        s[int(0.06 * SR): int(0.06 * SR) + len(b)] += b * 0.8
        return s * 0.45
    if kind == "chime":
        d = 1.1
        s = np.zeros(int(SR * d))
        for k, f in enumerate((1318.5, 1975.5)):
            t = t_axis(d - k * 0.1)
            tone = np.sin(2 * np.pi * f * t) + 0.25 * np.sin(2 * np.pi * f * 2.76 * t) * np.exp(-t / 0.08)
            s[int(k * 0.1 * SR):] += tone * env(d - k * 0.1, 0.002, 0.35)
        return s * 0.32
    if kind == "tap":
        d = 0.06
        return (filt(noise(d), lo=1500, hi=5000) * env(d, 0.0005, 0.005) * 0.6
                + np.sin(2 * np.pi * 220 * t_axis(d)) * env(d, 0.001, 0.02) * 0.5) * 0.6
    if kind == "lock":
        d = 0.12
        c = filt(noise(d), lo=400, hi=3000) * env(d, 0.0005, 0.008)
        s = c.copy()
        s[int(0.035 * SR):] += c[: len(c) - int(0.035 * SR)] * 0.7
        return s * 0.7
    if kind == "unlock":
        d = 0.4
        return filt(noise(d), lo=1200, hi=7000) * np.sin(np.linspace(0, np.pi, int(SR * d))) ** 2 * 0.22 + sweep(400, 900, d) * env(d, 0.05, 0.15) * 0.08
    if kind == "whoosh":
        d = 0.65
        x = noise(d)
        out = np.zeros_like(x)
        segs = 8
        for i in range(segs):  # moving band
            a, b = i * len(x) // segs, (i + 1) * len(x) // segs
            out[a:b] = filt(x, lo=300 + i * 250, hi=1200 + i * 700)[a:b]
        return out * np.sin(np.linspace(0, np.pi, len(x))) ** 2 * 0.28
    # 8-bit voice for the pixel mascot
    if kind == "pop":
        return filt(np.sign(sweep(300, 760, 0.18)), hi=5000) * env(0.18, 0.003, 0.08) * 0.16
    if kind == "bubble":
        return np.sign(sweep(880, 620, 0.07)) * env(0.07, 0.002, 0.03) * 0.09
    if kind == "jump":
        return filt(np.sign(sweep(260, 980, 0.28)), hi=5000) * env(0.28, 0.003, 0.12) * 0.14
    if kind == "peek":
        a = square(1046.5, 0.05, 0.25) * env(0.05, 0.002, 0.03)
        b = square(1568.0, 0.07, 0.25) * env(0.07, 0.002, 0.04)
        s = np.concatenate([a, b])
        return filt(s, hi=6000) * 0.07
    if kind == "sparkle":
        d = 1.6
        s = np.zeros(int(SR * d))
        for k, n in enumerate((84, 88, 91, 96, 100)):
            t = t_axis(d - k * 0.07)
            s[int(k * 0.07 * SR):] += np.sin(2 * np.pi * midi(n) * t) * env(d - k * 0.07, 0.002, 0.45)
        s += filt(noise(d), lo=7000) * env(d, 0.01, 0.4) * 0.15
        return s * 0.22
    raise ValueError(kind)


def sfx(cues):
    buf = np.zeros((2, N))
    for c in cues:
        k = c["type"]
        pan = rng.uniform(-0.15, 0.15) if k == "key" else 0.0
        g = rng.uniform(0.75, 1.0) if k == "key" else 1.0
        place(buf, sfx_sound(k), c["t"], g, pan)
    return buf


def main():
    with open(os.path.join(HERE, "cues.json")) as f:
        cues = json.load(f)
    m = music(cues)
    s = sfx(cues)
    mix = m + s
    peak = np.max(np.abs(mix))
    if peak > 0.89:  # keep 1 dB of headroom
        g = 0.89 / peak
        m, s, mix = m * g, s * g, mix * g
    write(os.path.join(HERE, "music.wav"), m)
    write(os.path.join(HERE, "sfx.wav"), s)
    write(os.path.join(HERE, "music_sfx.wav"), mix)
    print(f"music {rms_db(m):.1f} dBFS RMS, sfx peak {20*np.log10(np.max(np.abs(s))+1e-12):.1f} dBFS, mix peak {20*np.log10(np.max(np.abs(mix))):.1f} dBFS, {len(cues)} cues")


if __name__ == "__main__":
    main()
