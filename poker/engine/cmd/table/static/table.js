/* The 50 bb heads-up table: the page for cmd/table (API.md). Plain, but the cards and chips move as on the
   San Juan / dmcg table v2: every server answer becomes an event queue (deal, blinds, each action's chips,
   streets dealt and flipped, the showdown, the pot to the winner), played by cardmotion.js. Input waits
   until the queue is empty; Skip (Esc) drains it.

   The page keeps V, the hand as it has been SHOWN, and turns each new state into the events between V and
   the state: state.events from V.step on, a change of round between them is a street. The session score
   and the history change only when the hand's last event has played, so they never give a result away. */
import { Motion, cardElement } from './cardmotion.js';

const $ = id => document.getElementById(id);
const HASH = new URLSearchParams(location.hash.slice(1));
const KEY = 'poker50.session', SPEED = 'poker50.speed';
const CW = 100, CH = 140, CHIP_W = 96, CHIP_H = 40, DB = 34;     // unscaled sprite sizes, px
const BEAT = { bot: 520, you: 260, street: 420, flip: 380, deal: 130, collect: 380, award: 650 };
const SUIT = { s: '\u2660', h: '\u2665', d: '\u2666', c: '\u2663' };
const ROUND = { 'before the flop': 0, flop: 1, turn: 2, river: 3, showdown: 4 };
const BOARD_SIZE = [0, 3, 4, 5];

let info = null, st = null, net = false;          // st: the server's last state; net: a request is out
const felt = $('felt');
const M = new Motion(felt, slot);
const V = { hand: 0, step: 0, round: 0, board: 0, over: true, button: null, stack: { you: 0, bot: 0 } };
const H = [];                                     // the finished hands shown, oldest first
const cards = { you: [], bot: [], board: [] };
let dealer = null;
window.table = { get st() { return st; }, get busy() { return net || M.busy; }, settled: () => !net && M.settled(), motion: M, shown: V,
  chipsOnTable: () => [...M.sprites].reduce((a, sp) => a + (sp.amount || 0), 0) };
Object.defineProperty(window, 'st', { get: () => st });
Object.defineProperty(window, 'busy', { get: () => net || M.busy });

/* ---------- numbers ---------- */
const chips = n => n.toLocaleString('en-US');
const bb = n => (n / info.bigBlind).toLocaleString('en-US', { maximumFractionDigits: 2 });
const sign = (n, s) => (n > 0 ? '+' : n < 0 ? '\u2212' : '') + s.replace('-', '');
const other = who => who === 'you' ? 'bot' : 'you';

/* ---------- layout: every slot comes from here (felt px) ---------- */
let G;
function layout() {
  const W = felt.clientWidth, Hh = felt.clientHeight;
  const ch = Math.min(Hh * 0.24, W * 0.1 * 1.4), cw = ch / 1.4, gap = cw * 1.1, m = Math.max(6, Hh * 0.035);
  const chs = Math.max(0.6, cw / 100);             // chip scale
  const holeY = { bot: m + ch / 2, you: Hh - m - ch / 2 };
  const boardY = Hh / 2;
  G = { W, H: Hh, cw, ch, cs: cw / CW, chs, holeY, boardY,
    holeX: i => W / 2 + (i - 0.5) * cw * 1.08,
    boardX: i => W / 2 + (i - 2) * gap,
    deck: { x: W / 2 - 2 * gap - cw * 1.55, y: boardY },
    pot: { x: W / 2 + 2 * gap + cw * 0.65 + CHIP_W / 2 * chs, y: boardY },
    bet: { bot: (holeY.bot + ch / 2 + boardY - ch / 2) / 2, you: (holeY.you - ch / 2 + boardY + ch / 2) / 2 },
    seatX: W / 2 + cw * 1.25 };
  for (const who of ['you', 'bot']) {
    const el = $('seat-' + who);
    el.style.left = G.seatX + 'px'; el.style.top = holeY[who] + 'px'; el.style.maxWidth = (W - G.seatX - 8) + 'px';
  }
  const dk = $('deck'); dk.style.left = G.deck.x + 'px'; dk.style.top = G.deck.y + 'px';
  dk.style.width = CW + 'px'; dk.style.height = CH + 'px'; dk.style.transform = `translate(-50%,-50%) scale(${G.cs})`;
}
function stackAt(who) { return { x: G.seatX + 30 * G.chs, y: G.holeY[who], r: 0, s: G.chs * 0.6 }; }
function slot(sp) {                               // a sprite's zone -> where it rests
  const z = sp.zone;
  if (z === 'deck') return { x: G.deck.x, y: G.deck.y, r: 0, s: G.cs, z: 2 };
  if (z === 'hole') return { x: G.holeX(sp.i), y: G.holeY[sp.who] + (sp.lift || 0) * G.ch, r: (sp.i - 0.5) * 4, s: G.cs, z: 5 + sp.i };
  if (z === 'board') return { x: G.boardX(sp.i), y: G.boardY, r: 0, s: G.cs, z: 5 };
  if (z === 'bet') return { x: G.W / 2, y: G.bet[sp.who], r: 0, s: G.chs, z: 50 };
  if (z === 'pot') return { x: G.pot.x, y: G.pot.y, r: 0, s: G.chs, z: 50 };
  if (z === 'stack') return { ...stackAt(sp.who), z: 50 };
  if (z === 'dealer') return { x: G.holeX(0) - G.cw * 0.95, y: G.holeY[sp.who] + G.ch * 0.32, r: 0, s: G.chs * 0.85, z: 60 };
  return null;
}

/* ---------- sprites ---------- */
function setFace(sp, name) {
  sp.name = name;
  const f = sp.el.querySelector('.front');
  if (!name) { f.innerHTML = ''; delete sp.el.dataset.card; return; }
  const r = name[0] === 'T' ? '10' : name[0], s = SUIT[name[1]];
  f.className = 'face front ' + (name[1] === 'h' || name[1] === 'd' ? 'red' : 'black');
  f.innerHTML = `<div class="ix">${r}<br>${s}</div><div class="pip">${s}</div><div class="ix lo">${r}<br>${s}</div>`;
  sp.el.dataset.card = name;
}
function newCard(who, i, name) {
  const el = cardElement('card');
  const sp = M.add(el, CW, CH, { at: { x: G.deck.x, y: G.deck.y, r: 0, s: G.cs } });
  Object.assign(sp, { zone: 'deck', who, i });
  el.dataset.who = who;
  setFace(sp, name);
  return sp;
}
function faceUp(sp, up, o = {}) { M.go(sp, { ...o, up }); sp.el.classList.toggle('down', !up); }
function chipEl() {
  const el = document.createElement('div'); el.className = 'chips';
  el.innerHTML = '<div class="stack"></div><b></b>';
  return el;
}
function setAmount(sp, amount) {
  sp.amount = amount;
  const n = Math.max(1, Math.min(6, Math.round(Math.log2(amount / info.bigBlind + 1) + 1)));
  const tier = amount < 2 * info.bigBlind ? 't1' : amount < 10 * info.bigBlind ? 't2' : amount < 30 * info.bigBlind ? 't3' : 't4';
  sp.el.querySelector('.stack').innerHTML = `<i class="${tier}"></i>`.repeat(n);
  sp.el.querySelector('b').textContent = chips(amount);
}
const zoneChips = (zone, who) => [...M.sprites].filter(sp => sp.amount != null && sp.zone === zone && (who == null || sp.who === who));
// Chips that land where another stack already rests merge into it.
function landChips(sp) {
  if (sp.zone === 'stack') { V.stack[sp.who] += sp.amount; showStack(sp.who); M.remove(sp); return; }
  const anchor = zoneChips(sp.zone, sp.who).find(o => o !== sp && !o.arrive);
  if (anchor) { setAmount(anchor, anchor.amount + sp.amount); M.remove(sp); bump(anchor); }
}
function bump(sp) { sp.v.s += 2.2 * G.chs; }            // a small kick on the scale spring: the stack takes the chips
function chipsTo(sp, zone, who, delay = 0) { sp.zone = zone; sp.who = who; M.go(sp, { delay, arrive: landChips }); }
function bet(who, amount) {                        // chips leave a seat's stack for its bet
  const sp = M.add(chipEl(), CHIP_W, CHIP_H, { at: stackAt(who) });
  setAmount(sp, amount);
  V.stack[who] -= amount; showStack(who);
  chipsTo(sp, 'bet', who);
}

/* ---------- seat panels ---------- */
function showStack(who) { $(who + '-stack').textContent = chips(V.stack[who]); $(who + '-stackbb').textContent = bb(V.stack[who]) + ' bb'; }
function say(who, text) {
  const el = $(who + '-said'); el.textContent = text;
  el.classList.remove('pop'); void el.offsetWidth; el.classList.add('pop');
}
function hush() { for (const who of ['you', 'bot']) $(who + '-said').textContent = ''; }

// The opponent in this hand, as the server states it: its exploitability is shown as written.
const botOf = id => info.bots.find(b => b.id === id) || info.bots[0];
function showBot(id) {
  const b = botOf(id);
  $('bot-name').textContent = b.name;
  $('bot-trained').textContent = b.trained || '';
  $('exploitability').textContent = b.exploitability;
  $('bot-note').textContent = b.note || '';
}

/* ---------- events: each starts some motion and returns its beat (ms) ---------- */
function evMuck() {
  hush(); for (const who of ['you', 'bot']) $(who + '-holds').textContent = '';
  $('outcome').textContent = ''; $('outcome').className = '';
  const all = [...cards.you, ...cards.bot, ...cards.board];
  all.forEach((sp, k) => { sp.zone = 'deck'; faceUp(sp, false, { delay: k * 25, arrive: s => M.remove(s) }); });
  cards.you = []; cards.bot = []; cards.board = [];
  return all.length ? 300 : 0;
}
function evSeats(s) {
  const you = s.seat.startsWith('small') ? 'sb' : 'bb';
  V.button = you === 'sb' ? 'you' : 'bot';
  $('you-pos').textContent = s.seat;
  $('bot-pos').textContent = you === 'sb' ? 'big blind' : 'small blind (button)';
  $('hand-number').textContent = s.hand;
  showBot(s.opponent);
  V.stack.you = V.stack.bot = info.stackBB * info.bigBlind; showStack('you'); showStack('bot');
  if (!dealer) { dealer = M.add(Object.assign(document.createElement('div'), { className: 'dealer', textContent: 'D' }), DB, DB, { at: { x: G.W / 2, y: G.boardY, s: G.chs } }); dealer.zone = 'dealer'; }
  dealer.who = V.button; M.go(dealer, {});
  return 260;
}
function evBlinds() { bet(V.button, info.smallBlind); bet(other(V.button), info.bigBlind); return 340; }
function evDeal(s) {
  const first = other(V.button), order = [first, V.button, first, V.button];
  order.forEach((who, k) => {
    const i = k >> 1, sp = newCard(who, i, who === 'you' ? s.you.cards[i] : null);
    cards[who][i] = sp; sp.zone = 'hole'; sp.el.classList.add('down');
    M.go(sp, { delay: k * BEAT.deal });
  });
  return 4 * BEAT.deal + 260;
}
function evFlipMine() { cards.you.forEach((sp, i) => faceUp(sp, true, { delay: i * 90 })); return BEAT.flip; }
function evAct(e) {
  say(e.seat, e.text);
  if (e.cost > 0) bet(e.seat, e.cost);
  if (e.kind === 'fold') {                        // the folder's cards go back to the deck, face down
    cards[e.seat].forEach((sp, k) => { sp.zone = 'deck'; faceUp(sp, false, { delay: 120 + k * 60, arrive: s => M.remove(s) }); });
    cards[e.seat] = [];
  }
  return e.seat === 'bot' ? BEAT.bot : BEAT.you;
}
function evCollect() {
  const bets = zoneChips('bet');
  bets.forEach((sp, k) => chipsTo(sp, 'pot', null, k * 60));
  return bets.length ? BEAT.collect : 0;
}
function evBoard(s, n) {                          // board cards to n, face down from the deck, then flipped in
  const fresh = [];
  for (let i = V.board; i < n; i++) {
    const sp = newCard('board', i, s.board[i]); sp.zone = 'board'; sp.el.classList.add('down');
    M.go(sp, { delay: (i - V.board) * BEAT.deal }); cards.board[i] = sp; fresh.push(sp);
  }
  V.board = n;
  return fresh.length * BEAT.deal + 240;
}
function evFlipBoard(from) { cards.board.slice(from).forEach((sp, k) => faceUp(sp, true, { delay: k * 110 })); return BEAT.flip + 60 * (cards.board.length - from); }
function dealStreet(q, s, r) {                    // a street: its cards fly out face down, then turn over
  const from = BOARD_SIZE[r - 1] || 0;
  q.push(() => evBoard(s, BOARD_SIZE[r]), () => evFlipBoard(from));
}
function evShowdown(s) {
  hush();
  cards.bot.forEach((sp, i) => { setFace(sp, s.bot.cards[i]); faceUp(sp, true, { delay: i * 120 }); });
  $('you-holds').textContent = s.you.holds || ''; $('bot-holds').textContent = s.bot.holds || '';
  return BEAT.flip + 400;
}
function evAward(s) {
  const pot = zoneChips('pot');
  let total = pot.reduce((a, sp) => a + sp.amount, 0);
  const won = s.net > 0 ? ['you'] : s.net < 0 ? ['bot'] : ['you', 'bot'];
  if (won.length === 2 && pot.length) {        // a split: the pot divides, half to each
    const half = total / 2, base = pot[0];
    pot.slice(1).forEach(sp => M.remove(sp));
    setAmount(base, half);
    const twin = M.add(chipEl(), CHIP_W, CHIP_H, { at: { ...base.p } }); setAmount(twin, half);
    chipsTo(base, 'stack', 'you'); chipsTo(twin, 'stack', 'bot', 80);
  } else pot.forEach((sp, k) => chipsTo(sp, 'stack', won[0], k * 60));
  for (const who of won) $('seat-' + who).classList.add('won');
  return total ? BEAT.award : 0;
}
function evFinish(s) {
  const out = $('outcome');
  out.textContent = s.outcome + ' (' + sign(s.net, bb(s.net)) + ' bb)';
  out.className = s.net > 0 ? 'won' : s.net < 0 ? 'lost' : 'split';
  if (s.finished && !H.some(r => r.number === s.finished.number)) { H.push(s.finished); addHistory(s.finished); }
  showTotals();
  return 0;
}

/* ---------- derive: a new state -> the events between what is shown and it ---------- */
function receive(s) {
  st = s;
  localStorage.setItem(KEY, s.session);
  const q = [];
  if (s.hand !== V.hand) {
    for (const who of ['you', 'bot']) $('seat-' + who).classList.remove('won');
    q.push(evMuck, () => evSeats(s), evBlinds, () => evDeal(s), evFlipMine);
    Object.assign(V, { hand: s.hand, step: 0, round: 0, board: 0, over: false });
  }
  const street = r => {                           // the betting is over for V.round: chips in, the next street out
    q.push(evCollect, () => { hush(); return 0; });
    if (r <= 3) dealStreet(q, s, r);
    V.round = r;
  };
  for (const e of s.events.slice(V.step)) {
    if (e.round > V.round) street(e.round);
    q.push(() => evAct(e));
  }
  V.step = s.events.length;
  const round = ROUND[s.round];
  if (!s.over && round > V.round) street(round);
  if (s.over && !V.over) {
    V.over = true;
    q.push(evCollect);
    if (s.round === 'showdown') {
      for (let r = V.round + 1; r <= 3; r++) {     // an all-in runs the board out, a street at a time
        dealStreet(q, s, r); q.push(() => BEAT.street);
      }
      V.round = 3;
      q.push(() => evShowdown(s));
    }
    q.push(() => evAward(s), () => evFinish(s));
  }
  M.enqueue(...q);
}

/* ---------- the action bar, stats and history ---------- */
const KEYS_OF = { fold: 'F', check: 'C', call: 'C', allin: 'A' };
function ui() {
  const bar = $('actions'); bar.replaceChildren();
  const ready = st && !net && !M.busy;
  document.body.classList.toggle('busy', !ready);
  $('next-wrap').hidden = !(ready && st.over);
  if (!ready || st.over) return;
  let n = 0;
  for (const a of st.actions) {
    const b = document.createElement('button');
    b.className = a.kind; b.dataset.index = a.index;
    const key = a.kind === 'bet' ? String(++n) : KEYS_OF[a.kind];
    b.dataset.key = key;
    b.innerHTML = `<span></span><kbd>${key}</kbd>`; b.firstChild.textContent = a.label;
    b.onclick = () => act(a.index);
    bar.append(b);
  }
}
M.onIdle = ui;
// The score from the hands shown, so it never runs ahead of the table: all hands, and per opponent when
// the session has met more than one. bb/100 carries its standard error; a few hundred hands are noise.
function score(recs) {
  const n = recs.length, nets = recs.map(r => r.net / info.bigBlind), sum = nets.reduce((a, x) => a + x, 0), mean = n ? sum / n : 0;
  const sd = n >= 2 ? Math.sqrt(nets.reduce((a, x) => a + (x - mean) ** 2, 0) / (n - 1)) : null;
  return { n, sum, bb100: 100 * mean, se: sd == null ? null : 100 * sd / Math.sqrt(n) };
}
const cls = x => x > 0 ? 'pos' : x < 0 ? 'neg' : '';
function showTotals() {
  const all = score(H);
  $('stat-hands').textContent = all.n;
  $('stat-net').textContent = sign(all.sum, all.sum.toFixed(1)); $('stat-net').className = cls(all.sum);
  $('stat-bb100').textContent = all.n ? sign(all.bb100, all.bb100.toFixed(1)) : '\u2013'; $('stat-bb100').className = all.n ? cls(all.bb100) : '';
  $('stat-se').textContent = all.se == null ? '' : '\u00b1 ' + all.se.toFixed(0) + ' (1 s.e.)';
  const ids = [...new Set(H.map(r => r.opponent))];
  const by = $('stat-by'); by.replaceChildren();
  if (ids.length > 1) for (const id of ids) {
    const s = score(H.filter(r => r.opponent === id)), row = document.createElement('div');
    row.textContent = `${botOf(id).name}: ${s.n} hands, ${sign(s.bb100, s.bb100.toFixed(1))} bb/100` + (s.se == null ? '' : ` \u00b1 ${s.se.toFixed(0)}`);
    by.append(row);
  }
}
function cardSpan(name) {
  const s = document.createElement('span'); s.className = 'c ' + (name[1] === 'h' || name[1] === 'd' ? 'red' : 'black');
  s.textContent = (name[0] === 'T' ? '10' : name[0]) + SUIT[name[1]]; return s;
}
function addHistory(rec) {
  const d = document.createElement('details'); d.className = 'hand'; d.dataset.number = rec.number;
  const sm = document.createElement('summary');
  const num = document.createElement('span'); num.className = 'n'; num.textContent = '#' + rec.number;
  const cs = document.createElement('span'); cs.className = 'cs';
  rec.you.forEach(c => cs.append(cardSpan(c)));
  if (rec.bot) { cs.append(' vs '); rec.bot.forEach(c => cs.append(cardSpan(c))); }
  if (rec.board.length) { cs.append(' \u2502 '); rec.board.forEach(c => cs.append(cardSpan(c))); }
  const nt = document.createElement('span'); nt.className = 'net ' + (rec.net > 0 ? 'pos' : rec.net < 0 ? 'neg' : '');
  nt.textContent = sign(rec.net, bb(rec.net)) + ' bb';
  sm.append(num, cs, nt); d.append(sm);
  if (info.bots.length > 1) d.title = 'against ' + botOf(rec.opponent).name;
  const ul = document.createElement('ul');
  const opp = info.bots.length > 1 ? ['Against ' + botOf(rec.opponent).name + '.'] : [];
  for (const line of [...opp, 'You were the ' + rec.seat + '.', ...rec.lines, rec.outcome + '.']) {
    const li = document.createElement('li'); li.textContent = line; if (line.startsWith('--')) li.className = 'street'; ul.append(li);
  }
  d.append(ul);
  $('history').prepend(d);
}

/* ---------- server ---------- */
async function call(method, path, body) {
  const res = await fetch(path, { method, headers: body ? { 'Content-Type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const data = await res.json().catch(() => ({ error: res.statusText }));
  if (!res.ok) { const err = new Error(data.error || res.statusText); err.status = res.status; throw err; }
  return data;
}
function clearTable() {
  M.skip();
  for (const sp of [...M.sprites]) M.remove(sp);
  dealer = null; cards.you = []; cards.bot = []; cards.board = [];
  Object.assign(V, { hand: 0, step: 0, round: 0, board: 0, over: true });
  H.length = 0; $('history').replaceChildren(); hush();
}
// A whole session from the server (a page load, a new session): the finished hands go straight to the
// history, except the one in play, which the table is about to show.
function load(s) {
  clearTable();
  for (const r of s.history || []) if (r.number !== s.hand) { H.push(r); addHistory(r); }
  showTotals();
  receive(s);
}
// One request at a time, and none while the table is still moving.
async function send(fn, whole) {
  if (net || M.busy) return;
  net = true; $('error').textContent = ''; ui();
  skipAnswer = false;
  try { const s = await fn(); net = false; whole ? load(s) : receive(s); if (skipAnswer) M.skip(); }
  catch (err) {
    net = false;
    $('error').textContent = err.message;
    try {
      if (err.status === 404) { load(await call('POST', 'api/new', choice())); $('error').textContent = 'The server had no record of your session; a new one has started.'; }
      else if (err.status === 409 && st) load(await call('GET', 'api/state?session=' + encodeURIComponent(st.session)));
    } catch (e) { $('error').textContent = e.message; }
  }
  ui();
}
const seatChoice = () => (document.querySelector('input[name=seat]:checked') || { value: 'alternate' }).value;
const choice = () => ({ seat: seatChoice(), opponent: $('opponent').value || undefined });   // both apply from the next hand dealt
const act = index => send(() => call('POST', 'api/act', { session: st.session, hand: st.hand, step: st.step, action: index }));
const nextHand = () => { if (st && st.over) send(() => call('POST', 'api/next', { session: st.session, hand: st.hand, ...choice() })); };

/* ---------- controls ---------- */
$('next').onclick = nextHand;
let skipAnswer = false;                           // Skip pressed while the answer is on its way: skip it too
function skip() { if (net) skipAnswer = true; M.skip(); }
$('skip').onclick = skip;
$('new-session').onclick = () => { if (confirm('Start a new session? The score and the history start again.')) send(() => call('POST', 'api/new', choice()), true); };
function setSpeed(v) { M.speed = v; $('speed').value = v; $('speedv').textContent = v + 'x'; }
$('speed').oninput = e => { setSpeed(+e.target.value); localStorage.setItem(SPEED, M.speed); };
function setRM(on) { M.rm = on; $('rm').checked = on; document.body.classList.toggle('rm', on); }
$('rm').onchange = e => setRM(e.target.checked);
document.addEventListener('keydown', e => {
  if (e.ctrlKey || e.metaKey || e.altKey || e.target.tagName === 'INPUT' && e.target.type !== 'radio' && e.target.type !== 'checkbox') return;
  const k = e.key.toUpperCase();
  if (k === 'ESCAPE') { skip(); return; }
  if (st && st.over && (k === 'ENTER' || k === 'N' || k === ' ')) { e.preventDefault(); nextHand(); return; }
  const b = [...document.querySelectorAll('#actions button')].find(b => b.dataset.key === k);
  if (b) { e.preventDefault(); b.click(); }
});
addEventListener('resize', layout);

/* ---------- boot ---------- */
(async function start() {
  layout();
  setSpeed(+(HASH.get('speed') || localStorage.getItem(SPEED) || 1));
  setRM(HASH.has('rm') ? HASH.get('rm') === '1' : matchMedia('(prefers-reduced-motion: reduce)').matches);
  M.start();
  try {
    info = await call('GET', 'api/info');
    $('blinds').textContent = `${info.stackBB} bb stacks, blinds ${info.smallBlind}/${info.bigBlind}`;
    $('opponent').replaceChildren(...info.bots.map(b => Object.assign(document.createElement('option'), { value: b.id, textContent: b.name })));
    $('opp-row').hidden = info.bots.length < 2;     // a choice only when there is one to make
    $('menu').textContent = 'Bet sizes: ' + info.menu + '.';
    net = true;
    const saved = localStorage.getItem(KEY);
    let s = null;
    if (saved) { try { s = await call('GET', 'api/state?session=' + encodeURIComponent(saved)); } catch (err) { if (err.status !== 404) throw err; } }
    if (!s) s = await call('POST', 'api/new', { seat: 'alternate' });
    net = false;
    for (const r of document.querySelectorAll('input[name=seat]')) r.checked = r.value === s.seatMode;
    $('opponent').value = s.opponent;
    load(s);
  } catch (err) { net = false; $('error').textContent = 'Could not reach the table: ' + err.message; }
})();
