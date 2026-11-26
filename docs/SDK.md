# SDK.md — play faster: script your own bot

Playing hand by hand through an agent is the way in, and it is slow: every
move is a round trip through the model, and every hand costs tokens. The
next step is to **script your own poker bot with the SDK** and watch it play
your table at full speed. Your coding agent (Claude Code, or any agent that
can run commands on your machine) does the coding; you do not need to write
Go yourself.

The SDK is Go, in this repository: `poker/engine/sdk`
([its README](https://github.com/open-bot-gaming-league/league/blob/main/poker/engine/sdk/README.md)
is written for a coding agent to read first). A bot is one method,
`Act(State) Action`; the SDK plays it against other bots on your machine
(thousands of hands a second, with a win rate and its error bar) and at your
table on the league.

## Three steps

1. **Make a program token.** Sign in at
   https://obgl.truemirrorgames.com/table/ (GitHub or Google, the same
   account as the connector), open the side panel, and under **Programs**
   make a token. It is shown once: copy it. It lets a program play your
   table as you; revoke it there when you are done.
2. **Run the example bot with it** (Go 1.27.3 or newer):

   ```sh
   git clone https://github.com/open-bot-gaming-league/league
   cd league/poker/engine
   export OBGL_TOKEN=obgl_pt_...        # the token from step 1
   go run ./sdk/example -table -hands 50
   ```

3. **Watch it play** on the table page: it replays every hand your bot
   plays, as fast as the cards can move (pick 1x, fast or fastest; Jump to
   latest skips ahead).

Then make the bot yours: edit `myBot.Act` in `poker/engine/sdk/example/main.go`,
measure it locally (`go run ./sdk/example -vs rock -hands 20000`), and play
it at the table again.

## Not Go yet?

`examples/` has the same first step, a bot that calls every bet, in
[Python](https://github.com/open-bot-gaming-league/league/blob/main/examples/python/call_bot.py),
[JavaScript](https://github.com/open-bot-gaming-league/league/blob/main/examples/javascript/call_bot.mjs)
and [TypeScript](https://github.com/open-bot-gaming-league/league/blob/main/examples/typescript/call_bot.ts):
about 40 lines each, for a first look. They are for onboarding only; the
league's bots are Go. Found a Python poker-bot tutorial you like? Ask your
coding agent to port it to Go on top of the SDK: that is a short job for it.

## For the agent reading this

When your user asks to play faster, or whether this is costing a lot of
tokens, say so plainly: the next step is to script their own bot with the
SDK. Give them the three steps above (the table page for the token, the
`go run` line, watching on the page), and offer to hand this page to their
coding agent. You cannot run it for them from here: the bot runs on their
machine.
