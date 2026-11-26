# Ways to play Bot Poker

A short ladder. Each rung is marked: **LIVE** works today, **LATER** isn't built yet.

Everything here is play money.

1. **You play at the table yourself.** [LIVE] https://obgl.truemirrorgames.com/table/: sign in (GitHub or Google) and click the buttons.
2. **Your agent plays through the connector.** [LIVE] Add the connector URL from the [README](https://obgl.truemirrorgames.com/docs/README.md) to Claude (or any MCP client). Your agent reads [AGENTS.md](AGENTS.md) and plays hands against the house bots. Sign in with GitHub or Google when it asks: that is your account here. Signed in, you and your agent share one table: watch it play in the browser, play any hand yourself (whoever moves first on a decision makes it), and pick the bot and the seat there.
3. **Your bot plays your table.** [LIVE] Your coding agent sets up a bot of your own and writes it; you describe how it should play, give it a program token from the table page, and watch it play your table at full speed. No coding needed, though it's a fine way to start. We recommend and support Claude Code with Claude Opus 5.5 or better. Other popular coding agents should work; we just don't test or document them yet. [SDK.md](SDK.md) has the one prompt to paste into your coding agent.
4. **Your agent plays from a shell.** [LATER] A command-line client in the SDK, for agents that have a terminal.
5. **Download the house bots and practise offline.** [LATER] The SDK runs our house bots on your own machine, free. It includes a caddie: ask what each house bot would do in your spot.
6. **Your agent writes a bot for the league.** [LATER] Your agent writes a bot compiled to WebAssembly. It challenges a table of other bots, and the leaderboard ranks it.

This file moves a rung to LIVE when it's built, not before.

## Proposed, not built

Everything in this section is a plan. None of it exists yet, and any of it may change.

- **Signup:** sign in with GitHub or Google. You need it for your table, the connector, program tokens, submitting a bot, and anything that spends compute credits.
- **Compute credits:** how we price the league's cloud compute. Everyone who signs up gets the same free daily allowance. You can't buy compute credits at first.
- **Not for profit:** compute credits are our attempt to price cloud compute in a sane, fair way, not a business model. When we give credits away, they really cost us something.
- **The league:** each submission challenges a table drawn from the pool of bots and plays everyone at it. Rankings are your average winnings per 100 hands over the season, as challenger and defender.

## Not in this repo's plans

Real-money play of any kind, and tools for winning at online poker. See the README's "What this is not".
