package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var errInvalidLabels = errors.New("invalid labels")

// parseLabelsText parses a labels text of the form `name="value",other="value"`
// into a list of labels sorted by name. The values are quoted strings using
// Go escape sequences, as produced by Glouton.
func parseLabelsText(text string) ([]promLabel, error) {
	var lbls []promLabel

	rest := skipSpaces(text)

	for rest != "" {
		var (
			label promLabel
			err   error
		)

		label, rest, err = parseLabel(rest)
		if err != nil {
			return nil, err
		}

		lbls = append(lbls, label)

		rest = skipSpaces(rest)
		if rest == "" {
			break
		}

		if rest[0] != ',' {
			return nil, fmt.Errorf("%w: expected ',' near '%s'", errInvalidLabels, rest)
		}

		rest = skipSpaces(rest[1:])
	}

	return sortLabels(lbls), nil
}

// parseLabel parses a single `name="value"` pair and returns the label and the
// text remaining after it.
func parseLabel(text string) (promLabel, string, error) {
	nameLen := 0
	for nameLen < len(text) && isLabelNameChar(text[nameLen], nameLen == 0) {
		nameLen++
	}

	if nameLen == 0 {
		return promLabel{}, "", fmt.Errorf("%w: expected a label name near '%s'", errInvalidLabels, text)
	}

	name := text[:nameLen]

	rest := skipSpaces(text[nameLen:])
	if rest == "" || rest[0] != '=' {
		return promLabel{}, "", fmt.Errorf("%w: expected '=' after '%s'", errInvalidLabels, name)
	}

	rest = skipSpaces(rest[1:])

	value, rest, err := parseQuotedValue(rest)
	if err != nil {
		return promLabel{}, "", err
	}

	return promLabel{name: name, value: value}, rest, nil
}

// parseQuotedValue parses a quoted label value and returns the unquoted value
// and the text remaining after it.
func parseQuotedValue(text string) (string, string, error) {
	if text == "" || text[0] != '"' {
		return "", "", fmt.Errorf("%w: expected a quoted value near '%s'", errInvalidLabels, text)
	}

	end := 1
	for end < len(text) && text[end] != '"' {
		if text[end] == '\\' {
			end++
		}

		end++
	}

	if end >= len(text) {
		return "", "", fmt.Errorf("%w: unterminated value near '%s'", errInvalidLabels, text)
	}

	value, err := strconv.Unquote(text[:end+1])
	if err != nil {
		return "", "", fmt.Errorf("%w: unquote value near '%s': %w", errInvalidLabels, text, err)
	}

	// Reject invalid UTF-8 instead of letting it through: protobuf string
	// fields must be valid UTF-8, so a single bad value would make the
	// storage reject the whole write request. This catches both escapes
	// producing raw bytes (\xff) and raw invalid bytes in the input, which
	// strconv.Unquote silently replaces with U+FFFD.
	if !utf8.ValidString(value) || !utf8.ValidString(text[:end+1]) {
		return "", "", fmt.Errorf("%w: invalid UTF-8 in value near '%s'", errInvalidLabels, text)
	}

	return value, text[end+1:], nil
}

func isLabelNameChar(c byte, first bool) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' {
		return true
	}

	return !first && c >= '0' && c <= '9'
}

func skipSpaces(text string) string {
	return strings.TrimLeft(text, " \t")
}

// sortLabels sorts the labels by name, keeping only the last occurrence of
// duplicated names.
func sortLabels(lbls []promLabel) []promLabel {
	slices.SortStableFunc(lbls, func(a, b promLabel) int {
		return strings.Compare(a.name, b.name)
	})

	result := lbls[:0]

	for i, label := range lbls {
		if i+1 < len(lbls) && lbls[i+1].name == label.name {
			continue
		}

		result = append(result, label)
	}

	return result
}

// setInstance returns the labels with the "instance" label set to the given
// value, replacing the existing one if any. The labels must be sorted, as
// returned by parseLabelsText, and stay sorted.
func setInstance(lbls []promLabel, instance string) []promLabel {
	index, found := slices.BinarySearchFunc(lbls, "instance", func(label promLabel, name string) int {
		return strings.Compare(label.name, name)
	})

	if found {
		lbls[index].value = instance

		return lbls
	}

	return slices.Insert(lbls, index, promLabel{name: "instance", value: instance})
}
