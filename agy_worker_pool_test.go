package main

import (
	"context"
	"testing"
	"time"
)

func TestCanonicalAgyModel(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"Gemini 3.8 Flash (Low)", "gemini-3.8-flash-low", true},
		{"Gemini 3.8 Flash (High)", "gemini-3.8-flash-high", true},
		{"Gemini 3.7 Flash", "gemini-3.7-flash-high", false},
		{"gemini-3.5-flash-low", "gemini-3.6-flash-low", true},
	}
	for _, c := range cases {
		got := canonicalAgyModel(c.a) == canonicalAgyModel(c.b)
		if got != c.want {
			t.Errorf("canonicalAgyModel(%q) == canonicalAgyModel(%q) got %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseAgyWarmWorkers(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"3", 3},
		{"17", 17},
		{"100", 64}, // capped at 64
		{"invalid", 0},
	}
	for _, tt := range tests {
		if got := parseAgyWarmWorkers(tt.in); got != tt.want {
			t.Errorf("parseAgyWarmWorkers(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestAgyWorkerPool_DisabledWhenZero(t *testing.T) {
	cfg := config{AgyWarmWorkers: 0}
	initAgyWorkerPool(cfg)
	pool := getAgyWorkerPool()
	if pool != nil && pool.IsEnabled() {
		t.Errorf("expected pool to be disabled or nil when AgyWarmWorkers=0")
	}
}

func TestAgyWorkerPool_QueueTimeout(t *testing.T) {
	pool := &AgyWorkerPool{
		size:         2,
		queueTimeout: 50 * time.Millisecond,
		idleWorkers:  make(chan *AgyWorker, 2),
	}
	ctx := context.Background()

	// Both workers are busy / unpopulated -> acquire should hit timeout
	_, err := pool.Acquire(ctx)
	if err != ErrAgyQueueTimeout {
		t.Errorf("expected ErrAgyQueueTimeout, got %v", err)
	}
}

func TestAgyWorkerPool_QueueAcquireSuccess(t *testing.T) {
	pool := &AgyWorkerPool{
		size:         1,
		queueTimeout: 200 * time.Millisecond,
		idleWorkers:  make(chan *AgyWorker, 1),
	}
	mockWorker := &AgyWorker{id: 99, model: "gemini-3.8-flash-low"}
	pool.idleWorkers <- mockWorker

	ctx := context.Background()
	w, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("unexpected error acquiring worker: %v", err)
	}
	if w.id != 99 {
		t.Errorf("expected worker id 99, got %d", w.id)
	}
}
