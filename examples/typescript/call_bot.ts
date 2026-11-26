// A bot that calls every bet, playing your table at the Open Bot Gaming League.
//
// For onboarding only: the league's bots are Go programs (poker/engine/sdk in this
// repository). Once this runs, ask your coding agent to port it to Go, or start from
// poker/engine/sdk/example. Node 22.18+ runs it as is (it strips the types); older:
// npx tsx call_bot.ts.
//
//   export OBGL_TOKEN=obgl_pt_...   # the table page, Programs: Make a token
//   node call_bot.ts 10             # play 10 hands; watch them on the table page

interface Legal { fold: boolean; check: boolean; call?: number; wager?: string; minTo?: number; maxTo?: number }
interface State { hand: number; step: number; over: boolean; legal?: Legal; net: number; outcome?: string;
  totals: { hands: number; bbPer100: number } }

const TABLE = (process.env.OBGL_TABLE || "https://obgl.truemirrorgames.com/table/").replace(/\/?$/, "/");
const HEADERS = { Authorization: `Bearer ${process.env.OBGL_TOKEN || ""}`, "Content-Type": "application/json" };
const sleep = (s: number) => new Promise(r => setTimeout(r, s * 1000));

async function api(method: string, path: string, body?: object): Promise<State> {
  for (let i = 0; i < 3; i++) {
    const r = await fetch(TABLE + "api/" + path, { method, headers: HEADERS, body: body && JSON.stringify(body) });
    if (r.ok) return r.json() as Promise<State>;
    if (r.status === 429) await sleep(Number(r.headers.get("Retry-After")) || 1);  // too fast: wait as told
    else if (r.status === 409) return api("GET", "state?brief=1");                 // someone else moved first
    else throw new Error(`${method} ${path}: ${r.status} ${(await r.json()).error}`);
  }
  throw new Error("the table keeps saying slow down");
}

let hands = Number(process.argv[2] || 10);
let s = await api("GET", "state?brief=1");
if (s.over) s = await api("POST", "next", { hand: s.hand });  // a hand finished before we came: deal ours
while (hands) {
  if (s.over) {
    console.log(`hand ${s.hand}: ${s.net > 0 ? "+" : ""}${s.net}  ${s.outcome}`);
    if (--hands) s = await api("POST", "next", { hand: s.hand });
  } else {  // call every bet: check when there is nothing to call
    const legal = s.legal!;
    const kind = legal.check ? "check" : legal.call ? "call" : "fold";
    s = await api("POST", "act", { hand: s.hand, step: s.step, kind });
  }
}
console.log(`score: ${s.totals.hands} hands, ${s.totals.bbPer100.toFixed(1)} bb/100`);
