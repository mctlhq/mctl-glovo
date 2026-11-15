package main

import (
	"bytes"
	"os"
	"testing"
)

// The Worker must serve exactly what glovo-home renders. Regenerate with
// `go run ./cmd/landing-gen` from the repository root.
func TestGeneratedFileIsCurrent(t *testing.T) {
	want, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("../../landing-worker/src/pages.gen.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("landing-worker/src/pages.gen.js is stale: run `go run ./cmd/landing-gen`")
	}
}
