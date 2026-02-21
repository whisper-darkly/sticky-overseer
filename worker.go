package overseer

import (
	"bufio"
	"context"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const ringBufferSize = 100

type WorkerConfig struct {
	TaskID         string
	Command        string
	Args           []string
	IncludeStdout  bool // if false, stdout is drained but not forwarded to callbacks
	IncludeStderr  bool // if false, stderr is drained but not forwarded to callbacks
}

// WorkerCallbacks are injected into each worker at start time, decoupling
// Worker from Hub and enabling isolated unit testing.
type WorkerCallbacks struct {
	OnOutput func(msg *OutputMessage) // receives pointer so callers may stamp Seq before addEvent
	LogEvent func(v any)
	OnExited func(w *Worker, exitCode int, intentional bool, t time.Time)
}

type Worker struct {
	PID       int
	TaskID    string
	Command   string
	Args      []string
	State     WorkerState
	StartedAt time.Time
	ExitedAt  *time.Time
	ExitCode  *int

	cmd             *exec.Cmd
	cancelFn        context.CancelFunc // non-nil for virtual workers; nil for OS-process workers
	mu              sync.Mutex
	events          []Event
	callbacks       WorkerCallbacks
	intentionalStop bool
	stopCh          chan struct{} // signals kill goroutine to proceed/cancel; used by OS-process workers only
}

func (w *Worker) addEvent(e Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.events) >= ringBufferSize {
		w.events = w.events[1:]
	}
	w.events = append(w.events, e)
}

func (w *Worker) getEvents(since *time.Time) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	if since == nil {
		out := make([]Event, len(w.events))
		copy(out, w.events)
		return out
	}
	var out []Event
	for _, e := range w.events {
		if !e.TS.Before(*since) {
			out = append(out, e)
		}
	}
	return out
}

func (w *Worker) lastEventAt() *time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.events) == 0 {
		return nil
	}
	t := w.events[len(w.events)-1].TS
	return &t
}

func StartWorker(cfg WorkerConfig, cb WorkerCallbacks) (*Worker, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)

	// Set process group to enable group signaling — ensures child processes
	// spawned by the command are also signaled on Stop().
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	w := &Worker{
		PID:       cmd.Process.Pid,
		TaskID:    cfg.TaskID,
		Command:   cfg.Command,
		Args:      cfg.Args,
		State:     WorkerRunning,
		StartedAt: time.Now().UTC(),
		cmd:       cmd,
		callbacks: cb,
		stopCh:    make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Larger buffer to handle long output lines (1MB initial, 10MB max).
	const scanBufSize = 1 << 20  // 1MB
	const scanMaxSize = 10 << 20 // 10MB

	// Pipes must always be drained even when output is filtered, to prevent
	// the child process from blocking on a full pipe buffer.
	scan := func(scanner *bufio.Scanner, stream Stream, include bool) {
		defer wg.Done()
		for scanner.Scan() {
			if !include {
				continue
			}
			now := time.Now().UTC()
			line := scanner.Text()
			// Call OnOutput first so it can stamp Seq (and optionally filter);
			// then store the event with the Seq value already set.
			msg := &OutputMessage{Type: "output", TaskID: w.TaskID, PID: w.PID, Stream: stream, Data: line, TS: now}
			w.callbacks.OnOutput(msg)
			evt := Event{Type: "output", TaskID: w.TaskID, PID: w.PID, Stream: stream, Data: line, TS: now, Seq: msg.Seq}
			w.addEvent(evt)
			w.callbacks.LogEvent(*msg)
		}
		// Check for scanner errors after loop — large lines (>scanMaxSize) cause
		// silent failures without this check.
		if err := scanner.Err(); err != nil {
			log.Printf("worker task=%s stream=%s scanner error: %v", cfg.TaskID, stream, err)
			w.mu.Lock()
			if w.State == WorkerRunning {
				w.State = WorkerExited
			}
			w.mu.Unlock()
		}
	}

	// Create scanners and set large buffer BEFORE scanning.
	stdoutScanner := bufio.NewScanner(stdout)
	stdoutScanner.Buffer(make([]byte, scanBufSize), scanMaxSize)
	go scan(stdoutScanner, StreamStdout, cfg.IncludeStdout)

	stderrScanner := bufio.NewScanner(stderr)
	stderrScanner.Buffer(make([]byte, scanBufSize), scanMaxSize)
	go scan(stderrScanner, StreamStderr, cfg.IncludeStderr)

	go func() {
		wg.Wait()
		err := cmd.Wait()
		now := time.Now().UTC()
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		w.mu.Lock()
		w.State = WorkerExited
		w.ExitedAt = &now
		w.ExitCode = &exitCode
		intentional := w.intentionalStop
		w.mu.Unlock()

		evt := Event{Type: "exited", TaskID: w.TaskID, PID: w.PID, ExitCode: &exitCode, Intentional: intentional, TS: now}
		w.addEvent(evt)
		log.Printf("worker task=%s pid=%d exited code=%d intentional=%v", w.TaskID, w.PID, exitCode, intentional)
		w.callbacks.OnExited(w, exitCode, intentional, now)
	}()

	return w, nil
}

func (w *Worker) Stop() {
	w.mu.Lock()
	if w.State != WorkerRunning {
		w.mu.Unlock()
		return
	}
	w.intentionalStop = true

	// Virtual worker: cancel context and return — no process to signal.
	if w.cmd == nil {
		cancelFn := w.cancelFn
		w.mu.Unlock()
		if cancelFn != nil {
			cancelFn()
		}
		return
	}

	if w.cmd.Process == nil {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	// Send SIGTERM to entire process group (negative PID) so child processes
	// spawned by the command are also signaled, preventing orphans.
	pgid, err := syscall.Getpgid(w.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = w.cmd.Process.Signal(syscall.SIGTERM)
	}

	// Cancellable kill goroutine: wait 5s or until stopCh is closed.
	go func() {
		select {
		case <-time.After(5 * time.Second):
			// 5 seconds elapsed, escalate to SIGKILL.
		case <-w.stopCh:
			// Stop was signalled — cancel the escalation.
			return
		}

		w.mu.Lock()
		running := w.State == WorkerRunning
		w.mu.Unlock()

		if running {
			pgid, err := syscall.Getpgid(w.cmd.Process.Pid)
			if err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				_ = w.cmd.Process.Kill()
			}
		}
	}()

	// Close stopCh so the kill goroutine exits immediately if Stop is called
	// multiple times or during shutdown.
	close(w.stopCh)
}

// VirtualWorkerFunc is the goroutine body for a virtual (non-process) worker.
// ctx is cancelled when Stop() is called on the worker.
// send emits a line of output to the task's subscribers.
// The return value is used as the exit code (0 = success).
type VirtualWorkerFunc func(ctx context.Context, send func(stream Stream, line string)) int

// StartVirtualWorker creates a Worker backed by a Go goroutine rather than an
// OS process. Useful for drivers that orchestrate Go-native work (e.g. file
// conversion pipelines, HTTP fetches, directory watchers) without spawning a
// subprocess.
//
// Stop() cancels the context passed to fn, signalling the function to exit.
// The worker exits when fn returns; the return value becomes the exit code.
func StartVirtualWorker(taskID string, fn VirtualWorkerFunc, cb WorkerCallbacks) (*Worker, error) {
	ctx, cancel := context.WithCancel(context.Background())

	w := &Worker{
		PID:       0, // no OS process
		TaskID:    taskID,
		State:     WorkerRunning,
		StartedAt: time.Now().UTC(),
		callbacks: cb,
		stopCh:    make(chan struct{}),
		cancelFn:  cancel,
	}

	send := func(stream Stream, line string) {
		now := time.Now().UTC()
		msg := &OutputMessage{
			Type: "output", TaskID: taskID, PID: 0,
			Stream: stream, Data: line, TS: now,
		}
		cb.OnOutput(msg)
		evt := Event{Type: "output", TaskID: taskID, PID: 0, Stream: stream, Data: line, TS: now, Seq: msg.Seq}
		w.addEvent(evt)
		cb.LogEvent(*msg)
	}

	go func() {
		exitCode := fn(ctx, send)
		cancel() // no-op if already cancelled by Stop()
		now := time.Now().UTC()
		w.mu.Lock()
		w.State = WorkerExited
		w.ExitedAt = &now
		w.ExitCode = &exitCode
		intentional := w.intentionalStop
		w.mu.Unlock()

		evt := Event{Type: "exited", TaskID: taskID, PID: 0, ExitCode: &exitCode, Intentional: intentional, TS: now}
		w.addEvent(evt)
		log.Printf("virtual worker task=%s exited code=%d intentional=%v", taskID, exitCode, intentional)
		cb.OnExited(w, exitCode, intentional, now)
	}()

	return w, nil
}

// NewWorkerCallbacks constructs a WorkerCallbacks value. Provided for external
// packages (e.g. the exec sub-package) that need to create callbacks in tests.
func NewWorkerCallbacks(
	onOutput func(msg *OutputMessage),
	logEvent func(v any),
	onExited func(w *Worker, exitCode int, intentional bool, t time.Time),
) WorkerCallbacks {
	return WorkerCallbacks{
		OnOutput: onOutput,
		LogEvent: logEvent,
		OnExited: onExited,
	}
}
