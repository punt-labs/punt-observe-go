package telemetry

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const eventScope = "punt.observe.event"

type exporter interface {
	exportSpans(context.Context, []sdktrace.ReadOnlySpan) error
	exportMetrics(context.Context, *metricdata.ResourceMetrics) error
	prune(context.Context) error
	close(context.Context) error
}

type spanBatch struct {
	spans    []sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (*spanBatch) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (b *spanBatch) OnEnd(s sdktrace.ReadOnlySpan) {
	b.spans = append(b.spans, privateSpan{s, b.resource})
}
func (*spanBatch) ForceFlush(context.Context) error { return nil }
func (*spanBatch) Shutdown(context.Context) error   { return nil }

type privateSpan struct {
	sdktrace.ReadOnlySpan
	resource *resource.Resource
}

func (s privateSpan) Resource() *resource.Resource { return s.resource }

type pipeline struct {
	traces      *sdktrace.TracerProvider
	tracer      trace.Tracer
	eventTracer trace.Tracer
	batch       *spanBatch
	resource    *resource.Resource
	metrics     *sdkmetric.MeterProvider
	reader      *sdkmetric.ManualReader
	counters    map[string]metric.Int64Counter
	histograms  map[string]metric.Int64Histogram
	sink        exporter
	pending     int
}

func (c *Client) initialize(tool string, config Config, days int, useSalt, valid bool) (*pipeline, error) {
	if !valid {
		return nil, errors.New("invalid telemetry configuration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	if useSalt {
		salt, err := installationSalt(ctx)
		if err != nil {
			return nil, err
		}
		c.salt.Store(&salt)
	}
	var sink exporter
	var err error
	if config.OTLPConsent && config.OTLPEndpoint != "" {
		sink, err = openOTLP(ctx, config.OTLPEndpoint)
	} else {
		sink, err = openSQLite(ctx, tool, days)
	}
	if err != nil {
		return nil, err
	}
	p, err := newPipeline(tool, c.schemas, sink)
	if err != nil {
		return nil, errors.Join(err, sink.close(ctx))
	}
	return p, nil
}

func newPipeline(tool string, schemas map[schemaKey]Schema, sink exporter) (*pipeline, error) {
	r := resource.NewSchemaless(attribute.String("service.name", tool))
	batch := &spanBatch{resource: r}
	traces := sdktrace.NewTracerProvider(
		sdktrace.WithResource(r), sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(batch),
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
			AttributeValueLengthLimit: 64, AttributeCountLimit: 32,
			EventCountLimit: 1, AttributePerEventCountLimit: 32,
		}),
	)
	reader := sdkmetric.NewManualReader(sdkmetric.WithTemporalitySelector(delta))
	metrics := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(r), sdkmetric.WithReader(reader),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
	)
	p := &pipeline{
		traces: traces, tracer: traces.Tracer("punt.observe"), eventTracer: traces.Tracer(eventScope),
		batch: batch, resource: r, metrics: metrics, reader: reader, sink: sink,
		counters: make(map[string]metric.Int64Counter), histograms: make(map[string]metric.Int64Histogram),
	}
	meter := metrics.Meter("punt.observe")
	for _, schema := range schemas {
		switch schema.Signal {
		case Counter:
			if _, ok := p.counters[schema.Name]; ok {
				continue
			}
			instrument, err := meter.Int64Counter(schema.Name, metric.WithUnit(string(schema.Unit)))
			if err != nil {
				return nil, err
			}
			p.counters[schema.Name] = instrument
		case Histogram:
			if _, ok := p.histograms[schema.Name]; ok {
				continue
			}
			instrument, err := meter.Int64Histogram(schema.Name, metric.WithUnit(string(schema.Unit)))
			if err != nil {
				return nil, err
			}
			p.histograms[schema.Name] = instrument
		}
	}
	return p, nil
}

func delta(sdkmetric.InstrumentKind) metricdata.Temporality { return metricdata.DeltaTemporality }

func (p *pipeline) observe(m measurement) {
	ctx := context.Background()
	switch m.schema.Signal {
	case Span:
		_, span := p.tracer.Start(ctx, m.schema.Name, trace.WithTimestamp(m.start), trace.WithAttributes(m.attrs...))
		span.End(trace.WithTimestamp(m.end))
	case Event:
		_, span := p.eventTracer.Start(ctx, m.schema.Name, trace.WithTimestamp(m.start),
			trace.WithAttributes(attribute.Int("punt.schema.version", m.schema.Version)))
		span.AddEvent(m.schema.Name, trace.WithTimestamp(m.start), trace.WithAttributes(m.attrs...))
		span.End(trace.WithTimestamp(m.end))
	case Counter:
		p.counters[m.schema.Name].Add(ctx, m.amount, metric.WithAttributes(m.attrs...))
	case Histogram:
		p.histograms[m.schema.Name].Record(ctx, m.amount, metric.WithAttributes(m.attrs...))
	}
	p.pending++
}

func (p *pipeline) flush(ctx context.Context) error {
	if p.pending == 0 {
		return nil
	}
	p.pending = 0
	spans := p.batch.spans
	p.batch.spans = nil
	var spansErr error
	if len(spans) != 0 {
		spansErr = p.sink.exportSpans(ctx, spans)
	}
	var metrics metricdata.ResourceMetrics
	metricsErr := p.reader.Collect(ctx, &metrics)
	if metricsErr == nil && len(metrics.ScopeMetrics) != 0 {
		// The SDK merges ambient resource attributes; only our allowlist may leave the pipeline.
		metrics.Resource = p.resource
		metricsErr = p.sink.exportMetrics(ctx, &metrics)
	}
	return errors.Join(spansErr, metricsErr)
}

func (p *pipeline) shutdown(ctx context.Context) error {
	ioCtx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	return errors.Join(
		p.flush(ioCtx), p.traces.Shutdown(ctx), p.metrics.Shutdown(ctx), p.sink.close(ctx),
	)
}
