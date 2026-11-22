package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/punt-labs/punt-observe-go/internal/privatefs"
	"go.opentelemetry.io/otel/attribute"
)

const (
	queueCapacity  = 256
	defaultDays    = 90
	maxDays        = 3650
	ioTimeout      = 750 * time.Millisecond
	startupTimeout = 3 * time.Second
	shutdownLimit  = 2 * time.Second
)

// Config selects capture and exporter behavior at a tool entry point. The zero
// value enables private SQLite capture. An endpoint is ignored without consent.
type Config struct {
	Disabled      bool
	RetentionDays int
	OTLPConsent   bool
	OTLPEndpoint  string
	Diagnostics   *slog.Logger
}

// Client records observations without exposing providers, exporters, or stores.
type Client struct {
	schemas map[schemaKey]Schema
	salt    atomic.Pointer[[]byte]
	queue   chan measurement
	ready   chan struct{}
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	stopped atomic.Bool
}

type measurement struct {
	schema Schema
	attrs  []attribute.KeyValue
	start  time.Time
	end    time.Time
	amount int64
}

// New starts private capture unless any off switch is set. Startup happens off
// the calling path; configuration and startup failures disable capture.
func New(tool string, config Config, schemas ...Schema) *Client {
	return newClient(tool, config, time.Hour, schemas)
}

func newClient(tool string, config Config, pruneEvery time.Duration, schemas []Schema) *Client {
	c := &Client{ready: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{})}
	perTool := strings.ToUpper(strings.ReplaceAll(tool, "-", "_")) + "_TELEMETRY"
	if config.Disabled || off(os.Getenv("PUNT_TELEMETRY")) || off(os.Getenv(perTool)) {
		close(c.ready)
		close(c.done)
		return c
	}
	c.queue = make(chan measurement, queueCapacity)
	days := config.RetentionDays
	if days == 0 {
		days = defaultDays
	}
	compiled, useSalt, err := registry(tool, schemas)
	c.schemas = compiled
	valid := err == nil && privatefs.ValidTool(tool) && days >= 1 && days <= maxDays
	go c.run(tool, config, days, useSalt, valid, pruneEvery)
	return c
}

func off(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "false", "no", "0":
		return true
	}
	return false
}

// Ready is closed once initialization completes, even when capture is disabled.
// Normal operation must not wait for Ready. Identifier fields are dropped until
// the installation salt is available.
func (c *Client) Ready() <-chan struct{} { return c.ready }

// Record validates an observation and attempts to enqueue it. Invalid, disabled
// and saturated capture silently drop the observation without delaying the tool.
func (c *Client) Record(o Observation) {
	if c.queue == nil || c.stopped.Load() {
		return
	}
	schema, ok := c.schemas[schemaKey{o.Name, o.Version}]
	if !ok || o.Duration < 0 || o.Amount < 0 {
		return
	}
	var salt []byte
	if p := c.salt.Load(); p != nil {
		salt = *p
	}
	attrs, ok := validate(schema, o.Attributes, salt)
	if !ok {
		return
	}
	now := time.Now()
	start := now
	end := start
	if schema.Signal == Span {
		if o.Started.IsZero() {
			start = now.Add(-o.Duration)
		} else {
			start = o.Started
			end = start.Add(o.Duration)
			if end.After(now) {
				return
			}
		}
	}
	select {
	case c.queue <- measurement{schema: schema, attrs: attrs, start: start, end: end, amount: o.Amount}:
	default:
	}
}

// Shutdown requests a best-effort bounded flush. Waiting ends on the caller's
// deadline; exporters are also subject to the shared package's shutdown limit.
func (c *Client) Shutdown(ctx context.Context) {
	if c.queue == nil {
		return
	}
	c.once.Do(func() {
		c.stopped.Store(true)
		close(c.stop)
	})
	ctx, cancel := context.WithTimeout(ctx, shutdownLimit)
	defer cancel()
	select {
	case <-c.done:
	case <-ctx.Done():
	}
}

type diagnostics struct {
	log     *slog.Logger
	last    time.Time
	pending chan struct{}
}

func (d *diagnostics) warn(reason string) {
	now := time.Now()
	if d.log == nil || now.Sub(d.last) < time.Minute {
		return
	}
	if d.pending != nil {
		select {
		case <-d.pending:
		default:
			return
		}
	}
	d.last = now
	done := make(chan struct{})
	d.pending = done
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
		defer cancel()
		d.log.WarnContext(ctx, "Telemetry degraded", "reason", reason)
	}()
}

func (c *Client) run(tool string, config Config, days int, useSalt, valid bool, pruneEvery time.Duration) {
	defer close(c.done)
	d := &diagnostics{log: config.Diagnostics}
	p, err := c.initialize(tool, config, days, useSalt, valid)
	if err != nil {
		c.stopped.Store(true)
		close(c.ready)
		d.warn("initialization failed")
		return
	}
	close(c.ready)
	flush := time.NewTicker(250 * time.Millisecond)
	prune := time.NewTicker(pruneEvery)
	defer flush.Stop()
	defer prune.Stop()
	for {
		select {
		case <-c.stop:
			c.finish(p, d)
			return
		default:
		}
		select {
		case m := <-c.queue:
			p.observe(m)
			if p.pending >= 64 {
				withIO(d, "export failed", p.flush)
			}
		case <-flush.C:
			withIO(d, "export failed", p.flush)
		case <-prune.C:
			withIO(d, "retention maintenance failed", p.sink.prune)
		case <-c.stop:
			c.finish(p, d)
			return
		}
	}
}

func (c *Client) finish(p *pipeline, d *diagnostics) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownLimit)
	defer cancel()
	for range queueCapacity {
		select {
		case m := <-c.queue:
			p.observe(m)
		default:
			goto drained
		}
	}
drained:
	if err := p.shutdown(ctx); err != nil {
		d.warn("shutdown failed")
	}
}

func withIO(d *diagnostics, reason string, action func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	if err := action(ctx); err != nil {
		d.warn(reason)
	}
}
