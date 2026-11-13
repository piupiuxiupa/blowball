package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SSE response headers. X-Accel-Buffering disables proxy buffering in nginx and
// similar reverse proxies so events reach the client promptly.
var sseHeaders = map[string]string{
	"Content-Type":      "text/event-stream",
	"Cache-Control":     "no-cache",
	"Connection":        "keep-alive",
	"X-Accel-Buffering": "no",
}

// PrepareSSE sets the standard SSE response headers on w, writes the 200
// status line, and flushes the header block. It is the exported entry point
// for SSE writers outside this package (the run-event subscribers of the
// turn-detach-resume capability); extra headers (e.g. X-Run-Id) must be set
// on w.Header() BEFORE this call so they ride with the response headers.
func PrepareSSE(w http.ResponseWriter) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("stream: http.ResponseWriter does not implement http.Flusher")
	}
	for k, v := range sseHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return nil
}

// WriteSSEFrame writes one SSE frame for e, optionally preceded by an `id:`
// line carrying id (the run event log's Redis Stream entry id, replayed by
// native EventSource clients as Last-Event-ID on reconnect). Callers flush.
func WriteSSEFrame(w io.Writer, e StreamEvent, id string) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("stream: marshal event: %w", err)
	}
	if id != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
			return fmt.Errorf("stream: write id: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, payload); err != nil {
		return fmt.Errorf("stream: write event: %w", err)
	}
	return nil
}

// WriteSSE consumes events from h.Events() and writes them to w in SSE format:
//
//	event: <type>\n
//	data: <json>\n
//	\n
//
// One event is written per StreamEvent. The writer is flushed after each event.
// WriteSSE returns when ctx is cancelled, when the hub is closed (Done()
// signaled and events drained), or on a write error. The returned error is
// ctx.Err() if the context was cancelled, nil on clean hub shutdown, or the
// underlying write error.
//
// w must implement http.Flusher; otherwise an error is returned without writing
// anything. Headers are set before the first write.
func WriteSSE(ctx context.Context, w http.ResponseWriter, h *Hub) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("stream: http.ResponseWriter does not implement http.Flusher")
	}

	// Headers must be set before the first write so they are sent with the
	// response headers rather than being ignored after the body has begun.
	for k, v := range sseHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := h.Events()
	done := h.Done()
	for {
		// Drain buffered events first so a hub that is closed with events still
		// in flight flushes them before we return. Only when the buffer is empty
		// do we honor ctx/Done shutdown signals.
		select {
		case e := <-events:
			if err := writeEvent(w, e); err != nil {
				return err
			}
			flusher.Flush()
		default:
			// Buffer empty; block until something happens.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				return nil
			case e, ok := <-events:
				if !ok {
					return nil
				}
				if err := writeEvent(w, e); err != nil {
					return err
				}
				flusher.Flush()
			}
		}
	}
}

// writeEvent serializes a single StreamEvent to the SSE wire format (no id
// line — hub-sourced events carry no stream cursor). The event payload is
// JSON-encoded with sorted map keys (json.Marshal default) so output is
// deterministic for a given event.
func writeEvent(w http.ResponseWriter, e StreamEvent) error {
	return WriteSSEFrame(w, e, "")
}
