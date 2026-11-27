// Command house downloads the league's house bots, Turtle and Ox, into the
// SDK's cache, where the matrix and the example find them by name.
//
//	go run ./sdk/cmd/house download all      # Turtle and Ox, about 850 MB
//	go run ./sdk/cmd/house download turtle   # Turtle alone, about 409 MB
//	go run ./sdk/cmd/house where             # the cache folder
//
// Each file's sha256 is checked; NOTICE.txt, the terms the files come
// under, is downloaded with them. Run it from poker/engine.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/open-bot-gaming-league/league/poker/engine/sdk"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: house download turtle|ox|all ... | house where")
	}
	switch os.Args[1] {
	case "download":
		if err := sdk.DownloadHouse(context.Background(), os.Args[2:]...); err != nil {
			log.Fatal(err)
		}
	case "where":
		dir, err := sdk.HouseCacheDir()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(dir)
	default:
		log.Fatalf("house: no command %q (download, where)", os.Args[1])
	}
}
