package main

import (
	"fmt"
	"testing"
)

func TestFQDNFromTopic(t *testing.T) {
	fqdns := []string{
		"myfqdn",
		"aaaa.bbbb",
		"aaaa.bbbb.cc",
	}

	for _, fqdn := range fqdns {
		topic := fmt.Sprintf("v1/agent/%s/data", fqdn)

		got, err := fqdnFromTopic(topic)
		if err != nil {
			t.Fatalf("Failed to get FQDN for topic %s: %v", topic, err)
		}

		if got != fqdn {
			t.Fatalf("Wanted '%s', got '%s'", fqdn, got)
		}
	}
}
