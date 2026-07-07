package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
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
			LabelsText:  `__name__="cpu_used"`,
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
