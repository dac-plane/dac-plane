// Package exectest provides a fake exec-server for tests.
package exectest

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// Server serves a set of files the way exec-server does.
type Server struct {
	URL   string
	mu    sync.Mutex
	files map[string]string
}

// Set writes a file, given by absolute path.
func (s *Server) Set(path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = content
}

func (s *Server) directory(dir string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	entries := []map[string]any{}
	prefix := strings.TrimRight(dir, "/") + "/"
	for name := range s.files {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		first, _, nested := strings.Cut(rest, "/")
		if !seen[first] {
			seen[first] = true
			entries = append(entries, map[string]any{"fileName": first, "isDirectory": nested, "isFile": !nested})
		}
	}
	return entries
}

func (s *Server) file(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.files[path]
	return content, ok
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var m struct {
			ID     *uint64 `json:"id"`
			Method string  `json:"method"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		if json.Unmarshal(data, &m) != nil || m.ID == nil {
			continue
		}
		uri, _ := url.Parse(m.Params.Path)
		reply := map[string]any{"id": *m.ID}
		switch m.Method {
		case "initialize":
			// A notification before the reply must not be taken for it.
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"method":"process/output","params":{}}`))
			reply["result"] = map[string]any{"sessionId": "session"}
		case "fs/readDirectory":
			reply["result"] = map[string]any{"entries": s.directory(uri.Path)}
		case "fs/readFile":
			content, ok := s.file(uri.Path)
			if !ok {
				reply["error"] = map[string]any{"code": -32000, "message": "not found"}
				break
			}
			reply["result"] = map[string]any{"dataBase64": base64.StdEncoding.EncodeToString([]byte(content))}
		}
		raw, _ := json.Marshal(reply)
		_ = conn.Write(r.Context(), websocket.MessageText, raw)
	}
}

// New starts a server that accepts only token.
func New(t *testing.T, token string, files map[string]string) *Server {
	t.Helper()
	if files == nil {
		files = map[string]string{}
	}
	s := &Server{files: files}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.serve(w, r)
	}))
	t.Cleanup(server.Close)
	s.URL = "ws" + strings.TrimPrefix(server.URL, "http")
	return s
}
