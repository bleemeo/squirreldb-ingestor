package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

const (
	httpReadHeaderTimeout = 10 * time.Second
	httpShutdownTimeout   = 5 * time.Second
)

// Reasons of the dropped messages and points counters.
const (
	dropReasonTopic    = "unexpected_topic"
	dropReasonDecode   = "decode_failure"
	dropReasonLabels   = "invalid_labels"
	dropReasonRejected = "write_rejected"
)

// metrics holds the internal metrics exposed on the HTTP endpoint.
type metrics struct {
	registry         *prometheus.Registry
	messagesReceived prometheus.Counter
	messagesDropped  *prometheus.CounterVec
	pointsReceived   prometheus.Counter
	pointsWritten    prometheus.Counter
	pointsDropped    *prometheus.CounterVec
	writeDuration    prometheus.Histogram
	mqttConnected    prometheus.Gauge
}

func newMetrics() *metrics {
	registry := prometheus.NewRegistry()
	factory := promauto.With(registry)

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return &metrics{
		registry: registry,
		messagesReceived: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_mqtt_messages_received_total",
			Help: "Total number of messages received over MQTT.",
		}),
		messagesDropped: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "ingestor_mqtt_messages_dropped_total",
			Help: "Total number of MQTT messages dropped because they could not be processed.",
		}, []string{"reason"}),
		pointsReceived: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_points_received_total",
			Help: "Total number of points received over MQTT.",
		}),
		pointsWritten: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_points_written_total",
			Help: "Total number of points successfully written to the remote storage.",
		}),
		pointsDropped: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "ingestor_points_dropped_total",
			Help: "Total number of points dropped because they could not be written.",
		}, []string{"reason"}),
		writeDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Name:    "ingestor_write_request_duration_seconds",
			Help:    "Duration of the requests to the remote write API.",
			Buckets: prometheus.DefBuckets,
		}),
		mqttConnected: factory.NewGauge(prometheus.GaugeOpts{
			Name: "ingestor_mqtt_connected",
			Help: "Whether the MQTT connection is currently established.",
		}),
	}
}

// runHTTPServer exposes the metrics and health endpoints until the context is
// canceled. It doesn't return until the server is stopped.
func (c *Ingestor) runHTTPServer(ctx context.Context) {
	if c.opts.HTTPAddress == "" {
		return
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(c.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", c.handleHealth)

	server := &http.Server{
		Addr:              c.opts.HTTPAddress,
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpShutdownTimeout)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	log.Info().Msgf("HTTP server listening on %s", c.opts.HTTPAddress)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Err(err).Msg("HTTP server failed")
	}
}

// handleHealth implements a health check that reports whether the MQTT
// connection is established.
func (c *Ingestor) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if c.connected.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))

		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("MQTT disconnected"))
}
