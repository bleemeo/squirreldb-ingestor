// Minimal Prometheus remote write client.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang/snappy"
)

const (
	// The timeout of requests sent to the API.
	clientTimeout = 10 * time.Second
	// Maximum size of an error response body included in error messages.
	maxErrorBodySize = 512
)

var (
	errInvalidURL       = errors.New("invalid remote write URL")
	errRequestRejected  = errors.New("the remote storage rejected the request")
	errServersideError  = errors.New("the remote storage failed to process the request")
	errTooManyRequests  = errors.New("the remote storage throttled the request")
	errUnexpectedStatus = errors.New("unexpected status")
)

// recoverableError wraps an error that is worth retrying, like a network
// failure or a server-side error.
type recoverableError struct {
	err error
}

func (e recoverableError) Error() string {
	return e.err.Error()
}

func (e recoverableError) Unwrap() error {
	return e.err
}

// isRecoverable returns whether the write that returned this error can be
// retried.
func isRecoverable(err error) bool {
	var recoverable recoverableError

	return errors.As(err, &recoverable)
}

// Writer allows writing to a Prometheus remote write API.
// It is not safe for concurrent use.
type Writer struct {
	url      string
	client   *http.Client
	metrics  *metrics
	buf      []byte
	protoBuf []byte
}

// NewWriter returns a new initialized writer.
func NewWriter(rawURL string, metrics *metrics) (*Writer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse remote write URL '%s': %w", rawURL, err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: '%s' should begin with http:// or https://", errInvalidURL, rawURL)
	}

	w := &Writer{
		url:     u.String(),
		client:  &http.Client{Timeout: clientTimeout},
		metrics: metrics,
	}

	return w, nil
}

// Write samples to the configured Prometheus remote write endpoint.
// Errors that are worth retrying are wrapped in a recoverable error,
// see isRecoverable.
func (w *Writer) Write(ctx context.Context, samples []sample) error {
	w.protoBuf = marshalWriteRequest(w.protoBuf, samples)

	// snappy uses len() to see if it needs to allocate a new slice. Make the
	// buffer as long as possible.
	w.buf = w.buf[0:cap(w.buf)]
	w.buf = snappy.Encode(w.buf, w.protoBuf)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(w.buf))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.Header.Set("User-Agent", "squirreldb-ingestor/"+version)

	start := time.Now()

	resp, err := w.client.Do(req)

	w.metrics.writeDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		return recoverableError{fmt.Errorf("remote write: %w", err)}
	}

	defer resp.Body.Close()

	return responseError(resp)
}

// responseError converts an HTTP response to an error, wrapped in a
// recoverable error when the request can be retried.
func responseError(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	detail := fmt.Sprintf("status %s: %s", resp.Status, strings.TrimSpace(string(body)))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return recoverableError{fmt.Errorf("%w: %s", errTooManyRequests, detail)}
	case resp.StatusCode/100 == 5:
		return recoverableError{fmt.Errorf("%w: %s", errServersideError, detail)}
	case resp.StatusCode/100 == 4:
		return fmt.Errorf("%w: %s", errRequestRejected, detail)
	default:
		return fmt.Errorf("%w: %s", errUnexpectedStatus, detail)
	}
}
