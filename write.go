// Prometheus remote write client, the code is inspired from
// https://github.com/prometheus/prometheus/blob/main/storage/remote/queue_manager.go

package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/rs/zerolog/log"
)

// The timeout of requests sent to the API.
const clientTimeout = 10 * time.Second

// Writer allows writing to a Prometheus remote write API.
type Writer struct {
	client remote.WriteClient
	buf    []byte
	pBuf   *proto.Buffer
}

type sample struct {
	labels labels.Labels
	value  float64
	// Timestamp in ms.
	timestamp int64
}

// NewWriter returns a new initialized writer.
func NewWriter(rawURL string) *Writer {
	u, err := url.Parse(rawURL)
	if err != nil {
		log.Fatal().Err(err).Msgf("Failed to parse remote write URL '%s'", rawURL)
	}

	conf := &remote.ClientConfig{
		URL:              &config.URL{URL: u},
		Timeout:          model.Duration(clientTimeout),
		HTTPClientConfig: config.DefaultHTTPClientConfig,
	}

	client, err := remote.NewWriteClient("", conf)
	if err != nil {
		log.Fatal().Err(err).Msgf("Failed to create remote write client")
	}

	w := &Writer{
		client: client,
		pBuf:   proto.NewBuffer(nil),
		buf:    []byte{},
	}

	return w
}

// Write samples to the configured prometheus remote Write endpoint.
func (w *Writer) Write(ctx context.Context, samples []sample) error {
	series := samplesToTimeseries(samples)

	req, err := w.buildWriteRequest(series)
	if err != nil {
		return err
	}

	if err := w.client.Store(ctx, req); err != nil {
		return fmt.Errorf("remote write: %w", err)
	}

	return nil
}

func (w *Writer) buildWriteRequest(samples []prompb.TimeSeries) ([]byte, error) {
	req := &prompb.WriteRequest{
		Timeseries: samples,
	}

	w.pBuf.Reset()

	err := w.pBuf.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal write request: %w", err)
	}

	// snappy uses len() to see if it needs to allocate a new slice. Make the
	// buffer as long as possible.
	w.buf = w.buf[0:cap(w.buf)]

	// Reuse the buffer allocated by snappy.
	w.buf = snappy.Encode(w.buf, w.pBuf.Bytes())

	return w.buf, nil
}

func samplesToTimeseries(samples []sample) []prompb.TimeSeries {
	series := make([]prompb.TimeSeries, len(samples))

	for nPending, d := range samples {
		series[nPending].Labels = labelsToLabelsProto(d.labels, series[nPending].Labels)
		series[nPending].Samples = []prompb.Sample{
			{
				Value:     d.value,
				Timestamp: d.timestamp,
			},
		}
	}

	return series
}

// labelsToLabelsProto transforms labels into prompb labels. The buffer slice
// will be used to avoid allocations if it is big enough to store the labels.
func labelsToLabelsProto(labels labels.Labels, buf []prompb.Label) []prompb.Label {
	result := buf[:0]

	if cap(buf) < len(labels) {
		result = make([]prompb.Label, 0, len(labels))
	}

	for _, l := range labels {
		result = append(result, prompb.Label{
			Name:  l.Name,
			Value: l.Value,
		})
	}

	return result
}
