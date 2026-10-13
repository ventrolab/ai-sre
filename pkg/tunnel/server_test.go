package tunnel

import (
	"io"
	"log/slog"
	"testing"
)

func TestAgentLimit(t *testing.T) {
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.MaxAgents = 1
	if r.full("a") {
		t.Fatal("empty registry is full")
	}
	r.set(&Agent{Name: "a"})
	if !r.full("b") {
		t.Fatal("a second agent must be refused")
	}
	if r.full("a") {
		t.Fatal("a reconnecting agent must not be refused")
	}
	r.MaxAgents = 0
	if r.full("b") {
		t.Fatal("0 means unlimited")
	}
}
