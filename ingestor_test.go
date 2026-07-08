package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bleemeo/squirreldb-ingestor/internal/payload"
)

// Constants shared by the tests of the package.
const (
	testMetricNameLabel = "__name__"
	testInstanceLabel   = "instance"
	testItemLabel       = "item"
	testFQDN            = "server1.example.com"
)

// testMetrics returns metrics suitable for tests: each call uses its own
// registry, so parallel tests don't collide.
func testMetrics() *metrics {
	return newMetrics(new(atomic.Bool))
}

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
		metrics: testMetrics(),
		queue:   newMessageQueue(queueMaxPoints),
	}

	data, err := payload.Encode([]payload.Metric{
		{LabelsText: `__name__="up",instance="untrusted"`, TimestampMS: 1600000000000, Value: 1},
	})
	if err != nil {
		t.Fatalf("Failed to encode the payload: %v", err)
	}

	msg := &fakeMessage{topic: "v1/agent/server1,example,com/data", payload: data}

	ingestor.onMessage(nil, msg)

	if msg.acked.Load() {
		t.Error("The message was acknowledged before its points were written")
	}

	queued := ingestor.queue.popAll()
	if len(queued) != 1 {
		t.Fatalf("Got %d queued messages, want 1", len(queued))
	}

	want := []promLabel{
		{name: testMetricNameLabel, value: "up"},
		{name: testInstanceLabel, value: testFQDN},
	}

	if len(queued[0].samples) != 1 || !reflect.DeepEqual(queued[0].samples[0].labels, want) {
		t.Errorf("Queued samples = %v, want one sample with labels %v", queued[0].samples, want)
	}

	queued[0].ack()

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
				metrics: testMetrics(),
				queue:   newMessageQueue(queueMaxPoints),
			}

			msg := &fakeMessage{topic: test.topic, payload: test.payload}

			ingestor.onMessage(nil, msg)

			if !msg.acked.Load() {
				t.Error("An unusable message must be acknowledged immediately")
			}

			if queued := ingestor.queue.popAll(); len(queued) != 0 {
				t.Errorf("An unusable message was queued: %v", queued)
			}
		})
	}
}

// TestQueueRejectsWhenFull ensures the queue rejects messages beyond its
// point limit (but always accepts at least one), so a message left
// unacknowledged is redelivered instead of growing memory without bound.
func TestQueueRejectsWhenFull(t *testing.T) {
	t.Parallel()

	q := newMessageQueue(3)

	twoPoints := message{samples: make([]sample, 2)}
	if !q.push(twoPoints) {
		t.Fatal("The first message was rejected, want accepted")
	}

	// 2 + 2 > 3: rejected.
	if q.push(twoPoints) {
		t.Error("A message exceeding the limit was accepted, want rejected")
	}

	// A single oversized message is accepted on an empty queue.
	q.popAll()

	if !q.push(message{samples: make([]sample, 10)}) {
		t.Error("An oversized message was rejected on an empty queue, want accepted")
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

	m := testMetrics()

	writer, err := NewWriter(Options{RemoteWriteURL: server.URL}, m)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	ingestor := &Ingestor{
		writer:  writer,
		metrics: m,
		queue:   newMessageQueue(queueMaxPoints),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		ingestor.runWriter(ctx)
	}()

	var acks atomic.Int64

	ingestor.queue.push(message{samples: testSamples(), ack: func() { acks.Add(1) }})

	ingestor.queue.push(message{samples: testSamples(), ack: func() { acks.Add(1) }})

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
