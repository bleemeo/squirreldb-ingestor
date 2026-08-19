package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"
)

func TestVarintSize(t *testing.T) {
	t.Parallel()

	values := []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 32, 1600000000000, math.MaxUint64}

	for _, value := range values {
		if size, want := varintSize(value), len(binary.AppendUvarint(nil, value)); size != uint64(want) {
			t.Errorf("varintSize(%d) = %d, want %d", value, size, want)
		}
	}
}

func TestMarshalWriteRequest(t *testing.T) {
	t.Parallel()

	samples := []sample{
		{
			labels: []promLabel{
				{name: testMetricNameLabel, value: "up"},
				{name: testInstanceLabel, value: "srv"},
			},
			value:       1,
			timestampMS: 1,
		},
	}

	// Encoding computed by hand from the protobuf wire format of the
	// remote write WriteRequest message.
	want := "0a2e" +
		"0a0e" + "0a08" + "5f5f6e616d655f5f" + "1202" + "7570" + // __name__="up"
		"0a0f" + "0a08" + "696e7374616e6365" + "1203" + "737276" + // instance="srv"
		"120b" + "09" + "000000000000f03f" + "10" + "01" // value=1, timestamp=1

	buf, _ := marshalWriteRequest(nil, nil, samples)
	if got := hex.EncodeToString(buf); got != want {
		t.Errorf("marshalWriteRequest() = %s, want %s", got, want)
	}
}

func TestMarshalWriteRequestEmpty(t *testing.T) {
	t.Parallel()

	if got, _ := marshalWriteRequest(nil, nil, nil); len(got) != 0 {
		t.Errorf("marshalWriteRequest(nil, nil, nil) = %x, want an empty buffer", got)
	}
}

func TestMarshalWriteRequestReusesBuffers(t *testing.T) {
	t.Parallel()

	samples := []sample{
		{
			labels:      []promLabel{{name: testMetricNameLabel, value: "cpu_used"}},
			value:       42.5,
			timestampMS: 1600000000000,
		},
		{
			labels:      []promLabel{{name: testMetricNameLabel, value: "mem_used"}},
			value:       0,
			timestampMS: 0,
		},
	}

	first, scratch := marshalWriteRequest(nil, nil, samples)
	second, _ := marshalWriteRequest(make([]byte, 1024), scratch, samples)

	if !bytes.Equal(first, second) {
		t.Errorf("Encoding with reused buffers gave %x, want %x", second, first)
	}
}
