# examples: a first bot in Python, JavaScript or TypeScript

Each is one file of about 40 lines that plays **your table** at the league
over the table's HTTP API (`poker/engine/cmd/table/API.md`) with a program
token, calling every bet. They are for onboarding only: the league's bots
are written in Go with the SDK (`poker/engine/sdk`, start at its README).
Run one to see your token work and your table move; then ask your coding
agent to port it, or the tutorial you like, to Go.

1. Sign in at https://obgl.truemirrorgames.com/table/ and, under **Programs**
   in the side panel, make your token (one per account; Regenerate there
   replaces it).
2. `export OBGL_TOKEN=obgl_pt_...`
3. Run one, and watch the table page:

| language | run | needs |
|---|---|---|
| Python | `python python/call_bot.py 10` | Python 3.9+, nothing else |
| JavaScript | `node javascript/call_bot.mjs 10` | Node 18+ |
| TypeScript | `node typescript/call_bot.ts 10` | Node 22.18+ (or `npx tsx typescript/call_bot.ts 10`) |

`OBGL_TABLE` points them at another table (default the league's).
`mcp/test_examples.py` runs all three against a test server.
