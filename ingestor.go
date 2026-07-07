package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/rs/zerolog/log"
)

const (
	// Delay between connection attempts to MQTT.
	mqttRetryDelay = 10 * time.Second
	// Delay between write attempts to the remote storage.
	storageRetryDelay = 10 * time.Second
	// Maximum number of points per write request to the remote storage.
	batchMaxPoints = 10000
	// Maximum delay before pending points are written to the remote storage.
	batchFlushInterval = time.Second
	// Number of decoded MQTT messages that can be queued waiting to be
	// written. When the queue is full the MQTT client stops consuming
	// messages and the broker buffers them.
	messageQueueSize = 64
	// Timeout of the last write attempt during shutdown.
	finalFlushTimeout = 5 * time.Second
	// Time given to the MQTT client to disconnect cleanly, in ms.
	mqttDisconnectTimeoutMS = 1000
)

var (
	errParseFQDN = errors.New("could not parse FQDN")
	errNotPem    = errors.New("not a PEM file")
)

var dataTopicRegex = regexp.MustCompile("^v1/agent/(.*)/data")

// Ingestor reads metrics from MQTT and writes them to a remote storage.
type Ingestor struct {
	client    paho.Client
	writer    *Writer
	opts      Options
	metrics   *metrics
	connected atomic.Bool
	// messages holds the decoded MQTT messages waiting to be written.
	messages chan message
	// It's bad practice to store a context in a struct,
	// but we need to use it in paho callbacks.
	ctx context.Context //nolint:containedctx
}

// message holds the points decoded from one MQTT message and the callback
// that acknowledges the message to the broker.
type message struct {
	samples []sample
	ack     func()
}

// batch accumulates the points of several MQTT messages before writing them
// to the remote storage in a single request.
type batch struct {
	samples []sample
	acks    []func()
}

func (b *batch) add(msg message) {
	b.samples = append(b.samples, msg.samples...)
	b.acks = append(b.acks, msg.ack)
}

// done acknowledges all the messages of the batch and empties it.
func (b *batch) done() {
	for _, ack := range b.acks {
		ack()
	}

	b.samples = b.samples[:0]
	b.acks = b.acks[:0]
}

type metricPayload struct {
	LabelsText  string  `json:"labels_text"`
	TimestampMS int64   `json:"time_ms"`
	Value       float64 `json:"value"`
}

// NewIngestor returns a new initialized ingestor.
func NewIngestor(opts Options) (*Ingestor, error) {
	c := &Ingestor{
		opts:     opts,
		metrics:  newMetrics(),
		messages: make(chan message, messageQueueSize),
	}

	writer, err := NewWriter(opts.RemoteWriteURL, c.metrics)
	if err != nil {
		return nil, err
	}

	c.writer = writer

	pahoOpts := paho.NewClientOptions()
	pahoOpts.SetCleanSession(false)
	pahoOpts.SetClientID(opts.MQTTClientID)
	pahoOpts.SetUsername(opts.MQTTUsername)
	pahoOpts.SetPassword(opts.MQTTPassword)
	pahoOpts.SetOnConnectHandler(c.onConnect)
	pahoOpts.SetConnectionLostHandler(c.onConnectionLost)
	// Messages are acknowledged manually, only after their points have been
	// written to the remote storage, so no point is lost if the ingestor is
	// stopped while the storage is unavailable.
	pahoOpts.SetAutoAckDisabled(true)

	for _, broker := range opts.MQTTBrokerURL {
		pahoOpts.AddBroker(broker)
	}

	if opts.MQTTSSLInsecure || opts.MQTTCAFile != "" {
		tlsConfig := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: opts.MQTTSSLInsecure, //nolint:gosec // G402: TLS InsecureSkipVerify set true.
		}

		if opts.MQTTCAFile != "" {
			rootCAs, err := loadRootCAs(opts.MQTTCAFile)
			if err != nil {
				return nil, fmt.Errorf("load CAs from %s: %w", opts.MQTTCAFile, err)
			}

			tlsConfig.RootCAs = rootCAs
		}

		pahoOpts.SetTLSConfig(tlsConfig)
	}

	c.client = paho.NewClient(pahoOpts)

	return c, nil
}

func loadRootCAs(caFile string) (*x509.CertPool, error) {
	rootCAs := x509.NewCertPool()

	certs, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	ok := rootCAs.AppendCertsFromPEM(certs)
	if !ok {
		return nil, errNotPem
	}

	return rootCAs, nil
}

// Run starts receiving metrics from MQTT and writing them to the remote
// storage, until the context is canceled.
func (c *Ingestor) Run(ctx context.Context) {
	c.ctx = ctx

	go c.runHTTPServer(ctx)

	writerDone := make(chan struct{})

	go func() {
		defer close(writerDone)

		c.runWriter(ctx)
	}()

	c.connect(ctx)

	<-ctx.Done()

	c.client.Disconnect(mqttDisconnectTimeoutMS)

	<-writerDone
}

func (c *Ingestor) connect(ctx context.Context) {
	err := c.connectOnce(ctx)

	for err != nil && ctx.Err() == nil {
		log.Warn().Err(err).Msgf("Failed to connect to MQTT, retry in %s", mqttRetryDelay)

		select {
		case <-time.After(mqttRetryDelay):
		case <-ctx.Done():
			return
		}

		err = c.connectOnce(ctx)
	}
}

func (c *Ingestor) connectOnce(ctx context.Context) error {
	// Use a timeout to return early if the context is canceled.
	const timeout = time.Second

	token := c.client.Connect()
	isTimeout := !token.WaitTimeout(timeout)

	for isTimeout && ctx.Err() == nil {
		isTimeout = !token.WaitTimeout(timeout)
	}

	if isTimeout {
		// The context was canceled before the connection completed.
		return fmt.Errorf("connect: %w", ctx.Err())
	}

	if token.Error() != nil {
		return fmt.Errorf("connect: %w", token.Error())
	}

	return nil
}

func (c *Ingestor) onConnect(_ paho.Client) {
	log.Info().Msg("MQTT connection established")

	c.connected.Store(true)
	c.metrics.mqttConnected.Set(1)

	topic := "v1/agent/+/data"
	if c.opts.MQTTID != "" {
		topic = fmt.Sprintf("%s/%s", topic, c.opts.MQTTID)
	}

	token := c.client.Subscribe(topic, 1, c.onMessage)
	token.Wait()

	// If there is an error, the client should reconnect so the subscription will be retried.
	if token.Error() != nil {
		log.Err(token.Error()).Msgf("Failed to subscribe")
	}
}

func (c *Ingestor) onConnectionLost(_ paho.Client, err error) {
	c.connected.Store(false)
	c.metrics.mqttConnected.Set(0)

	log.Warn().Err(err).Msg("MQTT connection lost")
}

// onMessage decodes an MQTT message and queues its points for writing. The
// message is acknowledged immediately if it can't be used, otherwise it will
// be acknowledged once its points have been written.
func (c *Ingestor) onMessage(_ paho.Client, m paho.Message) {
	c.metrics.messagesReceived.Inc()

	samples := c.decodeMessage(m)
	if len(samples) == 0 {
		m.Ack()

		return
	}

	select {
	case c.messages <- message{samples: samples, ack: m.Ack}:
	case <-c.ctx.Done():
	}
}

// decodeMessage converts an MQTT message to samples. It returns no samples if
// the message can't be used.
func (c *Ingestor) decodeMessage(m paho.Message) []sample {
	fqdn, err := fqdnFromTopic(m.Topic())
	if err != nil {
		log.Warn().Err(err).Msg("Skip message received on an unexpected topic")
		c.metrics.messagesDropped.WithLabelValues(dropReasonTopic).Inc()

		return nil
	}

	// Decode the zlib encoded JSON payload.
	var metrics []metricPayload

	if err := decode(m.Payload(), &metrics); err != nil {
		log.Warn().Err(err).Msg("Failed to decode payload")
		c.metrics.messagesDropped.WithLabelValues(dropReasonDecode).Inc()

		return nil
	}

	c.metrics.pointsReceived.Add(float64(len(metrics)))

	log.Debug().Str("instance", fqdn).Msgf("Received %d points", len(metrics))

	samples := make([]sample, 0, len(metrics))

	for _, metric := range metrics {
		lbls, err := parseLabelsText(metric.LabelsText)
		if err != nil || len(lbls) == 0 {
			// Skip the point: sending it without labels would make the
			// remote storage reject the whole write request.
			log.Warn().Err(err).Msgf("Skip point with invalid labels '%s'", metric.LabelsText)
			c.metrics.pointsDropped.WithLabelValues(dropReasonLabels).Inc()

			continue
		}

		// Replace the "instance" label of the metrics by the FQDN contained in the topic.
		// MQTT topic can use authentication, so we can trust the topic name.
		samples = append(samples, sample{
			labels:      setInstance(lbls, fqdn),
			value:       metric.Value,
			timestampMS: metric.TimestampMS,
		})
	}

	return samples
}

// runWriter writes the queued points to the remote storage in batches, until
// the context is canceled.
func (c *Ingestor) runWriter(ctx context.Context) {
	ticker := time.NewTicker(batchFlushInterval)
	defer ticker.Stop()

	pending := &batch{}

	for {
		select {
		case msg := <-c.messages:
			pending.add(msg)

			if len(pending.samples) >= batchMaxPoints {
				c.flush(ctx, pending)
			}
		case <-ticker.C:
			c.flush(ctx, pending)
		case <-ctx.Done():
			c.finalFlush(ctx, pending)

			return
		}
	}
}

// flush writes the batch to the remote storage, retrying as long as the error
// is recoverable. The messages of the batch are acknowledged on success, and
// also when the remote storage definitively rejected the points: retrying a
// rejected request would fail forever and block the ingestion.
func (c *Ingestor) flush(ctx context.Context, pending *batch) {
	if len(pending.samples) == 0 {
		return
	}

	err := c.writer.Write(ctx, pending.samples)

	for err != nil && isRecoverable(err) && ctx.Err() == nil {
		log.Warn().Err(err).Msgf("Failed to write points to the remote storage, retry in %s", storageRetryDelay)

		select {
		case <-time.After(storageRetryDelay):
		case <-ctx.Done():
			// Keep the batch, the final flush will make a last attempt.
			return
		}

		err = c.writer.Write(ctx, pending.samples)
		if err == nil {
			log.Info().Msg("Writing to remote storage recovered, resuming normal operation")
		}
	}

	switch {
	case err == nil:
		c.metrics.pointsWritten.Add(float64(len(pending.samples)))
		pending.done()
	case isRecoverable(err):
		// The context was canceled during a retry, keep the batch for the
		// final flush.
	default:
		log.Error().Err(err).Msgf("The remote storage rejected %d points, dropping them", len(pending.samples))
		c.metrics.pointsDropped.WithLabelValues(dropReasonRejected).Add(float64(len(pending.samples)))
		pending.done()
	}
}

// finalFlush makes a last write attempt during shutdown. Points that can't be
// written are not acknowledged: the broker will deliver them again on the
// next start.
func (c *Ingestor) finalFlush(ctx context.Context, pending *batch) {
	// Collect the messages that were already queued.
	for drained := false; !drained; {
		select {
		case msg := <-c.messages:
			pending.add(msg)
		default:
			drained = true
		}
	}

	if len(pending.samples) == 0 {
		return
	}

	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
	defer cancel()

	if err := c.writer.Write(flushCtx, pending.samples); err != nil {
		log.Warn().Err(err).Msgf(
			"Failed to write %d points before shutdown, they will be delivered again on the next start",
			len(pending.samples),
		)

		return
	}

	c.metrics.pointsWritten.Add(float64(len(pending.samples)))
	pending.done()
}

// Get the server FQDN from the MQTT topic.
// The topic is expected to be of the form "v1/agent/fqdn/data".
func fqdnFromTopic(topic string) (string, error) {
	matches := dataTopicRegex.FindStringSubmatch(topic)

	if len(matches) == 2 {
		// Glouton replaces '.' with ',' in the FQDN so it can be used
		// in a NATS topic, convert it back to a '.'.
		topic := strings.ReplaceAll(matches[1], ",", ".")

		return topic, nil
	}

	return "", fmt.Errorf("topic %s: %w", topic, errParseFQDN)
}

// Decode a zlib compressed JSON payload.
func decode(input []byte, obj any) error {
	decoder, err := zlib.NewReader(bytes.NewReader(input))
	if err != nil {
		return fmt.Errorf("zlib reader: %w", err)
	}

	err = json.NewDecoder(decoder).Decode(obj)
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}

	//nolint:gosec // G110: Potential DoS vulnerability via a decompression bomb.
	// False positive: copying to discard can't lead to memory exhaustion.
	_, err = io.Copy(io.Discard, decoder)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	if err := decoder.Close(); err != nil {
		return fmt.Errorf("close decoder: %w", err)
	}

	return nil
}
