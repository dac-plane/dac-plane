package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AssahBismarkabah/dac-plane/internal/application"
	"github.com/AssahBismarkabah/dac-plane/internal/identity"
	"github.com/AssahBismarkabah/dac-plane/internal/ids"
)

func (a *API) taskEvents(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	task, err := pathID[ids.Task](r, "task")
	if err != nil {
		return err
	}
	return a.events(w, r, actor, task)
}

func (a *API) tenantEvents(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext) error {
	return a.events(w, r, actor, ids.TaskID{})
}

// events serves one stream as Server-Sent Events when the client accepts
// text/event-stream, and as a JSON page otherwise. The cursor comes from
// Last-Event-ID or the after query parameter. A malformed cursor is reset,
// like one from another generation, rather than refused.
func (a *API) events(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext, task ids.TaskID) error {
	limit := defaultPageSize
	if text := r.URL.Query().Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > application.MaxEventPage {
			return fmt.Errorf("%w: limit must be 1 to %d", application.ErrInvalidCommand, application.MaxEventPage)
		}
		limit = parsed
	}
	cursorText := r.Header.Get("Last-Event-ID")
	if cursorText == "" {
		cursorText = r.URL.Query().Get("after")
	}
	cursor, malformed := application.EventCursor{}, false
	if cursorText != "" {
		parsed, err := parseCursor(cursorText)
		cursor, malformed = parsed, err != nil
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		page, err := a.page(r.Context(), actor, task, cursor, malformed, limit)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, eventPageBody(page))
		return nil
	}
	return a.stream(w, r, actor, task, cursor, malformed, limit)
}

// page reads one page; a malformed cursor yields an immediate reset to the
// current generation.
func (a *API) page(ctx context.Context, actor identity.AuthorizationContext, task ids.TaskID, cursor application.EventCursor, malformed bool, limit int) (application.EventPage, error) {
	if malformed {
		// A cursor no generation can match forces the reset path, which also
		// checks the caller may read the stream.
		cursor = application.EventCursor{Generation: ^uint64(0)}
	}
	return application.ListEvents(ctx, a.pool, actor, task, cursor, limit)
}

// stream writes Server-Sent Events until the client disconnects or the
// stream lifetime ends. The first read happens before any byte is written,
// so an unreadable stream is still an ordinary problem response.
func (a *API) stream(w http.ResponseWriter, r *http.Request, actor identity.AuthorizationContext, task ids.TaskID, cursor application.EventCursor, malformed bool, limit int) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("httpapi: response writer cannot stream")
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.options.StreamLifetime)
	defer cancel()
	page, err := a.page(ctx, actor, task, cursor, malformed, limit)
	if err != nil {
		return err
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "retry: %d\n\n", a.options.PollInterval.Milliseconds())
	flusher.Flush()

	poll := time.NewTicker(a.options.PollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(a.options.Heartbeat)
	defer heartbeat.Stop()
	for {
		if wrote, err := writePage(w, page); err != nil {
			return nil
		} else if wrote {
			flusher.Flush()
			heartbeat.Reset(a.options.Heartbeat)
		}
		cursor = page.Next
		full := len(page.Events) == limit
		// The page is written: nothing below may send it again.
		page = application.EventPage{Next: cursor}
		if full {
			// More may be waiting; read again without waiting for the tick.
			if page, err = a.page(ctx, actor, task, cursor, false, limit); err != nil {
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
			continue
		case <-poll.C:
		}
		// Each read re-checks the principal and its tenant-reader permission,
		// so a revocation applies to an open stream at the next poll.
		if page, err = a.page(ctx, actor, task, cursor, false, limit); err != nil {
			return nil
		}
	}
}

// writePage writes a page as SSE: a reset event, or one lifecycle event per
// record change, each with its cursor as the event id.
func writePage(w http.ResponseWriter, page application.EventPage) (bool, error) {
	if page.Reset {
		data, _ := json.Marshal(map[string]string{"next_cursor": formatCursor(page.Next)})
		_, err := fmt.Fprintf(w, "id: %s\nevent: reset\ndata: %s\n\n", formatCursor(page.Next), data)
		return true, err
	}
	for _, event := range page.Events {
		body := eventBody(page.Next, event)
		data, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: lifecycle\ndata: %s\n\n", body.Cursor, data); err != nil {
			return false, err
		}
	}
	return len(page.Events) > 0, nil
}
