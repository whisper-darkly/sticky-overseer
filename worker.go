package main

import (
	"bufio"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const ringBufferSize = 100

type WorkerConfig struct {
	TaskID  string
	Command string
	Args    []string
}

type Worker struct {
	PID       int
	TaskID    string
	Command   string
	Args      []string
	State     string // "running" or "exited"
	StartedAt time.Time
	ExitedAt  *time.Time
	ExitCode  *int

	cmd            *exec.Cmd
	mu             sync.Mutex
	events         []Event
	hub            *Hub
	intentionalStop bool
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

func StartWorker(hub *Hub, cfg WorkerConfig) (*Worker, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
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
		State:     "running",
		StartedAt: time.Now().UTC(),
		cmd:       cmd,
		hub:       hub,
	}

	var wg sync.WaitGroup
	wg.Add(2)

	scan := func(scanner *bufio.Scanner, stream string) {
		defer wg.Done()
		for scanner.Scan() {
			now := time.Now().UTC()
			line := scanner.Text()
			evt := Event{Type: "output", TaskID: w.TaskID, PID: w.PID, Stream: stream, Data: line, TS: now}
			w.addEvent(evt)
			msg := OutputMessage{Type: "output", TaskID: w.TaskID, PID: w.PID, Stream: stream, Data: line, TS: now}
			hub.Broadcast(msg)
			hub.logEvent(msg)
		}
	}

	go scan(bufio.NewScanner(stdout), "stdout")
	go scan(bufio.NewScanner(stderr), "stderr")

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
		w.State = "exited"
		w.ExitedAt = &now
		w.ExitCode = &exitCode
		intentional := w.intentionalStop
		w.mu.Unlock()

		evt := Event{Type: "exited", TaskID: w.TaskID, PID: w.PID, ExitCode: &exitCode, Intentional: intentional, TS: now}
		w.addEvent(evt)
		log.Printf("worker task=%s pid=%d exited code=%d intentional=%v", w.TaskID, w.PID, exitCode, intentional)
		hub.onWorkerExited(w, exitCode, intentional, now)
	}()

	return w, nil
}

func (w *Worker) Stop() {
	w.mu.Lock()
	if w.State != "running" || w.cmd.Process == nil {
		w.mu.Unlock()
		return
	}
	w.intentionalStop = true
	w.mu.Unlock()

	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		time.Sleep(5 * time.Second)
		w.mu.Lock()
		running := w.State == "running"
		w.mu.Unlock()
		if running {
			_ = w.cmd.Process.Kill()
		}
	}()
}
