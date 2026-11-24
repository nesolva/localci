// Package web is the dashboard: server-rendered pages over the store, kept live by the event log's SSE stream.
// Pages never poll: a small script listens to /live and re-renders the open page's live region when an event that
// concerns it arrives.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/store"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

type server struct {
	st      *store.Store
	bus     *event.Bus
	pages   map[string]*template.Template
	config  func() (*config.Effective, error)
	machine capacity.Machine
	now     func() time.Time
}

// Option customises the dashboard.
type Option func(*server)

// WithConfig lets the Settings page show the effective configuration and where each value came from.
func WithConfig(load func() (*config.Effective, error)) Option {
	return func(s *server) { s.config = load }
}

// WithMachine tells the Settings page what the host offers, to show capacity next to current use.
func WithMachine(m capacity.Machine) Option { return func(s *server) { s.machine = m } }

// Handler serves the dashboard. Mount it at the root of a listener.
func Handler(st *store.Store, bus *event.Bus, opts ...Option) http.Handler {
	s := &server{st: st, bus: bus, now: time.Now, pages: map[string]*template.Template{}}
	for _, o := range opts {
		o(s)
	}
	for _, name := range []string{"overview", "run", "dag", "test", "log", "settings", "findings", "notfound"} {
		s.pages[name] = template.Must(template.New(name).Funcs(funcs).ParseFS(tmplFS, "templates/layout.html", "templates/timeline.html", "templates/"+name+".html"))
	}
	static, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.Handle("GET /live", event.SSE(st, bus))
	mux.HandleFunc("GET /{$}", s.overview)
	mux.HandleFunc("GET /runs/{id}", s.run)
	mux.HandleFunc("GET /runs/{id}/dag", s.runDAG)
	mux.HandleFunc("GET /runs/{id}/tests/{test}", s.test)
	mux.HandleFunc("GET /runs/{id}/tests/{test}/screenshot", s.screenshot)
	mux.HandleFunc("GET /runs/{id}/log", s.log)
	mux.HandleFunc("GET /findings", s.findings)
	mux.HandleFunc("GET /findings/{id}/screenshot", s.findingScreenshot)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("/", s.notFound)
	return secure(mux)
}

// secure sets the headers every page carries: everything comes from this origin, nothing is sniffed or framed.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Printf("web: render %s: %v", name, err)
		http.Error(w, "could not render this page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	log.Printf("web: %s: %v", r.URL.Path, err)
	http.Error(w, "the store could not be read", http.StatusInternalServerError)
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusNotFound, "notfound", Page{Title: "Not found", Seq: s.seq(r.Context())})
}

// seq is the cursor a page starts following from: what the page shows is at least this fresh.
func (s *server) seq(ctx context.Context) int64 {
	n, _ := s.st.LastSeq(ctx)
	return n
}

// Page is what the layout needs from every page.
type Page struct {
	Title string
	Nav   string // overview | findings | settings
	Run   string // set when the page follows one run; otherwise it follows everything
	Seq   int64
	Live  bool
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func lower(s string) string { return strings.ToLower(s) }
