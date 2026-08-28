package api

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/kconfesor/zerg/internal/store"
)

// The reverse proxy for services an agent started, on an origin of its own.
//
// §13.4 is the reason this is a separate listener rather than a path on the
// cockpit. A service artifact is agent-generated code running in a browser.
// Served from the cockpit's origin it would have same-origin access to the
// cockpit's state and to the command API, which has no authentication: an
// agent bug, or a prompt injection in a file it read, could drive the
// orchestrator from inside the page a person opened to look at its work.
//
// A different port is a different origin, and that is the whole mechanism.
// Everything else here follows from it: this handler serves nothing but
// proxied services, it never sees a cockpit route, and the cockpit embeds it
// in a sandboxed iframe.

// Proxy answers requests on the service origin.
type Proxy struct {
	db  *store.DB
	log *slog.Logger
}

func NewProxy(db *store.DB, log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{db: db, log: log}
}

// Handler is the whole of this origin: /{artifactID}/... and nothing else.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{id}/", p.serve)
	mux.HandleFunc("/{id}", p.serve)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately not a redirect to the cockpit: this origin exists to be
		// untrusted, and a link from it to the trusted one is the shape of the
		// thing it is meant to prevent.
		http.Error(w, "this port serves the services agents started, at /<artifact-id>/",
			http.StatusNotFound)
	})
	return mux
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := p.db.GetArtifact(r.Context(), id)
	if err != nil {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if a.Kind != store.ArtifactService {
		http.Error(w, "that artifact is a file, not a service", http.StatusBadRequest)
		return
	}
	if a.StoppedAt != nil {
		// The process is gone and the port may belong to something else now.
		// Said rather than proxied: connecting anyway is how a dead link
		// becomes a live one to the wrong program.
		http.Error(w, "this service has stopped", http.StatusGone)
		return
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", fmt.Sprint(a.Port))}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The service is reached at /<id>/..., and it does not know that.
			// A dev server asked for /<id>/main.js answers 404, and every
			// absolute path in the page it serves would come back here with
			// the prefix missing.
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/"+id)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.Host = target.Host

			// Forwarded headers are deliberately not set. They tell an app to
			// build absolute URLs pointing at this origin, which is exactly
			// what should not be encouraged: this origin is a viewer, not a
			// deployment.
			pr.Out.Header.Del("X-Forwarded-For")
		},
		ModifyResponse: func(resp *http.Response) error {
			// A service that tries to leave the iframe, or set cookies on this
			// origin, is not doing anything the viewer needs.
			resp.Header.Del("Set-Cookie")
			resp.Header.Set("X-Content-Type-Options", "nosniff")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The commonest case by far: the process died and nobody told the
			// database. Answered as what it is, with what to do about it.
			p.log.Debug("proxying a service failed", "artifact", id, "err", err)
			http.Error(w,
				"this service is not answering; it may have stopped since it was registered",
				http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// ServiceURL is where the cockpit can reach a service, built from the request
// that asked.
//
// From the request rather than from configuration: the cockpit reached this
// daemon on some host -- loopback, a tailnet name, an IP -- and the proxy is
// the same host on its own port. Deriving it means one answer that is right
// for whoever is asking, instead of a configured URL that is wrong for
// everybody but the machine it was configured on.
func ServiceURL(r *http.Request, proxyPort int, id string) string {
	if proxyPort == 0 {
		return ""
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/%s/", scheme, net.JoinHostPort(host, fmt.Sprint(proxyPort)), id)
}
