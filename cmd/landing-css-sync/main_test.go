package main

import (
	"bytes"
	"testing"
)

func TestReadLimited(t *testing.T) {
	if b, err := readLimited(bytes.NewReader(make([]byte, 10)), 10, "x"); err != nil || len(b) != 10 {
		t.Fatalf("exact size: %d bytes, %v", len(b), err)
	}
	if _, err := readLimited(bytes.NewReader(make([]byte, 11)), 10, "x"); err == nil {
		t.Fatal("a body over the limit must be an error, not a truncated read")
	}
}
