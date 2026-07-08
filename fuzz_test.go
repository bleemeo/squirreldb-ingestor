package main

import (
	"bytes"
	"compress/zlib"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func zlibPayload(data string) []byte {
	var buf bytes.Buffer

	writer := zlib.NewWriter(&buf)
	_, _ = writer.Write([]byte(data))
	_ = writer.Close()

	return buf.Bytes()
}

func FuzzParseLabelsText(f *testing.F) {
	seeds := []string{
		"",
		`__name__="cpu_used_fuzz"`,
		`item="/home",__name__="disk_used"`,
		` a = "1" , b = "2" `,
		`path="a\\b",quote="say \"hi\"",newline="a\nb"`,
		`a="1",a="2"`,
		`name="unterminated`,
		`=`,
		`a="\`,
		"name=\"\xff\xfe\"",
		`a="\x41",b="é",c="\777"`,
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		lbls, err := parseLabelsText(text)
		if err != nil {
			if lbls != nil {
				t.Errorf("parseLabelsText(%q) returned both labels %v and error %v", text, lbls, err)
			}

			return
		}

		// The labels must be sorted by name, without duplicated names.
		for i := 1; i < len(lbls); i++ {
			if lbls[i-1].name >= lbls[i].name {
				t.Errorf("parseLabelsText(%q) = %v is not sorted", text, lbls)
			}
		}

		// Formatting the labels back and reparsing them must give the same labels.
		var sb strings.Builder

		for i, label := range lbls {
			if i > 0 {
				sb.WriteByte(',')
			}

			sb.WriteString(label.name)
			sb.WriteByte('=')
			sb.WriteString(strconv.Quote(label.value))
		}

		again, err := parseLabelsText(sb.String())
		if err != nil {
			t.Errorf("parseLabelsText(%q) failed to reparse its own output %q: %v", text, sb.String(), err)

			return
		}

		if len(lbls) != 0 && !reflect.DeepEqual(lbls, again) {
			t.Errorf("parseLabelsText(%q) = %v, but reparsing its output gave %v", text, lbls, again)
		}
	})
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte("not zlib at all"))
	f.Add([]byte{0x78, 0x9c})
	f.Add(zlibPayload(`[]`))
	f.Add(zlibPayload(`[{"labels_text":"__name__=\"up\"","time_ms":1600000000000,"value":42.5}]`))
	f.Add(zlibPayload(`[{"labels_text":`))
	f.Add(zlibPayload(`[] trailing garbage`))

	f.Fuzz(func(_ *testing.T, payload []byte) {
		var metrics []metricPayload

		// The decoding must never panic, whatever the payload is.
		_ = decode(payload, &metrics)
	})
}

func FuzzMarshalWriteRequest(f *testing.F) {
	f.Add("__name__", "up", 1.0, int64(1600000000000))
	f.Add("", "", 0.0, int64(0))
	f.Add("a", strings.Repeat("x", 300), -1.5, int64(-42))
	f.Add("name", "value with \" and \\", 3.14, int64(1)<<62)

	f.Fuzz(func(t *testing.T, name, value string, v float64, ts int64) {
		samples := []sample{{
			labels:      []promLabel{{name: name, value: value}},
			value:       v,
			timestampMS: ts,
		}}

		buf := marshalWriteRequest(nil, samples)

		// The encoded length must match the sizes computed by the encoder:
		// a mismatch would produce a length prefix inconsistent with the
		// actual content, and the storage would fail to decode the request.
		tsSize := timeSeriesSize(samples[0])

		wantSize := 1 + varintSize(tsSize) + tsSize
		if uint64(len(buf)) != wantSize {
			t.Errorf("marshalWriteRequest() encoded %d bytes, want %d", len(buf), wantSize)
		}
	})
}
