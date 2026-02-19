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

type Worker struct {
	PID       int
	Command   string
	Args      []string
	State     string // "running" or "exited"
	StartedAt time.Time
	ExitedAt  *time.Time
	ExitCode  *int

	cmd    *exec.Cmd
	mu     sync.Mutex
	events []Event
	hub    *Hub
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

func (w *Worker) Info() WorkerInfo {
	info := WorkerInfo{
		PID:         w.PID,
		Command:     w.Command,
		Args:        w.Args,
		State:       w.State,
		StartedAt:   w.StartedAt,
		ExitedAt:    w.ExitedAt,
		ExitCode:    w.ExitCode,
		LastEventAt: w.lastEventAt(),
	}
	if info.Args == nil {
		info.Args = []string{}
	}
	return info
}

func StartWorker(hub *Hub, command string, args []string) (*Worker, error) {
	cmd := exec.Command(command, args...)
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
		Command:   command,
		Args:      args,
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
			evt := Event{Type: "output", PID: w.PID, Stream: stream, Data: line, TS: now}
			w.addEvent(evt)
			hub.Broadcast(OutputMessage{Type: "output", PID: w.PID, Stream: stream, Data: line, TS: now})
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
		w.mu.Unlock()

		evt := Event{Type: "exited", PID: w.PID, ExitCode: &exitCode, TS: now}
		w.addEvent(evt)
		hub.Broadcast(ExitedMessage{Type: "exited", PID: w.PID, ExitCode: exitCode, TS: now})
		log.Printf("worker pid=%d exited code=%d", w.PID, exitCode)
	}()

	return w, nil
}

func (w *Worker) Stop() {
	if w.State != "running" || w.cmd.Process == nil {
		return
	}
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		time.Sleep(5 * time.Second)
		if w.State == "running" {
			_ = w.cmd.Process.Kill()
		}
	}()
}
