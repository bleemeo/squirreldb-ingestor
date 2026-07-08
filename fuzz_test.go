package main

import (
	"encoding/binary"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

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
		`bad="\xff"`,
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

		for i, label := range lbls {
			// The labels must be sorted by name, without duplicated names.
			if i > 0 && lbls[i-1].name >= label.name {
				t.Errorf("parseLabelsText(%q) = %v is not sorted", text, lbls)
			}

			// The values feed protobuf string fields, which must be valid UTF-8.
			if !utf8.ValidString(label.value) {
				t.Errorf("parseLabelsText(%q) accepted the invalid UTF-8 value %q", text, label.value)
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

		buf, _ := marshalWriteRequest(nil, nil, samples)

		// The length prefix must be consistent with the encoded content:
		// a mismatch would make the storage fail to decode the request.
		if len(buf) == 0 || buf[0] != tagWriteRequestTimeseries {
			t.Fatalf("marshalWriteRequest() = %x, want a TimeSeries frame", buf)
		}

		size, n := binary.Uvarint(buf[1:])
		if n <= 0 || uint64(len(buf)) != 1+uint64(n)+size {
			t.Errorf("Length prefix %d inconsistent with the %d encoded bytes", size, len(buf))
		}
	})
}
