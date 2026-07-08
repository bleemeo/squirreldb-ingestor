package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
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

// metrics holds the internal metrics exposed on the HTTP endpoint. The
// counters incremented on the hot path are resolved once here instead of
// calling WithLabelValues per point.
type metrics struct {
	registry              *prometheus.Registry
	messagesReceived      prometheus.Counter
	messagesDeferred      prometheus.Counter
	messagesDroppedTopic  prometheus.Counter
	messagesDroppedDecode prometheus.Counter
	pointsReceived        prometheus.Counter
	pointsWritten         prometheus.Counter
	pointsDroppedLabels   prometheus.Counter
	pointsDroppedRejected prometheus.Counter
	writeDuration         prometheus.Histogram
}

func newMetrics(connected *atomic.Bool) *metrics {
	registry := prometheus.NewRegistry()
	factory := promauto.With(registry)

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	factory.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "ingestor_mqtt_connected",
		Help: "Whether the MQTT connection is currently established.",
	}, func() float64 {
		if connected.Load() {
			return 1
		}

		return 0
	})

	messagesDropped := factory.NewCounterVec(prometheus.CounterOpts{
		Name: "ingestor_mqtt_messages_dropped_total",
		Help: "Total number of MQTT messages dropped because they could not be processed.",
	}, []string{"reason"})

	pointsDropped := factory.NewCounterVec(prometheus.CounterOpts{
		Name: "ingestor_points_dropped_total",
		Help: "Total number of points dropped because they could not be written.",
	}, []string{"reason"})

	return &metrics{
		registry: registry,
		messagesReceived: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_mqtt_messages_received_total",
			Help: "Total number of messages received over MQTT.",
		}),
		messagesDeferred: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_mqtt_messages_deferred_total",
			Help: "Total number of MQTT messages left unacknowledged because the queue was full, " +
				"to be redelivered by the broker.",
		}),
		messagesDroppedTopic:  messagesDropped.WithLabelValues(dropReasonTopic),
		messagesDroppedDecode: messagesDropped.WithLabelValues(dropReasonDecode),
		pointsReceived: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_points_received_total",
			Help: "Total number of points received over MQTT.",
		}),
		pointsWritten: factory.NewCounter(prometheus.CounterOpts{
			Name: "ingestor_points_written_total",
			Help: "Total number of points successfully written to the remote storage.",
		}),
		pointsDroppedLabels:   pointsDropped.WithLabelValues(dropReasonLabels),
		pointsDroppedRejected: pointsDropped.WithLabelValues(dropReasonRejected),
		writeDuration: factory.NewHistogram(prometheus.HistogramOpts{
			Name:    "ingestor_write_request_duration_seconds",
			Help:    "Duration of the requests to the remote write API.",
			Buckets: prometheus.DefBuckets,
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
	mux.HandleFunc("/healthz", handleLiveness)
	mux.HandleFunc("/readyz", c.handleReadiness)

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

// handleLiveness implements a liveness probe: it only reports that the
// process is alive and serving. The state of the MQTT connection — an
// external dependency — is exposed by the readiness probe instead, so that
// a broker outage doesn't turn into a pod restart loop.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadiness implements a readiness probe that reports whether the MQTT
// connection is established.
func (c *Ingestor) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	if c.connected.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))

		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("MQTT disconnected"))
}
