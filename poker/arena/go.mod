module github.com/open-bot-gaming-league/league/poker/arena

go 1.27.3

require (
	github.com/bytecodealliance/wasmtime-go/v51 v51.0.0
	github.com/open-bot-gaming-league/league/poker/engine v0.0.0
)

replace github.com/open-bot-gaming-league/league/poker/engine => ../engine
