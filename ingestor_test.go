package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Constants shared by the tests of the package.
const (
	testMetricNameLabel = "__name__"
	testInstanceLabel   = "instance"
	testItemLabel       = "item"
	testFQDN            = "server1.example.com"
)

func TestFQDNFromTopic(t *testing.T) {
	t.Parallel()

	fqdns := []string{
		"myfqdn",
		"aaaa.bbbb",
		"aaaa.bbbb.cc",
	}

	for _, fqdn := range fqdns {
		topic := fmt.Sprintf("v1/agent/%s/data", fqdn)

		got, err := fqdnFromTopic(topic)
		if err != nil {
			t.Fatalf("Failed to get FQDN for topic %s: %v", topic, err)
		}

		if got != fqdn {
			t.Fatalf("Wanted '%s', got '%s'", fqdn, got)
		}
	}
}

// TestFQDNFromTopicWithMQTTID ensures the FQDN is found in the topics used
// when messages are distributed to multiple ingestors with NATS subject
// mapping (see the --mqtt-id option).
func TestFQDNFromTopicWithMQTTID(t *testing.T) {
	t.Parallel()

	got, err := fqdnFromTopic("v1/agent/server1,example,com/data/1")
	if err != nil {
		t.Fatalf("Failed to get FQDN: %v", err)
	}

	if want := testFQDN; got != want {
		t.Fatalf("Wanted '%s', got '%s'", want, got)
	}
}

func TestFQDNFromTopicInvalid(t *testing.T) {
	t.Parallel()

	topics := []string{
		"",
		"v1/agent/data",
		"v2/agent/myfqdn/data",
		"some/other/topic",
	}

	for _, topic := range topics {
		if got, err := fqdnFromTopic(topic); err == nil {
			t.Errorf("fqdnFromTopic(%q) = %q, want an error", topic, got)
		}
	}
}

func compress(t *testing.T, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	writer := zlib.NewWriter(&buf)

	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("Failed to compress the payload: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close the zlib writer: %v", err)
	}

	return buf.Bytes()
}

func TestDecode(t *testing.T) {
	t.Parallel()

	want := []metricPayload{
		{
			LabelsText:  `__name__="cpu_used_decode"`,
			TimestampMS: 1600000000000,
			Value:       42.5,
		},
	}

	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Failed to marshal the payload: %v", err)
	}

	var got []metricPayload

	if err := decode(compress(t, payload), &got); err != nil {
		t.Fatalf("decode() failed: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("decode() = %v, want %v", got, want)
	}
}

func TestDecodeInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "not-zlib", payload: []byte("not zlib")},
		{name: "empty-payload", payload: nil},
		{name: "not-json", payload: nil}, // The payload is set below.
	}

	tests[2].payload = compress(t, []byte("not json"))

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var got []metricPayload

			if err := decode(test.payload, &got); err == nil {
				t.Errorf("decode(%q) succeeded, want an error", test.payload)
			}
		})
	}
}

// TestDecodeBomb ensures a payload expanding over the decompressed size limit
// is rejected instead of exhausting the memory.
func TestDecodeBomb(t *testing.T) {
	t.Parallel()

	// A highly compressible 60 MB payload: a few kilobytes compressed.
	bomb := compress(t, []byte(`[{"labels_text":"`+strings.Repeat("a", 60<<20)+`"}]`))

	if len(bomb) > 1<<20 {
		t.Fatalf("The compressed bomb is %d bytes, expected it to be small", len(bomb))
	}

	var got []metricPayload

	if err := decode(bomb, &got); err == nil {
		t.Error("decode() accepted a decompression bomb, want an error")
	}
}

// fakeMessage implements paho.Message for the tests.
type fakeMessage struct {
	topic   string
	payload []byte
	acked   atomic.Bool
}

func (m *fakeMessage) Duplicate() bool   { return false }
func (m *fakeMessage) Qos() byte         { return 1 }
func (m *fakeMessage) Retained() bool    { return false }
func (m *fakeMessage) Topic() string     { return m.topic }
func (m *fakeMessage) MessageID() uint16 { return 1 }
func (m *fakeMessage) Payload() []byte   { return m.payload }
func (m *fakeMessage) Ack()              { m.acked.Store(true) }

func TestOnMessage(t *testing.T) {
	t.Parallel()

	ingestor := &Ingestor{
		metrics:  newMetrics(),
		messages: make(chan message, 1),
		ctx:      t.Context(),
	}

	payload, err := json.Marshal([]metricPayload{
		{LabelsText: `__name__="up",instance="untrusted"`, TimestampMS: 1600000000000, Value: 1},
	})
	if err != nil {
		t.Fatalf("Failed to marshal the payload: %v", err)
	}

	msg := &fakeMessage{topic: "v1/agent/server1,example,com/data", payload: compress(t, payload)}

	ingestor.onMessage(nil, msg)

	if msg.acked.Load() {
		t.Error("The message was acknowledged before its points were written")
	}

	var queued message

	select {
	case queued = <-ingestor.messages:
	default:
		t.Fatal("No message was queued")
	}

	want := []promLabel{
		{name: testMetricNameLabel, value: "up"},
		{name: testInstanceLabel, value: testFQDN},
	}

	if len(queued.samples) != 1 || !reflect.DeepEqual(queued.samples[0].labels, want) {
		t.Errorf("Queued samples = %v, want one sample with labels %v", queued.samples, want)
	}

	queued.ack()

	if !msg.acked.Load() {
		t.Error("The ack callback didn't acknowledge the MQTT message")
	}
}

func TestOnMessageInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		topic   string
		payload []byte
	}{
		{name: "bad-topic", topic: "some/other/topic", payload: nil},
		{name: "bad-payload", topic: "v1/agent/myfqdn/data", payload: []byte("not zlib")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ingestor := &Ingestor{
				metrics:  newMetrics(),
				messages: make(chan message, 1),
				ctx:      t.Context(),
			}

			msg := &fakeMessage{topic: test.topic, payload: test.payload}

			ingestor.onMessage(nil, msg)

			if !msg.acked.Load() {
				t.Error("An unusable message must be acknowledged immediately")
			}

			select {
			case queued := <-ingestor.messages:
				t.Errorf("An unusable message was queued: %v", queued)
			default:
			}
		})
	}
}

// TestRunWriterBatchesMessages ensures that points received in separate MQTT
// messages are written in a single remote write request.
func TestRunWriterBatchesMessages(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	m := newMetrics()

	writer, err := NewWriter(Options{RemoteWriteURL: server.URL}, m)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	ingestor := &Ingestor{
		writer:   writer,
		metrics:  m,
		messages: make(chan message, 8),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		ingestor.runWriter(ctx)
	}()

	var acks atomic.Int64

	ingestor.messages <- message{samples: testSamples(), ack: func() { acks.Add(1) }}

	ingestor.messages <- message{samples: testSamples(), ack: func() { acks.Add(1) }}

	// Wait for the periodic flush.
	deadline := time.Now().Add(5 * time.Second)
	for acks.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if acks.Load() != 2 {
		t.Fatalf("Got %d acknowledgments, want 2", acks.Load())
	}

	if requests.Load() != 1 {
		t.Errorf("The messages were written in %d requests, want 1 (batched)", requests.Load())
	}

	cancel()
	<-done
}
