package apiserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type event struct {
	Type string `json:"type"`
	Time string `json:"time"`
	Data any    `json:"data,omitempty"`
}

type eventHub struct {
	mu   sync.Mutex
	subs map[chan event]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[chan event]struct{}{}}
}

func (h *eventHub) publish(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (h *eventHub) subscribe() chan event {
	ch := make(chan event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

func (s *Server) publish(typ string, data any) {
	if s.events == nil {
		return
	}
	s.events.publish(event{Type: typ, Time: time.Now().Format(time.RFC3339), Data: data})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream_unsupported", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
