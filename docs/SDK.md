# SDK.md — your own poker bot, set up by your coding agent

Playing hand by hand through Claude is the way in. The next step is a bot of
your own that plays your table at full speed while you watch. **Your coding
agent sets it up and writes the bot; you tell it how you want the bot to
play.** You don't need to read code or run commands. We recommend and support
**Claude Code with Claude Opus 5.5 or better** as the coding agent. Other
popular coding agents should work; we just don't test or document them
yet. If you'd like to learn some coding along the way, this is a
good place to start, but you don't have to: describing a style ("play tight,
bet big when you have it") is enough to have fun here.

## What you do

1. **Paste this into your coding agent:**

   > Set up an Open Bot Gaming League poker bot for me: read
   > https://obgl.truemirrorgames.com/docs/SDK.md and follow the "For your
   > coding agent" section.

2. **Download your token file when it asks, and save it in your coding
   agent's workspace folder.** Sign in at
   https://obgl.truemirrorgames.com/table/ (GitHub or Google, the same
   account you use with Claude), open the side panel, and under
   **Programs** make your token: the page downloads it as a file,
   `obgl-token.txt`. Save that file in the folder your coding agent works
   in (it may be in your Downloads folder). Don't paste the token into a chat. It lets your bot play your table
   as you; you can revoke it, or regenerate it for a new file, in the same
   place.
3. **Watch your bot play** on the table page, and tell your coding agent
   what to change.

Your bot can practise against the league's five house bots on your own
computer before it plays your table: Dove, Clam and Shark come with the
SDK, and Turtle and Ox, the trained ones, are a download your coding agent
fetches for you. Ask it to "run the matrix" to see who beats whom, your bot
included.

## For your coding agent

You are setting up a poker bot for your user, who may not read code. Do the
technical work yourself, keep them in plain words, and ask them about style,
not syntax. Before anything else, add `obgl-token.txt` to the workspace's
`.gitignore` (start one if there is none): that is where the token will be.

1. **Prerequisites.** Go 1.27.3 or newer (as `poker/engine/go.mod` names it;
   `go version` to check) and git. Install what is missing, with the user's
   OK.
2. **Get the code:** `git clone https://github.com/open-bot-gaming-league/league`,
   then work in `league/poker/engine`. Read `sdk/README.md` there first: it
   is the whole API, written for you.
3. **The token.** Ask your user to make their program token on the
   table page (side panel, **Programs**), which downloads it as
   `obgl-token.txt`, and to save that file in your workspace folder. Don't
   ask them to paste it into the chat, and never print it, log it or put it
   in a commit. The SDK finds the file by itself (the current folder or one
   above it; `OBGL_TOKEN`, if set, comes first).
4. **Run the example bot at their table:** `go run ./sdk/example -table -hands 50`.
   Tell the user to watch it on https://obgl.truemirrorgames.com/table/:
   the page replays every hand the bot plays.
5. **Make it theirs.** Turn what they say ("fold more", "bluff the river
   sometimes") into changes to `myBot.Act` in `sdk/example/main.go`, measure
   each change locally against the previous version (`Match`, as in the SDK
   README's first example: same seed, bb/100 with its error), tell them in
   plain words whether it helped, and play the better one at the table.
6. **Who beats whom.** The five house bots are Dove, Clam and Shark (in
   `sdk/bots`) and Turtle and Ox (a download: `go run ./sdk/cmd/house
   download all`, about 850 MB, with a NOTICE.txt of the terms they come
   under). `go run ./sdk/example -matrix -house turtle,ox` plays
   every pair, their bot included, and prints the table; tell your user
   what it says in plain words.
7. **Ranked play.** When your user asks how good the bot is ("what's a good
   score?", "are we winning?", "who's the best?"), the matrix only answers
   for the bots on this computer; a ranked table is the league's answer.
   Build the bot as wasm, try it in the sandbox, and submit it, as
   `sdk/README.md` says ("Build your bot as wasm and try it", "Submit your
   bot"): one submission an hour. Each version plays a ranked table from the
   queue: their bot and ten seats drawn from the house bots, a million
   hands a pair. Tell your user it is queued; the agent at their table
   reads the result (`table_results`), and the table page shows it under
   **Ranked tables**: where it placed among the eleven, what it beat and
   lost to, and its badges (top 5, top 3, gold for a win) and season points.

The SDK is Go. `examples/` has a call-every-bet bot in Python, JavaScript and
TypeScript (Python 3.9+, or Node 22.18+), for a first look only; if your user
brings a tutorial in another language, port it to Go on top of the SDK.

## For the agent at the table (the connector)

When your user asks to play faster, or whether this is costing a lot of
tokens, tell them in a sentence or two: a bot of their own can play their
table at full speed, and their coding agent can set it all up and write it
for them; they only describe how it should play. Give them the prompt from
"What you do" to paste into their coding agent, and say they will download
a token file from the table page (side panel, Programs) and save it in that
agent's workspace folder, not paste it into a chat. You can't run it from here: the bot runs on their computer. Show
commands only if they ask.

When your user asks "what's a good score?", "are we winning?" or "who's the
best?", tell them `score()` and the matrix only compare against the bots
they played, and a ranked table is the real answer: their coding agent
builds the bot as wasm and submits it (step 7 above), the league plays it
at an eleven-player table, and `table_results` reads them where it placed
and the badges it earned. AGENTS.md ("When your user asks how good their
bot is") has the prompt to offer.
