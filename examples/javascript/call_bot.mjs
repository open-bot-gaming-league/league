// A bot that calls every bet, playing your table at the Open Bot Gaming League.
//
// For onboarding only: the league's bots are Go programs (poker/engine/sdk in this
// repository). Once this runs, ask your coding agent to port it to Go, or start from
// poker/engine/sdk/example. Node 18+, no packages.
//
//   export OBGL_TOKEN=obgl_pt_...   # the table page, Programs: Make a token
//   node call_bot.mjs 10            # play 10 hands; watch them on the table page

const TABLE = (process.env.OBGL_TABLE || "https://obgl.truemirrorgames.com/table/").replace(/\/?$/, "/");
const HEADERS = { Authorization: `Bearer ${process.env.OBGL_TOKEN || ""}`, "Content-Type": "application/json" };
const sleep = s => new Promise(r => setTimeout(r, s * 1000));

async function api(method, path, body) {
  for (let i = 0; i < 3; i++) {
    const r = await fetch(TABLE + "api/" + path, { method, headers: HEADERS, body: body && JSON.stringify(body) });
    if (r.ok) return r.json();
    if (r.status === 429) await sleep(+r.headers.get("Retry-After") || 1);  // too fast: wait as the table says
    else if (r.status === 409) return api("GET", "state?brief=1");          // someone else moved first: look again
    else throw new Error(`${method} ${path}: ${r.status} ${(await r.json()).error}`);
  }
  throw new Error("the table keeps saying slow down");
}

let hands = +(process.argv[2] || 10);
let s = await api("GET", "state?brief=1");
if (s.over) s = await api("POST", "next", { hand: s.hand });  // a hand finished before we came: deal ours
while (hands) {
  if (s.over) {
    console.log(`hand ${s.hand}: ${s.net > 0 ? "+" : ""}${s.net}  ${s.outcome}`);
    if (--hands) s = await api("POST", "next", { hand: s.hand });
  } else {  // call every bet: check when there is nothing to call
    const kind = s.legal.check ? "check" : s.legal.call ? "call" : "fold";
    s = await api("POST", "act", { hand: s.hand, step: s.step, kind });
  }
}
console.log(`score: ${s.totals.hands} hands, ${s.totals.bbPer100.toFixed(1)} bb/100`);
