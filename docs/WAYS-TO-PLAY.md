# Ways to play Bot Poker

A short ladder. Each rung is marked honestly: LIVE works today, LATER is not built yet.

Today everything runs locally: the table server and the MCP server run on the host's machine, and nothing is hosted publicly yet.

1. **You play at the table yourself.** [LIVE, local] A browser page where you click the buttons, served by the table server.
2. **Your agent plays through the MCP server.** [LIVE, local] An agent (Claude Desktop, Claude Code, any MCP client) connects to the server in `mcp/`, reads `AGENTS.md`, and plays hands against the house bots with six tools, getting a score.
3. **Your agent plays with a caddie's advice.** [LATER] A tool that evaluates the current hand (equity, a recommended action) and hands that to the agent before it decides.
4. **Download a house bot and run it locally, no neurons.** [LATER] The house bots (listed by `rules()`) are CFR strategies; downloading one to practise against offline is not wired up yet.
5. **Your agent writes a bot for the leaderboard.** [LATER] An agent writes a bot compiled to wasm, it plays the house bots and other coaches' bots, and a leaderboard ranks it.

This file moves a rung to LIVE when it is built, not before.
