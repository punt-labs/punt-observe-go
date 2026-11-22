// Package smoke drives the shared package through its public entry-point APIs.
package smoke

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/punt-labs/punt-observe-go/logging"
	"github.com/punt-labs/punt-observe-go/telemetry"
)

const tool = "punt-observe-smoke"
const operation = tool + ".cli.run"
const parentIDEnv = "PUNT_OBSERVE_SMOKE_PARENT_ID"

var schemas = []telemetry.Schema{
	{Name: operation, Version: 1, Signal: telemetry.Span, Fields: map[string]telemetry.Field{
		"outcome": {Kind: telemetry.Enum, Members: []telemetry.EnumMember{"ok", "failed"}, Required: true},
		"subject": {Kind: telemetry.Identifier},
	}},
	{Name: tool + ".cli.finished", Version: 1, Signal: telemetry.Event},
	{Name: tool + ".cli.records", Version: 1, Signal: telemetry.Counter, Unit: telemetry.UnitOne},
}

// Config controls the example run. No production input is accepted.
type Config struct {
	Role    string
	Records int
	Spawn   bool
	Fail    bool
}

// Run exercises diagnostics and all observation signal classes.
func Run(ctx context.Context, cfg Config) (err error) {
	if cfg.Records < 0 || cfg.Records > 10000 {
		return errors.New("records must be between 0 and 10000")
	}
	var opts []logging.Option
	if cfg.Records > 0 {
		opts = append(opts, logging.WithLevel(slog.LevelDebug))
	}
	h, err := logging.New(tool, cfg.Role, opts...)
	if err != nil {
		return errors.New("cannot initialize private diagnostics")
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	log := slog.New(h).With("component", tool+"."+cfg.Role, "request_id", requestID(cfg.Role))
	if cfg.Spawn {
		return runDetached(ctx, log, cfg)
	}
	started := time.Now()
	capture := telemetry.New(tool, telemetry.Config{Diagnostics: log}, schemas...)
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		capture.Shutdown(flush)
	}()
	log.InfoContext(ctx, "Started diagnostic smoke operation")
	log.WarnContext(ctx, "Rejected malformed fixture request",
		"error", errors.New("provider: https://fixture-user:fixture-pass@example.invalid/path-secret?q=query-secret#fragment-secret\nBearer fixture-auth\x1b[2J"))
	data := strings.Repeat("diagnostic fixture field ", 1900)
	for n := range cfg.Records {
		log.DebugContext(ctx, "Diagnostic load fixture", "line", n, "fields", data)
	}
	outcome := telemetry.EnumMember("ok")
	if cfg.Fail {
		outcome = "failed"
		log.InfoContext(ctx, "Diagnostic smoke operation failed")
	} else {
		log.InfoContext(ctx, "Completed diagnostic smoke operation")
	}
	attrs := []telemetry.Value{telemetry.EnumValue("outcome", outcome)}
	select {
	case <-capture.Ready():
		attrs = append(attrs, telemetry.HashIdentifier("subject", "fixture-person@example.invalid"))
	default:
	}
	capture.Record(telemetry.Observation{Name: operation, Version: 1, Started: started, Duration: time.Since(started), Attributes: attrs})
	capture.Record(telemetry.Observation{Name: tool + ".cli.finished", Version: 1})
	capture.Record(telemetry.Observation{Name: tool + ".cli.records", Version: 1, Amount: int64(cfg.Records)})
	if cfg.Fail {
		return errors.New("fixture operation failed")
	}
	return nil
}

func requestID(role string) string {
	id := os.Getpid()
	if role == "worker" {
		if parent, err := strconv.Atoi(os.Getenv(parentIDEnv)); err == nil && parent > 0 {
			id = parent
		}
	}
	return "smoke-" + strconv.Itoa(id)
}

func runDetached(ctx context.Context, log *slog.Logger, cfg Config) error {
	binary, err := os.Executable()
	if err != nil {
		return errors.New("cannot resolve smoke executable")
	}
	args := []string{"-role", "worker", "-records", strconv.Itoa(cfg.Records)}
	if cfg.Fail {
		args = append(args, "-fail")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), parentIDEnv+"="+strconv.Itoa(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	log.InfoContext(ctx, "Spawning detached smoke worker")
	if err := cmd.Run(); err != nil {
		log.InfoContext(ctx, "Detached smoke worker failed")
		return errors.New("detached smoke worker failed")
	}
	log.InfoContext(ctx, "Detached smoke worker completed")
	return nil
}
