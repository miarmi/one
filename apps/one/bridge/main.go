// Command bridge carries chats from Beeper Desktop on the owner's Mac to ONE and
// carries approved replies back. It reads Beeper's local API, talks to ONE over
// https, keeps its place in a small file, and never logs message text or
// tokens. It never turns on Beeper's remote access.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	once := flag.Bool("once", false, "do one round and stop (to see that it works)")
	flag.Parse()
	lg := log.New(os.Stderr, "bridge ", log.LstdFlags)
	cfg, err := ConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bridge:", err)
		os.Exit(2)
	}
	b, err := NewBridge(cfg, lg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bridge:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lg.Printf("started version %s", version)
	if *once {
		if err := b.Cycle(ctx); err != nil {
			os.Exit(1)
		}
		return
	}
	b.Run(ctx)
	lg.Printf("stopped")
}
