package main

import "testing"

func TestParseDisplayDensityPrefersOverride(t *testing.T) {
	out := "Physical density: 420\nOverride density: 480\n"
	if got := parseDisplayDensity(out); got != 480 {
		t.Fatalf("parseDisplayDensity = %d, want 480", got)
	}
}
