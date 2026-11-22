package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type otlpExporter struct {
	traces    sdktrace.SpanExporter
	metrics   *otlpmetrichttp.Exporter
	transport *http.Transport
}

func openOTLP(ctx context.Context, raw string) (*otlpExporter, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.ForceQuery || endpoint.Fragment != "" || !allowedScheme(endpoint) {
		return nil, errors.New("invalid OTLP endpoint")
	}
	transport := &http.Transport{}
	client := &http.Client{
		Timeout: ioTimeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	headers := map[string]string{}
	spans, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint.JoinPath("v1", "traces").String()),
		otlptracehttp.WithHTTPClient(client), otlptracehttp.WithTimeout(ioTimeout),
		otlptracehttp.WithHeaders(headers), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	)
	if err != nil {
		return nil, err
	}
	metrics, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(endpoint.JoinPath("v1", "metrics").String()),
		otlpmetrichttp.WithHTTPClient(client), otlpmetrichttp.WithTimeout(ioTimeout),
		otlpmetrichttp.WithHeaders(headers), otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
		otlpmetrichttp.WithTemporalitySelector(delta),
	)
	if err != nil {
		return nil, errors.Join(err, spans.Shutdown(ctx))
	}
	return &otlpExporter{traces: spans, metrics: metrics, transport: transport}, nil
}

func allowedScheme(endpoint *url.URL) bool {
	if endpoint.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(endpoint.Hostname())
	return endpoint.Scheme == "http" && ip.IsLoopback()
}

func (e *otlpExporter) exportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return e.traces.ExportSpans(ctx, spans)
}

func (e *otlpExporter) exportMetrics(ctx context.Context, metrics *metricdata.ResourceMetrics) error {
	return e.metrics.Export(ctx, metrics)
}

func (*otlpExporter) prune(context.Context) error { return nil }

func (e *otlpExporter) close(ctx context.Context) error {
	defer e.transport.CloseIdleConnections()
	return errors.Join(e.traces.Shutdown(ctx), e.metrics.Shutdown(ctx))
}
