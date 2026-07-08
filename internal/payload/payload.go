// Package payload implements the encoding of the metric payloads that
// Glouton publishes over MQTT: zlib compressed JSON.
package payload

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTooLarge reports a payload whose decompressed size exceeds the limit
// given to Decode.
var ErrTooLarge = errors.New("decompressed payload too large")

// Metric is one point of a payload.
type Metric struct {
	LabelsText  string  `json:"labels_text"`
	TimestampMS int64   `json:"time_ms"`
	Value       float64 `json:"value"`
}

// Encode marshals the metrics as a zlib compressed JSON payload.
func Encode(metrics []Metric) ([]byte, error) {
	data, err := json.Marshal(metrics)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	return Compress(data)
}

// Compress returns the zlib compressed data.
func Compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer

	writer := zlib.NewWriter(&buf)

	if _, err := writer.Write(data); err != nil {
		return nil, fmt.Errorf("compress: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close zlib writer: %w", err)
	}

	return buf.Bytes(), nil
}

// Decode decompresses and unmarshals a payload. Payloads expanding over
// maxDecompressedSize are rejected with ErrTooLarge: without a bound, a
// small crafted payload could expand to gigabytes and exhaust the memory.
func Decode(input []byte, maxDecompressedSize int64) ([]Metric, error) {
	decoder, err := zlib.NewReader(bytes.NewReader(input))
	if err != nil {
		return nil, fmt.Errorf("zlib reader: %w", err)
	}

	limited := &io.LimitedReader{R: decoder, N: maxDecompressedSize + 1}

	var metrics []Metric

	if err := json.NewDecoder(limited).Decode(&metrics); err != nil {
		if limited.N <= 0 {
			// The JSON was truncated by the size limit.
			return nil, ErrTooLarge
		}

		return nil, fmt.Errorf("decode JSON: %w", err)
	}

	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, fmt.Errorf("copy: %w", err)
	}

	if limited.N <= 0 {
		return nil, ErrTooLarge
	}

	if err := decoder.Close(); err != nil {
		return nil, fmt.Errorf("close decoder: %w", err)
	}

	return metrics, nil
}
