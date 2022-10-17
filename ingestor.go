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
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/rs/zerolog/log"
)

const (
	// Delay between connection attempts to MQTT.
	mqttRetryDelay = 10 * time.Second
	// Delay between write attempts to the remote storage.
	storageRetryDelay = 10 * time.Second
)

var (
	errParseFQDN = errors.New("could not parse FQDN")
	errNotPem    = errors.New("not a PEM file")
)

var dataTopicRegex = regexp.MustCompile("^v1/agent/(.*)/data$")

// Ingestor reads metrics from MQTT and write them to a remote storage.
type Ingestor struct {
	client paho.Client
	writer *Writer
	// It's bad practise to store a context in a struct,
	// but we need to use it in paho callbacks.
	ctx context.Context //nolint:containedctx
}

type metricPayload struct {
	LabelsText  string  `json:"labels_text"`
	TimestampMS int64   `json:"time_ms"`
	Value       float64 `json:"value"`
}

// NewIngestor returns a new initialized ingestor.
func NewIngestor(opts Options) *Ingestor {
	c := &Ingestor{
		writer: NewWriter(opts.RemoteWriteURL),
	}

	pahoOpts := paho.NewClientOptions()
	pahoOpts.SetCleanSession(false)
	pahoOpts.SetClientID(opts.MQTTUsername)
	pahoOpts.SetUsername(opts.MQTTUsername)
	pahoOpts.SetPassword(opts.MQTTPassword)
	pahoOpts.SetOnConnectHandler(c.onConnect)
	pahoOpts.SetConnectionLostHandler(onConnectionLost)

	for _, broker := range opts.MQTTBrokerURL {
		pahoOpts.AddBroker(broker)
	}

	if opts.MQTTSSLInsecure || opts.MQTTCAFile != "" {
		tlsConfig := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: opts.MQTTSSLInsecure, //nolint:gosec // G402: TLS InsecureSkipVerify set true.
		}

		if opts.MQTTCAFile != "" {
			if rootCAs, err := loadRootCAs(opts.MQTTCAFile); err != nil {
				log.Err(err).Msgf("Unable to load CAs from %s", opts.MQTTCAFile)
			} else {
				tlsConfig.RootCAs = rootCAs
			}
		}

		pahoOpts.SetTLSConfig(tlsConfig)
	}

	c.client = paho.NewClient(pahoOpts)

	return c
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

// Run starts receiving metrics from MQTT and writing them to the remote storage.
func (c *Ingestor) Run(ctx context.Context) {
	c.ctx = ctx

	c.connect(ctx)

	<-ctx.Done()
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

	if token.Error() != nil {
		return fmt.Errorf("connect: %w", token.Error())
	}

	return nil
}

func (c *Ingestor) onConnect(_ paho.Client) {
	log.Info().Msg("MQTT connection established")

	token := c.client.Subscribe("v1/agent/+/data", 1, c.onMessage)
	token.Wait()

	// If there is an error, the client should reconnect so the subscription will be retried.
	if token.Error() != nil {
		log.Err(token.Error()).Msgf("Failed to subscribe")
	}
}

func onConnectionLost(_ paho.Client, err error) {
	log.Warn().Err(err).Msg("MQTT connection lost")
}

func (c *Ingestor) onMessage(_ paho.Client, m paho.Message) {
	fqdn, err := fqdnFromTopic(m.Topic())
	if err != nil {
		log.Warn().Err(err).Msg("Skip data: %v")

		return
	}

	// Decode the zlib encoded JSON payload.
	var metrics []metricPayload

	err = decode(m.Payload(), &metrics)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to decode payload")

		return
	}

	log.Debug().Str("instance", fqdn).Msgf("Received %d points", len(metrics))

	// Convert the metrics to samples.
	samples := make([]sample, 0, len(metrics))

	for _, metric := range metrics {
		lbls := textToLabels(metric.LabelsText)

		// Replace the "instance" label of the metrics by the FQDN contained in the topic.
		// MQTT topic can use authentication, so we can trust the topic name.
		builder := labels.NewBuilder(lbls)
		builder.Set("instance", fqdn)

		samples = append(samples, sample{
			labels:    builder.Labels(),
			value:     metric.Value,
			timestamp: metric.TimestampMS,
		})
	}

	// Write the samples to the remote storage.
	err = c.writer.Write(c.ctx, samples)
	for err != nil && c.ctx.Err() == nil {
		log.Warn().Err(err).Msgf("Failed to write points to the remote storage, retry in %s", storageRetryDelay)

		select {
		case <-time.After(storageRetryDelay):
		case <-c.ctx.Done():
		}

		err = c.writer.Write(context.Background(), samples)
		if err == nil {
			log.Info().Msg("Writing to remote storage recovered, resuming normal operation")
		}
	}
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

// textToLabels converts labels text to a list of label.
func textToLabels(text string) labels.Labels {
	lbls, err := parser.ParseMetricSelector("{" + text + "}")
	if err != nil {
		log.Warn().Err(err).Msgf("Failed to decode labels '%s'", text)

		return nil
	}

	results := make(labels.Labels, 0, len(lbls))

	for _, v := range lbls {
		results = append(results, labels.Label{Name: v.Name, Value: v.Value})
	}

	return results
}

// Decode a zlib compressed JSON payload.
func decode(input []byte, obj interface{}) error {
	decoder, err := zlib.NewReader(bytes.NewReader(input))
	if err != nil {
		return fmt.Errorf("zlib reader: %w", err)
	}

	err = json.NewDecoder(decoder).Decode(obj)
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}

	//nolint:gosec // G110: Potential DoS vulnerability via decompression bomb.
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
