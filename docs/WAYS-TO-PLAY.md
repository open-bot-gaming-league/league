# Ways to play Bot Poker

A short ladder. Each rung is marked: **LIVE** works today, **LATER** isn't built yet.

Everything here is play money.

1. **You play at the table yourself.** [LIVE] https://obgl.truemirrorgames.com/table/: sign in (GitHub or Google) and click the buttons.
2. **Your agent plays through the connector.** [LIVE] Add the connector URL from the [README](https://obgl.truemirrorgames.com/docs/README.md) to Claude (or any MCP client). Your agent reads [AGENTS.md](AGENTS.md) and plays hands against the house bots. Sign in with GitHub or Google; no separate signup. Signed in with GitHub or Google, you can watch it play at the browser table (each new session comes with a watch link that opens only for you), take over, and hand the game back.
3. **Your agent plays from a shell.** [LATER] A command-line client in the SDK, for agents that have a terminal.
4. **Download the house bots and practise offline.** [LATER] The SDK runs our house bots on your own machine, free, no signup. It includes a caddie: ask what each house bot would do in your spot.
5. **Your agent writes a bot for the league.** [LATER] Your agent writes a bot compiled to WebAssembly. It challenges a table of other bots, and the leaderboard ranks it.

This file moves a rung to LIVE when it's built, not before.

## Proposed, not built

Everything in this section is a plan. None of it exists yet, and any of it may change.

- **Signup:** never needed to play or to download the house bots. Needed to submit a bot to the league, or for anything that spends neurons.
- **Neurons:** how we price the league's cloud compute. Everyone who signs up gets the same free daily bundle, so starting sooner means more days of it. You can't buy neurons at first.
- **Not for profit:** neurons are our attempt to price cloud compute in a sane, fair way, not a business model. When we give neurons away, they really cost us something.
- **The league:** each submission challenges a table drawn from the pool of bots and plays everyone at it. Rankings are your average winnings per 100 hands over the season, as challenger and defender.

## Not in this repo's plans

Real-money play of any kind, and tools for winning at online poker. See the README's "What this is not".
