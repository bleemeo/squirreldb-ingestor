package main

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
)

// readVarint is a reference decoder used to validate appendVarint.
func readVarint(t *testing.T, buf []byte) (uint64, []byte) {
	t.Helper()

	var (
		value uint64
		shift uint
	)

	for i, b := range buf {
		value |= uint64(b&0x7f) << shift
		shift += 7

		if b < 0x80 {
			return value, buf[i+1:]
		}
	}

	t.Fatal("truncated varint")

	return 0, nil
}

func TestAppendVarint(t *testing.T) {
	t.Parallel()

	values := []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 32, 1600000000000, math.MaxUint64}

	for _, value := range values {
		buf := appendVarint(nil, value)

		if size := varintSize(value); size != uint64(len(buf)) {
			t.Errorf("varintSize(%d) = %d, want %d", value, size, len(buf))
		}

		got, rest := readVarint(t, buf)
		if got != value || len(rest) != 0 {
			t.Errorf("Decoded varint %d (with %d remaining bytes), want %d", got, len(rest), value)
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

	got := hex.EncodeToString(marshalWriteRequest(nil, samples))
	if got != want {
		t.Errorf("marshalWriteRequest() = %s, want %s", got, want)
	}
}

func TestMarshalWriteRequestEmpty(t *testing.T) {
	t.Parallel()

	if got := marshalWriteRequest(nil, nil); len(got) != 0 {
		t.Errorf("marshalWriteRequest(nil, nil) = %x, want an empty buffer", got)
	}
}

func TestMarshalWriteRequestReusesBuffer(t *testing.T) {
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

	first := marshalWriteRequest(nil, samples)
	second := marshalWriteRequest(make([]byte, 1024), samples)

	if !bytes.Equal(first, second) {
		t.Errorf("Encoding with a reused buffer gave %x, want %x", second, first)
	}
}
