package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Hub struct {
	mu            sync.RWMutex
	clients       map[*websocket.Conn]bool
	workers       map[int]*Worker
	pinnedCommand string
	eventLog      *json.Encoder
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]bool),
		workers: make(map[int]*Worker),
	}
}

func (h *Hub) logEvent(v interface{}) {
	if h.eventLog == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	_ = h.eventLog.Encode(v)
}

func (h *Hub) AddClient(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()
}

func (h *Hub) RemoveClient(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

func (h *Hub) Broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn := range h.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("write error: %v", err)
		}
	}
}

func (h *Hub) HandleClient(conn *websocket.Conn) {
	h.AddClient(conn)
	defer func() {
		h.RemoveClient(conn)
		conn.Close()
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg IncomingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "invalid JSON"})
			continue
		}

		switch msg.Type {
		case "start":
			h.handleStart(conn, msg)
		case "list":
			h.handleList(conn, msg)
		case "stop":
			h.handleStop(conn, msg)
		case "replay":
			h.handleReplay(conn, msg)
		default:
			sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "unknown message type"})
		}
	}
}

func (h *Hub) handleStart(conn *websocket.Conn, msg IncomingMessage) {
	command := msg.Command
	if h.pinnedCommand != "" {
		if command != "" && command != h.pinnedCommand {
			sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: fmt.Sprintf("command must be %q (pinned)", h.pinnedCommand)})
			return
		}
		command = h.pinnedCommand
	}
	if command == "" {
		sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "command is required"})
		return
	}
	args := msg.Args
	if args == nil {
		args = []string{}
	}
	w, err := StartWorker(h, command, args)
	if err != nil {
		sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: err.Error()})
		return
	}
	h.mu.Lock()
	h.workers[w.PID] = w
	h.mu.Unlock()
	log.Printf("started worker pid=%d cmd=%s", w.PID, msg.Command)
	started := StartedMessage{Type: "started", ID: msg.ID, PID: w.PID, TS: w.StartedAt}
	sendJSON(conn, started)
	h.logEvent(started)
}

func (h *Hub) handleList(conn *websocket.Conn, msg IncomingMessage) {
	var since *time.Time
	if msg.Since != "" {
		t, err := time.Parse(time.RFC3339, msg.Since)
		if err != nil {
			sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "invalid since timestamp"})
			return
		}
		since = &t
	}

	h.mu.RLock()
	var workers []WorkerInfo
	for _, w := range h.workers {
		info := w.Info()
		if since != nil {
			if info.LastEventAt == nil || info.LastEventAt.Before(*since) {
				continue
			}
		}
		workers = append(workers, info)
	}
	h.mu.RUnlock()

	if workers == nil {
		workers = []WorkerInfo{}
	}
	sendJSON(conn, WorkersMessage{Type: "workers", ID: msg.ID, Workers: workers})
}

func (h *Hub) handleStop(conn *websocket.Conn, msg IncomingMessage) {
	h.mu.RLock()
	w, ok := h.workers[msg.PID]
	h.mu.RUnlock()
	if !ok {
		sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "worker not found"})
		return
	}
	if w.State != "running" {
		sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "worker already exited"})
		return
	}
	w.Stop()
	log.Printf("stopping worker pid=%d", msg.PID)
}

func (h *Hub) handleReplay(conn *websocket.Conn, msg IncomingMessage) {
	h.mu.RLock()
	w, ok := h.workers[msg.PID]
	h.mu.RUnlock()
	if !ok {
		sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "worker not found"})
		return
	}

	var since *time.Time
	if msg.Since != "" {
		t, err := time.Parse(time.RFC3339, msg.Since)
		if err != nil {
			sendJSON(conn, ErrorMessage{Type: "error", ID: msg.ID, Message: "invalid since timestamp"})
			return
		}
		since = &t
	}

	events := w.getEvents(since)
	for _, evt := range events {
		switch evt.Type {
		case "output":
			sendJSON(conn, OutputMessage{Type: "output", PID: evt.PID, Stream: evt.Stream, Data: evt.Data, TS: evt.TS})
		case "exited":
			ec := 0
			if evt.ExitCode != nil {
				ec = *evt.ExitCode
			}
			sendJSON(conn, ExitedMessage{Type: "exited", PID: evt.PID, ExitCode: ec, TS: evt.TS})
		}
	}
}

func sendJSON(conn *websocket.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	conn.WriteMessage(websocket.TextMessage, data)
}
