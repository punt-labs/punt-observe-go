// Command punt-observe-smoke demonstrates diagnostics and telemetry.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/punt-labs/punt-observe-go/internal/smoke"
)

func main() {
	var cfg smoke.Config
	flag.StringVar(&cfg.Role, "role", "cli", "diagnostic role")
	flag.IntVar(&cfg.Records, "records", 0, "extra DEBUG records (rotation fixture)")
	flag.BoolVar(&cfg.Spawn, "spawn", false, "run as a detached worker with discarded streams")
	flag.BoolVar(&cfg.Fail, "fail", false, "simulate an unsuccessful operation")
	flag.Parse()
	if err := smoke.Run(context.Background(), cfg); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
