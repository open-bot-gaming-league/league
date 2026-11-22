"""A headless-browser check of the table's Play and Watch, driven step by
step by mcp/test_table_browser.py (which runs the server, the fake GitHub
and the agent, and tells this script what to expect). Not shipped.

    <python with playwright> table_browser_check.py <base URL> <watch URL> [shot dir]

Reads commands from stdin, one per line, and answers each with one line
on stdout ("OK ..." or "FAIL ..."):

    PLAY             sign in at /table/ (GitHub), play one hand by clicking, check the history
    WATCH            open the watch link; the table shows the agent's session, no action buttons
    EXPECT <h> <s>   within 2 s the page shows hand h at step s, and the table settles
    TAKEOVER         click Take over: action buttons appear; click one; answer "OK <hand> <step>"
    TAGIN            click Tag in my agent: the page drops to Watch
    DONE             no browser errors; exit

Chrome: $OBGL_BROWSER_CHROME, else Playwright's own.
"""

import os
import sys
import time

from playwright.sync_api import sync_playwright

BASE, WATCH = sys.argv[1].rstrip("/"), sys.argv[2]
SHOTS = sys.argv[3] if len(sys.argv) > 3 else None
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


def main():
    with sync_playwright() as p:
        chrome = os.environ.get("OBGL_BROWSER_CHROME")
        browser = p.chromium.launch(**({"executable_path": chrome} if chrome else {}))
        page = browser.new_context(viewport={"width": 1600, "height": 900}).new_page()
        page.on("console", lambda m: problems.append(f"console {m.type}: {m.text}") if m.type in ("error", "warning") else None)
        page.on("pageerror", lambda e: problems.append(f"page error: {e}"))
        for line in sys.stdin:
            cmd = line.split()
            if not cmd:
                continue
            try:
                if cmd[0] == "PLAY":
                    page.goto(BASE + "/table/")
                    page.get_by_role("button", name="Sign in with GitHub").click()  # the fake GitHub approves
                    page.wait_for_url(BASE + "/table/")
                    settle(page)
                    assert page.locator("#league").is_visible() and page.locator("#account-name").inner_text() == "alice"
                    assert page.locator("#watch-bar").is_hidden()
                    hand = play_hand(page)
                    assert page.locator(f'#history details.hand[data-number="{hand}"]').count() == 1
                    shot(page, "play")
                    say(f"OK played hand {hand}")
                elif cmd[0] == "WATCH":
                    page.goto(WATCH)
                    settle(page)
                    assert page.locator("#watch-bar").is_visible(), "no watch bar"
                    assert page.locator("#actions button").count() == 0, "action buttons while watching"
                    assert page.locator("#you-name").inner_text() == "Your agent"
                    assert page.evaluate("() => window.table.st.session") is None, "a watcher holds the session id"
                    down = page.evaluate("() => [...document.querySelectorAll('#felt .card[data-who=bot]')].every(c => c.classList.contains('down'))")
                    assert down or page.evaluate("() => window.table.st.over"), "the bot's cards are showing"
                    shot(page, "watch")
                    st = page.evaluate("() => [window.table.st.hand, window.table.st.step]")
                    say(f"OK {st[0]} {st[1]}")
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
                elif cmd[0] == "TAKEOVER":
                    page.locator("#take-over").click()
                    page.wait_for_function("() => document.querySelectorAll('#actions button').length > 0 && window.table.settled()", timeout=5000)
                    assert page.locator("#watch-bar").is_hidden() and page.url == BASE + "/table/"
                    assert page.locator("#you-name").inner_text() == "You"
                    page.locator("#actions button.check, #actions button.call").first.click()
                    settle(page)
                    st = page.evaluate("() => [window.table.st.hand, window.table.st.step, window.table.st.over]")
                    if st[2]:  # the click ended the hand: deal the next, so there is a decision to hand over
                        page.locator("#next").click()
                        settle(page)
                        st = page.evaluate("() => [window.table.st.hand, window.table.st.step, window.table.st.over]")
                    shot(page, "taken-over")
                    say(f"OK {st[0]} {st[1]}")
                elif cmd[0] == "TAGIN":
                    page.locator("#tag-in").click()
                    page.wait_for_function("() => !document.getElementById('watch-bar').hidden", timeout=5000)
                    assert "/table/watch/" in page.url and page.locator("#actions button").count() == 0
                    assert "Handed to your agent" in page.locator("#watch-status").inner_text()
                    shot(page, "tagged-in")
                    say("OK tagged in")
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
