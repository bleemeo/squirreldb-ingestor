package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

var errParseFQDN = errors.New("could not parse FQDN")

var dataTopicRegex = regexp.MustCompile("^v1/agent/(.*)/data$")

type Consumer struct {
	client paho.Client
	writer *writer
}

type metricPayload struct {
	LabelsText string `json:"labels_text"`
	// Timestamp in seconds.
	Timestamp int64   `json:"time"`
	Value     float64 `json:"value"`
}

func NewConsumer(opts Options) *Consumer {
	c := &Consumer{
		writer: NewWriter(opts.RemoteWriteURL),
	}

	pahoOpts := paho.NewClientOptions()
	pahoOpts.AddBroker(opts.MQTTBrokerURL)
	pahoOpts.SetUsername(opts.MQTTUsername)
	pahoOpts.SetPassword(opts.MQTTPassword)
	pahoOpts.SetOnConnectHandler(c.onConnect)
	pahoOpts.SetConnectionLostHandler(onConnectionLost)

	c.client = paho.NewClient(pahoOpts)

	return c
}

func (c *Consumer) Run(ctx context.Context) {
	c.client.Connect()

	select {
	case <-ctx.Done():
	}
}

func (c *Consumer) onConnect(_ paho.Client) {
	log.Println("MQTT connection established")

	token := c.client.Subscribe("v1/agent/+/data", 1, c.onMessage)

	token.Wait()

	if token.Error() != nil {
		log.Println("Failed to subscribe:", token.Error())

		return
	}
}

func onConnectionLost(_ paho.Client, err error) {
	log.Println("MQTT connection lost:", err)
}

func (c *Consumer) onMessage(_ paho.Client, m paho.Message) {
	fqdn, err := fqdnFromTopic(m.Topic())
	if err != nil {
		log.Printf("Skip data: %v", err)

		return
	}

	// Decode the zlib encoded JSON payload.
	var metrics []metricPayload

	err = decode(m.Payload(), &metrics)
	if err != nil {
		log.Println("Failed to decode:", err)
	}

	log.Printf("%v: received %d points on %s\n", time.Now().Format("15:04:05"), len(metrics), m.Topic())

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
			timestamp: metric.Timestamp * 1000,
		})
	}

	// Write the samples to the remote storage.
	err = c.writer.write(context.Background(), samples)
	if err != nil {
		log.Printf("Failed to write: %v", err)
	}
}

// Get the server FQDN from the MQTT topic.
// The topic is expected to be of the form "v1/agent/fqdn/data".
func fqdnFromTopic(topic string) (string, error) {
	matches := dataTopicRegex.FindStringSubmatch(topic)

	if len(matches) == 2 {
		return matches[1], nil
	}

	return "", fmt.Errorf("topic %s: %w", topic, errParseFQDN)
}

// textToLabels converts labels text to a list of label.
func textToLabels(text string) labels.Labels {
	lbls, err := parser.ParseMetricSelector("{" + text + "}")
	if err != nil {
		log.Printf("Failed to decode labels %#v: %v", text, err)

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
		return err
	}

	err = json.NewDecoder(decoder).Decode(obj)
	if err != nil {
		return err
	}

	_, err = io.Copy(io.Discard, decoder)
	if err != nil {
		return err
	}

	err = decoder.Close()

	return err
}
