package main

import (
	"context"
	"testing"
	"time"
)

func TestParseCodexWarmWorkers(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 7},
		{"0", 0},
		{"-1", 0},
		{"3", 3},
		{"7", 7},
		{"17", 17},
		{"100", 64}, // capped at 64
		{"invalid", 0},
	}
	for _, tt := range tests {
		if got := parseCodexWarmWorkers(tt.in); got != tt.want {
			t.Errorf("parseCodexWarmWorkers(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestCodexWorkerPool_DisabledWhenZero(t *testing.T) {
	cfg := config{CodexWarmWorkers: 0}
	initCodexWorkerPool(cfg)
	pool := getCodexWorkerPool()
	if pool != nil && pool.IsEnabled() {
		t.Errorf("expected pool to be disabled or nil when CodexWarmWorkers=0")
	}
}

func TestCodexWorkerPool_QueueTimeout(t *testing.T) {
	pool := &CodexWorkerPool{
		size:         2,
		queueTimeout: 50 * time.Millisecond,
		idleWorkers:  make(chan *CodexWorker, 2),
	}
	ctx := context.Background()

	// Both workers are unpopulated -> acquire should hit timeout
	_, _, err := pool.Acquire(ctx)
	if err != ErrCodexQueueTimeout {
		t.Errorf("expected ErrCodexQueueTimeout, got %v", err)
	}
}

func TestCodexWorkerPool_AcquireAndRelease(t *testing.T) {
	pool := &CodexWorkerPool{
		size:         1,
		queueTimeout: 200 * time.Millisecond,
		idleWorkers:  make(chan *CodexWorker, 1),
	}
	mockWorker := &CodexWorker{id: 42, client: newCodexWorkerClient()}
	pool.idleWorkers <- mockWorker

	ctx := context.Background()
	w, release, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error acquiring worker: %v", err)
	}
	if w.ID() != 42 {
		t.Errorf("expected worker id 42, got %d", w.ID())
	}
	if pool.ActiveCount() != 1 {
		t.Errorf("expected activeCount 1, got %d", pool.ActiveCount())
	}

	release()
	if pool.ActiveCount() != 0 {
		t.Errorf("expected activeCount 0 after release, got %d", pool.ActiveCount())
	}

	// Verify worker returned to idle channel
	select {
	case returned := <-pool.idleWorkers:
		if returned.ID() != 42 {
			t.Errorf("expected returned worker id 42, got %d", returned.ID())
		}
	default:
		t.Errorf("expected worker back in idleWorkers channel")
	}
}
