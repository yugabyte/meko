package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const contentAmzJSON = "application/x-amz-json-1.1"

func (p *Pool) PublicHandler() http.Handler { return &handler{pool: p, public: true} }

func (p *Pool) ControlHandler() http.Handler { return &handler{pool: p} }

type handler struct {
	pool   *Pool
	public bool
}

func reply(w http.ResponseWriter, status int, body any, contentType string, headers map[string]string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
	raw := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func replyJSON(w http.ResponseWriter, status int, body any) {
	reply(w, status, body, "application/json", nil)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.get(w, r)
	case http.MethodPost:
		h.post(w, r)
	default:
		replyJSON(w, http.StatusNotFound, obj{"message": "not found"})
	}
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	p := h.pool
	switch {
	case path == "/healthz":
		replyJSON(w, 200, obj{"status": "ok"})
	case h.public && (path == "/"+p.cfg.PoolID+"/.well-known/jwks.json" || path == "/.well-known/jwks.json"):
		replyJSON(w, 200, obj{"keys": []any{p.jwk()}})
	case !h.public && path == "/control/users":
		p.mu.Lock()
		defer p.mu.Unlock()
		replyJSON(w, 200, p.ctlUsers())
	default:
		replyJSON(w, http.StatusNotFound, obj{"message": "not found"})
	}
}

func (h *handler) post(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	payload := obj{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			replyJSON(w, 400, obj{"__type": "SerializationException", "message": "bad json"})
			return
		}
	}
	if h.public {
		h.cognito(w, r, payload)
		return
	}
	h.control(w, r, payload)
}

func (h *handler) cognito(w http.ResponseWriter, r *http.Request, payload obj) {
	p := h.pool
	target := r.Header.Get("X-Amz-Target")
	op := target[strings.LastIndex(target, ".")+1:]
	p.mu.Lock()
	out, err := p.operation(op, payload)
	if err == nil {
		p.save()
	}
	p.mu.Unlock()
	var ce *cognitoError
	switch {
	case err == nil:
		p.cfg.Logf("%s ok", op)
		reply(w, 200, out, contentAmzJSON, nil)
	case errors.As(err, &ce):
		p.cfg.Logf("%s %s: %s", op, ce.kind, ce.message)
		reply(w, 400, obj{"__type": ce.kind, "message": ce.message}, contentAmzJSON, map[string]string{"x-amzn-ErrorType": ce.kind})
	default:
		p.cfg.Logf("%s: %v", op, err)
		reply(w, 500, obj{"__type": "InternalServerError", "message": err.Error()}, contentAmzJSON, map[string]string{"x-amzn-ErrorType": "InternalServerError"})
	}
}

func (h *handler) control(w http.ResponseWriter, r *http.Request, payload obj) {
	p := h.pool
	var run func(obj) (obj, error)
	switch r.URL.Path {
	case "/control/users":
		run = p.ctlUpsertUser
	case "/control/tokens":
		run = p.ctlMint
	default:
		replyJSON(w, http.StatusNotFound, obj{"message": "not found"})
		return
	}
	p.mu.Lock()
	out, err := run(payload)
	if err == nil {
		p.save()
	}
	p.mu.Unlock()
	var ce *cognitoError
	var me *missingField
	switch {
	case err == nil:
		replyJSON(w, 200, out)
	case errors.As(err, &ce):
		replyJSON(w, 400, obj{"error": ce.kind, "message": ce.message})
	case errors.As(err, &me):
		replyJSON(w, 400, obj{"error": "missing field", "message": "'" + me.name + "'"})
	default:
		replyJSON(w, 500, obj{"error": "internal", "message": err.Error()})
	}
}

type missingField struct{ name string }

func (e *missingField) Error() string { return "missing field " + e.name }

func (p *Pool) ctlUsers() obj {
	out := []obj{}
	for _, u := range p.ordered() {
		out = append(out, obj{"email": u.Attrs["email"], "sub": u.Sub, "status": u.Status})
	}
	return obj{"users": out}
}

func (p *Pool) ctlUpsertUser(r obj) (obj, error) {
	email, ok := r["email"].(string)
	if !ok {
		return nil, &missingField{"email"}
	}
	password, ok := r["password"].(string)
	if !ok {
		return nil, &missingField{"password"}
	}
	status := "CONFIRMED"
	if v, ok := r["status"].(string); ok {
		status = v
	}
	first, last := "Mekoqa", "Account"
	if v, ok := r["first_name"].(string); ok {
		first = v
	}
	if v, ok := r["last_name"].(string); ok {
		last = v
	}
	verified := "false"
	if status == "CONFIRMED" {
		verified = "true"
	}
	attrs := map[string]string{"email": email, "email_verified": verified, "given_name": first, "family_name": last}
	u := p.users[key(email)]
	if u == nil {
		var err error
		if u, err = p.newUser(email, password, attrs, status); err != nil {
			return nil, err
		}
	} else {
		u.Password, u.Status = password, status
		for k, v := range attrs {
			u.Attrs[k] = v
		}
	}
	return obj{"sub": u.Sub, "email": email, "status": u.Status}, nil
}

func (p *Pool) ctlMint(r obj) (obj, error) {
	var u *user
	if want, ok := r["sub"]; ok {
		for _, c := range p.ordered() {
			if c.Sub == want {
				u = c
				break
			}
		}
		if u == nil {
			return nil, fail("UserNotFoundException", "no user with that sub")
		}
	} else {
		email, ok := r["email"].(string)
		if !ok {
			return nil, &missingField{"email"}
		}
		var err error
		if u, err = p.find(email); err != nil {
			return nil, err
		}
	}
	use := "access"
	if v, ok := r["token_use"].(string); ok {
		use = v
	}
	var opts tokenOpts
	if v, ok := r["expires_in"].(float64); ok {
		ttl := int(v)
		opts.ttl = &ttl
	}
	opts.issuer, _ = r["issuer"].(string)
	opts.clientID, _ = r["client_id"].(string)
	tok, err := p.token(u, use, opts)
	if err != nil {
		return nil, err
	}
	return obj{"token": tok}, nil
}

// An empty CertFile serves the public listener over plain HTTP, for running by hand.
type Listeners struct {
	PublicAddr  string
	ControlAddr string
	CertFile    string
	KeyFile     string
}

func (p *Pool) Serve(ctx context.Context, l Listeners) error {
	public := &http.Server{Handler: p.PublicHandler(), ReadHeaderTimeout: 30 * time.Second}
	control := &http.Server{Handler: p.ControlHandler(), ReadHeaderTimeout: 30 * time.Second}
	pub, err := net.Listen("tcp", l.PublicAddr)
	if err != nil {
		return err
	}
	ctl, err := net.Listen("tcp", l.ControlAddr)
	if err != nil {
		_ = pub.Close()
		return err
	}
	if l.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(l.CertFile, l.KeyFile)
		if err != nil {
			_ = pub.Close()
			_ = ctl.Close()
			return fmt.Errorf("load the serving certificate: %w", err)
		}
		public.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		// A non-nil empty map turns HTTP/2 off, which the mock never offered.
		public.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}
	p.cfg.Logf("cognito mock for %s on %s and %s", p.Issuer(), pub.Addr(), ctl.Addr())

	errc := make(chan error, 2)
	go func() {
		if l.CertFile != "" {
			errc <- public.ServeTLS(pub, "", "")
		} else {
			errc <- public.Serve(pub)
		}
	}()
	go func() { errc <- control.Serve(ctl) }()

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdown)
	_ = control.Shutdown(shutdown)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
