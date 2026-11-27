/* The 50 bb heads-up table: the page for cmd/table (API.md). Plain, but the cards and chips move as on an
   earlier table page of ours: every server answer becomes an event queue (deal, blinds, each action's chips,
   streets dealt and flipped, the showdown, the pot to the winner), played by cardmotion.js. Input waits
   until the queue is empty; Skip (Esc) drains it.

   The page keeps V, the hand as it has been SHOWN, and turns each new state into the events between V and
   the state: state.events from V.step on, a change of round between them is a street. The session score
   and the history change only when the hand's last event has played, so they never give a result away.

   Behind the league's server (mcp/table_web.py: api/info then carries `league`) the page also signs in, and
   the table is the account's one table, which the user's agent (over MCP) and the user's programs (with a
   program token, made in the Programs box) play too: whoever moves first on a decision makes it. The page
   polls (every second while someone else is at the table, else every three), animates the moves others make
   as they land, and says who moved last (`presence`); the dock is open whenever a decision is pending. On
   the bare table (no `league`) the page is the only player, as before.

   Replay. A program plays faster than any page could show, so the page asks for the hands finished since the
   last one it has (`api/state?since=H`: each with its events) and plays every one through the same event
   queue as a live hand, at a replay speed of its own (1x, fast, fastest: the clock 1, 2, 3 times, and at
   fastest the beats between steps halved; a card flip keeps its length), then the live state. It replays
   whenever a program moved lately or more than one hand is waiting; Jump to latest (J) skips what waits.

   Asleep. Nobody should spend bandwidth on a table nobody is looking at: the page stops polling and puts up a
   pause screen (Resume) when its tab is hidden (Page Visibility), or when there has been no user input and no
   new state for 5 minutes (#idle=<seconds> in the URL changes that, for tests). Resume fetches the state once
   and the polling goes on. A game the agent is playing keeps a visible tab awake: each of its moves is a new state.

   The dock. On your turn the table shows three buttons, Fold, Check/Call and Raise to X, and above them, when you
   may bet, the sizer: detents, a slider and a box, all in big blinds (the server is sent chips). The button bets the
   size shown (API.md "Bet sizes"). There is one remembered size for each spot (preflop, postflop bet, postflop
   raise), kept as a multiple of the bet you face or a fraction of the pot, never as chips. */
import { Motion, cardElement } from './cardmotion.js';

const $ = id => document.getElementById(id);
const HASH = new URLSearchParams(location.hash.slice(1));
const KEY = 'poker50.session', SPEED = 'poker50.speed';
const CW = 100, CH = 140, CHIP_W = 96, CHIP_H = 40, DB = 34;     // unscaled sprite sizes, px
const BEAT = { bot: 520, you: 260, street: 420, flip: 380, deal: 130, collect: 380, award: 650 };
// Replaying hands others played (a program at the table): three speeds. `clock` scales the motion clock;
// `short` scales the beats between steps (deals, moves, chips), never the card flip.
const REPLAY = { '1x': { clock: 1, short: 1 }, fast: { clock: 2, short: 1 }, fastest: { clock: 3, short: 0.5 } };
const RSPEED = 'poker50.replay', MAX_BEHIND = 200;
let replaySpeed = REPLAY[localStorage.getItem(RSPEED)] ? localStorage.getItem(RSPEED) : 'fastest';
let liveSpeed = 1;                                // the Table settings slider's speed, for everything else
const queued = new Set();                         // finished hands queued to replay, not yet shown to the end
let lastQueued = 0;                               // the latest finished hand queued or shown
const shownHands = [];                            // [{number, events, replay}] each hand the table showed to its end (for tests)
const acted = {};                                 // hand -> moves animated so far
const beat = name => BEAT[name] * (replayOn() ? REPLAY[replaySpeed].short : 1);
const SUIT = { s: '\u2660', h: '\u2665', d: '\u2666', c: '\u2663' };
const ROUND = { 'before the flop': 0, flop: 1, turn: 2, river: 3, showdown: 4 };
const BOARD_SIZE = [0, 3, 4, 5];

let info = null, st = null, net = false;          // st: the server's last state; net: a request is out
let league = null;                                // info.league behind the league's server, else null
const POLL = { replay: 300, busy: 1000, quiet: 3000 };   // ms between polls: replaying, someone else at the table lately, or not
const LATELY = 30;                                // s: what "lately" means, for POLL and the presence line
const IDLE_MS = 1000 * (+HASH.get('idle') || 300);  // asleep after this long with no input and no new state
const felt = $('felt');
const M = new Motion(felt, slot);
const V = { hand: 0, step: 0, round: 0, board: 0, over: true, button: null, stack: { you: 0, bot: 0 } };
const H = [];                                     // the finished hands shown, oldest first
const cards = { you: [], bot: [], board: [] };
let dealer = null;
window.table = { get st() { return st; }, get asleep() { return asleep; }, handsShown: shownHands, get behind() { return queued.size; }, get replaying() { return replayOn(); }, get busy() { return net || M.busy; }, settled: () => !net && M.settled(), motion: M, shown: V,
  chipsOnTable: () => [...M.sprites].reduce((a, sp) => a + (sp.amount || 0), 0) };
Object.defineProperty(window, 'st', { get: () => st });
Object.defineProperty(window, 'busy', { get: () => net || M.busy });

/* ---------- numbers ---------- */
const bb = n => (n / info.bigBlind).toLocaleString('en-US', { maximumFractionDigits: 2 });
// Text from the server (the log, the outcome) counts in chips: show every bare number in it in big blinds.
const inBB = text => text.replace(/(?<![\w.])\d[\d,]*(?![\w.])/g, n => bb(+n.replace(/,/g, '')) + ' bb');
const sign = (n, s) => (n > 0 ? '+' : n < 0 ? '\u2212' : '') + s.replace('-', '');
const other = who => who === 'you' ? 'bot' : 'you';

/* ---------- layout: every slot comes from here (felt px) ---------- */
let G;
function layout() {
  const W = felt.clientWidth, Hh = felt.clientHeight;
  const ch = Math.min(Hh * 0.24, W * (W < 520 ? 0.16 : 0.1) * 1.4), cw = ch / 1.4, gap = cw * 1.1, m = Math.max(6, Hh * 0.035);
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
  sp.el.querySelector('b').textContent = bb(amount) + ' bb';
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
function showStack(who) { $(who + '-stack').textContent = bb(V.stack[who]) + ' bb'; }
function say(who, text) {
  const el = $(who + '-said'); el.textContent = text;
  el.classList.remove('pop'); void el.offsetWidth; el.classList.add('pop');
}
function hush() { for (const who of ['you', 'bot']) $(who + '-said').textContent = ''; }

// The opponent in this hand, as the server states it: its note (its style) and how it was trained, shown as written.
const botOf = id => info.bots.find(b => b.id === id) || info.bots[0];
function showBot(id) {
  const b = botOf(id);
  $('bot-name').textContent = b.name;
  $('bot-trained').textContent = b.trained || '';
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
    M.go(sp, { delay: k * beat('deal') });
  });
  return 4 * beat('deal') + 260;
}
function evFlipMine() { cards.you.forEach((sp, i) => faceUp(sp, true, { delay: i * 90 })); return BEAT.flip; }
function evAct(e, hand) {
  acted[hand] = (acted[hand] || 0) + 1;
  say(e.seat, inBB(e.text));
  if (e.cost > 0) bet(e.seat, e.cost);
  if (e.kind === 'fold') {                        // the folder's cards go back to the deck, face down
    cards[e.seat].forEach((sp, k) => { sp.zone = 'deck'; faceUp(sp, false, { delay: 120 + k * 60, arrive: s => M.remove(s) }); });
    cards[e.seat] = [];
  }
  return e.seat === 'bot' ? beat('bot') : beat('you');
}
function evCollect() {
  const bets = zoneChips('bet');
  bets.forEach((sp, k) => chipsTo(sp, 'pot', null, k * 60));
  return bets.length ? beat('collect') : 0;
}
function evBoard(s, n) {                          // board cards to n, face down from the deck, then flipped in
  const fresh = [];
  for (let i = V.board; i < n; i++) {
    const sp = newCard('board', i, s.board[i]); sp.zone = 'board'; sp.el.classList.add('down');
    M.go(sp, { delay: (i - V.board) * beat('deal') }); cards.board[i] = sp; fresh.push(sp);
  }
  V.board = n;
  return fresh.length * beat('deal') + 240;
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
  return total ? beat('award') : 0;
}
function evFinish(s) {
  const out = $('outcome');
  out.textContent = inBB(s.outcome);  // the server's words carry the amount once (a split pot has none)
  out.className = s.net > 0 ? 'won' : s.net < 0 ? 'lost' : 'split';
  if (s.finished && !H.some(r => r.number === s.finished.number)) { H.push(s.finished); addHistory(s.finished); }
  shownHands.push({ number: s.hand, events: acted[s.hand] || 0, replay: queued.delete(s.hand) });
  delete acted[s.hand];
  showTotals(); showReplay();
  return 0;
}

/* ---------- derive: a new state -> the events between what is shown and it ---------- */
function receive(s) {
  st = s;
  if (s.over) lastQueued = Math.max(lastQueued, s.hand);
  lastActive = Date.now();                               // a new state is activity: a live game stays awake
  if (s.session) localStorage.setItem(KEY, s.session);   // the bare table's session, for a reload
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
    q.push(() => evAct(e, s.hand));
  }
  V.step = s.events.length;
  const round = ROUND[s.round];
  if (!s.over && round > V.round) street(round);
  if (s.over && !V.over) {
    V.over = true;
    q.push(evCollect);
    if (s.round === 'showdown') {
      for (let r = V.round + 1; r <= 3; r++) {     // an all-in runs the board out, a street at a time
        dealStreet(q, s, r); q.push(() => beat('street'));
      }
      V.round = 3;
      q.push(() => evShowdown(s));
    }
    q.push(() => evAward(s), () => evFinish(s));
  }
  M.enqueue(...q);
}

/* ---------- the dock: three buttons, and the sizer above them ---------- */
function ui() {
  const bar = $('actions'); bar.replaceChildren();
  const ready = st && !net && !M.busy;
  document.body.classList.toggle('busy', !ready);
  showPresence(); showReplay();
  $('next-wrap').hidden = !(ready && st.over);
  const deciding = ready && !st.over;
  showStatus(deciding);
  sizer(deciding);
  if (!deciding) return;
  const l = st.legal, call = l.call || 0;
  const button = (kind, html, key, onclick) => {
    const b = document.createElement('button');
    b.className = kind; b.dataset.key = key;
    b.innerHTML = html + `<kbd>${key}</kbd>`;
    b.onclick = onclick;
    bar.append(b);
    return b;
  };
  if (l.fold) button('fold', 'Fold', 'F', () => act({ kind: 'fold' }));
  if (l.check) button('check', 'Check', 'C', () => act({ kind: 'check' }));
  else if (call) button('call', `Call <small>${bb(call)} bb</small>`, 'C', () => act({ kind: 'call' }));
  if (l.wager) { button('bet', '', 'R', sizedBet).id = 'size-go'; showWager(); }
}
M.onIdle = ui;
function showStatus(deciding) {
  const turn = $('turn'), pot = $('potline');
  turn.replaceChildren(); pot.textContent = '';
  if (deciding) {
    const l = st.legal, b = document.createElement('b'); b.textContent = 'Your turn';
    turn.append(b, ' \u00b7 ' + (l.call ? `facing ${bb(st.bot.inRound)} bb` : 'you may check or bet'));
    pot.textContent = `Pot ${bb(st.pot)} bb \u00b7 to call ${l.call ? bb(l.call) + ' bb' : 'nothing'}`;
  }
}

/* ---------- the sizer ---------- */
// Amounts are "raise to" chips (API.md "Bet sizes"): any whole number from legal.minTo to legal.maxTo; here they
// move in half big blinds (the small blind), and the ends are exact. Three spots, each with its own detents and its
// own remembered size. The track spaces the legal detents evenly and is linear in chips between them: on a
// linear track every postflop size would sit in the left tenth and all-in would take the rest.
//   pre    preflop:                x the bet you face   2x 2.5x 3x 4x
//   bet    postflop, first to act: fraction of the pot  1/3 1/2 3/4 pot   (the pot after calling, counted
//                                  from where a call leaves you, as the menu's sizes and the bots count it)
//   raise  postflop, facing a bet: x the bet you face   2.5x 3x 4x
// Every spot also has Min and All-in.
const SPOTS = {
  pre:   { unit: 'x',   detents: [2, 2.5, 3, 4],     labels: ['2x', '2.5x', '3x', '4x'],           def: 2.5 },
  bet:   { unit: 'pot', detents: [1 / 3, 1 / 2, 3 / 4, 1], labels: ['\u2153 pot', '\u00bd pot', '\u00be pot', 'Pot'], def: 1 / 2 },
  raise: { unit: 'x',   detents: [2.5, 3, 4],        labels: ['2.5x', '3x', '4x'],                 def: 3 },
};
const SIZE_KEY = 'poker50.size.';                 // + spot: the last size bet there, as a unit (x or pot fraction)
const SNAP = 0.12;                                // the thumb snaps within this share of the gap between two detents
const spotOf = () => st.round === 'before the flop' ? 'pre' : st.legal.call ? 'raise' : 'bet';
const toLegal = n => Math.min(st.legal.maxTo, Math.max(st.legal.minTo, Math.round(n / info.smallBlind) * info.smallBlind));
function unitChips(spot, k) {                     // unit value -> raise-to chips, unrounded
  return SPOTS[spot].unit === 'pot' ? st.you.inRound + (st.legal.call || 0) + k * (st.pot + (st.legal.call || 0)) : k * st.bot.inRound;
}
function chipsUnit(spot, c) {
  return SPOTS[spot].unit === 'pot' ? (c - st.you.inRound - (st.legal.call || 0)) / (st.pot + (st.legal.call || 0)) : c / st.bot.inRound;
}
const remembered = spot => { const k = parseFloat(localStorage.getItem(SIZE_KEY + spot)); return isNaN(k) ? null : k; };
let size = null;                                  // the sizer as set for this decision: { spot, stops, c }
let sizedFor = null;                              // the decision it was set for: re-renders keep the amount
function stopsFor(spot) {                         // the legal detents, ascending, each once
  const l = st.legal, out = [{ c: l.minTo, label: 'Min' }];
  SPOTS[spot].detents.forEach((k, i) => { const c = toLegal(unitChips(spot, k)); if (c > l.minTo && c < l.maxTo) out.push({ c, label: SPOTS[spot].labels[i] }); });
  out.push({ c: l.maxTo, label: 'All-in' });
  return out.filter((d, i, a) => i === 0 || d.c !== a[i - 1].c);
}
function posOf(c) {                               // chips -> 0..1000 on the track
  const s = size.stops, n = s.length - 1;
  for (let i = 0; i < n; i++) if (c <= s[i + 1].c) return 1000 * (i + (c - s[i].c) / (s[i + 1].c - s[i].c)) / n;
  return 1000;
}
function fromPos(p) {
  const s = size.stops, n = s.length - 1, x = p / 1000 * n, i = Math.min(n - 1, Math.floor(x));
  return s[i].c + (x - i) * (s[i + 1].c - s[i].c);
}
const boxText = c => String(+(c / info.bigBlind).toFixed(2));
function sizer(deciding) {
  const l = deciding && st.legal, show = !!(l && l.wager && l.minTo < l.maxTo);   // minTo == maxTo: only all-in
  $('sizer').hidden = !show;
  if (!show) { if (deciding) { size = null; sizedFor = null; } return; }   // (hidden while a request is out: keep the amount)
  const at = [st.session, st.hand, st.step].join(':');
  if (at === sizedFor) return;
  sizedFor = at;
  const spot = spotOf(), stops = stopsFor(spot);
  size = { spot, stops, c: st.legal.minTo };
  $('detents').replaceChildren(...stops.map((d, i) => {
    const b = document.createElement('button'); b.type = 'button'; b.dataset.c = d.c;
    b.innerHTML = `<span></span><kbd>${i + 1}</kbd>`; b.firstChild.textContent = d.label;
    b.onclick = () => setSize(d.c);
    return b;
  }));
  const k = remembered(spot), mem = k == null ? null : toLegal(unitChips(spot, k));
  $('ticks').innerHTML = stops.map(d => `<b style="left:${posOf(d.c) / 10}%"></b>`).join('') + (mem == null ? '' : `<i class="memo" title="your last size here" style="left:${posOf(mem) / 10}%"></i>`);
  $('size-amount').min = boxText(st.legal.minTo); $('size-amount').max = boxText(st.legal.maxTo);
  setSize(mem ?? toLegal(unitChips(spot, SPOTS[spot].def)));
}
function setSize(c, { snap = false, typing = false } = {}) {   // typing: leave the number box as typed
  if (!size) return;
  if (snap) {
    const gap = 1000 / (size.stops.length - 1), near = size.stops.find(d => Math.abs(posOf(d.c) - posOf(c)) < gap * SNAP);
    if (near) c = near.c;
  }
  size.c = toLegal(c);
  const p = posOf(size.c);
  $('size-range').value = p;
  $('size-range').style.setProperty('--p', p / 10 + '%');
  if (!typing) $('size-amount').value = boxText(size.c);
  for (const b of document.querySelectorAll('#detents button')) b.classList.toggle('on', +b.dataset.c === size.c);
  showWager();
}
function showWager() {                            // the third button says what it will bet
  const b = $('size-go'); if (!b) return;
  const l = st.legal, c = size ? size.c : l.maxTo, allin = c >= l.maxTo;
  b.classList.toggle('allin', allin);
  b.innerHTML = `${allin ? 'All-in' : l.wager === 'raise' ? 'Raise to' : 'Bet'} <small>${bb(c)} bb</small><kbd>R</kbd>`;
}
function sizedBet() {                             // the amount shown is the amount sent
  if (!st || !st.legal || !st.legal.wager) return;
  if (size) { setSize(size.c); if (size.c < st.legal.maxTo) localStorage.setItem(SIZE_KEY + size.spot, chipsUnit(size.spot, size.c)); }
  act({ kind: st.legal.wager, amount: size ? size.c : st.legal.maxTo });
}
const nudge = steps => size && setSize(size.c + steps * info.smallBlind);
function toDetent(dir) {                          // the next detent up or down from the current size
  if (!size) return;
  const s = size.stops, up = s.find(d => d.c > size.c), down = [...s].reverse().find(d => d.c < size.c);
  const d = dir > 0 ? up : down; if (d) setSize(d.c);
}
$('size-range').oninput = e => setSize(fromPos(+e.target.value), { snap: true });
$('size-amount').oninput = e => { const v = parseFloat(e.target.value); if (!isNaN(v)) setSize(Math.round(v * info.bigBlind), { typing: true }); };
$('size-amount').onchange = () => size && setSize(size.c);
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
  for (const line of [...opp, 'You were the ' + rec.seat + '.', ...playedBy(rec.number), ...rec.lines.map(inBB), inBB(rec.outcome) + '.']) {
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
  queued.clear(); pending.length = 0; feeding = false;
}
// A whole session from the server (a page load, a new session): the finished hands go straight to the
// history, except the one in play, which the table is about to show.
function load(s) {
  clearTable();
  for (const r of s.history || []) if (r.number !== s.hand) { H.push(r); addHistory(r); }
  lastQueued = Math.max(0, ...H.map(r => r.number));
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
      if (err.status === 409 && league) {         // someone else at the table moved first: show the table as it is now
        $('error').textContent = 'Someone else at the table moved first.';
        load(await call('GET', 'api/state'));
      }
      else if (err.status === 404 && league) load(await call('GET', 'api/state'));   // the table was reset elsewhere
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

/* ---------- the league: who is at the table, and the program token ---------- */
const lately = o => o && o.ago <= LATELY;
const someoneElse = () => {
  const p = st && st.presence;
  return !!p && (p.last && p.last.kind !== 'browser' && lately(p.last) || p.others.some(lately));
};
const capital = s => s[0].toUpperCase() + s.slice(1);
function showPresence() {
  const line = $('presence');
  if (!league) { line.hidden = true; return; }
  const p = st && st.presence, last = p && p.last;
  let text = '';
  if (last && last.kind !== 'browser' && lately(last)) text = `${capital(last.name)} is playing (last move ${last.ago < 2 ? 'just now' : last.ago + ' s ago'}).`;
  else if (p) {
    const here = p.others.filter(lately);
    const agent = here.find(o => o.kind === 'agent');
    if (agent && st && !st.over) text = 'Your agent is at the table: it may be thinking about this decision.';
    else if (here.length) text = here.map(o => capital(o.name)).join(', ') + ' at the table.';
  }
  line.textContent = text; line.hidden = !text;
}
/* ---------- replay: the hands someone else played, shown at the table's own pace ---------- */
// A program plays several hands a second; the page shows each of them (from the history's events) as fast as
// the chosen replay speed lets the cards move, then the live state. On when a program moved lately or more
// than one hand is waiting; Jump to latest skips what is waiting.
function replayOn() {
  const last = st && st.presence && st.presence.last;
  return queued.size > 1 || !!(last && last.kind === 'program' && lately(last));
}
function applySpeed() { M.speed = replayOn() ? REPLAY[replaySpeed].clock : liveSpeed; }
function showReplay() {
  if (!league) return;
  const on = replayOn();
  $('replay').hidden = !on;
  $('behind').textContent = queued.size > 1 ? `${queued.size - 1} more hand${queued.size > 2 ? 's' : ''} to show` : '';
  for (const r of document.querySelectorAll('input[name=rspeed]')) r.checked = r.value === replaySpeed;
  applySpeed();
}
// A finished hand from the history, as the final state of its hand: what receive() animates.
function recState(rec) {
  const last = rec.events[rec.events.length - 1];
  return {
    hand: rec.number, step: rec.events.length, seat: rec.seat, opponent: rec.opponent, over: true,
    round: rec.showdown ? 'showdown' : Object.keys(ROUND)[last ? last.round : 0],
    you: { cards: rec.you }, bot: { cards: rec.bot || [] }, board: rec.board, events: rec.events,
    outcome: rec.outcome, net: rec.net, finished: rec, presence: st && st.presence, session: st && st.session,
  };
}
// States wait in `pending` until the table has shown the one before (V is the table as shown, so a hand is
// queued only when the last has been dealt out); feedNext, run as the last event of each, starts the next.
const pending = [];
let feeding = false;
function feedNext() {
  const s = pending.shift();
  if (!s) { feeding = false; return 0; }
  receive(s);
  M.enqueue(feedNext);
  return 0;
}
function queueState(s) {
  pending.push(s);
  if (!feeding) { feeding = true; M.enqueue(feedNext); }
}
const latest = () => pending.length ? pending[pending.length - 1] : st;   // the last state queued or shown
// A polled state (with `history` since the last hand queued): queue the hands it lists that the page has not
// shown, then the state itself.
function take(s) {
  for (const r of (s.history || []).filter(r => r.number > lastQueued)) {
    lastQueued = r.number;
    const at = latest();
    if (r.number < at.hand || r.number === at.hand && at.over) continue;   // its end was already shown live
    if (!r.events) { fillHistory({ history: [r] }); continue; }          // older than the table keeps moves for
    queued.add(r.number);
    queueState(recState(r));
  }
  if (queued.size > MAX_BEHIND) {
    M.skip();
    $('error').textContent = `Skipped ahead: the program was more than ${MAX_BEHIND} hands ahead of the table.`;
  }
  const at = latest();
  if (s.hand !== at.hand || s.step !== at.step || s.over !== at.over) queueState(s);
  else if (pending.length) pending[pending.length - 1] = s;   // the same point, as the table states it (totals and all)
  else st = s;
}
function jumpToLatest() { if (queued.size) { M.skip(); schedulePoll(); } }
$('jump').onclick = jumpToLatest;
for (const r of document.querySelectorAll('input[name=rspeed]')) r.onchange = () => { replaySpeed = r.value; localStorage.setItem(RSPEED, replaySpeed); showReplay(); };

// Who moved in a finished hand, when that was not only you.
function playedBy(number) {
  const who = st && st.presence && st.presence.hands[number];
  return who && who.some(w => w !== 'you') ? ['Played by ' + who.join(' and ') + '.'] : [];
}
// Programs: the account's one program token, which a program sends as "Authorization: Bearer ..." to play this
// table. Shown once, when made (or regenerated); after that, its name, when it was made and last used.
async function showToken() {
  const { token: t } = await call('GET', 'api/token');
  $('token-make-row').hidden = !!t;
  $('token-have').hidden = !t;
  if (!t) return;
  const when = s => new Date(s * 1000).toLocaleString();
  $('token-name').textContent = t.label;
  $('token-when').textContent = `made ${when(t.created_at)} \u00b7 ${t.last_used_at ? 'last used ' + when(t.last_used_at) : 'not used yet'}`;
}
// A token just made: a file to save in the coding agent's workspace (made here from the answer, so the token
// is sent once), downloaded at once, and on the page too until it is left.
let tokenURL = null;
function showMade(made) {
  if (tokenURL) URL.revokeObjectURL(tokenURL);
  tokenURL = URL.createObjectURL(new Blob([made.token + '\n'], { type: 'text/plain' }));
  const link = $('token-download');
  link.href = tokenURL;
  link.click();
  $('token-value').textContent = made.token;
  $('token-show').open = false;
  $('token-new').hidden = false; $('token-label').value = '';
}
async function tokenStep(fn) {
  $('token-error').textContent = '';
  try { await fn(); } catch (err) { $('token-error').textContent = err.message; }
  showToken().catch(err => { $('token-error').textContent = err.message; });
}
const makeToken = () => tokenStep(async () => showMade(await call('POST', 'api/token', { label: $('token-label').value })));
$('token-regenerate').onclick = () => tokenStep(async () => {
  if (!confirm('Regenerate the token? The one you have stops working at once; the new one is shown once.')) return;
  showMade(await call('POST', 'api/token/regenerate', {}));
});
$('token-revoke').onclick = () => tokenStep(async () => {
  if (!confirm('Revoke the token? A program using it stops at once.')) return;
  await call('POST', 'api/token/revoke', {});
  $('token-new').hidden = true;
  if (tokenURL) { URL.revokeObjectURL(tokenURL); tokenURL = null; }
});
// Copy: the clipboard where the page may use it; otherwise (plain http, an iframe) select the text, ready for Ctrl+C.
function copyText(btn, code) {
  const label = btn.firstChild;
  const done = ok => {
    btn.classList.toggle('ok', ok); label.textContent = ok ? 'Copied' : 'Select + Ctrl+C';
    setTimeout(() => { btn.classList.remove('ok'); label.textContent = 'Copy'; }, 1600);
  };
  const select = () => {
    const r = document.createRange(); r.selectNodeContents(code);
    getSelection().removeAllRanges(); getSelection().addRange(r);
    try { return document.execCommand('copy'); } catch { return false; }
  };
  if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(code.textContent).then(() => done(true), () => done(select()));
  else done(select());
}
$('token-make').onclick = makeToken;
$('token-copy').onclick = () => copyText($('token-copy'), $('token-value'));
$('programs').addEventListener('toggle', () => { if ($('programs').open) showToken().catch(err => { $('token-error').textContent = err.message; }); });
// Ranked tables: the account's submitted bots' tables, newest first, each done one linked to its results.
// Only where the server keeps them (league.ranked).
async function showRanked() {
  let tables;
  try { ({ tables } = await call('GET', 'api/tables')); }
  catch (err) { $('ranked-error').textContent = err.message; return; }
  $('ranked-error').textContent = '';
  $('ranked').hidden = false;
  const when = s => new Date(s * 1000).toLocaleString();
  const rows = tables.map(t => {
    const li = document.createElement('li');
    const name = `${t.bot} v${t.version}`;
    if (t.state === 'done') {
      const a = Object.assign(document.createElement('a'), { href: t.url, textContent: name });
      li.append(a, ` placed ${t.place} of ${t.players} \u00b7 ${when(t.finished_at)}`);
    } else if (t.state === 'queued') li.textContent = `${name}: queued${t.position ? ', number ' + t.position : ''}`;
    else if (t.state === 'running') li.textContent = `${name}: playing since ${when(t.started_at)}`;
    else li.textContent = `${name}: did not finish: ${t.reason}`;
    return li;
  });
  if (!rows.length) rows.push(Object.assign(document.createElement('li'), { className: 'dim', textContent: 'None yet: submit a bot (the SDK\u2019s bot submit).' }));
  $('ranked-list').replaceChildren(...rows);
}
$('ranked').addEventListener('toggle', () => { if ($('ranked').open) showRanked(); });
// The hands the page missed (others played them between two polls) go straight to the history, oldest first.
function fillHistory(s) {
  const add = (s.history || []).filter(r => r.number !== s.hand && !H.some(h => h.number === r.number));
  if (!add.length) return;
  H.push(...add); H.sort((a, b) => a.number - b.number);
  $('history').replaceChildren(); H.forEach(addHistory); showTotals();
}
// Each poll brings what moved since (or the end of a hand the page was still on: catchUp), only once the
// table has shown the last answer: every second while someone else is at the table, else every three.
let pollTimer = null;
function schedulePoll() {
  clearTimeout(pollTimer);
  if (!league || !st || asleep) return;
  if (document.hidden) { sleep('This tab was in the background.'); return; }
  idleTimer = idleTimer || setInterval(idleCheck, 1000);
  pollTimer = setTimeout(poll, replayOn() ? POLL.replay : someoneElse() ? POLL.busy : POLL.quiet);
}
// Asleep: no polling at all, a pause screen over the table, until Resume.
let asleep = false, lastActive = Date.now(), idleTimer = null;
function idleCheck() {
  if (net || M.busy) lastActive = Date.now();     // the table is still showing what came in: that is activity
  else if (Date.now() - lastActive >= IDLE_MS) sleep('Nothing has happened here for a while.');
}
function sleep(why) {
  if (asleep || !league) return;
  asleep = true;
  clearTimeout(pollTimer); clearInterval(idleTimer); idleTimer = null;
  $('asleep-why').textContent = why;
  $('asleep').hidden = false; $('resume').focus();
}
function resume() {                               // one fetch of the state, then the polling goes on
  if (!asleep) return;
  asleep = false; lastActive = Date.now();
  $('asleep').hidden = true;
  poll();
}
$('resume').onclick = resume;
for (const ev of ['pointerdown', 'pointermove', 'keydown', 'wheel', 'touchstart']) addEventListener(ev, () => { lastActive = Date.now(); }, { capture: true, passive: true });
document.addEventListener('visibilitychange', () => { if (document.hidden) sleep('This tab was in the background.'); });
async function poll() {
  // Replaying, the page asks while the table still moves (what it gets is queued behind), once what is
  // waiting runs low; otherwise only once the table has shown the last answer.
  if (net || (M.busy && !replayOn()) || queued.size >= 3) { schedulePoll(); return; }
  net = true;
  try {
    const s = await call('GET', `api/state?since=${lastQueued}`);
    net = false;
    if (s.session !== st.session) { queued.clear(); load(await call('GET', 'api/state')); ui(); schedulePoll(); return; }   // reset elsewhere
    take(s);
  } catch (err) { net = false; $('error').textContent = err.message; }
  ui(); schedulePoll();
}

/* ---------- controls ---------- */
$('next').onclick = nextHand;
let skipAnswer = false;                           // Skip pressed while the answer is on its way: skip it too
function skip() { if (net) skipAnswer = true; M.skip(); }
$('skip').onclick = skip;
$('new-session').onclick = () => { if (confirm('Reset the score? A new table: the score and the history start again.')) send(() => call('POST', 'api/new', choice()), true); };
function setSpeed(v) { liveSpeed = v; applySpeed(); $('speed').value = v; $('speedv').textContent = v + 'x'; }
$('speed').oninput = e => { setSpeed(+e.target.value); localStorage.setItem(SPEED, liveSpeed); };
function setRM(on) { M.rm = on; $('rm').checked = on; document.body.classList.toggle('rm', on); }
$('rm').onchange = e => setRM(e.target.checked);
$('sign-out').onclick = async () => { await call('POST', 'signout', {}).catch(() => {}); localStorage.removeItem(KEY); location.assign('./'); };
let slideTimer = null;
const setDrawer = open => {
  document.body.classList.add('sliding'); clearTimeout(slideTimer); slideTimer = setTimeout(() => document.body.classList.remove('sliding'), 300);
  document.body.classList.toggle('drawer', open); $('drawer-btn').setAttribute('aria-expanded', open);
};
$('drawer-btn').onclick = () => setDrawer(!document.body.classList.contains('drawer'));
$('scrim').onclick = () => setDrawer(false);
document.addEventListener('keydown', e => {
  if (asleep) return;                             // behind the pause screen the table is not played
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  const el = e.target, k = e.key.toUpperCase();
  const field = el.tagName === 'SELECT' || el.tagName === 'TEXTAREA' || el.tagName === 'INPUT' && (el.type === 'number' || el.type === 'text');
  if (k === 'ESCAPE') { setDrawer(false); skip(); return; }
  if (field) {                                    // the box takes its own keys; Enter in it bets
    if (k === 'ENTER' && el.id === 'size-amount' && $('size-go')) { e.preventDefault(); sizedBet(); }
    if (k === 'ENTER' && el.id === 'token-label') { e.preventDefault(); makeToken(); }
    return;
  }
  if (el.closest('#side, #asleep') && /^(BUTTON|SUMMARY|INPUT|A)$/.test(el.tagName)) return;   // their own keys: Enter, Space, arrows
  if (k === 'H') { setDrawer(!document.body.classList.contains('drawer')); return; }
  if (k === 'J' && queued.size) { e.preventDefault(); jumpToLatest(); return; }
  if (st && st.over && (k === 'ENTER' || k === 'N' || k === ' ')) { e.preventDefault(); nextHand(); return; }
  if (!document.querySelector('#actions button')) return;      // not your turn to decide
  const go = () => e.preventDefault();
  if (k === 'R' || k === 'ENTER') { go(); $('size-go') && sizedBet(); return; }
  const b = [...document.querySelectorAll('#actions button')].find(b => b.dataset.key === k);
  if (b) { go(); b.click(); return; }
  if ($('sizer').hidden || !size) return;
  if (/^[1-6]$/.test(k)) { go(); const d = size.stops[+k - 1]; if (d) setSize(d.c); }                 // pick a size, do not bet
  else if (k === 'ARROWUP' || k === 'ARROWRIGHT') { go(); nudge(e.shiftKey ? 2 : 1); }
  else if (k === 'ARROWDOWN' || k === 'ARROWLEFT') { go(); nudge(e.shiftKey ? -2 : -1); }
  else if (k === 'PAGEUP' || k === 'PAGEDOWN') { go(); toDetent(k === 'PAGEUP' ? 1 : -1); }
});
new ResizeObserver(layout).observe(felt);         // the window, and the dock changing height

/* ---------- boot ---------- */
(async function start() {
  layout();
  setSpeed(+(HASH.get('speed') || localStorage.getItem(SPEED) || 1));
  setRM(HASH.has('rm') ? HASH.get('rm') === '1' : matchMedia('(prefers-reduced-motion: reduce)').matches);
  M.start();
  try {
    info = await call('GET', 'api/info');
    $('blinds').textContent = `Heads-up no-limit hold\u2019em \u00b7 ${info.stackBB} bb stacks, blinds ${info.smallBlind / info.bigBlind}/1 bb`;
    $('opponent').replaceChildren(...info.bots.map(b => Object.assign(document.createElement('option'), { value: b.id, textContent: b.name })));
    $('opp-row').hidden = info.bots.length < 2;     // a choice only when there is one to make
    $('menu').textContent = 'The bots\u2019 bet sizes: ' + info.menu + '. Any other size is read as one of these.';
    league = info.league || null;
    if (league) {
      if (!league.account) { signIn(); return; }
      $('league').hidden = false;
      $('account-name').textContent = league.account.name;
      if (league.ranked) showRanked();
    }
    net = true;
    let s = null;
    if (league) s = await call('GET', 'api/state');   // the account's one table (made on first sight)
    else {
      const saved = localStorage.getItem(KEY);
      if (saved) { try { s = await call('GET', 'api/state?session=' + enc(saved)); } catch (err) { if (err.status !== 404) throw err; } }
      if (!s) s = await call('POST', 'api/new', { seat: 'alternate' });
    }
    net = false;
    for (const r of document.querySelectorAll('input[name=seat]')) r.checked = r.value === s.seatMode;
    $('opponent').value = s.opponent;
    load(s);
    schedulePoll();
  } catch (err) {
    net = false;
    $('error').textContent = 'Could not reach the table: ' + err.message;
  }
})();
