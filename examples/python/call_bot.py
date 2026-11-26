"""A bot that calls every bet, playing your table at the Open Bot Gaming League.

For onboarding only: the league's bots are Go programs (poker/engine/sdk in this
repository). Once this runs, ask your coding agent to port it to Go, or start from
poker/engine/sdk/example. Python 3.9+, standard library only.

    export OBGL_TOKEN=obgl_pt_...   # the table page, Programs: Make a token
    python call_bot.py 10           # play 10 hands; watch them on the table page
"""
import json, os, sys, time, urllib.error, urllib.request

TABLE = os.environ.get("OBGL_TABLE", "https://obgl.truemirrorgames.com/table/").rstrip("/") + "/"
HEADERS = {"Authorization": "Bearer " + os.environ.get("OBGL_TOKEN", ""), "Content-Type": "application/json"}


def api(method, path, body=None):
    for _ in range(3):
        req = urllib.request.Request(TABLE + "api/" + path, data=body and json.dumps(body).encode(), headers=HEADERS, method=method)
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            if e.code == 429:  # too fast: wait as long as the table says
                time.sleep(int(e.headers.get("Retry-After", "1")))
            elif e.code == 409:  # someone else at the table moved first: look again
                return api("GET", "state?brief=1")
            else:
                sys.exit(f"{method} {path}: {e.code} {json.load(e).get('error')}")
    sys.exit("the table keeps saying slow down")


hands = int(sys.argv[1]) if len(sys.argv) > 1 else 10
s = api("GET", "state?brief=1")
if s["over"]:  # a hand finished before we came: deal ours
    s = api("POST", "next", {"hand": s["hand"]})
while hands:
    if s["over"]:
        print(f"hand {s['hand']}: {s['net']:+d}  {s['outcome']}")
        hands -= 1
        if hands:
            s = api("POST", "next", {"hand": s["hand"]})
    else:  # call every bet: check when there is nothing to call
        legal = s["legal"]
        kind = "check" if legal["check"] else "call" if legal.get("call") else "fold"
        s = api("POST", "act", {"hand": s["hand"], "step": s["step"], "kind": kind})
print(f"score: {s['totals']['hands']} hands, {s['totals']['bbPer100']:+.1f} bb/100")
