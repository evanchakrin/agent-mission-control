// Draw the Mission Control satellite mark and emit a real multi-size .ico.
// Done by hand rather than by rasterising the SVG so the installer can produce
// the icon on any machine with nothing but Node — no browser, no dependencies.
// ICO can carry uncompressed 32-bit BGRA DIBs, which skips PNG/zlib entirely.
'use strict';

const SS = 4;                                  // supersample factor -> anti-aliasing
const C = {
  bg:   [0x0a, 0x0d, 0x13],
  teal: [0x5e, 0xea, 0xd4],
  indi: [0x81, 0x8c, 0xf8],
  rose: [0xfb, 0x71, 0x85],
  blue: [0x60, 0xa5, 0xfa],
  plum: [0xc0, 0x84, 0xfc],
};

// distance helpers, all in the SVG's 64x64 coordinate space
const dist = (x, y, cx, cy) => Math.hypot(x - cx, y - cy);
function segDist(x, y, x1, y1, x2, y2) {
  const dx = x2 - x1, dy = y2 - y1;
  const L = dx * dx + dy * dy;
  let t = L ? ((x - x1) * dx + (y - y1) * dy) / L : 0;
  t = Math.max(0, Math.min(1, t));
  return Math.hypot(x - (x1 + t * dx), y - (y1 + t * dy));
}
function roundRect(x, y, w, h, r) {
  const cx = Math.min(Math.max(x, r), w - r);
  const cy = Math.min(Math.max(y, r), h - r);
  return dist(x, y, cx, cy) <= r;
}

// colour of one supersample point, or null for transparent
function sample(x, y) {
  if (!roundRect(x, y, 64, 64, 14)) return null;
  if (dist(x, y, 32, 9) <= 3.5) return C.rose;
  if (dist(x, y, 52, 42) <= 3.5) return C.blue;
  if (dist(x, y, 14, 44) <= 3.5) return C.plum;
  if (dist(x, y, 32, 32) <= 5) return C.indi;
  const ring = Math.abs(dist(x, y, 32, 32) - 17);
  if (ring <= 1.5) return C.teal;
  if (segDist(x, y, 32, 27, 32, 12.5) <= 1) return C.teal;
  if (segDist(x, y, 36, 35, 49, 41) <= 1) return C.teal;
  if (segDist(x, y, 28, 35, 16, 42) <= 1) return C.teal;
  return C.bg;
}

// one size -> BGRA rows, bottom-up as the DIB format requires
function render(size) {
  const px = Buffer.alloc(size * size * 4);
  const step = 64 / size;
  for (let py = 0; py < size; py++) {
    for (let pxi = 0; pxi < size; pxi++) {
      let r = 0, g = 0, b = 0, a = 0;
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const x = (pxi + (sx + 0.5) / SS) * step;
          const y = (py + (sy + 0.5) / SS) * step;
          const c = sample(x, y);
          if (c) { r += c[0]; g += c[1]; b += c[2]; a += 255; }
        }
      }
      const n = SS * SS;
      const row = size - 1 - py;                       // bottom-up
      const o = (row * size + pxi) * 4;
      const cov = a / n / 255;
      px[o] = cov ? Math.round(b / (a / 255)) : 0;     // B
      px[o + 1] = cov ? Math.round(g / (a / 255)) : 0; // G
      px[o + 2] = cov ? Math.round(r / (a / 255)) : 0; // R
      px[o + 3] = Math.round(cov * 255);               // A
    }
  }
  return px;
}

function dib(size, px) {
  const head = Buffer.alloc(40);
  head.writeUInt32LE(40, 0);
  head.writeInt32LE(size, 4);
  head.writeInt32LE(size * 2, 8);   // height is doubled: colour + (unused) mask
  head.writeUInt16LE(1, 12);
  head.writeUInt16LE(32, 14);
  head.writeUInt32LE(px.length, 20);
  const mask = Buffer.alloc(Math.ceil(size / 32) * 4 * size); // all zero = opaque
  return Buffer.concat([head, px, mask]);
}

function buildIco(sizes) {
  const imgs = sizes.map(s => dib(s, render(s)));
  const head = Buffer.alloc(6);
  head.writeUInt16LE(0, 0); head.writeUInt16LE(1, 2); head.writeUInt16LE(sizes.length, 4);
  const dir = Buffer.alloc(16 * sizes.length);
  let off = 6 + dir.length;
  sizes.forEach((s, i) => {
    const e = i * 16;
    dir[e] = s >= 256 ? 0 : s;      // 0 means 256
    dir[e + 1] = s >= 256 ? 0 : s;
    dir.writeUInt16LE(1, e + 4);
    dir.writeUInt16LE(32, e + 6);
    dir.writeUInt32LE(imgs[i].length, e + 8);
    dir.writeUInt32LE(off, e + 12);
    off += imgs[i].length;
  });
  return Buffer.concat([head, dir, ...imgs]);
}

module.exports = { buildIco };
if (require.main === module) {
  const out = process.argv[2];
  if (!out) { console.error('usage: node make-icon.js <out.ico>'); process.exit(1); }
  require('fs').writeFileSync(out, buildIco([16, 32, 48, 64, 128, 256]));
  console.log('wrote', out);
}
