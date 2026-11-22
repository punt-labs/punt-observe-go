// Package logging provides a private, redacting diagnostic sink for slog.
package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/punt-labs/punt-observe-go/internal/privatefs"
)

// Handler is a private file handler. WithAttrs and WithGroup share its sink.
type Handler struct {
	sink       *sink
	level      slog.Leveler
	role       string
	home       string
	attrs      []boundAttr
	groups     []string
	lastReport *atomic.Int64
}

type boundAttr struct {
	groups []string
	attr   slog.Attr
}

// Option configures a diagnostic handler.
type Option func(*Handler)

// WithLevel sets the minimum durable level. The default is INFO.
func WithLevel(level slog.Leveler) Option {
	return func(h *Handler) {
		if level != nil {
			h.level = level
		}
	}
}

// New opens a private durable sink for an entry point. The caller owns Close.
func New(tool, role string, opts ...Option) (*Handler, error) {
	if !privatefs.ValidTool(tool) || !privatefs.ValidTool(role) {
		return nil, errors.New("invalid diagnostic tool or role")
	}
	dir, err := privatefs.Open(tool, "logs")
	if err != nil {
		return nil, fmt.Errorf("prepare diagnostic directory: %w", err)
	}
	s := &sink{dir: dir, name: tool + ".log"}
	if err := s.setup(); err != nil {
		return nil, fmt.Errorf("prepare diagnostic sink: %w", errors.Join(err, s.close()))
	}
	h := &Handler{sink: s, level: slog.LevelInfo, role: role, lastReport: &atomic.Int64{}}
	h.home, _ = os.UserHomeDir()
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// Close releases the file sink for this handler and all its derived handlers.
func (h *Handler) Close() error { return h.sink.close() }

// Enabled reports whether the record level meets the sink's configured level.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle formats, redacts, and writes a record as one bounded physical line.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if !h.Enabled(ctx, r.Level) {
		return nil
	}
	line, ok := cleanRecord(h.format(r), h.home)
	if !ok {
		return nil
	}
	if err := h.sink.write(ctx, line); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		now := time.Now().Unix()
		last := h.lastReport.Load()
		if now-last >= 60 && h.lastReport.CompareAndSwap(last, now) {
			_, _ = fmt.Fprintln(os.Stderr, "Private diagnostic write failed")
		}
		return err
	}
	return nil
}

// WithAttrs returns a handler with the supplied attributes bound to its current groups.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append([]boundAttr(nil), h.attrs...)
	for _, attr := range attrs {
		next.attrs = append(next.attrs, boundAttr{groups: h.groups, attr: attr})
	}
	return &next
}

// WithGroup returns a handler that qualifies subsequently added attributes.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}

func (h *Handler) format(r slog.Record) string {
	if len(r.Message) >= maxRecordBytes {
		return ""
	}
	var body strings.Builder
	component := h.role
	add := func(groups []string, a slog.Attr) bool {
		if len(groups) == 0 && a.Key == "component" {
			if value := a.Value.Resolve(); value.Kind() == slog.KindString {
				component = value.String()
				return len(component) < maxRecordBytes
			}
		}
		return appendAttr(&body, strings.Join(groups, "."), a, 0)
	}
	for _, a := range h.attrs {
		if !add(a.groups, a.attr) {
			return ""
		}
	}
	valid := true
	r.Attrs(func(a slog.Attr) bool {
		valid = add(h.groups, a)
		return valid
	})
	if !valid || len(component)+len(r.Message)+body.Len() >= maxRecordBytes {
		return ""
	}
	stamp := r.Time
	if stamp.IsZero() {
		stamp = time.Now()
	}
	level := r.Level.String()
	if r.Level == slog.LevelWarn {
		level = "WARNING"
	}
	return stamp.Local().Format("2006-01-02 15:04:05") + " [" + level + "] " +
		component + ": " + r.Message + body.String()
}

func appendAttr(out *strings.Builder, group string, attr slog.Attr, depth int) bool {
	if depth >= 64 || out.Len()+len(group)+len(attr.Key)+2 >= maxRecordBytes {
		return false
	}
	if attr.Equal(slog.Attr{}) {
		return true
	}
	key := attr.Key
	if group != "" && key != "" {
		key = group + "." + key
	} else if key == "" {
		key = group
	}
	if credentialKey.MatchString(key) {
		return appendValue(out, key, "[redacted]")
	}
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		for _, child := range value.Group() {
			if !appendAttr(out, key, child, depth+1) {
				return false
			}
		}
		return true
	}
	if value.Kind() == slog.KindAny {
		switch v := value.Any().(type) {
		case error:
			return appendValue(out, key, errorText(v))
		case []byte:
			if out.Len()+len(key)+len(v)+2 >= maxRecordBytes {
				return false
			}
			return appendValue(out, key, string(v))
		case nil:
			return appendValue(out, key, "<nil>")
		default:
			return appendValue(out, key, "[unsupported value]")
		}
	}
	return appendValue(out, key, value.String())
}

func errorText(err error) (value string) {
	defer func() {
		if recover() != nil {
			value = "[invalid error]"
		}
	}()
	return err.Error()
}

func appendValue(out *strings.Builder, key, value string) bool {
	if out.Len()+len(key)+len(value)+2 >= maxRecordBytes {
		return false
	}
	out.WriteByte(' ')
	out.WriteString(key)
	out.WriteByte('=')
	out.WriteString(value)
	return true
}
