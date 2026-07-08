package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		if _, err := NewWriter(Options{RemoteWriteURL: rawURL}, testMetrics()); err == nil {
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
		// 400 and 413 mean the storage rejected this content: not retried.
		{name: "bad-request", status: http.StatusBadRequest, wantErr: true, wantRecoverable: false},
		{name: "payload-too-large", status: http.StatusRequestEntityTooLarge, wantErr: true, wantRecoverable: false},
		// Configuration errors are retried: dropping data would not fix them.
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true, wantRecoverable: true},
		{name: "forbidden", status: http.StatusForbidden, wantErr: true, wantRecoverable: true},
		{name: "not-found", status: http.StatusNotFound, wantErr: true, wantRecoverable: true},
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

			writer, err := NewWriter(Options{RemoteWriteURL: server.URL}, testMetrics())
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

	writer, err := NewWriter(Options{RemoteWriteURL: server.URL}, testMetrics())
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

	writer, err := NewWriter(Options{RemoteWriteURL: server.URL}, testMetrics())
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

	want, _ := marshalWriteRequest(nil, nil, samples)
	if !bytes.Equal(decoded, want) {
		t.Errorf("Request body = %x, want %x", decoded, want)
	}
}

func TestNewWriterInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
	}{
		{
			name: "password-without-username",
			opts: Options{RemoteWritePassword: "secret"},
		},
		{
			name: "basic-and-bearer",
			opts: Options{RemoteWriteUsername: "user", RemoteWriteBearerToken: "token"},
		},
		{
			name: "authorization-header-and-bearer",
			opts: Options{
				RemoteWriteBearerToken: "token",
				RemoteWriteHeader:      []string{"Authorization=Basic dXNlcjo="},
			},
		},
		{
			name: "header-without-value",
			opts: Options{RemoteWriteHeader: []string{"X-Scope-OrgID"}},
		},
		{
			name: "header-without-name",
			opts: Options{RemoteWriteHeader: []string{"=value"}},
		},
		{
			name: "reserved-header",
			opts: Options{RemoteWriteHeader: []string{"content-encoding=gzip"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			opts := test.opts
			opts.RemoteWriteURL = "http://localhost:9201/api/v1/write"

			if _, err := NewWriter(opts, testMetrics()); err == nil {
				t.Errorf("NewWriter(%+v) succeeded, want an error", opts)
			}
		})
	}
}

func TestWriterAuthentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		opts        Options
		wantHeaders map[string]string
	}{
		{
			name: "basic-auth",
			opts: Options{RemoteWriteUsername: "user", RemoteWritePassword: "pass"},
			wantHeaders: map[string]string{
				// base64("user:pass")
				"Authorization": "Basic dXNlcjpwYXNz",
			},
		},
		{
			name: "bearer-token",
			opts: Options{RemoteWriteBearerToken: "some-token"},
			wantHeaders: map[string]string{
				"Authorization": "Bearer some-token",
			},
		},
		{
			name: "custom-headers",
			opts: Options{RemoteWriteHeader: []string{"X-Scope-OrgID=tenant1", "X-Custom=a=b"}},
			wantHeaders: map[string]string{
				"X-Scope-Orgid": "tenant1",
				// The value is cut on the first '='.
				"X-Custom": "a=b",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var headers http.Header

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers = r.Header.Clone()

				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			opts := test.opts
			opts.RemoteWriteURL = server.URL

			writer, err := NewWriter(opts, testMetrics())
			if err != nil {
				t.Fatalf("NewWriter failed: %v", err)
			}

			if err := writer.Write(t.Context(), testSamples()); err != nil {
				t.Fatalf("Write() failed: %v", err)
			}

			for name, want := range test.wantHeaders {
				if got := headers.Get(name); got != want {
					t.Errorf("Header %s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// TestWriterURLUserinfo ensures the credentials embedded in the remote write
// URL are used for basic authentication.
func TestWriterURLUserinfo(t *testing.T) {
	t.Parallel()

	var authorization string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	rawURL := strings.Replace(server.URL, "http://", "http://user:pass@", 1)

	writer, err := NewWriter(Options{RemoteWriteURL: rawURL}, testMetrics())
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	if err := writer.Write(t.Context(), testSamples()); err != nil {
		t.Fatalf("Write() failed: %v", err)
	}

	// base64("user:pass")
	if want := "Basic dXNlcjpwYXNz"; authorization != want {
		t.Errorf("Authorization = %q, want %q", authorization, want)
	}
}

func TestWriterRetryAfter(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	writer, err := NewWriter(Options{RemoteWriteURL: server.URL}, testMetrics())
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	err = writer.Write(t.Context(), testSamples())
	if err == nil {
		t.Fatal("Write() succeeded, want an error")
	}

	recoverable, ok := asRecoverable(err)
	if !ok {
		t.Fatalf("asRecoverable(%v) = false, want true", err)
	}

	if recoverable.retryAfter != 7*time.Second {
		t.Errorf("retryAfter = %s, want 7s", recoverable.retryAfter)
	}

	if got := retryDelay(err, 0); got != 7*time.Second {
		t.Errorf("retryDelay(%v, 0) = %s, want 7s", err, got)
	}
}

func TestRetryDelay(t *testing.T) {
	t.Parallel()

	// Without a server-provided delay, the backoff grows exponentially
	// from storageRetryMinDelay to storageRetryMaxDelay with jitter.
	err := recoverableError{err: errWriteFailed}

	for attempt := range 20 {
		delay := retryDelay(err, attempt)

		if delay < storageRetryMinDelay/2 || delay > storageRetryMaxDelay {
			t.Errorf("retryDelay(err, %d) = %s, want between %s and %s",
				attempt, delay, storageRetryMinDelay/2, storageRetryMaxDelay)
		}
	}

	// The delay requested by the server is honored, but capped.
	err = recoverableError{err: errWriteFailed, retryAfter: time.Hour}

	if got := retryDelay(err, 0); got != retryAfterMaxDelay {
		t.Errorf("retryDelay(err with huge Retry-After, 0) = %s, want %s", got, retryAfterMaxDelay)
	}
}

// newTestBatch returns a batch of one message holding testSamples, and the
// counter of its acknowledgments.
func newTestBatch() (*batch, *atomic.Int64) {
	acks := &atomic.Int64{}
	pending := &batch{}
	pending.add(message{samples: testSamples(), ack: func() { acks.Add(1) }})

	return pending, acks
}

func newTestIngestor(t *testing.T, serverURL string) *Ingestor {
	t.Helper()

	m := testMetrics()

	writer, err := NewWriter(Options{RemoteWriteURL: serverURL}, m)
	if err != nil {
		t.Fatalf("NewWriter failed: %v", err)
	}

	return &Ingestor{writer: writer, metrics: m, queue: newMessageQueue(queueMaxPoints)}
}

func TestFlushSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ingestor := newTestIngestor(t, server.URL)
	pending, acks := newTestBatch()

	ingestor.flush(t.Context(), pending)

	if acks.Load() != 1 {
		t.Errorf("Got %d acknowledgments, want 1", acks.Load())
	}

	if pending.points != 0 {
		t.Errorf("The batch still holds %d points, want 0", pending.points)
	}
}

// TestFlushDropsRejectedMessage ensures that a single message definitively
// rejected by the remote storage is dropped and acknowledged instead of
// being retried forever, which would block the ingestion.
func TestFlushDropsRejectedMessage(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "out of order sample", http.StatusBadRequest)
	}))
	defer server.Close()

	ingestor := newTestIngestor(t, server.URL)
	pending, acks := newTestBatch()

	ingestor.flush(t.Context(), pending)

	if requests.Load() != 1 {
		t.Errorf("The rejected write was sent %d times, want 1 (no retry)", requests.Load())
	}

	if acks.Load() != 1 {
		t.Errorf("Got %d acknowledgments, want 1", acks.Load())
	}

	if pending.points != 0 {
		t.Errorf("The batch still holds %d points, want 0", pending.points)
	}
}

// TestFlushIsolatesRejectedMessage ensures that when the storage rejects a
// batch, the batch is bisected so only the offending message is dropped: the
// points of the other agents are still written.
func TestFlushIsolatesRejectedMessage(t *testing.T) {
	t.Parallel()

	var accepted atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("Failed to read the request body: %v", err)
		}

		decoded, err := snappy.Decode(nil, body)
		if err != nil {
			t.Errorf("Failed to snappy decode the request body: %v", err)
		}

		if bytes.Contains(decoded, []byte("poison-instance")) {
			http.Error(w, "invalid sample", http.StatusBadRequest)

			return
		}

		accepted.Add(1)

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ingestor := newTestIngestor(t, server.URL)

	var goodAcks, poisonAcks atomic.Int64

	poisonSamples := []sample{{
		labels: []promLabel{
			{name: testMetricNameLabel, value: "up"},
			{name: testInstanceLabel, value: "poison-instance"},
		},
		value:       1,
		timestampMS: 1600000000000,
	}}

	pending := &batch{}
	pending.add(message{samples: testSamples(), ack: func() { goodAcks.Add(1) }})
	pending.add(message{samples: poisonSamples, ack: func() { poisonAcks.Add(1) }})

	ingestor.flush(t.Context(), pending)

	if goodAcks.Load() != 1 || poisonAcks.Load() != 1 {
		t.Errorf("Got %d/%d acknowledgments for the good/poison messages, want 1/1",
			goodAcks.Load(), poisonAcks.Load())
	}

	if accepted.Load() != 1 {
		t.Errorf("The storage accepted %d requests, want exactly 1 (the good message alone)", accepted.Load())
	}

	if pending.points != 0 {
		t.Errorf("The batch still holds %d points, want 0", pending.points)
	}
}

// TestFlushKeepsBatchOnCanceledContext ensures that a batch that fails with a
// recoverable error during shutdown is kept unacknowledged, so the broker
// delivers it again on the next start.
func TestFlushKeepsBatchOnCanceledContext(t *testing.T) {
	t.Parallel()

	ingestor := newTestIngestor(t, "http://localhost:9")
	pending, acks := newTestBatch()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ingestor.flush(ctx, pending)

	if acks.Load() != 0 {
		t.Errorf("Got %d acknowledgments, want 0", acks.Load())
	}

	if len(pending.messages) != 1 || pending.points != 1 {
		t.Errorf("The batch holds %d messages and %d points, want 1 and 1", len(pending.messages), pending.points)
	}
}
