package main

import "testing"

func TestSkipDNSCheck(t *testing.T) {
	for domain, want := range map[string]bool{
		"localhost":       true,
		"app.localhost.":  true,
		"127.0.0.1":       true,
		"::1":             true,
		"hop.example.com": false,
		"localhost.dev":   false,
	} {
		if got := skipDNSCheck(domain); got != want {
			t.Errorf("skipDNSCheck(%q) = %v, want %v", domain, got, want)
		}
	}
}
