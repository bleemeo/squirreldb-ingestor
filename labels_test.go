package main

import (
	"reflect"
	"testing"
)

func TestParseLabelsText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []promLabel
	}{
		{
			name: "empty-text",
			text: "",
			want: nil,
		},
		{
			name: "single-label",
			text: `__name__="cpu_used"`,
			want: []promLabel{{name: testMetricNameLabel, value: "cpu_used"}},
		},
		{
			name: "multiple-labels-sorted",
			text: `item="/home",__name__="disk_used"`,
			want: []promLabel{
				{name: testMetricNameLabel, value: "disk_used"},
				{name: testItemLabel, value: "/home"},
			},
		},
		{
			name: "spaces",
			text: ` __name__ = "up" , instance = "server1" `,
			want: []promLabel{
				{name: testMetricNameLabel, value: "up"},
				{name: testInstanceLabel, value: "server1"},
			},
		},
		{
			name: "escaped-values",
			text: `path="a\\b",quote="say \"hi\"",newline="a\nb"`,
			want: []promLabel{
				{name: "newline", value: "a\nb"},
				{name: "path", value: `a\b`},
				{name: "quote", value: `say "hi"`},
			},
		},
		{
			name: "special-characters-in-value",
			text: `selector="{__name__=~\"node_.*\"}",empty=""`,
			want: []promLabel{
				{name: "empty", value: ""},
				{name: "selector", value: `{__name__=~"node_.*"}`},
			},
		},
		{
			name: "duplicated-name-keeps-last",
			text: `a="1",a="2"`,
			want: []promLabel{{name: "a", value: "2"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseLabelsText(test.text)
			if err != nil {
				t.Fatalf("parseLabelsText(%q) failed: %v", test.text, err)
			}

			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("parseLabelsText(%q) = %v, want %v", test.text, got, test.want)
			}
		})
	}
}

func TestParseLabelsTextErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
	}{
		{name: "missing-value", text: `name=`},
		{name: "unquoted-value", text: `name=value`},
		{name: "unterminated-value", text: `name="value`},
		{name: "missing-name", text: `="value"`},
		{name: "name-starts-with-digit", text: `1name="value"`},
		{name: "missing-comma", text: `a="1" b="2"`},
		{name: "single-quotes", text: `name='value'`},
		{name: "trailing-garbage", text: `a="1"; b="2"`},
		{name: "escaped-invalid-utf8", text: `a="\xff"`},
		{name: "raw-invalid-utf8", text: "a=\"\xff\xfe\""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseLabelsText(test.text)
			if err == nil {
				t.Errorf("parseLabelsText(%q) = %v, want an error", test.text, got)
			}
		})
	}
}

func TestSetInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		lbls []promLabel
		want []promLabel
	}{
		{
			name: "add",
			lbls: []promLabel{{name: testMetricNameLabel, value: "up"}},
			want: []promLabel{
				{name: testMetricNameLabel, value: "up"},
				{name: testInstanceLabel, value: testFQDN},
			},
		},
		{
			name: "replace",
			lbls: []promLabel{
				{name: testMetricNameLabel, value: "up"},
				{name: testInstanceLabel, value: "untrusted"},
				{name: testItemLabel, value: "/"},
			},
			want: []promLabel{
				{name: testMetricNameLabel, value: "up"},
				{name: testInstanceLabel, value: testFQDN},
				{name: testItemLabel, value: "/"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := setInstance(test.lbls, testFQDN)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("setInstance() = %v, want %v", got, test.want)
			}
		})
	}
}
