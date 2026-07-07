package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/golang/snappy"
)

func testSamples() []sample {
	return []sample{
		{
			labels: []promLabel{
				{name: testMetricNameLabel, value: "up"},
				{name: testInstanceLabel, value: testFQDN},
			},
			value:       1,
			timestampMS: 1600000000000,
		},
	}
}

func TestNewWriterInvalidURL(t *testing.T) {
	t.Parallel()

	urls := []string{"://bad", "localhost:9201", "ftp://localhost"}

	for _, rawURL := range urls {
		if _, err := NewWriter(rawURL, newMetrics()); err == nil {
			t.Errorf("NewWriter(%q) succeeded, want an error", rawURL)
		}
	}
}

func TestWriterWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		status          int
		wantErr         bool
		wantRecoverable bool
	}{
		{name: "ok", status: http.StatusOK, wantErr: false},
		{name: "no-content", status: http.StatusNoContent, wantErr: false},
		{name: "bad-request", status: http.StatusBadRequest, wantErr: true, wantRecoverable: false},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true, wantRecoverable: false},
		{name: "too-many-requests", status: http.StatusTooManyRequests, wantErr: true, wantRecoverable: true},
		{name: "server-error", status: http.StatusInternalServerError, wantErr: true, wantRecoverable: true},
		{name: "bad-gateway", status: http.StatusBadGateway, wantErr: true, wantRecoverable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
			}))
			defer server.Close()

			writer, err := NewWriter(server.URL, newMetrics())
			if err != nil {
				t.Fatalf("NewWriter failed: %v", err)
			}

			err = writer.Write(t.Context(), testSamples())

			if (err != nil) != test.wantErr {
				t.Fatalf("Write() error = %v, want error: %v", err, test.wantErr)
			}

			if err != nil && isRecoverable(err) != test.wantRecoverable {
				t.Errorf("isRecoverable(%v) = %v, want %v", err, isRecoverable(err), test.wantRecoverable)
			}
		})
	}
}

func TestWriterWriteNetworkError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.Close()

	writer, err := NewWriter(server.URL, newMetrics())
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	err = writer.Write(t.Context(), testSamples())
	if err == nil {
		t.Fatal("Write() succeeded on a closed server, want an error")
	}

	if !isRecoverable(err) {
		t.Errorf("isRecoverable(%v) = false, want true for a network error", err)
	}
}

func TestWriterWriteRequest(t *testing.T) {
	t.Parallel()

	var (
		body    []byte
		headers http.Header
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error

		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("Failed to read the request body: %v", err)
		}

		headers = r.Header.Clone()

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	writer, err := NewWriter(server.URL, newMetrics())
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	samples := testSamples()

	if err := writer.Write(t.Context(), samples); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	wantHeaders := map[string]string{
		"Content-Type":                      "application/x-protobuf",
		"Content-Encoding":                  "snappy",
		"X-Prometheus-Remote-Write-Version": "0.1.0",
	}

	for name, want := range wantHeaders {
		if got := headers.Get(name); got != want {
			t.Errorf("Header %s = %q, want %q", name, got, want)
		}
	}

	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		t.Fatalf("Failed to snappy decode the request body: %v", err)
	}

	if want := marshalWriteRequest(nil, samples); !bytes.Equal(decoded, want) {
		t.Errorf("Request body = %x, want %x", decoded, want)
	}
}

// newTestIngestor returns an ingestor whose writer points to the given
// server, and a batch of one message counting its acknowledgments.
func newTestIngestor(t *testing.T, serverURL string) (*Ingestor, *batch, *atomic.Int64) {
	t.Helper()

	m := newMetrics()

	writer, err := NewWriter(serverURL, m)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	ingestor := &Ingestor{writer: writer, metrics: m}

	acks := &atomic.Int64{}
	pending := &batch{}
	pending.add(message{samples: testSamples(), ack: func() { acks.Add(1) }})

	return ingestor, pending, acks
}

func TestFlushSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ingestor, pending, acks := newTestIngestor(t, server.URL)

	ingestor.flush(t.Context(), pending)

	if acks.Load() != 1 {
		t.Errorf("Got %d acknowledgments, want 1", acks.Load())
	}

	if len(pending.samples) != 0 {
		t.Errorf("The batch still holds %d samples, want 0", len(pending.samples))
	}
}

// TestFlushDropsRejectedBatch ensures that a batch definitively rejected by
// the remote storage is dropped and acknowledged instead of being retried
// forever, which would block the ingestion of all the other metrics.
func TestFlushDropsRejectedBatch(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "out of order sample", http.StatusBadRequest)
	}))
	defer server.Close()

	ingestor, pending, acks := newTestIngestor(t, server.URL)

	ingestor.flush(t.Context(), pending)

	if requests.Load() != 1 {
		t.Errorf("The rejected write was sent %d times, want 1 (no retry)", requests.Load())
	}

	if acks.Load() != 1 {
		t.Errorf("Got %d acknowledgments, want 1", acks.Load())
	}

	if len(pending.samples) != 0 {
		t.Errorf("The batch still holds %d samples, want 0", len(pending.samples))
	}
}

// TestFlushKeepsBatchOnCanceledContext ensures that a batch that fails with a
// recoverable error during shutdown is kept unacknowledged, so the broker
// delivers it again on the next start.
func TestFlushKeepsBatchOnCanceledContext(t *testing.T) {
	t.Parallel()

	ingestor, pending, acks := newTestIngestor(t, "http://localhost:9")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ingestor.flush(ctx, pending)

	if acks.Load() != 0 {
		t.Errorf("Got %d acknowledgments, want 0", acks.Load())
	}

	if len(pending.samples) != 1 {
		t.Errorf("The batch holds %d samples, want 1", len(pending.samples))
	}
}
