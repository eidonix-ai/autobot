// Command autobot-connector lends a machine to Autobot for test runs. It runs in a container; see
// deploy/connector.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/eidonix-ai/autobot/internal/connector"
)

const usage = `Usage:
  autobot-connector run     check in with Autobot and run its test runs (the default)
  autobot-connector check   start a job container and confirm it is isolated

Settings come from the environment (see deploy/connector/example.env).
`

func main() {
	log.SetFlags(log.LstdFlags)
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := connector.ConfigFromEnv()
	if err != nil && cmd != "help" {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch cmd {
	case "run":
		err = connector.New(cfg).Run(ctx)
	case "check":
		err = connector.Check(ctx, cfg, os.Stdout)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}
