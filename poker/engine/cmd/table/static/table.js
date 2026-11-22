/* The 50 bb heads-up table: the page for cmd/table (API.md). Plain, but the cards and chips move as on an
   earlier table page of ours: every server answer becomes an event queue (deal, blinds, each action's chips,
   streets dealt and flipped, the showdown, the pot to the winner), played by cardmotion.js. Input waits
   until the queue is empty; Skip (Esc) drains it.

   The page keeps V, the hand as it has been SHOWN, and turns each new state into the events between V and
   the state: state.events from V.step on, a change of round between them is a street. The session score
   and the history change only when the hand's last event has played, so they never give a result away.

   Behind the league's server (mcp/table_web.py: api/info then carries `league`) the page also signs in and
   has two modes. Play: the signed-in user picks the moves. Watch (/table/watch/<token>): read-only, on a
   session the user owns, polled about once a second; each move is animated as it lands. Every state says
   who is in control (`control`): the mode follows it. Take over (Watch -> Play) and Tag in my agent (Play ->
   the user's agent, which claims it over MCP) switch it. On the bare table (no `league`) it is Play only,
   as before.

   On your turn the menu's buttons are shortcuts; the bet slider under them (presets, a range, a number box)
   bets or raises any legal amount (API.md "Bet sizes"). */
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
let league = null;                                // info.league behind the league's server, else null
let watchToken = (location.pathname.match(/\/watch\/([^/]+)$/) || [])[1] || null;
let mode = 'play';                                // 'play' | 'watch' (league only): follows st.control
const POLL = { watch: 1000, play: 3000 };         // ms between polls: watching, and playing (to see a hand-over)
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
  if (s.session) localStorage.setItem(KEY, s.session);   // a watched state has none: watching is not playing
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
  document.body.classList.toggle('watching', mode === 'watch');
  showControl();
  $('next-wrap').hidden = !(ready && st.over && mode === 'play');
  const deciding = ready && !st.over && mode === 'play';
  sizer(deciding);
  if (!deciding) return;
  let n = 0;
  for (const a of st.actions) {
    const b = document.createElement('button');
    b.className = a.kind; b.dataset.index = a.index;
    const key = a.kind === 'bet' ? String(++n) : KEYS_OF[a.kind];
    b.dataset.key = key;
    b.innerHTML = `<span></span><kbd>${key}</kbd>`; b.firstChild.textContent = a.label;
    b.onclick = () => act(a.kind === 'bet' ? { kind: 'bet', amount: a.amount } : { kind: a.kind });
    bar.append(b);
  }
}
M.onIdle = ui;

/* ---------- the bet slider: any legal size ---------- */
// Amounts are "raise to" chips (API.md "Bet sizes"): any whole number from legal.minTo to legal.maxTo. The
// menu buttons above stay as shortcuts. Presets are fractions of the pot after calling, counted from what
// you have in this round plus the call, as the menu's own sizes and the bots' translation count them.
const toLegal = n => Math.min(st.legal.maxTo, Math.max(st.legal.minTo, Math.round(n)));
function potTo(f) {
  const call = st.legal.call || 0;
  return toLegal(st.you.inRound + call + f * (st.pot + call));
}
const presetTo = p => p === 'allin' ? st.legal.maxTo : potTo(+p);
let sizedFor = null;                              // the decision the sizer was set for: re-renders keep the amount
function sizer(deciding) {
  const l = deciding && st.legal, show = !!(l && l.wager && l.minTo < l.maxTo);   // minTo == maxTo: only all-in
  $('sizer').hidden = !show;
  if (!show) return;
  const at = [st.session, st.hand, st.step].join(':');
  if (at === sizedFor) return;
  sizedFor = at;
  for (const el of [$('size-range'), $('size-amount')]) { el.min = l.minTo; el.max = l.maxTo; }
  setSize(l.minTo);
}
function setSize(n, typing = false) {               // typing: leave the number box as typed
  const l = st.legal, call = l.call || 0;
  n = toLegal(n);
  $('size-range').value = n;
  if (!typing) $('size-amount').value = n;
  $('size-go').firstChild.textContent = (l.wager === 'raise' ? 'Raise to ' : 'Bet ') + chips(n);
  const f = (n - st.you.inRound - call) / (st.pot + call);
  $('size-note').textContent = `${bb(n)} bb, ${f.toLocaleString('en-US', { maximumFractionDigits: 2 })}x pot`;
  for (const b of document.querySelectorAll('#presets button')) b.classList.toggle('on', presetTo(b.dataset.preset) === n);
}
function sizedBet() {
  const n = toLegal(+$('size-amount').value || st.legal.minTo);
  setSize(n);
  act({ kind: st.legal.wager, amount: n });
}
for (const b of document.querySelectorAll('#presets button')) b.onclick = () => setSize(presetTo(b.dataset.preset));
$('size-range').oninput = e => setSize(+e.target.value);
$('size-amount').oninput = e => { if (e.target.value !== '') setSize(+e.target.value, true); };
$('size-amount').onchange = e => setSize(+e.target.value || st.legal.minTo);
for (const el of [$('size-range'), $('size-amount')]) el.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); sizedBet(); } });
$('size-go').onclick = sizedBet;
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
  const handovers = (st && st.control ? st.control.switches : []).filter(x => x.hand === rec.number).map(switchLine);
  for (const line of [...opp, 'You were the ' + rec.seat + '.', ...handovers, ...rec.lines, rec.outcome + '.']) {
    const li = document.createElement('li'); li.textContent = line; if (line.startsWith('--')) li.className = 'street'; ul.append(li);
  }
  d.append(ul);
  $('history').prepend(d);
}

/* ---------- server ---------- */
async function call(method, path, body) {
  const res = await fetch(path, { method, headers: body ? { 'Content-Type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const data = await res.json().catch(() => ({ error: res.statusText }));
  if (res.status === 401 && data.signIn) signIn();   // the league's sign-in ran out: sign in again, then back here
  if (!res.ok) { const err = new Error(data.error || res.statusText); err.status = res.status; err.data = data; throw err; }
  return data;
}
const enc = encodeURIComponent;
function signIn() { location.assign('signin?next=' + enc(location.pathname)); }
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
      if (err.status === 409 && err.data.control) { st.control = err.data.control; follow(st); }   // someone else plays now
      else if (err.status === 404 && league) { /* not this account's session: nothing to start over */ }
      else if (err.status === 404) { load(await call('POST', 'api/new', choice())); $('error').textContent = 'The server had no record of your session; a new one has started.'; }
      else if (err.status === 409 && st) load(await call('GET', 'api/state?session=' + encodeURIComponent(st.session)));
    } catch (e) { $('error').textContent = e.message; }
  }
  ui();
}
const seatChoice = () => (document.querySelector('input[name=seat]:checked') || { value: 'alternate' }).value;
const choice = () => ({ seat: seatChoice(), opponent: $('opponent').value || undefined });   // both apply from the next hand dealt
const act = move => send(() => call('POST', 'api/act', { session: st.session, hand: st.hand, step: st.step, ...move }));   // move: {kind, amount}
const nextHand = () => { if (st && st.over) send(() => call('POST', 'api/next', { session: st.session, hand: st.hand, ...choice() })); };

/* ---------- the league: who is in control, Watch and Play, the hand-overs ---------- */
// The mode follows the state's control: Play when this browser is in control, else Watch (the URL then is the
// session's watch link, so a reload stays on it).
function follow(s) {
  if (!league || !s.control) return;
  mode = s.control.yours ? 'play' : 'watch';
  const path = new URL(mode === 'watch' ? s.control.watch : './', document.baseURI).pathname;
  if (mode === 'watch') watchToken = s.control.watch.split('/').pop();
  if (location.pathname !== path) history.replaceState(null, '', path);
}
function showControl() {
  if (!league) return;
  const c = st && st.control, idle = st && !net;
  $('watch-bar').hidden = mode !== 'watch';
  $('watch-status').textContent = !c ? '' : c.waiting
    ? 'Handed to your agent. Ask it to pick this session up: my_sessions(), then claim_session().'
    : 'Your agent is playing; you are watching.';
  $('take-over').disabled = !idle;
  $('tag-in').hidden = mode !== 'play' || !c || !c.canTagIn;
  $('tag-in').disabled = !idle || M.busy;
  $('you-name').textContent = mode === 'watch' && c && !c.waiting ? 'Your agent' : 'You';
}
const SWITCH = {
  'agent>browser': 'You took over from your agent', 'browser>unclaimed': 'You handed the session to your agent',
  'unclaimed>agent': 'Your agent picked the session up', 'browser>agent': 'Your agent took over from you',
  'agent>agent': 'Your agent took over on another connection', 'unclaimed>browser': 'You took the session back',
  'agent>unclaimed': 'The session was handed to your agent',
};
function switchLine(x) { return (SWITCH[x.from + '>' + x.to] || `Control: ${x.from} to ${x.to}`) + ` (step ${x.step}).`; }
// A switch of control: no animation to wait for, the hand stays where it is.
async function handOver(fn) {
  if (net || !st) return;
  net = true; $('error').textContent = ''; ui();
  try { const s = await fn(); net = false; follow(s); receive(s); }
  catch (err) { net = false; $('error').textContent = err.message; }
  ui(); schedulePoll();
}
const takeOver = () => { if (mode === 'watch') handOver(() => call('POST', 'api/takeover', { token: watchToken })); };
const tagIn = () => { if (mode === 'play' && !M.busy) handOver(() => call('POST', 'api/tagin', { session: st.session })); };
// The hands a watcher missed (it polled past them) go straight to the history, oldest first.
function fillHistory(s) {
  const add = (s.history || []).filter(r => r.number !== s.hand && !H.some(h => h.number === r.number));
  if (!add.length) return;
  H.push(...add); H.sort((a, b) => a.number - b.number);
  $('history').replaceChildren(); H.forEach(addHistory); showTotals();
}
// Watching: each poll brings what moved since (or the end of a hand the page was still on: catchUp), only
// once the table has shown the last answer. Playing: a slower poll, to see a hand-over to the agent.
let pollTimer = null;
function schedulePoll() { clearTimeout(pollTimer); if (league && st) pollTimer = setTimeout(poll, POLL[mode]); }
async function poll() {
  if (net || M.busy) { schedulePoll(); return; }
  net = true;
  try {
    const s = mode === 'watch'
      ? await call('GET', `api/watch?token=${enc(watchToken)}&brief=1&hand=${st.hand}&over=${st.over ? 1 : 0}`)
      : await call('GET', `api/state?session=${enc(st.session)}&brief=1`);
    net = false;
    fillHistory(s);
    if (s.hand !== st.hand || s.step !== st.step || s.over !== st.over || JSON.stringify(s.control) !== JSON.stringify(st.control)) {
      follow(s); receive(s);
    }
  } catch (err) { net = false; $('error').textContent = err.message; }
  ui(); schedulePoll();
}

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
$('take-over').onclick = takeOver;
$('tag-in').onclick = tagIn;
$('sign-out').onclick = async () => { await call('POST', 'signout', {}).catch(() => {}); localStorage.removeItem(KEY); location.assign('./'); };
document.addEventListener('keydown', e => {
  if (e.ctrlKey || e.metaKey || e.altKey || e.target.tagName === 'INPUT' && e.target.type !== 'radio' && e.target.type !== 'checkbox') return;
  const k = e.key.toUpperCase();
  if (k === 'ESCAPE') { skip(); return; }
  if (mode === 'watch') { if (k === 'T') { e.preventDefault(); takeOver(); } return; }
  if (st && st.over && (k === 'ENTER' || k === 'N' || k === ' ')) { e.preventDefault(); nextHand(); return; }
  if (k === 'R' && !$('sizer').hidden) { e.preventDefault(); sizedBet(); return; }
  const b = [...document.querySelectorAll('#actions button')].find(b => b.dataset.key === k);
  if (b) { e.preventDefault(); b.click(); }
});
new ResizeObserver(layout).observe(felt);         // the window, and the bar growing when the bet slider shows

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
    league = info.league || null;
    if (league) {
      if (!league.account) { signIn(); return; }
      $('league').hidden = false;
      $('account-name').textContent = league.account.name + (league.account.guest ? ' (guest)' : '');
      if (league.account.guest) $('tag-note').textContent = 'A guest cannot hand a session to an agent: an agent signs in as another guest.';
    } else watchToken = null;                       // the bare table has no Watch
    net = true;
    let s = null;
    if (watchToken) s = await call('GET', 'api/watch?token=' + enc(watchToken));
    else {
      const saved = localStorage.getItem(KEY);
      if (saved) { try { s = await call('GET', 'api/state?session=' + enc(saved)); } catch (err) { if (err.status !== 404) throw err; } }
      if (!s) s = await call('POST', 'api/new', { seat: 'alternate' });
    }
    net = false;
    for (const r of document.querySelectorAll('input[name=seat]')) r.checked = r.value === s.seatMode;
    $('opponent').value = s.opponent;
    follow(s);
    load(s);
    schedulePoll();
  } catch (err) {
    net = false;
    $('error').textContent = err.status === 404 && watchToken ? 'No session here: a watch link opens only for the account that started it.'
      : 'Could not reach the table: ' + err.message;
  }
})();
