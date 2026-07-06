package main

import "testing"

// The version variable defaults to "dev" and is only ever changed by the
// release build's ldflags injection. If this fails, the ldflags contract in
// the Dockerfile/release workflow has drifted from the variable it targets.
func TestVersionDefaultsToDev(t *testing.T) {
	if version != "dev" {
		t.Fatalf("version default = %q, want %q (release versions come from ldflags, not source)", version, "dev")
	}
}
