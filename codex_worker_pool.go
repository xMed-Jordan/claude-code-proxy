package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrCodexQueueTimeout is returned when all standby workers are busy and the queue timeout expires.
var ErrCodexQueueTimeout = errors.New("codex standby worker pool queue timeout exceeded")

// CodexWorker represents a persistent standby worker slot for handling Codex/ChatGPT requests.
type CodexWorker struct {
	id        int
	client    *http.Client
	turnCount atomic.Int64
	createdAt time.Time
	mu        sync.Mutex
}

// Client returns the dedicated HTTP client for this worker.
func (w *CodexWorker) Client() *http.Client {
	if w == nil || w.client == nil {
		return http.DefaultClient
	}
	return w.client
}

// ID returns the numeric worker identifier.
func (w *CodexWorker) ID() int {
	if w == nil {
		return 0
	}
	return w.id
}

// CodexWorkerPool manages a pool of standby background workers for Codex.
type CodexWorkerPool struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          config
	size         int
	queueTimeout time.Duration
	idleWorkers  chan *CodexWorker
	mu           sync.Mutex
	closed       bool
	activeCount  atomic.Int64
}

var (
	globalCodexPoolMu sync.RWMutex
	globalCodexPool   *CodexWorkerPool
)

// parseCodexWarmWorkers parses the worker count configuration with a sensible default of 7.
func parseCodexWarmWorkers(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 7
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	if n > 64 {
		n = 64
	}
	return n
}

// parseCodexQueueTimeout parses the queue timeout duration, defaulting to 30 seconds.
func parseCodexQueueTimeout(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 30 * time.Second
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 30 * time.Second
	}
	return time.Duration(n) * time.Second
}

// newCodexWorkerClient creates a dedicated HTTP client with persistent connection keep-alives.
func newCodexWorkerClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   0, // Unlimited client timeout; timeouts are controlled by request Context
	}
}

// initCodexWorkerPool initializes the global standby worker pool if configured.
func initCodexWorkerPool(cfg config) {
	globalCodexPoolMu.Lock()
	defer globalCodexPoolMu.Unlock()

	if globalCodexPool != nil {
		globalCodexPool.Stop()
		globalCodexPool = nil
	}

	if cfg.CodexWarmWorkers <= 0 {
		return
	}

	queueTimeout := cfg.CodexQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = 30 * time.Second
	}

	poolCtx, poolCancel := context.WithCancel(context.Background())

	pool := &CodexWorkerPool{
		ctx:          poolCtx,
		cancel:       poolCancel,
		cfg:          cfg,
		size:         cfg.CodexWarmWorkers,
		queueTimeout: queueTimeout,
		idleWorkers:  make(chan *CodexWorker, cfg.CodexWarmWorkers),
	}

	// Verify auth status on initialization
	if auth, err := loadCodexAuth(cfg); err == nil {
		acct := auth.Tokens.AccountID
		if acct == "" {
			acct = "active"
		}
		log.Printf("[codex-pool] Auth token verified for ChatGPT account: %s", acct)
	} else {
		log.Printf("[codex-pool] WARNING: Auth verification failed on startup: %v", err)
	}

	fmt.Printf("[codex-pool] Initializing %d standby workers for Codex (ChatGPT backend)...\n", cfg.CodexWarmWorkers)
	pool.Start()
	globalCodexPool = pool
}

// getCodexWorkerPool returns the active global worker pool (or nil if disabled).
func getCodexWorkerPool() *CodexWorkerPool {
	globalCodexPoolMu.RLock()
	defer globalCodexPoolMu.RUnlock()
	return globalCodexPool
}

// IsEnabled returns true if the pool is active and ready.
func (p *CodexWorkerPool) IsEnabled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.size > 0
}

// Size returns the configured worker capacity.
func (p *CodexWorkerPool) Size() int {
	if p == nil {
		return 0
	}
	return p.size
}

// ActiveCount returns how many workers are currently processing requests.
func (p *CodexWorkerPool) ActiveCount() int {
	if p == nil {
		return 0
	}
	return int(p.activeCount.Load())
}

// Start populates the pool with standby workers.
func (p *CodexWorkerPool) Start() {
	for i := 1; i <= p.size; i++ {
		w := &CodexWorker{
			id:        i,
			client:    newCodexWorkerClient(),
			createdAt: time.Now(),
		}
		select {
		case p.idleWorkers <- w:
			fmt.Printf("[codex-pool] Worker #%d ready on standby (idle/ready count: %d/%d)\n", w.id, len(p.idleWorkers), p.size)
		default:
		}
	}
}

// Stop shuts down the worker pool.
func (p *CodexWorkerPool) Stop() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Unlock()

	// Drain idle workers and close idle connections
	for {
		select {
		case w := <-p.idleWorkers:
			if w != nil && w.client != nil {
				w.client.CloseIdleConnections()
			}
		default:
			return
		}
	}
}

// Acquire gets an idle standby worker from the pool or waits until queue timeout.
// It returns the worker and a release function to be called when processing finishes.
func (p *CodexWorkerPool) Acquire(ctx context.Context) (*CodexWorker, func(), error) {
	if !p.IsEnabled() {
		return nil, func() {}, errors.New("codex worker pool is disabled")
	}

	select {
	case w, ok := <-p.idleWorkers:
		if !ok || w == nil {
			return nil, func() {}, errors.New("codex worker pool closed")
		}
		p.activeCount.Add(1)
		t0 := time.Now()
		var released atomic.Bool
		release := func() {
			if released.CompareAndSwap(false, true) {
				durMs := time.Since(t0).Milliseconds()
				turns := w.turnCount.Add(1)
				act := p.activeCount.Add(-1)
				log.Printf("[codex-pool] Request served by worker #%d in %dms (active: %d/%d, turn: %d)",
					w.id, durMs, act, p.size, turns)
				p.idleWorkers <- w
			}
		}
		return w, release, nil

	case <-time.After(p.queueTimeout):
		return nil, func() {}, ErrCodexQueueTimeout

	case <-ctx.Done():
		return nil, func() {}, ctx.Err()
	}
}

// AcquireCodexWorker is a helper to acquire a worker if the global pool is active.
// If the pool is disabled or nil, it returns (nil, noop, nil) so callers can proceed directly with http.DefaultClient.
func AcquireCodexWorker(ctx context.Context) (*CodexWorker, func(), error) {
	pool := getCodexWorkerPool()
	if pool == nil || !pool.IsEnabled() {
		return nil, func() {}, nil
	}
	return pool.Acquire(ctx)
}
