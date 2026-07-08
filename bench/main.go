// Load generator for the SquirrelDB Ingestor pipeline.
//
// It simulates a fleet of Glouton agents publishing metrics over MQTT
// (QoS 1, zlib compressed JSON payloads) and optionally measures the
// end-to-end latency until the points are queryable in the storage.
// See bench/README.md for the methodology and indicative results.
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alexflint/go-arg"
	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	connectTimeout = 30 * time.Second
	publishTimeout = 30 * time.Second
	// Delay between agent starts, to avoid a connection storm.
	agentStartDelay = 2 * time.Millisecond
	probeInterval   = time.Second
	probePollDelay  = 20 * time.Millisecond
	probeTimeout    = 30 * time.Second
)

var errProbeTimeout = errors.New("the probe was not queryable before the timeout")

// Options of the load generator.
type Options struct {
	BrokerURL string        `arg:"--broker,env:BENCH_BROKER"               default:"tcp://localhost:1883"`
	Username  string        `arg:"--mqtt-username,env:BENCH_MQTT_USERNAME"`
	Password  string        `arg:"--mqtt-password,env:BENCH_MQTT_PASSWORD"`
	Agents    int           `arg:"--agents"                                default:"100"`
	Points    int           `arg:"--points"                                default:"100"`
	Interval  time.Duration `arg:"--interval"                              default:"10s"`
	Duration  time.Duration `arg:"--duration"                              default:"60s"`
	QueryURL  string        `arg:"--query-url"                             help:"Prometheus URL, enables latency probes"`
}

type metricPayload struct {
	LabelsText  string  `json:"labels_text"`
	TimestampMS int64   `json:"time_ms"`
	Value       float64 `json:"value"`
}

func main() {
	var opts Options

	arg.MustParse(&opts)

	writer := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05.000"}
	log.Logger = log.Output(writer).With().Timestamp().Logger()

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(signalCtx, opts.Duration)
	defer cancel()

	offered := float64(opts.Agents) * float64(opts.Points) / opts.Interval.Seconds()
	log.Info().Msgf(
		"Starting %d agents publishing %d points every %s (%.0f points/s offered) for %s",
		opts.Agents, opts.Points, opts.Interval, offered, opts.Duration,
	)

	var (
		published    atomic.Int64
		failedAgents atomic.Int64
		wg           sync.WaitGroup
	)

	start := time.Now()

	for i := range opts.Agents {
		wg.Go(func() {
			if err := runAgent(ctx, opts, i, &published); err != nil {
				failedAgents.Add(1)
				log.Warn().Err(err).Msgf("Agent %d failed", i)
			}
		})

		select {
		case <-time.After(agentStartDelay):
		case <-ctx.Done():
		}
	}

	var latencies []time.Duration

	if opts.QueryURL != "" {
		wg.Go(func() {
			latencies = runProbes(ctx, opts)
		})
	}

	wg.Wait()

	elapsed := time.Since(start)

	log.Info().Msgf("Published %d points in %s (%.0f points/s, %.0f offered), %d agents failed",
		published.Load(), elapsed.Round(time.Second),
		float64(published.Load())/elapsed.Seconds(), offered, failedAgents.Load(),
	)

	reportLatencies(latencies)
}

func newClient(opts Options, clientID string) (paho.Client, error) {
	pahoOpts := paho.NewClientOptions()
	pahoOpts.AddBroker(opts.BrokerURL)
	pahoOpts.SetClientID(clientID)
	pahoOpts.SetUsername(opts.Username)
	pahoOpts.SetPassword(opts.Password)
	pahoOpts.SetCleanSession(true)

	client := paho.NewClient(pahoOpts)

	token := client.Connect()
	if !token.WaitTimeout(connectTimeout) {
		return nil, fmt.Errorf("%s: connect timeout", clientID) //nolint:err113
	}

	if token.Error() != nil {
		return nil, fmt.Errorf("%s: connect: %w", clientID, token.Error())
	}

	return client, nil
}

// runAgent simulates one Glouton: it publishes a batch of points at a fixed
// interval on its own topic, like a real agent does.
func runAgent(ctx context.Context, opts Options, id int, published *atomic.Int64) error {
	client, err := newClient(opts, "bench-"+strconv.Itoa(id))
	if err != nil {
		return err
	}

	defer client.Disconnect(100)

	metrics := make([]metricPayload, opts.Points)
	for k := range metrics {
		metrics[k].LabelsText = fmt.Sprintf(`__name__="bench_metric",bench_idx="%d"`, k)
	}

	topic := "v1/agent/bench-" + strconv.Itoa(id) + "/data"

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		now := time.Now()
		for k := range metrics {
			metrics[k].TimestampMS = now.UnixMilli()
			metrics[k].Value = float64(now.Unix())
		}

		payload, err := compress(metrics)
		if err != nil {
			return err
		}

		token := client.Publish(topic, 1, false, payload)
		if !token.WaitTimeout(publishTimeout) {
			return fmt.Errorf("%s: publish timeout", topic) //nolint:err113
		}

		if token.Error() != nil {
			return fmt.Errorf("publish: %w", token.Error())
		}

		published.Add(int64(len(metrics)))
	}
}

func compress(metrics []metricPayload) ([]byte, error) {
	data, err := json.Marshal(metrics)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	var buf bytes.Buffer

	writer := zlib.NewWriter(&buf)

	if _, err := writer.Write(data); err != nil {
		return nil, fmt.Errorf("compress: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close zlib writer: %w", err)
	}

	return buf.Bytes(), nil
}

// runProbes measures the end-to-end latency: it publishes a probe point with
// a sequence number as value, then polls the storage query API until the
// point is visible.
func runProbes(ctx context.Context, opts Options) []time.Duration {
	client, err := newClient(opts, "bench-probe")
	if err != nil {
		log.Warn().Err(err).Msg("The probe client failed to connect")

		return nil
	}

	defer client.Disconnect(100)

	var latencies []time.Duration

	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	for seq := 1; ; seq++ {
		select {
		case <-ctx.Done():
			return latencies
		case <-ticker.C:
		}

		latency, err := probeOnce(ctx, opts, client, seq)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn().Err(err).Msg("Probe failed")
			}

			continue
		}

		latencies = append(latencies, latency)
	}
}

func probeOnce(ctx context.Context, opts Options, client paho.Client, seq int) (time.Duration, error) {
	payload, err := compress([]metricPayload{{
		LabelsText:  `__name__="bench_probe"`,
		TimestampMS: time.Now().UnixMilli(),
		Value:       float64(seq),
	}})
	if err != nil {
		return 0, err
	}

	start := time.Now()

	token := client.Publish("v1/agent/bench-probe/data", 1, false, payload)
	if !token.WaitTimeout(publishTimeout) || token.Error() != nil {
		return 0, fmt.Errorf("publish probe: %w", token.Error())
	}

	for time.Since(start) < probeTimeout && ctx.Err() == nil {
		value, err := queryValue(ctx, opts.QueryURL, "bench_probe")
		if err == nil && value == float64(seq) {
			return time.Since(start), nil
		}

		select {
		case <-time.After(probePollDelay):
		case <-ctx.Done():
		}
	}

	return 0, errProbeTimeout
}

// queryValue returns the current value of a metric using the Prometheus
// query API.
func queryValue(ctx context.Context, baseURL, metric string) (float64, error) {
	queryURL := baseURL + "/api/v1/query?query=" + url.QueryEscape(metric)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("query: %w", err)
	}

	defer resp.Body.Close()

	var result struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Data.Result) == 0 || len(result.Data.Result[0].Value) != 2 {
		return 0, nil
	}

	raw, _ := result.Data.Result[0].Value[1].(string)

	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse value: %w", err)
	}

	return value, nil
}

func reportLatencies(latencies []time.Duration) {
	if len(latencies) == 0 {
		return
	}

	slices.Sort(latencies)

	log.Info().Msgf("End-to-end latency over %d probes: p50=%s p95=%s max=%s",
		len(latencies),
		percentile(latencies, 0.50).Round(time.Millisecond),
		percentile(latencies, 0.95).Round(time.Millisecond),
		latencies[len(latencies)-1].Round(time.Millisecond),
	)
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	index := int(p * float64(len(sorted)-1))

	return sorted[index]
}
