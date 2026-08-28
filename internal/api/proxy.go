package api

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/kconfesor/zerg/internal/store"
)

// The services agents start, each on an origin of its own.
//
// §13.4 is why these are not served from the cockpit's origin. A service is
// agent-generated code running in a browser; same-origin with the cockpit it
// would reach the cockpit's state and the command API, which has no
// authentication, so an agent bug or a prompt injection in a file it read
// could drive the orchestrator from inside the page somebody opened to look at
// its work. A different port is a different origin, and that is the mechanism.
//
// One listener per service rather than one shared origin with the service id
// in the path. The shared origin worked for a static site and could not work
// for anything else: a Vite dev server writes absolute script paths, so a page
// served under /<id>/ asks for /src/main.ts at the root of the shared origin,
// gets the viewer's 404, and renders blank while returning 200 for the HTML.
// Every dev server with a router or a hashed asset path has the same problem,
// which is most of them. A whole origin per service also means their cookies
// and storage are separated by the browser rather than by rewriting, and that
// websockets -- hot reload -- work without special handling.
//
// It is a link opened in a tab, not a frame the cockpit embeds. The proxy
// still exists rather than linking straight at the service: a dev server binds
// loopback, so its own address works only on the machine the daemon runs on,
// and reading a preview from a phone is the case that would break.

// Viewer owns the origins that services are reached on.
type Viewer struct {
	db  *store.DB
	log *slog.Logger

	// host is where the cockpit binds, which is where these bind too. A
	// preview only reachable from the daemon's own machine is no use to the
	// phone that is reading the approval.
	host string

	// cert and key are the cockpit's, so a preview on a tailnet is https for
	// the same name and does not warn.
	cert, key string

	// touch says somebody is still looking, which is what the runner's idle
	// timer measures. A request through here is the honest signal: it is true
	// from a phone, from a second tab, and from a page left open, and none of
	// those can be seen from the cockpit.
	touch func(projectID string)

	mu   sync.Mutex
	open map[string]*origin
}

type origin struct {
	port int
	srv  *http.Server
}

func NewViewer(db *store.DB, host string, log *slog.Logger) *Viewer {
	if log == nil {
		log = slog.Default()
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return &Viewer{db: db, host: host, log: log, open: map[string]*origin{}}
}

// WithTouch reports use back to whoever is timing out idle previews.
func (v *Viewer) WithTouch(fn func(projectID string)) *Viewer {
	v.touch = fn
	return v
}

// WithTLS gives previews the cockpit's certificate.
func (v *Viewer) WithTLS(cert, key string) *Viewer {
	v.cert, v.key = cert, key
	return v
}

// PortFor is the port this service can be reached on, opening an origin for it
// the first time it is asked for.
//
// Lazily, because most services are never looked at: a card can produce three
// previews across its laps and somebody opens one of them.
func (v *Viewer) PortFor(a *store.Artifact) int {
	if a == nil || !a.Live() {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	if o, ok := v.open[a.ID]; ok {
		return o.port
	}

	// A port of the viewer's own rather than the service's. The service's is
	// bound by the service, on loopback or on everything depending on how it
	// was started, and either way it is not this daemon's to reuse.
	ln, err := net.Listen("tcp", net.JoinHostPort(v.host, "0"))
	if err != nil {
		v.log.Warn("could not open an origin for a service", "artifact", a.ID, "err", err)
		return 0
	}
	port := ln.Addr().(*net.TCPAddr).Port

	// And on loopback as well, the way the cockpit has a second listener: the
	// link is built from the host the browser used, so a cockpit reached at
	// 127.0.0.1 hands out a 127.0.0.1 preview link.
	var local net.Listener
	if v.host != "127.0.0.1" && v.host != "localhost" {
		local, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			// Not fatal. Losing the local address is worth less than the view.
			v.log.Warn("a preview is not available locally", "artifact", a.ID, "err", err)
		}
	}

	srv := &http.Server{
		Handler: v.handler(a.ID, a.ProjectID, a.Port),
		// No read or write timeout: what is on the other side is somebody's
		// dev server, and a websocket for hot reload is the normal case.
		ReadHeaderTimeout: 10 * time.Second,
	}
	serve := func(l net.Listener, encrypted bool) {
		go func() {
			var err error
			if encrypted {
				err = srv.ServeTLS(l, v.cert, v.key)
			} else {
				err = srv.Serve(l)
			}
			if err != nil && err != http.ErrServerClosed {
				v.log.Debug("a preview origin stopped", "artifact", a.ID, "err", err)
			}
		}()
	}
	serve(ln, v.cert != "")
	if local != nil {
		// Plain on loopback even when the other is encrypted, matching the
		// cockpit: a certificate is issued for a name, and 127.0.0.1 is not it.
		serve(local, false)
	}

	v.open[a.ID] = &origin{port: port, srv: srv}
	return port
}

// handler proxies one service, whole. Nothing is rewritten: the point of an
// origin per service is that the service's own paths are already right.
func (v *Viewer) handler(artifactID, projectID string, servicePort int) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(servicePort))}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			// Forwarded headers are deliberately not set. They tell an app to
			// build absolute URLs pointing at this origin, which is exactly
			// what should not be encouraged: this is a viewer, not a
			// deployment.
			pr.Out.Header.Del("X-Forwarded-For")
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Content-Type-Options", "nosniff")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The commonest case by far: the process died and nobody told the
			// database. Answered as what it is, with what to do about it.
			v.log.Debug("proxying a service failed", "artifact", artifactID, "err", err)
			http.Error(w,
				"this service is not answering; it may have stopped since it was registered",
				http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v.touch != nil {
			v.touch(projectID)
		}
		rp.ServeHTTP(w, r)
	})
}

// Close takes down one service's origin, freeing its port.
func (v *Viewer) Close(artifactID string) {
	v.mu.Lock()
	o, ok := v.open[artifactID]
	delete(v.open, artifactID)
	v.mu.Unlock()
	if ok {
		o.srv.Close()
	}
}

// CloseAll takes them all down, for a daemon shutting down.
func (v *Viewer) CloseAll() {
	v.mu.Lock()
	all := v.open
	v.open = map[string]*origin{}
	v.mu.Unlock()
	for _, o := range all {
		o.srv.Close()
	}
}

// ServiceURL is where a browser can reach a service, built from the request
// that asked.
//
// From the request rather than from configuration: the cockpit was reached on
// some host -- loopback, a tailnet name, an IP -- and the preview is the same
// host on a port of its own. Deriving it means one answer that is right for
// whoever is asking, instead of a configured URL that is wrong for everybody
// but the machine it was configured on.
func ServiceURL(r *http.Request, port int) string {
	if port == 0 {
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
	return fmt.Sprintf("%s://%s/", scheme, net.JoinHostPort(host, strconv.Itoa(port)))
}
