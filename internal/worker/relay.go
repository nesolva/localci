package worker

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// relay is the worker's loopback door to the primary. Node and git cannot be told to trust a pinned self-signed
// certificate, so the tests and git talk plain HTTP to 127.0.0.1 here and this process forwards over the pinned TLS
// connection. Only two kinds of path pass: the run hooks (the caller's own hook token travels unchanged) and git
// fetches (the worker credential is added here, so it never reaches a test process).
type relay struct {
	l     net.Listener
	srv   *http.Server
	cred  string
	proxy *httputil.ReverseProxy
}

func startRelay(c Config) (*relay, error) {
	target, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &relay{l: l, cred: c.Credential}
	r.proxy = &httputil.ReverseProxy{
		Transport: PinnedClient(c.Fingerprint).Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			req := pr.Out
			req.URL.Scheme, req.URL.Host, req.Host = target.Scheme, target.Host, target.Host
			if strings.HasPrefix(req.URL.Path, "/worker/v1/") {
				req.Header.Set("Authorization", "Bearer "+r.cred)
			}
		},
		FlushInterval: -1,
	}
	r.srv = &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: 10 * time.Second}
	go r.srv.Serve(l)
	return r, nil
}

func (r *relay) serve(w http.ResponseWriter, req *http.Request) {
	p := req.URL.Path
	hook := strings.HasPrefix(p, "/runs/") && strings.Contains(p, "/hooks/")
	git := strings.HasPrefix(p, "/worker/v1/git/")
	if !hook && !git {
		http.NotFound(w, req)
		return
	}
	r.proxy.ServeHTTP(w, req)
}

// base is the URL tests and git use.
func (r *relay) base() string { return "http://" + r.l.Addr().String() }

func (r *relay) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r.srv.Shutdown(ctx)
}
