package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrAgyQueueTimeout is returned when all warm workers are busy and the queue timeout expires.
var ErrAgyQueueTimeout = errors.New("agy warm worker pool queue timeout exceeded")

// AgyWorker represents a persistent warm agy process running in stream-json mode.
type AgyWorker struct {
	id        int
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	model     string
	createdAt time.Time
	turnCount int
	mu        sync.Mutex
	closed    bool
}

// AgyWorkerPool manages a pool of warm background agy workers.
type AgyWorkerPool struct {
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          config
	size         int
	model        string
	queueTimeout time.Duration
	maxTurns     int
	idleWorkers  chan *AgyWorker
	mu           sync.Mutex
	nextID       atomic.Int64
	closed       bool
	activeCount  atomic.Int64
}

var (
	globalAgyPoolMu sync.RWMutex
	globalAgyPool   *AgyWorkerPool
)

// canonicalAgyModel normalizes model identifiers so "Gemini 3.8 Flash (Low)" and "gemini-3.8-flash-low" match.
func canonicalAgyModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	s = strings.ReplaceAll(s, "3.5", "3.6")
	return s
}

// initAgyWorkerPool initializes the global warm worker pool if configured.
func initAgyWorkerPool(cfg config) {
	globalAgyPoolMu.Lock()
	defer globalAgyPoolMu.Unlock()

	if globalAgyPool != nil {
		globalAgyPool.Stop()
		globalAgyPool = nil
	}

	if cfg.AgyWarmWorkers <= 0 {
		return
	}

	model := cfg.AgyWorkerModel
	if model == "" {
		model = "gemini-3.8-flash-medium"
	}
	model = normalizeAgyModelName(model)

	queueTimeout := cfg.AgyQueueTimeout
	if queueTimeout <= 0 {
		queueTimeout = 15 * time.Second
	}

	maxTurns := cfg.AgyWorkerMaxTurns
	if maxTurns <= 0 {
		maxTurns = 50
	}

	poolCtx, poolCancel := context.WithCancel(context.Background())

	pool := &AgyWorkerPool{
		ctx:          poolCtx,
		cancel:       poolCancel,
		cfg:          cfg,
		size:         cfg.AgyWarmWorkers,
		model:        model,
		queueTimeout: queueTimeout,
		maxTurns:     maxTurns,
		idleWorkers:  make(chan *AgyWorker, cfg.AgyWarmWorkers),
	}

	fmt.Printf("[agy-pool] Initializing %d warm workers for model %q...\n", cfg.AgyWarmWorkers, model)
	pool.Start()
	globalAgyPool = pool
}

// getAgyWorkerPool returns the active global worker pool (or nil if disabled).
func getAgyWorkerPool() *AgyWorkerPool {
	globalAgyPoolMu.RLock()
	defer globalAgyPoolMu.RUnlock()
	return globalAgyPool
}

// IsEnabled returns true if the pool is active and ready.
func (p *AgyWorkerPool) IsEnabled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.size > 0
}

// Start launches the initial set of warm workers in background goroutines.
func (p *AgyWorkerPool) Start() {
	for i := 0; i < p.size; i++ {
		go p.replenishOne()
	}
}

// Stop shuts down all warm workers in the pool.
func (p *AgyWorkerPool) Stop() {
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

	// Drain and close idle workers
	for {
		select {
		case w := <-p.idleWorkers:
			if w != nil {
				w.Close()
			}
		default:
			return
		}
	}
}

// replenishOne warms up a single worker and adds it to the idle channel.
func (p *AgyWorkerPool) replenishOne() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	poolCtx := p.ctx
	if poolCtx == nil {
		poolCtx = context.Background()
	}
	p.mu.Unlock()

	id := int(p.nextID.Add(1))
	w, err := spawnAgyWorker(poolCtx, p.cfg, id, p.model)
	if err != nil {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if !closed {
			time.Sleep(2 * time.Second)
			go p.replenishOne()
		}
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		w.Close()
		return
	}

	select {
	case p.idleWorkers <- w:
		p.activeCount.Add(1)
		fmt.Printf("[agy-pool] Worker #%d ready in pool (idle/ready count: %d/%d)\n", id, len(p.idleWorkers), p.size)
	default:
		w.Close()
	}
}

// Acquire gets an idle warm worker from the pool or waits until queue timeout.
func (p *AgyWorkerPool) Acquire(ctx context.Context) (*AgyWorker, error) {
	if !p.IsEnabled() {
		return nil, errors.New("agy worker pool is disabled")
	}

	select {
	case w, ok := <-p.idleWorkers:
		if !ok || w == nil {
			return nil, errors.New("agy worker pool closed")
		}
		go p.replenishOne()
		return w, nil

	case <-time.After(p.queueTimeout):
		return nil, ErrAgyQueueTimeout

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Release closes or recycles a worker after turn completion.
func (p *AgyWorkerPool) Release(w *AgyWorker) {
	if w == nil {
		return
	}
	p.activeCount.Add(-1)
	w.Close()
}

// Execute runs a prompt on an acquired warm worker and returns the result.
func (p *AgyWorkerPool) Execute(ctx context.Context, prompt, requestedModel string) (agyResult, error) {
	if requestedModel != "" && canonicalAgyModel(requestedModel) != canonicalAgyModel(p.model) {
		return agyResult{}, fmt.Errorf("model %q does not match pool model %q", requestedModel, p.model)
	}

	w, err := p.Acquire(ctx)
	if err != nil {
		return agyResult{}, err
	}
	defer p.Release(w)

	res, err := w.Execute(ctx, prompt)
	if err == nil && res.Ok {
		fmt.Printf("[agy-pool] Request served by warm worker #%d in %dms\n", w.id, res.DurationMs)
	}
	return res, err
}

// spawnAgyWorker starts a persistent agy process in stream-json mode and waits for init.
func spawnAgyWorker(poolCtx context.Context, cfg config, id int, model string) (*AgyWorker, error) {
	if poolCtx == nil {
		poolCtx = context.Background()
	}
	bin := resolveAgyCLIPath(cfg)
	if bin == "" {
		return nil, errors.New("agy binary not found")
	}

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"-p=",
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(poolCtx, bin, args...)
	cmd.Dir = os.TempDir()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe error: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe error: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("failed to start agy worker: %w", err)
	}

	reader := bufio.NewReader(stdout)

	initChan := make(chan error, 1)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				initChan <- fmt.Errorf("worker exited before init: %w", err)
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var evt struct {
				Event string `json:"event"`
				Error string `json:"error,omitempty"`
			}
			if err := json.Unmarshal([]byte(line), &evt); err == nil {
				if evt.Event == "init" {
					initChan <- nil
					return
				}
				if evt.Event == "result" && evt.Error != "" {
					initChan <- fmt.Errorf("worker startup error: %s", evt.Error)
					return
				}
			}
		}
	}()

	select {
	case err := <-initChan:
		if err != nil {
			stdin.Close()
			stdout.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return nil, err
		}
	case <-time.After(30 * time.Second):
		stdin.Close()
		stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, errors.New("timed out waiting for worker init event")
	case <-poolCtx.Done():
		stdin.Close()
		stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, poolCtx.Err()
	}

	return &AgyWorker{
		id:        id,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    reader,
		model:     model,
		createdAt: time.Now(),
	}, nil
}

// Execute sends a single turn prompt to the worker stdin and waits for the result event.
func (w *AgyWorker) Execute(ctx context.Context, prompt string) (agyResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed || w.cmd.Process == nil {
		return agyResult{}, errors.New("worker is closed")
	}

	w.turnCount++
	t0 := time.Now()

	msg := map[string]any{
		"event": "user",
		"message": map[string]any{
			"content": prompt,
		},
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return agyResult{}, fmt.Errorf("marshal stream input error: %w", err)
	}

	if _, err := w.stdin.Write(append(payload, '\n')); err != nil {
		return agyResult{}, fmt.Errorf("failed to write to worker stdin: %w", err)
	}

	type streamResult struct {
		Event  string `json:"event"`
		Result struct {
			Status          string  `json:"status"`
			Response        string  `json:"response"`
			Error           string  `json:"error"`
			DurationSeconds float64 `json:"duration_seconds"`
			Usage           struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"result"`
	}

	resultChan := make(chan agyResult, 1)
	errChan := make(chan error, 1)

	go func() {
		for {
			line, err := w.stdout.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					errChan <- errors.New("worker stdout closed unexpectedly")
				} else {
					errChan <- err
				}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			var sr streamResult
			if err := json.Unmarshal([]byte(line), &sr); err == nil {
				if sr.Event == "result" {
					durMs := int64(sr.Result.DurationSeconds * 1000)
					if durMs == 0 {
						durMs = time.Since(t0).Milliseconds()
					}
					if sr.Result.Status == "ERROR" {
						errText := sr.Result.Error
						if errText == "" {
							errText = "agy execution failed"
						}
						resultChan <- agyResult{
							Ok:         false,
							Error:      errText,
							DurationMs: durMs,
							Model:      w.model,
						}
					} else {
						resultChan <- agyResult{
							Ok:         true,
							Response:   sr.Result.Response,
							DurationMs: durMs,
							Model:      w.model,
						}
					}
					return
				}
			}
		}
	}()

	select {
	case res := <-resultChan:
		return res, nil
	case err := <-errChan:
		return agyResult{}, err
	case <-ctx.Done():
		return agyResult{}, ctx.Err()
	}
}

// Close terminates the worker process.
func (w *AgyWorker) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return
	}
	w.closed = true

	if w.stdin != nil {
		_ = w.stdin.Close()
	}
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
		_ = w.cmd.Wait()
	}
}

// resolveAgyCLIPath finds the agy executable across config, environment, sibling, and system paths.
func resolveAgyCLIPath(cfg config) string {
	if cli := strings.TrimSpace(cfg.AgyCLI); cli != "" {
		if _, err := os.Stat(cli); err == nil {
			return cli
		}
	}
	if env := strings.TrimSpace(os.Getenv("AGYJ_AGY_BIN")); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.exe"
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), name)
		if st, statErr := os.Stat(sibling); statErr == nil && !st.IsDir() {
			return sibling
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, cand := range []string{
		"/usr/local/bin/agy",
		"/root/.local/bin/agy",
		"/usr/bin/agy",
	} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return name
}

// runAgyStreamJSON executes a single prompt non-interactively using agy's stream-json protocol
// over stdin and stdout. This completely avoids OS command-line argument limits (MAX_ARG_STRLEN / E2BIG)
// for large prompts and supports directories passed via --add-dir.
func runAgyStreamJSON(ctx context.Context, cfg config, prompt, model string, addDirs []string) (agyResult, error) {
	bin := resolveAgyCLIPath(cfg)
	if bin == "" {
		return agyResult{}, errors.New("agy binary not found")
	}

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"-p=",
	}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	for _, d := range addDirs {
		if d = strings.TrimSpace(d); d != "" {
			args = append(args, "--add-dir", d)
		}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = os.TempDir()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return agyResult{}, fmt.Errorf("stdin pipe error: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return agyResult{}, fmt.Errorf("stdout pipe error: %w", err)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return agyResult{}, fmt.Errorf("failed to start agy: %w", err)
	}

	t0 := time.Now()
	reader := bufio.NewReader(stdout)

	// Wait for init event
	initChan := make(chan error, 1)
	go func() {
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				initChan <- fmt.Errorf("worker exited before init: %w", rerr)
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var evt struct {
				Event string `json:"event"`
				Error string `json:"error,omitempty"`
			}
			if err := json.Unmarshal([]byte(line), &evt); err == nil {
				if evt.Event == "init" {
					initChan <- nil
					return
				}
				if evt.Event == "result" && evt.Error != "" {
					initChan <- fmt.Errorf("worker startup error: %s", evt.Error)
					return
				}
			}
		}
	}()

	select {
	case err := <-initChan:
		if err != nil {
			stdin.Close()
			stdout.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
			return agyResult{}, fmt.Errorf("%w [stderr: %s]", err, truncateString(strings.TrimSpace(stderr.String()), 300))
		}
	case <-time.After(30 * time.Second):
		stdin.Close()
		stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return agyResult{}, errors.New("timed out waiting for agy init event")
	case <-ctx.Done():
		stdin.Close()
		stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return agyResult{}, ctx.Err()
	}

	// Send user turn via stdin
	msg := map[string]any{
		"event": "user",
		"message": map[string]any{
			"content": prompt,
		},
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		stdin.Close()
		stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return agyResult{}, fmt.Errorf("marshal stream input error: %w", err)
	}

	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		stdin.Close()
		stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return agyResult{}, fmt.Errorf("failed to write to agy stdin: %w", err)
	}

	type streamResult struct {
		Event  string `json:"event"`
		Result struct {
			Status          string  `json:"status"`
			Response        string  `json:"response"`
			Error           string  `json:"error"`
			DurationSeconds float64 `json:"duration_seconds"`
			Usage           struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"result"`
	}

	resultChan := make(chan agyResult, 1)
	errChan := make(chan error, 1)

	go func() {
		defer stdin.Close()
		defer stdout.Close()
		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				if rerr == io.EOF {
					errChan <- errors.New("agy stdout closed unexpectedly")
				} else {
					errChan <- rerr
				}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			var sr streamResult
			if err := json.Unmarshal([]byte(line), &sr); err == nil {
				if sr.Event == "result" {
					durMs := int64(sr.Result.DurationSeconds * 1000)
					if durMs == 0 {
						durMs = time.Since(t0).Milliseconds()
					}
					if sr.Result.Status == "ERROR" {
						errText := sr.Result.Error
						if errText == "" {
							errText = "agy execution failed"
						}
						resultChan <- agyResult{
							Ok:         false,
							Error:      errText,
							DurationMs: durMs,
							Model:      model,
						}
					} else {
						resultChan <- agyResult{
							Ok:         true,
							Response:   sr.Result.Response,
							DurationMs: durMs,
							Model:      model,
						}
					}
					return
				}
			}
		}
	}()

	select {
	case res := <-resultChan:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return res, nil
	case err := <-errChan:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return agyResult{}, fmt.Errorf("%w [stderr: %s]", err, truncateString(strings.TrimSpace(stderr.String()), 300))
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		return agyResult{}, ctx.Err()
	}
}
