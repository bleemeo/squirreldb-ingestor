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
	"strconv"
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
	errInvalidURL    = errors.New("invalid remote write URL")
	errInvalidAuth   = errors.New("invalid remote write authentication options")
	errInvalidHeader = errors.New("invalid remote write header")
	// errRequestRejected reports that the remote storage definitively
	// rejected the content of the request: retrying it unchanged would
	// fail forever.
	errRequestRejected = errors.New("the remote storage rejected the request")
	errWriteFailed     = errors.New("remote write failed")
)

// recoverableError wraps an error that is worth retrying: a network failure,
// a server-side error, throttling, or a configuration error (bad credentials,
// wrong URL) that dropping data would not fix.
type recoverableError struct {
	err error
	// retryAfter is the delay requested by the server, or 0.
	retryAfter time.Duration
}

func (e recoverableError) Error() string {
	return e.err.Error()
}

func (e recoverableError) Unwrap() error {
	return e.err
}

// asRecoverable returns the recoverable error wrapped in err, if any.
func asRecoverable(err error) (recoverableError, bool) {
	var recoverable recoverableError

	ok := errors.As(err, &recoverable)

	return recoverable, ok
}

// isRecoverable returns whether the write that returned this error can be
// retried.
func isRecoverable(err error) bool {
	_, ok := asRecoverable(err)

	return ok
}

// Writer allows writing to a Prometheus remote write API.
// It is not safe for concurrent use.
type Writer struct {
	url      string
	client   *http.Client
	headers  http.Header
	metrics  *metrics
	protoBuf []byte
	scratch  []byte
}

// NewWriter returns a new initialized writer.
func NewWriter(opts Options, metrics *metrics) (*Writer, error) {
	u, err := url.Parse(opts.RemoteWriteURL)
	if err != nil {
		return nil, fmt.Errorf("parse remote write URL '%s': %w", opts.RemoteWriteURL, err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: '%s' should begin with http:// or https://", errInvalidURL, opts.RemoteWriteURL)
	}

	headers, err := writeRequestHeaders(opts)
	if err != nil {
		return nil, err
	}

	w := &Writer{
		url:     u.String(),
		client:  &http.Client{Timeout: clientTimeout, Transport: newTransport()},
		headers: headers,
		metrics: metrics,
	}

	return w, nil
}

// newTransport returns the transport used for the write requests: the
// default transport without proxy support, as the storage is expected to be
// reachable directly even when HTTP_PROXY environment variables are set.
func newTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}

	transport = transport.Clone()
	transport.Proxy = nil

	return transport
}

// writeRequestHeaders builds the headers sent with the write requests,
// including the authentication and the user-provided headers.
func writeRequestHeaders(opts Options) (http.Header, error) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/x-protobuf")
	headers.Set("Content-Encoding", "snappy")
	headers.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	headers.Set("User-Agent", "squirreldb-ingestor/"+version)

	for _, header := range opts.RemoteWriteHeader {
		name, value, found := strings.Cut(header, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("%w: '%s' should have the form 'Name=Value'", errInvalidHeader, header)
		}

		canonical := http.CanonicalHeaderKey(name)
		if canonical == "Content-Type" || canonical == "Content-Encoding" ||
			canonical == "X-Prometheus-Remote-Write-Version" {
			return nil, fmt.Errorf("%w: '%s' is reserved by the remote write protocol", errInvalidHeader, name)
		}

		headers.Set(name, value)
	}

	if opts.RemoteWritePassword != "" && opts.RemoteWriteUsername == "" {
		return nil, fmt.Errorf("%w: a password requires a username", errInvalidAuth)
	}

	authSources := 0

	for _, configured := range []bool{
		opts.RemoteWriteUsername != "",
		opts.RemoteWriteBearerToken != "",
		headers.Get("Authorization") != "",
	} {
		if configured {
			authSources++
		}
	}

	if authSources > 1 {
		return nil, fmt.Errorf(
			"%w: use only one of basic auth, a bearer token and an Authorization header", errInvalidAuth,
		)
	}

	if opts.RemoteWriteUsername != "" {
		(&http.Request{Header: headers}).SetBasicAuth(opts.RemoteWriteUsername, opts.RemoteWritePassword)
	}

	if opts.RemoteWriteBearerToken != "" {
		headers.Set("Authorization", "Bearer "+opts.RemoteWriteBearerToken)
	}

	return headers, nil
}

// Write samples to the configured Prometheus remote write endpoint.
// Errors that are worth retrying are wrapped in a recoverable error,
// see isRecoverable; the other errors mean the storage definitively
// rejected this content.
func (w *Writer) Write(ctx context.Context, samples []sample) error {
	w.protoBuf, w.scratch = marshalWriteRequest(w.protoBuf, w.scratch, samples)

	// The request body must not be reused across attempts: net/http may
	// still read it from another goroutine after Do returns (e.g. on a
	// timeout), so a shared buffer could be mutated while being read.
	body := snappy.Encode(nil, w.protoBuf)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header = w.headers.Clone()

	start := time.Now()

	resp, err := w.client.Do(req)

	w.metrics.writeDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		return recoverableError{err: fmt.Errorf("remote write: %w", err)}
	}

	defer resp.Body.Close()

	return responseError(resp)
}

// responseError converts an HTTP response to an error, wrapped in a
// recoverable error when the request can be retried.
func responseError(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		// Drain the body so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodySize))

		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	detail := fmt.Sprintf("status %s: %s", resp.Status, strings.TrimSpace(string(body)))

	// Only 400 and 413 mean the storage refused this content. The other
	// statuses are treated as recoverable and retried: server errors and
	// throttling are transient, and the remaining 4xx (401, 403, 404...)
	// are configuration errors that dropping data would not fix.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge {
		return fmt.Errorf("%w: %s", errRequestRejected, detail)
	}

	return recoverableError{
		err:        fmt.Errorf("%w: %s", errWriteFailed, detail),
		retryAfter: parseRetryAfter(resp),
	}
}

// parseRetryAfter returns the delay requested by the Retry-After header of
// the response, or 0.
func parseRetryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	if date, err := http.ParseTime(value); err == nil {
		if delay := time.Until(date); delay > 0 {
			return delay
		}
	}

	return 0
}
