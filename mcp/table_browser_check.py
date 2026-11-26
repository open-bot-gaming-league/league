"""A headless-browser check of the table page, driven step by step by
mcp/test_table_browser.py (which runs the server, the fake GitHub, the
agent and a program, and tells this script what to expect). Not shipped.

    <python with playwright> table_browser_check.py <base URL> [shot dir]

Reads commands from stdin, one per line, and answers each with one line
on stdout ("OK ..." or "FAIL ..."):

    PLAY             sign in at /table/ (GitHub): the account's table; its score is the server's; play one hand
                     by clicking, check the score counts it and the history has it
    SIZER            at 1280 wide, on a decision with a range of sizes: the detents are the spot's (Min, its sizes
                     as the model prices them, All-in), evenly spaced on the track; a detent button, a slider
                     near a detent (snap) and between two (free, 0.5 bb steps), the bb box (typed, rounded to
                     0.5 bb, clamped), and the keys 1-6, arrows, Shift+arrows and PageUp/PageDown move the size
                     and the Raise button follows, without sending anything; R sends it: the table logs a bet
                     to exactly the chips shown, and remembers the size as a unit
    KEYS             F folds, C checks or calls, N deals the next hand, Enter bets the shown size (and deals
                     when the hand is over); A no longer goes all-in
    MEMORY           the size bet in a spot comes back in that spot in later hands (preflop, as 4x), a
                     remembered pot fraction in the postflop bet spot, and a spot's memory is its own
    RAISE            at phone width (390): the dock fits, nothing scrolls sideways, the drawer opens and closes
                     (button, scrim, H), your cards stay above the dock; then price a size from the state, type
                     it into the box in bb, and bet it; the table's log shows that many chips; "OK <chips>"
    STATE            "OK <hand> <step>": where the page's table is, once settled
    SCORE            the table's running score on the felt (hands, net bb, bb/100) equals the server's
                     totals for the hands finished; answer "OK <hands> <net bb>"
    CATCHUP <h> <s>  the page (busy with a backlog of moves) gets to hand h at step s and settles, within 40 s
    EXPECT <h> <s>   within 2.5 s the page shows hand h at step s (a move someone else made), and settles
    PRESENCE <words> within 5 s the presence line (who is at the table) contains <words>
    TOKEN <label>    in the side panel's Programs box, make a token named <label>: it is shown once, Copy puts
                     it on the clipboard (or selects it with no clipboard), and the list has <label>;
                     answer "OK <token>"
    REVOKE <label>   revoke the token named <label> (confirming); the list no longer has it
    OLDLINK          in a fresh browser (no cookies) open an old watch link (/table/watch/<anything>): sign
                     in (GitHub); it lands on /table/, the account's table; "OK <hand> <step>"
    SLEEPER <s>      a second page on the table, told to sleep after <s> idle seconds (#idle=<s>);
                     it counts its /table/api/state requests from here on
    AWAKE            the sleeper has not paused, and its polling goes on
    ASLEEP           within 15 s the sleeper puts up the pause screen (Resume), then makes no
                     /table/api/state request for 4 s
    HIDE / SHOW      the sleeper's tab goes to the background (Page Visibility) / comes back: it pauses
                     at once and stays paused (and quiet) when shown again until Resume
    RESUME <h> <s>   click Resume: one request at once, then the polling goes on; the pause screen is gone;
                     the page then shows hand h at step s (what happened while it slept)
    DONE             no browser errors; exit

Chrome: $OBGL_BROWSER_CHROME, else Playwright's own.
"""

import os
import re
import sys
import time

from playwright.sync_api import sync_playwright

BASE = sys.argv[1].rstrip("/")
SHOTS = sys.argv[2] if len(sys.argv) > 2 else None
SETTLE = 20000  # ms: a whole all-in run-out at 1x is ~6 s
problems = []


def say(line):
    print(line, flush=True)


def shot(page, name):
    if SHOTS:
        os.makedirs(SHOTS, exist_ok=True)
        page.screenshot(path=os.path.join(SHOTS, name + ".png"))


def settle(page, timeout=SETTLE):
    page.wait_for_function("() => window.table && window.table.st && window.table.settled()", timeout=timeout)


def play_hand(page):
    hand = page.evaluate("() => window.table.st.hand")
    for _ in range(40):
        if page.evaluate("() => window.table.st.over"):
            return hand
        page.locator("#actions button.check, #actions button.call").first.click()
        settle(page)
    raise AssertionError("the hand did not end")


PHONE = {"width": 390, "height": 844}


def score_on_page(page):
    """(hands, net bb, bb/100 or None) as the page prints them."""
    num = lambda t: float(t.replace("\u2212", "-").replace("+", ""))  # noqa: E731  (the page prints a real minus)
    hands = int(page.locator("#stat-hands").inner_text())
    net = num(page.locator("#stat-net").inner_text())
    per100 = page.locator("#stat-bb100").inner_text()
    return hands, net, None if per100 == "\u2013" else num(per100)


class Sleeper:
    """A second page on the table that counts the requests it makes for the table's state."""

    def __init__(self, browser, seconds):
        self.page = browser.new_context(viewport={"width": 1600, "height": 900}).new_page()
        self.requests = []
        self.page.on("request", lambda r: self.requests.append(time.monotonic()) if "/table/api/state" in r.url else None)
        self.page.on("pageerror", lambda e: problems.append(f"page error: {e}"))
        self.sign_in()
        self.page.goto(f"{BASE}/table/?sleeper#idle={seconds}")  # (a new query: a page load, not only a new hash)
        settle(self.page)

    def sign_in(self):
        self.page.goto(BASE + "/table/")
        self.page.get_by_role("button", name="Continue with GitHub").click()
        self.page.wait_for_url(BASE + "/table/")
        settle(self.page)

    asleep = property(lambda self: self.page.evaluate("() => window.table.asleep"))

    def quiet_for(self, seconds):
        """No state request for `seconds`."""
        n = len(self.requests)
        self.page.wait_for_timeout(seconds * 1000)  # (not time.sleep: playwright delivers events only inside its own calls)
        assert len(self.requests) == n, f"{len(self.requests) - n} state requests while paused"

    def polling(self):
        """At least 2 more state requests within 10 s (the table does not poll while it animates a move)."""
        settle(self.page)
        n = len(self.requests)
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and len(self.requests) < n + 2:
            self.page.wait_for_timeout(100)
        assert len(self.requests) >= n + 2, f"the page is not polling ({len(self.requests) - n} of {len(self.requests)} requests; asleep={self.asleep}, table={self.page.evaluate('() => [window.table.busy, window.table.settled()]')}, {self.page.url})"


def set_hidden(page, hidden):
    """Play the browser putting the tab in the background (headless Chrome has no such thing)."""
    page.evaluate(
        """hidden => {
            Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
            Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => hidden ? 'hidden' : 'visible' });
            document.dispatchEvent(new Event('visibilitychange'));
        }""",
        hidden,
    )


def check_score(page):
    """The score on the felt is the server's, for the hands finished (the table has settled)."""
    assert page.locator("#felt #stats").is_visible(), "the score is not on the felt"
    hands, net, per100 = score_on_page(page)
    t = page.evaluate("() => window.table.st.totals")
    assert hands == t["hands"], f"the page shows {hands} hands, the server {t['hands']}"
    assert abs(net - t["netBB"]) < 0.06, f"the page shows net {net} bb, the server {t['netBB']}"
    if hands:
        assert per100 is not None and abs(per100 - t["bbPer100"]) < 0.06, f"the page shows {per100} bb/100, the server {t['bbPer100']}"
    else:
        assert per100 is None
    return hands, net


def check_outcome(page):
    """The outcome line of a hand just over states the result once: a single bb amount for a win or a
    loss (it was said twice), none for a split pot (nobody won an amount)."""
    text = page.locator("#outcome").inner_text()
    amounts = re.findall(r"(?<![\w.])\d[\d,]*(?:\.\d+)?(?= bb\b)", text)
    want = 0 if "split pot" in text else 1
    assert len(amounts) == want, f"the outcome line should state {want} bb amount(s), it says: {text!r}"
    return text


def within_viewport(page, selector):
    """Every element matching `selector` lies wholly inside the viewport."""
    return page.evaluate(
        """sel => [...document.querySelectorAll(sel)].every(el => {
            const r = el.getBoundingClientRect();
            return r.width > 0 && r.left >= 0 && r.top >= 0 && r.right <= innerWidth && r.bottom <= innerHeight;
        })""",
        selector,
    )


def advance_to(page, want, limit=80):
    """Play on (check/call, next hand) until a decision where want(state) holds; its state."""
    for _ in range(limit):
        st = page.evaluate("() => window.table.st")
        if not st["over"] and want(st):
            return st
        page.locator("#next" if st["over"] else "#actions button.check, #actions button.call").first.click()
        settle(page)
    raise AssertionError(f"no such decision in {limit} steps")


def has_sizer(st):
    legal = st.get("legal") or {}
    return bool(legal.get("wager")) and legal["maxTo"] - legal["minTo"] >= 100


SPOT = {"pre": ("x", [2, 2.5, 3, 4]), "bet": ("pot", [1 / 3, 1 / 2, 3 / 4, 1]), "raise": ("x", [2.5, 3, 4])}


def spot_of(st):
    return "pre" if st["round"] == "before the flop" else "raise" if (st["legal"].get("call") or 0) else "bet"


def to_legal(st, c):
    lo, hi = st["legal"]["minTo"], st["legal"]["maxTo"]
    return min(hi, max(lo, round(c / 50) * 50))


def unit_chips(st, spot, k):
    call = st["legal"].get("call") or 0
    return st["you"]["inRound"] + call + k * (st["pot"] + call) if SPOT[spot][0] == "pot" else k * st["bot"]["inRound"]


def expected_stops(st):
    """The model, written out here again: Min, the spot's sizes that fall strictly between, All-in."""
    lo, hi, spot = st["legal"]["minTo"], st["legal"]["maxTo"], spot_of(st)
    out = [lo]
    for k in SPOT[spot][1]:
        c = to_legal(st, unit_chips(st, spot, k))
        if lo < c < hi and c != out[-1]:
            out.append(c)
    return out + [hi]


SIZER_JS = """() => ({
  detents: [...document.querySelectorAll('#detents button')].map(b => ({ c: +b.dataset.c, label: b.firstChild.textContent, on: b.classList.contains('on') })),
  ticks: [...document.querySelectorAll('#ticks b')].map(b => parseFloat(b.style.left)),
  memo: (m => m ? parseFloat(m.style.left) : null)(document.querySelector('#ticks .memo')),
  range: +document.getElementById('size-range').value,
  box: document.getElementById('size-amount').value,
  go: [...document.getElementById('size-go').childNodes].filter(n => n.nodeName !== 'KBD').map(n => n.textContent).join('').replace(/\\s+/g, ' ').trim(),
  step: window.table.st.step, hand: window.table.st.hand })"""


def sizer_now(page):
    return page.evaluate(SIZER_JS)


def blur(page):
    page.evaluate("() => document.activeElement && document.activeElement.blur()")


def set_slider(page, pos):
    page.evaluate("p => { const r = document.getElementById('size-range'); r.value = p; r.dispatchEvent(new Event('input', { bubbles: true })); }", pos)


def shown_chips(page):
    """What the sizer shows, in chips: the box (bb) and the button agree."""
    s = sizer_now(page)
    chips = round(float(s["box"]) * 100)
    assert f"{float(s['box']):g} bb" in s["go"], f"the box says {s['box']} bb, the button '{s['go']}'"
    return chips


def logged_bet(page, st, pressed):
    """After `pressed` ran at decision `st`: the table's logged move for it."""
    pressed()
    # The page has the move once its state carries the event for this step (or the hand has moved on);
    # settle() alone can return before the answer to the key or click has arrived.
    try:
        page.wait_for_function(
            "([hand, step]) => window.table.st.hand !== hand || window.table.st.events.length > step",
            arg=[st["hand"], st["step"]], timeout=SETTLE)
    except Exception as e:
        got = page.evaluate("() => { const s = window.table.st; return [s.hand, s.step, s.events.length]; }")
        raise AssertionError(f"no logged move for hand {st['hand']} step {st['step']} within {SETTLE} ms "
                             f"(the page is at hand/step/events {got}): the key or click did nothing") from e
    settle(page)
    after = page.evaluate("() => window.table.st")
    assert after["hand"] == st["hand"], "the hand moved on before the log could be read"
    return after["events"][st["step"]]


def raise_off_menu(page):
    """At phone width: the dock fits; then bet, typed into the box in bb, an amount none of the menu's buttons offers."""
    st = advance_to(page, has_sizer)
    page.wait_for_timeout(300)
    settle(page)                                                 # the cards have moved to the narrow felt
    check_phone_dock(page)
    shot(page, "a-390-decision")
    lo, hi, call = st["legal"]["minTo"], st["legal"]["maxTo"], st["legal"].get("call", 0)
    menu = {a["amount"] for a in st["actions"] if "amount" in a}
    amount = next(n for n in range(lo + (hi - lo) // 3, hi) if n % 50 == 0 and n not in menu)
    box = page.locator("#size-amount")
    box.fill(f"{amount / 100:g}")
    assert 0 < sizer_now(page)["range"] < 1000, "the slider did not follow the box"
    word = "Raise to" if st["legal"]["wager"] == "raise" else "Bet"
    assert sizer_now(page)["go"] == f"{word} {amount / 100:g} bb", sizer_now(page)["go"]
    assert shown_chips(page) == amount
    mine = logged_bet(page, st, lambda: page.locator("#size-go").click())
    assert mine["seat"] == "you" and mine["to"] == amount, f"the table logged {mine} for a bet to {amount}"
    return amount


def check_sizer(page):
    st = page.evaluate("() => window.table.st")
    lo, hi = st["legal"]["minTo"], st["legal"]["maxTo"]
    s = sizer_now(page)
    want = expected_stops(st)
    assert [d["c"] for d in s["detents"]] == want, f"detents {[d['c'] for d in s['detents']]}, the model says {want}"
    assert s["detents"][0]["label"] == "Min" and s["detents"][-1]["label"] == "All-in" and len(want) >= 3
    n = len(want) - 1
    assert all(abs(t - 100 * i / n) < 0.1 for i, t in enumerate(s["ticks"])), f"detents not evenly spaced on the track: {s['ticks']}"
    assert sum(d["on"] for d in s["detents"]) <= 1
    spot = spot_of(st)
    # a detent button: the box, the slider and the Raise button follow, and nothing is sent
    for i, c in enumerate(want):
        page.locator("#detents button").nth(i).click()
        s = sizer_now(page)
        assert s["box"] == f"{c / 100:g}" and s["go"].endswith(f"{c / 100:g} bb") and abs(s["range"] - 1000 * i / n) < 1.5, (i, c, s)
        assert s["detents"][i]["on"] and s["step"] == st["step"]
    assert s["go"].startswith("All-in"), s["go"]                 # the last detent: all-in says so
    # the slider: near a detent it snaps there; between two it is free, in 0.5 bb steps
    gap = 1000 / n
    for i in range(1, n):
        set_slider(page, 1000 * i / n + 0.10 * gap)
        s = sizer_now(page)
        assert s["box"] == f"{want[i] / 100:g}" and abs(s["range"] - 1000 * i / n) < 1.5, f"no snap at detent {i}: {s}"
        set_slider(page, 1000 * i / n - 0.10 * gap)
        assert sizer_now(page)["box"] == f"{want[i] / 100:g}", f"no snap below detent {i}"
    wide = max(range(n), key=lambda i: want[i + 1] - want[i])      # between two detents the thumb is free: in the widest gap
    if want[wide + 1] - want[wide] >= 200:
        mid = 1000 * (wide + 0.5) / n
        set_slider(page, mid)
        s = sizer_now(page)
        chips = round(float(s["box"]) * 100)
        assert want[wide] < chips < want[wide + 1] and chips % 50 == 0 and abs(s["range"] - mid) < 3, f"between detents {wide} and {wide + 1}: {s}"
        assert chips not in want
        set_slider(page, mid + 1000 / n * 0.2)
        assert round(float(sizer_now(page)["box"]) * 100) % 50 == 0 and shown_chips(page) > chips, "the thumb does not move in 0.5 bb steps between detents"
    set_slider(page, 1000)
    assert shown_chips(page) == hi
    set_slider(page, 0)
    assert shown_chips(page) == lo
    # a real mouse press on the track, on a detent's tick, snaps to it
    if n >= 2:
        tick = page.locator("#ticks b").nth(1).bounding_box()
        page.mouse.click(tick["x"] + tick["width"] / 2 + 2, tick["y"] + tick["height"] / 2)
        assert shown_chips(page) == want[1], "a click on a detent's tick did not land on the detent"
    # the box, in bb: typed amounts go to the nearest 0.5 bb inside the range; the slider and button follow
    box = page.locator("#size-amount")
    typed = (want[0] + (want[-1] - want[0]) // 3) // 50 * 50
    box.fill(f"{typed / 100:g}")
    assert shown_chips(page) == typed and 0 < sizer_now(page)["range"] < 1000, sizer_now(page)
    box.fill(f"{typed / 100 + 0.3:.1f}")
    box.press("Tab")                                              # leaving the box writes the amount it settled on
    assert page.locator("#size-amount").input_value() == f"{(typed + 50) / 100:g}", f"typed {typed / 100 + 0.3:.1f}, the box settled on {page.locator('#size-amount').input_value()}"
    box.fill("9999")
    box.press("Tab")
    assert shown_chips(page) == hi and sizer_now(page)["go"].startswith("All-in")
    box.fill("0")
    box.press("Tab")
    assert shown_chips(page) == lo
    # the keys: 1-6 pick a size without betting; arrows nudge 0.5 bb (Shift: 1 bb); PageUp/PageDown the next detent
    blur(page)
    clamp = lambda c: min(hi, max(lo, c))  # noqa: E731
    page.keyboard.press("2")
    assert shown_chips(page) == want[1] and sizer_now(page)["step"] == st["step"]
    page.keyboard.press("ArrowUp")
    cur = clamp(want[1] + 50)
    assert shown_chips(page) == cur, "ArrowUp"
    page.keyboard.press("Shift+ArrowUp")
    cur = clamp(cur + 100)
    assert shown_chips(page) == cur, "Shift+ArrowUp"
    page.keyboard.press("ArrowDown")
    page.keyboard.press("ArrowDown")
    cur = clamp(clamp(cur - 50) - 50)
    assert shown_chips(page) == cur, "ArrowDown"
    below = [d for d in want if d < cur]
    page.keyboard.press("PageDown")
    assert shown_chips(page) == (below[-1] if below else cur), "PageDown: the detent below"
    cur = shown_chips(page)
    above = [d for d in want if d > cur]
    page.keyboard.press("PageUp")
    assert shown_chips(page) == (above[0] if above else cur), "PageUp: the detent above"
    page.keyboard.press("1")
    page.keyboard.press("PageUp")
    assert shown_chips(page) == want[1], "PageUp from Min"
    page.keyboard.press("2")
    page.keyboard.press("9")
    assert shown_chips(page) == want[1], "a key beyond the last detent moved the size"
    assert sizer_now(page)["step"] == st["step"] and page.evaluate("() => window.table.st.hand") == st["hand"], "a size key sent a move"
    # a slider with the focus still takes the table's keys (F, C, R...), and nudges 0.5 bb on the arrows
    page.locator("#size-range").focus()
    page.keyboard.press("ArrowRight")
    assert shown_chips(page) == clamp(want[1] + 50), "arrows on the focused slider"
    page.keyboard.press("1")
    assert shown_chips(page) == lo
    shot(page, "a-1280-decision")
    # the bet sent is the amount shown, in chips
    page.keyboard.press("2")
    sent = shown_chips(page)
    label = sizer_now(page)["go"]
    mine = logged_bet(page, st, lambda: page.keyboard.press("r"))
    assert mine["seat"] == "you" and mine["kind"] == "bet" and mine["to"] == sent, f"shown {sent} ({label}), logged {mine}"
    assert label.startswith("All-in") == (sent == hi)
    mem = page.evaluate("k => localStorage.getItem('poker50.size.' + k)", spot)
    if sent < hi:
        unit = (sent - st["you"]["inRound"] - (st["legal"].get("call") or 0)) / (st["pot"] + (st["legal"].get("call") or 0)) if SPOT[spot][0] == "pot" else sent / st["bot"]["inRound"]
        assert mem is not None and abs(float(mem) - unit) < 1e-6, f"remembered {mem} for {spot}, the bet was {unit}"
    return sent, spot


def no_sideways_scroll(page):
    assert page.evaluate("() => document.documentElement.scrollWidth <= innerWidth && document.body.scrollWidth <= innerWidth"), \
        page.evaluate("() => [document.documentElement.scrollWidth, document.body.scrollWidth, innerWidth]")


def check_phone_dock(page):
    """390 wide, on a decision with a sizer: nothing scrolls sideways, the dock is wholly in view and below the felt,
    your cards are above it, and the side panel is a drawer (button, scrim, H, Esc)."""
    no_sideways_scroll(page)
    assert within_viewport(page, "#detents button, #size-range, #size-amount, #actions button"), "the dock does not fit the phone's screen"
    box = lambda sel: page.locator(sel).first.bounding_box()  # noqa: E731
    bar, felt = box("#bar"), box("#felt")
    assert bar["y"] >= felt["y"] + felt["height"] - 1 and bar["y"] + bar["height"] <= page.viewport_size["height"] + 1, (bar, felt)
    cards = page.evaluate("() => [...document.querySelectorAll('#felt .card[data-who=you]')].map(c => { const r = c.getBoundingClientRect(); return [r.top, r.bottom, r.left, r.right]; })")
    assert cards and all(b <= bar["y"] and l >= 0 and r <= page.viewport_size["width"] for _, b, l, r in cards), f"your cards are not clear of the dock: {cards}"
    assert page.locator("#side").is_hidden() and page.locator("#drawer-btn").is_visible(), "the side panel is not a drawer on a phone"
    page.locator("#drawer-btn").click()
    page.wait_for_timeout(400)                                   # it slides in
    assert page.locator("#side").is_visible() and within_viewport(page, "#side"), "the drawer does not open on screen"
    no_sideways_scroll(page)
    page.mouse.click(8, 400)                                     # the scrim
    page.wait_for_function("() => getComputedStyle(document.getElementById('side')).visibility === 'hidden'")
    blur(page)
    page.keyboard.press("h")
    page.wait_for_function("() => getComputedStyle(document.getElementById('side')).visibility === 'visible'", timeout=2000)   # H opens it
    page.keyboard.press("Escape")
    page.wait_for_function("() => getComputedStyle(document.getElementById('side')).visibility === 'hidden'")


def local_key(page, spot):
    return page.evaluate("k => localStorage.getItem('poker50.size.' + k)", spot)


def faster(page):
    """Animations at 2x for the long hunts below."""
    page.evaluate("() => localStorage.setItem('poker50.speed', '2')")
    page.reload()
    settle(page)


def copy_works(page, button, code):
    """A Copy button: it fills the clipboard and, with no clipboard, selects the text."""
    text = page.locator(code).inner_text().strip()
    page.context.grant_permissions(["clipboard-read", "clipboard-write"], origin=BASE)
    page.locator(button).click()
    page.wait_for_function("b => document.querySelector(b + ' span').textContent === 'Copied'", arg=button, timeout=3000)
    assert page.evaluate("() => navigator.clipboard.readText()") == text, "Copy did not put the text on the clipboard"
    page.wait_for_function("b => document.querySelector(b + ' span').textContent === 'Copy'", arg=button, timeout=4000)
    page.evaluate("() => Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })")   # as on plain http
    page.evaluate("() => getSelection().removeAllRanges()")
    page.locator(button).click()
    assert page.evaluate("() => getSelection().toString().trim()") == text, "with no clipboard, Copy did not select the text"
    page.wait_for_function("b => document.querySelector(b + ' span').textContent === 'Copy'", arg=button, timeout=4000)
    return text


def token_labels(page):
    return page.evaluate("() => [...document.querySelectorAll('#token-list li b')].map(b => b.textContent)")


def main():
    with sync_playwright() as p:
        chrome = os.environ.get("OBGL_BROWSER_CHROME")
        browser = p.chromium.launch(**({"executable_path": chrome} if chrome else {}))
        page = browser.new_context(viewport={"width": 1600, "height": 900}).new_page()
        page.on("console", lambda m: problems.append(f"console {m.type}: {m.text}") if m.type in ("error", "warning") else None)
        page.on("pageerror", lambda e: problems.append(f"page error: {e}"))
        page.on("dialog", lambda d: d.accept())   # the page confirms a revoke and a reset
        for line in sys.stdin:
            cmd = line.split()
            if not cmd:
                continue
            try:
                if cmd[0] == "PLAY":
                    page.goto(BASE + "/table/")
                    page.get_by_role("button", name="Continue with GitHub").click()  # the fake GitHub approves
                    page.wait_for_url(BASE + "/table/")
                    settle(page)
                    assert page.locator("#league").is_visible() and page.locator("#account-name").inner_text() == "alice"
                    before = check_score(page)[0]
                    hand = play_hand(page)
                    assert check_score(page)[0] == before + 1, "the score did not count the hand just played"
                    outcome = check_outcome(page)
                    tall = page.viewport_size
                    page.set_viewport_size({"width": 900, "height": 1100})  # the side panel goes under the felt
                    assert within_viewport(page, "#stats"), "the score is not wholly in view on a tall window"
                    page.set_viewport_size(tall)
                    assert page.locator(f'#history details.hand[data-number="{hand}"]').count() == 1
                    shot(page, "play")
                    say(f"OK played hand {hand}: {outcome}")
                elif cmd[0] == "SIZER":
                    size = page.viewport_size
                    page.set_viewport_size({"width": 1280, "height": 720})
                    try:
                        st = advance_to(page, has_sizer)
                        sent, spot = check_sizer(page)
                        shot(page, "a-1280-over")
                    finally:
                        page.set_viewport_size(size)
                    settle(page)
                    say(f"OK {spot} {sent}")
                elif cmd[0] == "KEYS":
                    st = advance_to(page, lambda s: bool(s["legal"].get("fold")))
                    blur(page)
                    mine = logged_bet(page, st, lambda: page.keyboard.press("f"))
                    assert mine["seat"] == "you" and mine["kind"] == "fold", mine
                    assert page.evaluate("() => window.table.st.over"), "F did not fold"
                    assert "folded" in check_outcome(page), "a fold's outcome line should say so"
                    shot(page, "a-1280-over")
                    size = page.viewport_size
                    page.set_viewport_size(PHONE)
                    shot(page, "a-390-over")
                    no_sideways_scroll(page)
                    assert within_viewport(page, "#next"), "Next hand is off the phone's screen"
                    page.set_viewport_size(size)
                    hand = st["hand"]
                    page.keyboard.press("n")
                    page.wait_for_function("h => window.table.st.hand === h", arg=hand + 1, timeout=5000)
                    settle(page)
                    st = advance_to(page, lambda s: True)
                    page.keyboard.press("a")                       # A went all-in once; it does nothing now
                    page.wait_for_timeout(400)
                    assert page.evaluate("() => window.table.st.step") == st["step"], "A sent a move"
                    mine = logged_bet(page, st, lambda: page.keyboard.press("c"))
                    assert mine["seat"] == "you" and mine["kind"] in ("check", "call"), mine
                    st = advance_to(page, has_sizer)
                    blur(page)
                    sent = shown_chips(page)
                    mine = logged_bet(page, st, lambda: page.keyboard.press("Enter"))
                    assert mine["seat"] == "you" and mine["kind"] == "bet" and mine["to"] == sent, f"Enter: shown {sent}, logged {mine}"
                    st = advance_to(page, lambda s: bool(s["legal"].get("fold")))
                    logged_bet(page, st, lambda: page.keyboard.press("f"))
                    page.keyboard.press("Enter")                   # a hand that is over: Enter deals
                    page.wait_for_function("h => window.table.st.hand === h", arg=st["hand"] + 1, timeout=5000)
                    settle(page)
                    say("OK keys")
                elif cmd[0] == "MEMORY":
                    page.evaluate("() => Object.keys(localStorage).filter(k => k.startsWith('poker50.size.')).forEach(k => localStorage.removeItem(k))")
                    faster(page)
                    st = advance_to(page, lambda s: has_sizer(s) and spot_of(s) == "pre")
                    s = sizer_now(page)
                    assert s["memo"] is None and s["box"] == f"{to_legal(st, unit_chips(st, 'pre', 2.5)) / 100:g}", f"no memory: the preflop box should open on 2.5x: {s}"
                    four = to_legal(st, unit_chips(st, "pre", 4))
                    stops = expected_stops(st)
                    assert four in stops, (four, stops)
                    blur(page)
                    page.keyboard.press(str(stops.index(four) + 1))
                    assert shown_chips(page) == four
                    mine = logged_bet(page, st, lambda: page.keyboard.press("r"))
                    assert mine["to"] == four
                    assert abs(float(local_key(page, "pre")) - 4) < 1e-9, "the preflop size is not kept as 4x"
                    assert local_key(page, "bet") is None and local_key(page, "raise") is None, "one spot's size was kept for the others"
                    hand = st["hand"]
                    st = advance_to(page, lambda s: s["hand"] > hand and has_sizer(s) and spot_of(s) == "pre")
                    s = sizer_now(page)
                    assert s["box"] == f"{to_legal(st, unit_chips(st, 'pre', 4)) / 100:g}", f"the next hand's preflop box should open on the last size, 4x: {s}"
                    assert s["memo"] is not None and [d["label"] for d in s["detents"] if d["on"]] in (["4x"], ["All-in"]), s
                    # a remembered pot fraction (set here, as a bet would) is the postflop bet spot's own
                    page.evaluate("() => localStorage.setItem('poker50.size.bet', '0.75')")
                    faster(page)
                    st = advance_to(page, lambda s: has_sizer(s) and spot_of(s) == "bet", limit=120)
                    s = sizer_now(page)
                    assert s["box"] == f"{to_legal(st, unit_chips(st, 'bet', 0.75)) / 100:g}" and s["memo"] is not None, f"the postflop bet box should open on 3/4 pot: {s}"
                    assert abs(float(local_key(page, "pre")) - 4) < 1e-9
                    # and a spot with no memory opens on its default: 3x the bet it faces
                    st = advance_to(page, lambda s: has_sizer(s) and spot_of(s) == "raise" and s["round"] != "before the flop", limit=150)
                    s = sizer_now(page)
                    assert s["memo"] is None and s["box"] == f"{to_legal(st, unit_chips(st, 'raise', 3)) / 100:g}", f"the raise box should open on 3x: {s}"
                    say("OK memory")
                elif cmd[0] == "RAISE":
                    size = page.viewport_size
                    page.set_viewport_size(PHONE)
                    try:
                        amount = raise_off_menu(page)
                        shot(page, "raise-phone")
                    finally:
                        page.set_viewport_size(size)
                    settle(page)
                    say(f"OK {amount}")
                elif cmd[0] == "STATE":
                    settle(page)
                    st = page.evaluate("() => [window.table.st.hand, window.table.st.step]")
                    say(f"OK {st[0]} {st[1]}")
                elif cmd[0] == "SCORE":
                    settle(page)
                    hands, net = check_score(page)
                    shot(page, "score")
                    say(f"OK {hands} {net}")
                elif cmd[0] == "CATCHUP":
                    page.wait_for_function(
                        "([h, s]) => window.table.st && window.table.st.hand === h && window.table.st.step === s",
                        arg=[int(cmd[1]), int(cmd[2])], timeout=40000,
                    )
                    settle(page, 40000)
                    say("OK caught up")
                elif cmd[0] == "EXPECT":
                    hand, step = int(cmd[1]), int(cmd[2])
                    start = time.monotonic()
                    page.wait_for_function(
                        "([h, s]) => window.table.st && window.table.st.hand === h && window.table.st.step === s",
                        arg=[hand, step], timeout=2500,
                    )
                    seen = time.monotonic() - start
                    settle(page)
                    say(f"OK seen in {seen:.2f}s")
                elif cmd[0] == "PRESENCE":
                    words = " ".join(cmd[1:])
                    page.wait_for_function("w => !document.getElementById('presence').hidden && document.getElementById('presence').textContent.includes(w)",
                                           arg=words, timeout=5000)
                    shot(page, "presence")
                    say("OK " + page.locator("#presence").inner_text())
                elif cmd[0] == "TOKEN":
                    label = " ".join(cmd[1:])
                    programs = page.locator("#programs")
                    if not programs.evaluate("d => d.open"):
                        programs.locator("summary").click()
                    page.wait_for_function("() => !document.getElementById('token-none').hidden || document.querySelectorAll('#token-list li').length", timeout=5000)
                    page.locator("#token-label").fill(label)
                    page.locator("#token-make").click()
                    page.wait_for_function("() => !document.getElementById('token-new').hidden", timeout=5000)
                    token = page.locator("#token-value").inner_text().strip()
                    assert token.startswith("obgl_pt_"), token
                    assert token in page.locator("#token-use").inner_text(), "the lines to use it do not have the token"
                    page.wait_for_function("l => [...document.querySelectorAll('#token-list li b')].some(b => b.textContent === l)", arg=label, timeout=5000)
                    assert copy_works(page, "#token-copy", "#token-value") == token
                    assert within_viewport(page, "#token-new"), "the new token is not wholly in view"
                    shot(page, "token")
                    say(f"OK {token}")
                elif cmd[0] == "REVOKE":
                    label = " ".join(cmd[1:])
                    n = len(token_labels(page))
                    page.locator("#token-list li", has_text=label).locator("button.revoke").click()
                    page.wait_for_function("n => document.querySelectorAll('#token-list li').length === n - 1", arg=n, timeout=5000)
                    assert label not in token_labels(page)
                    say("OK revoked")
                elif cmd[0] == "OLDLINK":
                    fresh = browser.new_context(viewport={"width": 1600, "height": 900}).new_page()
                    fresh.on("pageerror", lambda e: problems.append(f"page error: {e}"))
                    fresh.goto(BASE + "/table/watch/an-old-watch-token")
                    assert fresh.url.startswith(BASE + "/login"), f"an unsigned old watch link went to {fresh.url}"
                    fresh.get_by_role("button", name="Continue with GitHub").click()  # the fake GitHub approves
                    fresh.wait_for_url(BASE + "/table/")
                    settle(fresh)
                    shot(fresh, "old-link")
                    st = fresh.evaluate("() => [window.table.st.hand, window.table.st.step]")
                    fresh.context.close()
                    say(f"OK {st[0]} {st[1]}")
                elif cmd[0] == "SLEEPER":
                    sleeper = Sleeper(browser, float(cmd[1]))
                    say("OK sleeper up")
                elif cmd[0] == "AWAKE":
                    assert not sleeper.asleep and sleeper.page.locator("#asleep").is_hidden(), "the page paused: " + sleeper.page.locator("#asleep-why").inner_text()
                    sleeper.polling()
                    say("OK awake")
                elif cmd[0] == "ASLEEP":
                    sleeper.page.wait_for_function("() => window.table.asleep", timeout=15000)
                    assert sleeper.page.locator("#asleep").is_visible() and sleeper.page.locator("#resume").is_visible(), "no pause screen"
                    shot(sleeper.page, "asleep")
                    sleeper.page.wait_for_timeout(1500)  # a request already out when it fell asleep
                    sleeper.quiet_for(4)
                    say("OK asleep and quiet")
                elif cmd[0] == "HIDE":
                    assert not sleeper.asleep
                    set_hidden(sleeper.page, True)
                    assert sleeper.asleep, "a hidden tab did not pause"
                    sleeper.page.wait_for_timeout(1500)
                    sleeper.quiet_for(3.5)
                    say("OK hidden and quiet")
                elif cmd[0] == "SHOW":
                    set_hidden(sleeper.page, False)
                    assert sleeper.asleep and sleeper.page.locator("#asleep").is_visible(), "the tab came back and the page woke by itself"
                    sleeper.quiet_for(2.5)
                    say("OK shown, still paused")
                elif cmd[0] == "RESUME":
                    n = len(sleeper.requests)
                    sleeper.page.locator("#resume").click()
                    sleeper.page.wait_for_function("() => !window.table.asleep")
                    assert sleeper.page.locator("#asleep").is_hidden()
                    sleeper.page.wait_for_function(
                        "([h, s]) => window.table.st.hand === h && window.table.st.step === s", arg=[int(cmd[1]), int(cmd[2])], timeout=5000)
                    assert len(sleeper.requests) >= n + 1, "Resume did not fetch the state"
                    settle(sleeper.page)
                    sleeper.polling()
                    say("OK resumed")
                elif cmd[0] == "DONE":
                    say("OK done" if not problems else "FAIL " + " | ".join(problems))
                    break
                else:
                    say(f"FAIL unknown command {cmd[0]}")
            except Exception as e:  # noqa: BLE001
                shot(page, "failure")
                say(f"FAIL {cmd[0]}: {type(e).__name__}: {str(e).splitlines()[0] if str(e) else ''}")
        browser.close()


if __name__ == "__main__":
    main()
