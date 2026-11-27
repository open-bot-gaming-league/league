// Command bot submits your bot to the league, and lists the ones you have
// submitted. A submitted bot is a WebAssembly module (your bot built with
// GOOS=wasip1) and, if it reads any, a folder of data files: at most 64 MB
// of module and 16 GB of data. The league checks the module and keeps it
// as the next version of the bot by that name; submit waits for that
// answer. One submission an hour.
//
//	go run ./sdk/cmd/bot submit -name mybot -wasm bot.wasm [-data dir]
//	go run ./sdk/cmd/bot list
//	go run ./sdk/cmd/bot status SUBMISSION   # where a submission stands
//
// It builds nothing: it uploads what you give it. The program token comes
// from OBGL_TOKEN or obgl-token.txt (as for playing your table); OBGL_TABLE
// names another league server. Output is one plain line a step. A failed
// upload can be resumed: run the same submit again, and it sends only what
// the league does not have yet.
//
// Run it from poker/engine, the Go module's root.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "submit":
		err = submit(ctx, os.Args[2:])
	case "list":
		err = list(ctx, os.Args[2:])
	case "status":
		err = status(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bot:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bot submit -name NAME -wasm FILE [-data DIR]\n       bot list\n       bot status SUBMISSION")
	os.Exit(2)
}

func league() (*sdk.Table, error) {
	t, err := sdk.TableFromEnv()
	if err != nil {
		return nil, err
	}
	if t.Token == "" {
		_, err := sdk.FindToken()
		return nil, err
	}
	return t, nil
}

func submit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	name := fs.String("name", "", "the bot's name: lowercase letters, digits and '-' (each submission is its next version)")
	wasm := fs.String("wasm", "", "the bot's WebAssembly module")
	data := fs.String("data", "", "a folder of data files the bot reads (optional)")
	fs.Parse(args)
	if *name == "" || *wasm == "" || fs.NArg() > 0 {
		usage()
	}
	t, err := league()
	if err != nil {
		return err
	}
	v, err := t.SubmitBot(ctx, sdk.SubmitOptions{Name: *name, Wasm: *wasm, DataDir: *data, Log: func(s string) { fmt.Println(s) }})
	if err != nil {
		return err
	}
	fmt.Println("submitted:", v)
	return nil
}

func list(ctx context.Context, args []string) error {
	if len(args) > 0 {
		usage()
	}
	t, err := league()
	if err != nil {
		return err
	}
	bots, err := t.Bots(ctx)
	if err != nil {
		return err
	}
	if len(bots) == 0 {
		fmt.Println("no bots submitted yet: go run ./sdk/cmd/bot submit -name NAME -wasm FILE")
	}
	for _, v := range bots {
		fmt.Println(v)
	}
	return nil
}

func status(ctx context.Context, args []string) error {
	if len(args) != 1 {
		usage()
	}
	t, err := league()
	if err != nil {
		return err
	}
	s, err := t.Submission(ctx, args[0])
	if err != nil {
		return err
	}
	switch s.State {
	case "checking":
		fmt.Printf("submission %s (%s): checking, since %s\n", s.ID, s.Name, time.Unix(s.RequestedAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
	case "finalized":
		fmt.Printf("submission %s: accepted: %v\n", s.ID, s.Bot)
		if s.Table != nil {
			fmt.Printf("ranked table %s: %s\n", s.Table.ID, s.Table.State)
		}
	case "rejected":
		fmt.Printf("submission %s (%s): refused: %s (%s)\n", s.ID, s.Name, strings.Join(s.Problems, "; "), sdk.RefusedNote)
	case "open":
		fmt.Printf("submission %s (%s): open: not finalized yet (run the same submit again)\n", s.ID, s.Name)
	default:
		fmt.Printf("submission %s (%s): %s\n", s.ID, s.Name, s.State)
	}
	return nil
}
