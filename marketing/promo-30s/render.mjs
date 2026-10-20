// Render promo.html to an MP4 (1080x1920, 30 fps) by stepping the timeline frame by frame.
//
//   node render.mjs                     -> build/video-only.mp4 (+ audio/cues.json)
//   node render.mjs --stills 1,8,13     -> stills/t-<sec>.png only
//
// Requires playwright (Chromium) and ffmpeg on PATH.
import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const FPS = 30, DURATION = 30, W = 1080, H = 1920;
const args = process.argv.slice(2);
const stillsArg = args.includes('--stills') ? args[args.indexOf('--stills') + 1] : null;
const out = args.includes('--out') ? args[args.indexOf('--out') + 1] : join(here, 'build', 'video-only.mp4');

mkdirSync(join(here, 'audio'), { recursive: true });
mkdirSync(join(here, 'build'), { recursive: true });
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || undefined });
const page = await browser.newPage({ viewport: { width: W, height: H }, deviceScaleFactor: 1 });
await page.goto(pathToFileURL(join(here, 'promo.html')).href + '?static');
await page.waitForFunction(() => window.READY === true);
await page.waitForTimeout(200);
// sound-effect cue sheet, consumed by audio/make_audio.py
writeFileSync(join(here, 'audio', 'cues.json'), '[\n' + (await page.evaluate(() => window.CUES)).map(c => JSON.stringify(c)).join(',\n') + '\n]\n');

if (stillsArg) {
  mkdirSync(join(here, 'stills'), { recursive: true });
  for (const s of stillsArg.split(',').map(Number)) {
    await page.evaluate(t => window.render(t), s);
    await page.screenshot({ path: join(here, 'stills', `t-${s}.png`) });
  }
} else {
  const ff = spawn('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'image2pipe', '-framerate', String(FPS), '-c:v', 'mjpeg', '-i', '-',
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '20', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', out], { stdio: ['pipe', 'inherit', 'inherit'] });
  const total = FPS * DURATION;
  for (let i = 0; i < total; i++) {
    await page.evaluate(t => window.render(t), i / FPS);
    const buf = await page.screenshot({ type: 'jpeg', quality: 95 });
    if (!ff.stdin.write(buf)) await new Promise(r => ff.stdin.once('drain', r));
    if (i % 90 === 0) console.log(`frame ${i}/${total}`);
  }
  ff.stdin.end();
  await new Promise((res, rej) => ff.on('close', c => (c === 0 ? res() : rej(new Error('ffmpeg exit ' + c)))));
  console.log('wrote', out);
}
await browser.close();
