# Ways to play Bot Poker

A short ladder. Each rung is marked honestly — this is a build log, not
a roadmap promise.

1. **You play at the table yourself.** [LIVE] The browser page at
   http://localhost:9777/poker/ — you click the buttons.
2. **Your Claude plays through this MCP server.** [LIVE after this
   build] A plain Claude, wired up with no help beyond `AGENTS.md` and
   the six tools in `obgl/mcp/`, plays hands against the house bots and
   gets a score. This is "stranger test 2".
3. **Your Claude plays with a caddie's advice.** [LATER] A second tool
   or resource that evaluates the current hand (equity, a recommended
   action) and hands that to the agent before it decides. Not built;
   stranger test 2's spec explicitly leaves this out — the point of
   that test is how a stranger plays unaided, not how well a caddie
   plays through it.
4. **Download a house bot, run it locally, no neurons.** [LATER] The
   bots on offer (see `rules()`'s `bots` list, or `api/info`) are CFR
   strategies; running one locally — rather than only playing against
   it on the live table — is not wired up as a download yet.
5. **Your Claude writes a wasm bot for the leaderboard.** [LATER] The
   furthest rung: an agent writes a bot to a wasm target, it runs
   against the house strategies or other coaches' bots, and a
   leaderboard ranks it. No leaderboard or wasm harness exists for
   poker yet (see OBGL's `DESIGN.md`/`EPICS.md` for what that would
   need).

Rung 2 is what `obgl/mcp/` builds. Rungs 3-5 are not started; this file
will move their marks to LIVE or NEXT as they get built, not before.
