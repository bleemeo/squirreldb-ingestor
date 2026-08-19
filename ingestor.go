package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/bleemeo/squirreldb-ingestor/internal/payload"
	"github.com/rs/zerolog/log"
)

const (
	// Delay between connection attempts to MQTT.
	mqttRetryDelay = 10 * time.Second
	// Bounds of the exponential backoff between write attempts to the
	// remote storage.
	storageRetryMinDelay = time.Second
	storageRetryMaxDelay = time.Minute
	// Upper bound applied to the delay requested by the remote storage
	// with a Retry-After header.
	retryAfterMaxDelay = 5 * time.Minute
	// Maximum number of points per write request to the remote storage.
	batchMaxPoints = 10000
	// Maximum delay before pending points are written to the remote storage.
	batchFlushInterval = time.Second
	// Timeout of the last write attempt during shutdown.
	finalFlushTimeout = 5 * time.Second
	// Time given to the MQTT client to disconnect cleanly, in ms.
	mqttDisconnectTimeoutMS = 1000
	// Maximum decompressed size of an MQTT payload, to reject decompression
	// bombs. Real messages are well under 1 MB.
	maxDecompressedSize = 10 << 20
	// Prefix of the MQTT topics the agents publish their metrics on.
	dataTopicPrefix = "v1/agent"
)

var (
	errParseFQDN = errors.New("could not parse FQDN")
	errNotPem    = errors.New("not a PEM file")
)

var dataTopicRegex = regexp.MustCompile("^" + dataTopicPrefix + "/(.*)/data")

// Ingestor reads metrics from MQTT and writes them to a remote storage.
type Ingestor struct {
	client    paho.Client
	writer    *Writer
	opts      Options
	metrics   *metrics
	connected atomic.Bool
	// queue holds the decoded MQTT messages waiting to be written.
	queue *messageQueue
}

// message holds the points decoded from one MQTT message and the callback
// that acknowledges the message to the broker.
type message struct {
	samples []sample
	ack     func()
}

// messageQueue is a queue of decoded messages between the MQTT handler and
// the writer. The MQTT handler must never block: a blocked handler stalls the
// paho routing goroutine, which also processes the keepalive responses, and
// the connection would flap during storage outages. So push never blocks; it
// always appends.
//
// Memory during a storage outage is bounded by the broker's in-flight window
// for the QoS-1 subscription (the receive-maximum, Mosquitto's
// max_inflight_messages, NATS' max_ack_pending): the broker stops delivering
// once that many messages are unacknowledged, and resumes on its own as the
// writer acknowledges them after writing. We do not enforce our own bound:
// dropping a message would lose its ack callback, so it would never be
// written and, on brokers that redeliver only on reconnection (e.g.
// Mosquitto), never delivered again either, while its in-flight slot stays
// held until the session is recreated.
type messageQueue struct {
	mu     sync.Mutex
	msgs   []message
	notify chan struct{}
}

func newMessageQueue() *messageQueue {
	return &messageQueue{notify: make(chan struct{}, 1)}
}

// push appends the message to the queue. It never blocks.
func (q *messageQueue) push(msg message) {
	q.mu.Lock()
	q.msgs = append(q.msgs, msg)
	q.mu.Unlock()

	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *messageQueue) popAll() []message {
	q.mu.Lock()
	defer q.mu.Unlock()

	msgs := q.msgs
	q.msgs = nil

	return msgs
}

// batch accumulates messages until their points are written to the remote
// storage in a single request.
type batch struct {
	messages []message
	points   int
}

func (b *batch) add(msg message) {
	b.messages = append(b.messages, msg)
	b.points += len(msg.samples)
}

func (b *batch) reset() {
	// Clear the whole backing array, not just the live slice: the retained
	// message structs would otherwise keep their samples and ack closures
	// (each referencing a paho message) alive until the array is overwritten.
	clear(b.messages[:cap(b.messages)])
	b.messages = b.messages[:0]
	b.points = 0
}

// NewIngestor returns a new initialized ingestor.
func NewIngestor(opts Options) (*Ingestor, error) {
	c := &Ingestor{
		opts:  opts,
		queue: newMessageQueue(),
	}

	c.metrics = newMetrics(&c.connected)

	writer, err := NewWriter(opts, c.metrics)
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

// topic returns the MQTT topic filter the ingestor listens on.
func (c *Ingestor) topic() string {
	topic := dataTopicPrefix + "/+/data"
	if c.opts.MQTTID != "" {
		topic += "/" + c.opts.MQTTID
	}

	return topic
}

// Run starts receiving metrics from MQTT and writing them to the remote
// storage, until the context is canceled.
func (c *Ingestor) Run(ctx context.Context) {
	// Register the message handler before connecting: with a persistent
	// session, the broker can redeliver unacknowledged messages right
	// after the connection is established, before the subscription call
	// of onConnect registers the route.
	c.client.AddRoute(c.topic(), c.onMessage)

	go c.runHTTPServer(ctx)

	writerDone := make(chan struct{})

	go func() {
		defer close(writerDone)

		c.runWriter(ctx)
	}()

	c.connect(ctx)

	<-ctx.Done()

	// Wait for the writer's final flush before disconnecting: paho
	// silently drops acknowledgments sent after the disconnection, and
	// the broker would deliver the already-written messages again.
	<-writerDone

	c.client.Disconnect(mqttDisconnectTimeoutMS)
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

	token := c.client.Subscribe(c.topic(), 1, c.onMessage)
	token.Wait()

	// If there is an error, the client should reconnect so the subscription will be retried.
	if token.Error() != nil {
		log.Err(token.Error()).Msgf("Failed to subscribe")
	}
}

func (c *Ingestor) onConnectionLost(_ paho.Client, err error) {
	c.connected.Store(false)

	log.Warn().Err(err).Msg("MQTT connection lost")
}

// onMessage decodes an MQTT message and queues its points for writing. The
// message is acknowledged immediately if it can't be used, otherwise it will
// be acknowledged once its points have been written. It never blocks: it
// runs on the paho routing goroutine.
func (c *Ingestor) onMessage(_ paho.Client, m paho.Message) {
	c.metrics.messagesReceived.Inc()

	samples := c.decodeMessage(m)
	if len(samples) == 0 {
		m.Ack()

		return
	}

	c.queue.push(message{samples: samples, ack: m.Ack})
}

// decodeMessage converts an MQTT message to samples. It returns no samples if
// the message can't be used.
func (c *Ingestor) decodeMessage(m paho.Message) []sample {
	fqdn, err := fqdnFromTopic(m.Topic())
	if err != nil {
		log.Warn().Err(err).Msg("Skip message received on an unexpected topic")
		c.metrics.messagesDroppedTopic.Inc()

		return nil
	}

	metrics, err := payload.Decode(m.Payload(), maxDecompressedSize)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to decode payload")
		c.metrics.messagesDroppedDecode.Inc()

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
			c.metrics.pointsDroppedLabels.Inc()

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
		case <-c.queue.notify:
			for _, msg := range c.queue.popAll() {
				pending.add(msg)

				if pending.points >= batchMaxPoints {
					c.flush(ctx, pending)
					ticker.Reset(batchFlushInterval)
				}
			}
		case <-ticker.C:
			c.flush(ctx, pending)
		case <-ctx.Done():
			c.finalFlush(ctx, pending)

			return
		}
	}
}

// flush writes the batch to the remote storage, retrying as long as the
// error is recoverable. When the storage definitively rejects the content,
// the batch is bisected to isolate the offending messages: only their points
// are dropped (and their messages acknowledged, so they are not delivered
// again), the others are written normally. If the context is canceled during
// a retry, the messages not yet written are kept for the final flush.
func (c *Ingestor) flush(ctx context.Context, pending *batch) {
	if pending.points == 0 {
		return
	}

	// Segments of messages to write, in order, so the points of a series
	// stay ordered even while bisecting.
	segments := [][]message{pending.messages}

	for len(segments) > 0 {
		segment := segments[0]

		err := c.writeWithRetry(ctx, segment)

		switch {
		case err == nil:
			c.completeSegment(segment)

			segments = segments[1:]
		case isRecoverable(err):
			// The context was canceled during a retry: keep the messages
			// not written yet for the final flush. Collect them first, as the
			// segments still alias pending's backing array that reset clears.
			remaining := make([]message, 0, len(segments))
			for _, segment := range segments {
				remaining = append(remaining, segment...)
			}

			pending.reset()

			for _, msg := range remaining {
				pending.add(msg)
			}

			return
		case len(segment) == 1:
			c.dropRejected(segment[0], err)

			segments = segments[1:]
		default:
			// The storage rejected the segment: bisect it to isolate the
			// offending messages.
			half := len(segment) / 2
			segments = append([][]message{segment[:half], segment[half:]}, segments[1:]...)
		}
	}

	pending.reset()
}

// completeSegment acknowledges the messages of a successfully written segment.
func (c *Ingestor) completeSegment(segment []message) {
	points := 0

	for _, msg := range segment {
		points += len(msg.samples)
		msg.ack()
	}

	c.metrics.pointsWritten.Add(float64(points))
}

// dropRejected drops a message definitively rejected by the remote storage.
// The message is acknowledged: retrying it would fail forever, and leaving
// it unacknowledged would make the broker deliver it again.
func (c *Ingestor) dropRejected(msg message, err error) {
	instance := ""
	if len(msg.samples) > 0 {
		instance = instanceLabel(msg.samples[0].labels)
	}

	log.Error().Err(err).Msgf(
		"The remote storage rejected %d points from instance '%s', dropping them", len(msg.samples), instance,
	)
	c.metrics.pointsDroppedRejected.Add(float64(len(msg.samples)))

	msg.ack()
}

// writeWithRetry writes the samples of the messages, retrying as long as the
// error is recoverable and the context is not canceled.
func (c *Ingestor) writeWithRetry(ctx context.Context, msgs []message) error {
	points := 0
	for _, msg := range msgs {
		points += len(msg.samples)
	}

	samples := make([]sample, 0, points)
	for _, msg := range msgs {
		samples = append(samples, msg.samples...)
	}

	err := c.writer.Write(ctx, samples)

	for attempt := 0; err != nil && isRecoverable(err) && ctx.Err() == nil; attempt++ {
		delay := retryDelay(err, attempt)

		log.Warn().Err(err).Msgf("Failed to write points to the remote storage, retry in %s", delay)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return err
		}

		err = c.writer.Write(ctx, samples)
		if err == nil {
			log.Info().Msg("Writing to remote storage recovered, resuming normal operation")
		}
	}

	return err
}

// retryDelay returns the delay before the given write retry attempt. It
// honors the delay requested by the server when there is one, and otherwise
// grows exponentially with jitter to avoid synchronized retries when several
// ingestors are running.
func retryDelay(err error, attempt int) time.Duration {
	if recoverable, ok := asRecoverable(err); ok && recoverable.retryAfter > 0 {
		return min(recoverable.retryAfter, retryAfterMaxDelay).Round(time.Millisecond)
	}

	const maxShift = 10 // Avoid a delay overflow with high attempt counts.

	delay := min(storageRetryMinDelay<<min(attempt, maxShift), storageRetryMaxDelay)

	// Full jitter on the upper half of the delay.
	//nolint:gosec // G404: the jitter doesn't need cryptographic randomness.
	return (delay/2 + rand.N(delay/2)).Round(time.Millisecond)
}

// finalFlush makes a last write attempt during shutdown. Points that can't be
// written are not acknowledged: the broker will deliver them again on the
// next start.
func (c *Ingestor) finalFlush(ctx context.Context, pending *batch) {
	// Collect the messages that were already queued.
	for _, msg := range c.queue.popAll() {
		pending.add(msg)
	}

	if pending.points == 0 {
		return
	}

	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
	defer cancel()

	samples := make([]sample, 0, pending.points)
	for _, msg := range pending.messages {
		samples = append(samples, msg.samples...)
	}

	if err := c.writer.Write(flushCtx, samples); err != nil {
		log.Warn().Err(err).Msgf(
			"Failed to write %d points before shutdown, they will be delivered again on the next start",
			pending.points,
		)

		return
	}

	c.completeSegment(pending.messages)
	pending.reset()
}

// instanceLabel returns the value of the "instance" label, if any.
func instanceLabel(lbls []promLabel) string {
	for _, label := range lbls {
		if label.name == "instance" {
			return label.value
		}
	}

	return ""
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
