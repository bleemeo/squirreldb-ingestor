// Hand-rolled protobuf encoding of the Prometheus remote write protocol.
//
// The remote write payload only uses a tiny subset of protobuf (strings,
// doubles and varints), encoding it manually avoids depending on the whole
// github.com/prometheus/prometheus module and a protobuf library.
//
// The messages encoded here are defined in
// https://github.com/prometheus/prometheus/blob/main/prompb/types.proto:
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; }
//	message TimeSeries { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label { string name = 1; string value = 2; }
//	message Sample { double value = 1; int64 timestamp = 2; }

package main

import (
	"encoding/binary"
	"math"
)

// Protobuf tags of the remote write messages (field number << 3 | wire type).
const (
	// WriteRequest.timeseries, field 1, length-delimited.
	tagWriteRequestTimeseries = 0x0a
	// TimeSeries.labels, field 1, length-delimited.
	tagTimeSeriesLabels = 0x0a
	// TimeSeries.samples, field 2, length-delimited.
	tagTimeSeriesSamples = 0x12
	// Label.name, field 1, length-delimited.
	tagLabelName = 0x0a
	// Label.value, field 2, length-delimited.
	tagLabelValue = 0x12
	// Sample.value, field 1, 64-bit.
	tagSampleValue = 0x09
	// Sample.timestamp, field 2, varint.
	tagSampleTimestamp = 0x10
)

// promLabel is a label of a Prometheus time series.
type promLabel struct {
	name  string
	value string
}

// sample is a point of a Prometheus time series.
type sample struct {
	// The labels must be sorted by name.
	labels []promLabel
	value  float64
	// Timestamp in ms.
	timestampMS int64
}

// marshalWriteRequest encodes the samples as a remote write WriteRequest.
// The buffer is reused if it is big enough.
func marshalWriteRequest(buf []byte, samples []sample) []byte {
	buf = buf[:0]

	for _, s := range samples {
		buf = append(buf, tagWriteRequestTimeseries)
		buf = appendVarint(buf, timeSeriesSize(s))
		buf = appendTimeSeries(buf, s)
	}

	return buf
}

func appendTimeSeries(buf []byte, s sample) []byte {
	for _, label := range s.labels {
		buf = append(buf, tagTimeSeriesLabels)
		buf = appendVarint(buf, labelSize(label))
		buf = appendString(buf, tagLabelName, label.name)
		buf = appendString(buf, tagLabelValue, label.value)
	}

	buf = append(buf, tagTimeSeriesSamples)
	buf = appendVarint(buf, sampleSize(s))
	buf = append(buf, tagSampleValue)
	buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(s.value))
	buf = append(buf, tagSampleTimestamp)
	// Negative timestamps are encoded as two's complement, like protobuf does.
	buf = appendVarint(buf, uint64(s.timestampMS)) //nolint:gosec

	return buf
}

func appendString(buf []byte, tag byte, s string) []byte {
	buf = append(buf, tag)
	buf = appendVarint(buf, uint64(len(s)))

	return append(buf, s...)
}

func appendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}

	return append(buf, byte(v))
}

func timeSeriesSize(s sample) uint64 {
	size := uint64(0)

	for _, label := range s.labels {
		lSize := labelSize(label)
		size += 1 + varintSize(lSize) + lSize
	}

	sSize := sampleSize(s)
	size += 1 + varintSize(sSize) + sSize

	return size
}

func labelSize(label promLabel) uint64 {
	return stringSize(label.name) + stringSize(label.value)
}

func sampleSize(s sample) uint64 {
	// Tag and 8 bytes for the value, tag and varint for the timestamp.
	// Negative timestamps are encoded as two's complement, like protobuf does.
	return 1 + 8 + 1 + varintSize(uint64(s.timestampMS)) //nolint:gosec
}

func stringSize(s string) uint64 {
	return 1 + varintSize(uint64(len(s))) + uint64(len(s))
}

func varintSize(v uint64) uint64 {
	size := uint64(1)

	for v >= 0x80 {
		size++
		v >>= 7
	}

	return size
}
