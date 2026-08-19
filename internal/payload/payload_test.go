package payload_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bleemeo/squirreldb-ingestor/internal/payload"
)

const maxSize = 10 << 20

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	want := []payload.Metric{
		{
			LabelsText:  `__name__="cpu_used_payload"`,
			TimestampMS: 1600000000000,
			Value:       42.5,
		},
	}

	data, err := payload.Encode(want)
	if err != nil {
		t.Fatalf("Encode() failed: %v", err)
	}

	got, err := payload.Decode(data, maxSize)
	if err != nil {
		t.Fatalf("Decode() failed: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Decode() = %v, want %v", got, want)
	}
}

func TestDecodeInvalid(t *testing.T) {
	t.Parallel()

	notJSON, err := payload.Compress([]byte("not json"))
	if err != nil {
		t.Fatalf("Compress() failed: %v", err)
	}

	tests := []struct {
		name  string
		input []byte
	}{
		{name: "not-zlib", input: []byte("not zlib")},
		{name: "empty-input", input: nil},
		{name: "not-json", input: notJSON},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got, err := payload.Decode(test.input, maxSize); err == nil {
				t.Errorf("Decode(%q) = %v, want an error", test.input, got)
			}
		})
	}
}

// TestDecodeBomb ensures a payload expanding over the decompressed size limit
// is rejected with ErrTooLarge instead of exhausting the memory.
func TestDecodeBomb(t *testing.T) {
	t.Parallel()

	// A highly compressible 60 MB payload: a few kilobytes compressed.
	bomb, err := payload.Compress([]byte(`[{"labels_text":"` + strings.Repeat("a", 60<<20) + `"}]`))
	if err != nil {
		t.Fatalf("Compress() failed: %v", err)
	}

	if len(bomb) > 1<<20 {
		t.Fatalf("The compressed bomb is %d bytes, expected it to be small", len(bomb))
	}

	_, err = payload.Decode(bomb, maxSize)
	if !errors.Is(err, payload.ErrTooLarge) {
		t.Errorf("Decode(bomb) error = %v, want ErrTooLarge", err)
	}
}

// TestDecodeTrailingBomb ensures the limit also covers data found after the
// JSON value.
func TestDecodeTrailingBomb(t *testing.T) {
	t.Parallel()

	bomb, err := payload.Compress([]byte(`[]` + strings.Repeat(" ", maxSize)))
	if err != nil {
		t.Fatalf("Compress() failed: %v", err)
	}

	_, err = payload.Decode(bomb, maxSize)
	if !errors.Is(err, payload.ErrTooLarge) {
		t.Errorf("Decode(trailing bomb) error = %v, want ErrTooLarge", err)
	}
}

func FuzzDecode(f *testing.F) {
	valid, err := payload.Encode([]payload.Metric{
		{LabelsText: `__name__="up"`, TimestampMS: 1600000000000, Value: 42.5},
	})
	if err != nil {
		f.Fatalf("Encode() failed: %v", err)
	}

	empty, err := payload.Compress([]byte(`[]`))
	if err != nil {
		f.Fatalf("Compress() failed: %v", err)
	}

	truncated, err := payload.Compress([]byte(`[{"labels_text":`))
	if err != nil {
		f.Fatalf("Compress() failed: %v", err)
	}

	f.Add([]byte("not zlib at all"))
	f.Add([]byte{0x78, 0x9c})
	f.Add(valid)
	f.Add(empty)
	f.Add(truncated)

	f.Fuzz(func(_ *testing.T, input []byte) {
		// The decoding must never panic, whatever the payload is.
		_, _ = payload.Decode(input, 1<<20)
	})
}
