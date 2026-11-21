/* cardmotion.js: card motion for a table page, as an ES module.

   Carried over from an earlier card-game table page of ours (its "frame loop" and "event
   queue" sections), chosen for its card feel, and made independent of that game:

   - Every sprite (a card, a chip stack) has a logical SLOT, asked of the page every frame, and a VISUAL that
     chases it with a slightly under-damped spring: k 500, c 30, about 6% overshoot on landing = the 'snick'.
     Fixed 1/240 s substeps, so it is the same at any frame rate. The slot/visual split comes from Mix and
     Jam's MIT-licensed Balatro-feel recreation (github.com/mixandjam/balatro-feel, CardVisual.cs); no code
     was copied. v2 has the same constants.
   - A card flips by the same spring on f (0 face up, 1 face down), which turns its .flip element (front and
     back faces) about Y.
   - One clock, scaled by speed (0.5x-2x), runs the springs, the holds (a sprite waits in place, e.g. a deal
     stagger) and the beats between events.
   - An event queue: each event is a function that starts some motion and returns the beat (ms) to wait
     before the next. The page locks input while it runs; skip() drains it with no waits and snaps every
     sprite home.
   - Reduced motion: sprites snap to their slots, and one that jumped far fades in instead of moving. The
     beats stay, so the order still reads.

   An earlier table page of ours keeps its own copy of this code, which deploys on its own; this
   module is the same motion with the game taken out, so a later v2 could import it. */

export const FEEL = { k: 500, c: 30, fly: 24, step: 1 / 240, fade: 30 };
const KEYS = ['x', 'y', 'r', 's', 'f'];

// Damped spring from p towards q, velocity v, over dt seconds of the scaled clock.
export function spring(p, v, q, dt, keys = KEYS) {
  for (let left = dt; left > 1e-6; left -= FEEL.step) {
    const h = Math.min(left, FEEL.step);
    for (const k of keys) { v[k] += (FEEL.k * (q[k] - p[k]) - FEEL.c * v[k]) * h; p[k] += v[k] * h; }
  }
}

// A card element: .card > .flip > .face.front + .face.back. The page fills the faces.
export function cardElement(className = 'card') {
  const el = document.createElement('div');
  el.className = className;
  el.innerHTML = '<div class="flip"><div class="face front"></div><div class="face back"></div></div>';
  return el;
}

export class Motion {
  /* root: the element sprites live in (position: relative or absolute; slots are in its px).
     slot(sprite) -> {x, y, r, s, z} (centre, degrees, scale, stacking) or null to stay put. */
  constructor(root, slot) {
    this.root = root; this.slot = slot;
    this.sprites = new Set();
    this.clock = 0; this.speed = 1; this.rm = false; this.skipping = false;
    this.queue = []; this.busy = false; this.seq = 0;
    this.waitUntil = null; this.waitRes = null; this.last = 0;
    this.onIdle = () => {};
  }

  /* A sprite for el, w x h px unscaled. o.at: where it starts ({x, y, r, s}); o.up: face up (cards). */
  add(el, w, h, o = {}) {
    const at = o.at || { x: 0, y: 0, r: 0, s: 1 };
    const sp = { el, w, h, up: !!o.up, hold: 0, seq: ++this.seq, arrive: null, flipEl: el.querySelector('.flip'),
      p: { x: at.x, y: at.y, r: at.r || 0, s: at.s == null ? 1 : at.s, f: o.up ? 0 : 1 }, v: { x: 0, y: 0, r: 0, s: 0, f: 0 },
      t: null, tf: '', ff: '', z: null, air: null };
    this.sprites.add(sp); this.root.appendChild(el); this.render(sp);
    return sp;
  }
  remove(sp) { sp.el.remove(); this.sprites.delete(sp); }

  /* Send a sprite off: it waits delay ms where it is, then chases its slot. arrive() runs once when it has
     landed (a chip merging into a stack, a card into the deck). */
  go(sp, o = {}) {
    sp.hold = this.clock + (o.delay || 0) / 1000;
    sp.seq = ++this.seq;
    if (o.up != null) sp.up = o.up;
    sp.arrive = o.arrive || null;
  }

  start() {
    const frame = now => { this.tick(now); requestAnimationFrame(frame); };
    requestAnimationFrame(frame);
    // rAF stops in background tabs and headless capture; keep the clock going.
    setInterval(() => { if (performance.now() - this.last > 60) this.tick(performance.now()); }, 40);
  }
  tick(now) { this.step(Math.min(0.1, this.last ? (now - this.last) / 1000 : 0)); this.last = now; }

  step(raw) {
    const dt = raw * this.speed; this.clock += dt;
    const snap = this.rm || this.skipping;
    for (const sp of [...this.sprites]) {
      const held = sp.hold > this.clock;
      if (!held || !sp.t) {
        const q = this.slot(sp);
        if (q) {
          const old = sp.t; sp.t = { ...q, f: sp.up ? 0 : 1 };
          if (this.rm && !this.skipping && old && Math.hypot(old.x - q.x, old.y - q.y) > FEEL.fade) sp.el.animate([{ opacity: 0 }, { opacity: 1 }], 200);
        }
      }
      if (!sp.t) continue;
      const p = sp.p, q = sp.t, v = sp.v;
      if (snap && !held) { for (const k of KEYS) { p[k] = q[k]; v[k] = 0; } }
      else if (!held) spring(p, v, q, dt);
      const dist = this.render(sp);
      if (sp.arrive && !held && dist < 1.5 && Math.abs(v.x) + Math.abs(v.y) < 30) { const fn = sp.arrive; sp.arrive = null; fn(sp); }
    }
    if (this.waitUntil != null && this.clock >= this.waitUntil) this.release();
  }

  render(sp) {                                   // transform and opacity only; returns the distance to the slot
    const p = sp.p, q = sp.t || p, dist = Math.hypot(q.x - p.x, q.y - p.y), flying = dist > FEEL.fly;
    const z = Math.round(flying ? 30000 + (sp.seq % 1000) : (q.z || 0) * 10);
    const tf = `translate3d(${(p.x - sp.w / 2).toFixed(1)}px,${(p.y - sp.h / 2).toFixed(1)}px,0) rotate(${p.r.toFixed(2)}deg) scale(${p.s.toFixed(4)})`;
    if (tf !== sp.tf) { sp.el.style.transform = tf; sp.tf = tf; }
    if (sp.flipEl) {
      const ff = `rotateY(${(Math.max(0, Math.min(1, p.f)) * 180).toFixed(1)}deg)`;
      if (ff !== sp.ff) { sp.flipEl.style.transform = ff; sp.ff = ff; }
    }
    if (z !== sp.z) { sp.el.style.zIndex = z; sp.z = z; }
    if (flying !== sp.air) { sp.el.classList.toggle('air', flying); sp.air = flying; }
    return dist;
  }

  // Every sprite home now: holds dropped, springs skipped, pending arrivals run.
  snapAll() {
    for (const sp of this.sprites) sp.hold = 0;
    const was = this.skipping; this.skipping = true; this.step(0); this.skipping = was;
  }

  /* ---------- the event queue ---------- */
  wait(ms) { return new Promise(r => { this.waitRes = r; this.waitUntil = this.clock + ms / 1000; }); }
  release() { const r = this.waitRes; this.waitUntil = this.waitRes = null; if (r) r(); }

  // Add events (functions returning a beat in ms) and run them in order.
  enqueue(...events) { this.queue.push(...events); if (!this.busy) this.run(); }
  async run() {
    this.busy = true;
    while (this.queue.length) {
      const ev = this.queue.shift();
      let ms = 0;
      try { ms = ev() || 0; } catch (x) { console.error(x); }
      if (ms && !this.skipping) await this.wait(ms);
    }
    if (this.skipping) { this.skipping = false; this.snapAll(); }
    this.busy = false;
    this.onIdle();
  }
  // Drain the queue at once: the remaining events still run, with no beats, and everything lands.
  skip() { if (!this.busy) return; this.skipping = true; this.release(); }

  // Nothing queued, nothing held, every sprite at rest on its slot (tests: no card left stuck mid-air).
  settled() {
    if (this.busy || this.queue.length) return false;
    for (const sp of this.sprites) {
      if (sp.hold > this.clock || sp.arrive || !sp.t) return false;
      const p = sp.p, q = sp.t, v = sp.v;
      if (Math.hypot(q.x - p.x, q.y - p.y) > 1.5 || Math.abs(q.f - p.f) > 0.02 || Math.abs(v.x) + Math.abs(v.y) > 30) return false;
    }
    return true;
  }
}
