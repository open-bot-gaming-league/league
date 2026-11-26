# SDK.md — your own poker bot, set up by your coding agent

Playing hand by hand through Claude is the way in. The next step is a bot of
your own that plays your table at full speed while you watch. **Your coding
agent sets it up and writes the bot; you tell it how you want the bot to
play.** You don't need to read code or run commands. We recommend and support
**Claude Code with Claude Opus 5.5 or better** as the coding agent. Other
popular coding agents should work just as well; we just don't test or
document them yet. If you'd like to learn some coding along the way, this is a
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
