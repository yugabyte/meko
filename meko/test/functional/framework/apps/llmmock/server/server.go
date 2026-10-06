// Package server is the LLM mock: an OpenAI and AWS Bedrock Runtime stand-in for
// the mekoqa test stack. Data plane: /v1/embeddings, /v1/chat/completions (also
// streaming), /v1/responses, /v1/models, and Bedrock Converse, ConverseStream
// and InvokeModel. Control plane: /_mock/*.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const hangDefaultMS = 600_000

var bedrockRe = regexp.MustCompile(`^/model/([^/]+)/(converse|converse-stream|invoke|invoke-with-response-stream)$`)

type Server struct {
	st   state
	logf func(string, ...any)
}

func New(logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{logf: logf}
}

func (s *Server) Serve(ctx context.Context, addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, l)
}

func (s *Server) ServeListener(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	s.logf("llmmock listening on %s", l.Addr())
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	if !strings.HasPrefix(r.URL.Path, "/_mock/health") {
		defer func() { s.logf("%s %s %d", r.Method, r.URL.RequestURI(), sw.status) }()
	}
	s.dispatch(sw, r)
}

func readBody(r *http.Request) obj {
	raw, _ := io.ReadAll(r.Body)
	if len(raw) == 0 {
		return obj{}
	}
	v, err := decode(raw)
	if err != nil {
		return obj{"_unparsed": string(raw)}
	}
	return asObj(v)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if name, ok := strings.CutPrefix(path, "/_mock/"); ok {
		s.control(w, r, name)
		return
	}
	body := readBody(r)
	path = strings.TrimPrefix(path, "/v1")
	post := r.Method == http.MethodPost
	switch {
	case path == "/embeddings" && post:
		s.openaiEmbeddings(w, r, body)
		return
	case path == "/chat/completions" && post:
		s.openaiChat(w, r, body)
		return
	case path == "/responses" && post:
		s.openaiResponses(w, r, body)
		return
	case path == "/models" && r.Method == http.MethodGet:
		data := make([]obj, len(models))
		for i, m := range models {
			data[i] = modelObj(m)
		}
		writeJSON(w, 200, obj{"object": "list", "data": data}, nil)
		return
	case strings.HasPrefix(path, "/models/") && len(path) > len("/models/") && r.Method == http.MethodGet:
		id, _ := url.PathUnescape(path[len("/models/"):])
		writeJSON(w, 200, modelObj(id), nil)
		return
	}
	if m := bedrockRe.FindStringSubmatch(path); m != nil && post {
		model, _ := url.PathUnescape(m[1])
		s.bedrock(w, r, model, m[2], body)
		return
	}
	writeJSON(w, 404, obj{"error": obj{
		"message": fmt.Sprintf("llmmock: no route for %s %s", r.Method, path),
		"type":    "not_found",
	}}, nil)
}

func modelObj(id string) obj {
	return obj{"id": id, "object": "model", "created": 0, "owned_by": "llmmock"}
}

func (s *Server) control(w http.ResponseWriter, r *http.Request, name string) {
	st := &s.st
	query := r.URL.Query()
	var body any = obj{}
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			v, err := decode(raw)
			if err != nil {
				v = obj{"_unparsed": string(raw)}
			}
			body = v
		}
	}
	ok := func() { writeJSON(w, 200, obj{"ok": true}, nil) }
	switch {
	case name == "health":
		ok()
		return
	case name == "state":
		st.mu.Lock()
		defer st.mu.Unlock()
		writeJSON(w, 200, obj{
			"fixtures": len(st.fixtures),
			"scripts":  nonNil(st.scripts),
			"faults":   nonNil(st.faults),
			"calls":    len(st.calls),
		}, nil)
		return
	case name == "stats":
		st.mu.Lock()
		defer st.mu.Unlock()
		routes := map[string]int{}
		for k, v := range st.routes {
			routes[k] = v
		}
		writeJSON(w, 200, obj{"seq": st.seq, "routes": routes}, nil)
		return
	case name == "reset" && r.Method == http.MethodPost:
		st.reset()
		ok()
		return
	case name == "calls":
		if r.Method == http.MethodDelete {
			st.mu.Lock()
			st.calls = nil
			st.mu.Unlock()
			ok()
			return
		}
		after := intOf(query.Get("after"))
		route, contains := query.Get("route"), query.Get("contains")
		st.mu.Lock()
		defer st.mu.Unlock()
		out := []*call{}
		for _, c := range st.calls {
			if c.Seq <= after {
				continue
			}
			if route != "" && route != c.Route && !contains1(c.Labels, route) {
				continue
			}
			if contains != "" && !strings.Contains(c.Text, contains) {
				continue
			}
			out = append(out, c)
		}
		writeJSON(w, 200, obj{"calls": out}, nil)
		return
	case name == "embeddings" || name == "scripts" || name == "faults":
		if s.controlStore(w, r, name, body, query) {
			return
		}
	}
	writeJSON(w, 404, obj{"error": "llmmock: unknown control path " + name}, nil)
}

func contains1(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) controlStore(w http.ResponseWriter, r *http.Request, store string, body any, query url.Values) bool {
	st := &s.st
	switch r.Method {
	case http.MethodPost, http.MethodPut:
		items, isList := body.([]any)
		if !isList {
			items = []any{body}
		}
		ids := []string{}
		for _, it := range items {
			id, err := s.add(store, it)
			if err != nil {
				writeJSON(w, 400, obj{"error": "llmmock: " + err.Error()}, nil)
				return true
			}
			ids = append(ids, id)
		}
		writeJSON(w, 200, obj{"ids": ids}, nil)
		return true
	case http.MethodDelete:
		st.mu.Lock()
		switch store {
		case "embeddings":
			text := query.Get("text")
			kept := st.fixtures[:0]
			for _, f := range st.fixtures {
				if text != "" && f.Text != text {
					kept = append(kept, f)
				}
			}
			st.fixtures = kept
		case "scripts":
			id := query.Get("id")
			kept := st.scripts[:0]
			for _, sc := range st.scripts {
				if id != "" && sc.ID != id {
					kept = append(kept, sc)
				}
			}
			if id == "" {
				kept = nil
			}
			st.scripts = kept
		case "faults":
			id := query.Get("id")
			kept := st.faults[:0]
			for _, f := range st.faults {
				if id != "" && f.ID != id {
					kept = append(kept, f)
				}
			}
			if id == "" {
				kept = nil
			}
			st.faults = kept
		}
		st.mu.Unlock()
		writeJSON(w, 200, obj{"ok": true}, nil)
		return true
	}
	return false
}

func (s *Server) add(store string, item any) (string, error) {
	st := &s.st
	raw := compact(item)
	id := hexID(8)
	st.mu.Lock()
	defer st.mu.Unlock()
	switch store {
	case "embeddings":
		var fx fixture
		if err := unmarshalStrict(raw, &fx); err != nil {
			return "", err
		}
		if _, ok := asObj(item)["text"]; !ok {
			return "", errors.New("an embedding fixture needs text")
		}
		kept := st.fixtures[:0]
		for _, f := range st.fixtures {
			if f.Text != fx.Text {
				kept = append(kept, f)
			}
		}
		st.fixtures = kept
		fx.ID = idOr(fx.ID, id)
		st.fixtures = append(st.fixtures, &fx)
		return fx.ID, nil
	case "scripts":
		var sc script
		if err := unmarshalStrict(raw, &sc); err != nil {
			return "", err
		}
		sc.ID = idOr(sc.ID, id)
		sc.Remaining = remaining(sc.Times)
		st.scripts = append(st.scripts, &sc)
		return sc.ID, nil
	default:
		var f fault
		if err := unmarshalStrict(raw, &f); err != nil {
			return "", err
		}
		f.ID = idOr(f.ID, id)
		f.Remaining = remaining(f.Times)
		st.faults = append(st.faults, &f)
		return f.ID, nil
	}
}

func idOr(have, fresh string) string {
	if have != "" {
		return have
	}
	return fresh
}
