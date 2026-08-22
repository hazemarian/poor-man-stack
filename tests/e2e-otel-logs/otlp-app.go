// otlp-app: sends structured logs via OTLP to the collector.
// Uses the standard OTel Go SDK.
//
// Build: go build -o app otlp-app.go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func main() {
	ctx := context.Background()
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName("otlp-test-app"),
		semconv.ServiceVersion("1.0.0"),
	)

	// Traces
	traceExp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
	if err != nil {
		log.Fatalf("trace exporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	defer tp.Shutdown(ctx)

	// Metrics
	metricExp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		log.Fatalf("metric exporter: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)), sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)
	defer mp.Shutdown(ctx)

	// Logs
	logExp, err := otlploggrpc.New(ctx, otlploggrpc.WithInsecure())
	if err != nil {
		log.Fatalf("log exporter: %v", err)
	}
	lp := log.NewLoggerProvider(log.WithProcessor(log.NewBatchProcessor(logExp)), log.WithResource(res))
	otel.SetLoggerProvider(lp)
	defer lp.Shutdown(ctx)

	logger := otel.Logger("test-logger")

	fmt.Println("OTLP app started, sending logs every 2 seconds...")

	for i := 0; i < 20; i++ {
		var record log.Record
		record.SetSeverity(log.SeverityInfo)
		record.SetSeverityText("INFO")
		record.SetBody(log.StringValue(fmt.Sprintf("OTLP log record #%d from otlp-test-app at %s", i+1, time.Now().Format(time.RFC3339))))
		record.SetTimestamp(time.Now())

		logger.Emit(ctx, record)

		// Also do a span to ensure traces flow
		tracer := otel.Tracer("test")
		_, span := tracer.Start(ctx, fmt.Sprintf("operation-%d", i))
		time.Sleep(10 * time.Millisecond)
		span.End()

		time.Sleep(2 * time.Second)
	}

	fmt.Println("Done. Sleeping to allow flush...")
	time.Sleep(15 * time.Second)
	os.Exit(0)
}
