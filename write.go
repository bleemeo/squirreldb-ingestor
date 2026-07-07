// Minimal Prometheus remote write client.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	errInvalidURL       = errors.New("invalid remote write URL")
	errInvalidAuth      = errors.New("invalid remote write authentication options")
	errInvalidHeader    = errors.New("invalid remote write header")
	errRequestRejected  = errors.New("the remote storage rejected the request")
	errServersideError  = errors.New("the remote storage failed to process the request")
	errTooManyRequests  = errors.New("the remote storage throttled the request")
	errUnexpectedStatus = errors.New("unexpected status")
)

// recoverableError wraps an error that is worth retrying, like a network
// failure or a server-side error.
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

// isRecoverable returns whether the write that returned this error can be
// retried.
func isRecoverable(err error) bool {
	var recoverable recoverableError

	return errors.As(err, &recoverable)
}

// retryAfterDelay returns the retry delay requested by the server with the
// error, or 0.
func retryAfterDelay(err error) time.Duration {
	var recoverable recoverableError

	if errors.As(err, &recoverable) {
		return recoverable.retryAfter
	}

	return 0
}

// Writer allows writing to a Prometheus remote write API.
// It is not safe for concurrent use.
type Writer struct {
	url      string
	client   *http.Client
	headers  http.Header
	metrics  *metrics
	buf      []byte
	protoBuf []byte
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
		client:  &http.Client{Timeout: clientTimeout},
		headers: headers,
		metrics: metrics,
	}

	return w, nil
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

	hasBasic := opts.RemoteWriteUsername != ""
	hasBearer := opts.RemoteWriteBearerToken != ""

	if opts.RemoteWritePassword != "" && !hasBasic {
		return nil, fmt.Errorf("%w: a password requires a username", errInvalidAuth)
	}

	if hasBasic && hasBearer {
		return nil, fmt.Errorf("%w: the basic and bearer token authentications are exclusive", errInvalidAuth)
	}

	if (hasBasic || hasBearer) && headers.Get("Authorization") != "" {
		return nil, fmt.Errorf("%w: an Authorization header can't be combined with other authentications", errInvalidAuth)
	}

	if hasBasic {
		// Same encoding as http.Request.SetBasicAuth.
		auth := opts.RemoteWriteUsername + ":" + opts.RemoteWritePassword
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
	}

	if hasBearer {
		headers.Set("Authorization", "Bearer "+opts.RemoteWriteBearerToken)
	}

	return headers, nil
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
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	detail := fmt.Sprintf("status %s: %s", resp.Status, strings.TrimSpace(string(body)))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return recoverableError{
			err:        fmt.Errorf("%w: %s", errTooManyRequests, detail),
			retryAfter: parseRetryAfter(resp),
		}
	case resp.StatusCode/100 == 5:
		return recoverableError{
			err:        fmt.Errorf("%w: %s", errServersideError, detail),
			retryAfter: parseRetryAfter(resp),
		}
	case resp.StatusCode/100 == 4:
		return fmt.Errorf("%w: %s", errRequestRejected, detail)
	default:
		return fmt.Errorf("%w: %s", errUnexpectedStatus, detail)
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
