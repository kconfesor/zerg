package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/kconfesor/zerg/internal/api"
	"github.com/kconfesor/zerg/internal/store"
)

// listenProxy binds the origin that serves agents' services.
//
// Bound here and served later, in two steps, because the port has to be known
// before the API is built -- the links it hands the cockpit contain it -- and
// the certificate is not resolved until after that. Returns zero when it could
// not bind, which makes service links absent rather than broken.
//
// Loopback only, even when the cockpit is on a tailnet. What is proxied is a
// program an agent wrote; the cockpit reaches it through the browser that is
// already talking to this machine, and there is no reason for the rest of the
// network to reach it directly.
func listenProxy(db *store.DB, log *slog.Logger) (port int, serve func(cert, key string), closer func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Warn("the service viewer is unavailable", "err", err)
		return 0, func(string, string) {}, func() {}
	}

	srv := &http.Server{
		Handler:           api.NewProxy(db, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No ReadTimeout or WriteTimeout: what is on the other side of this is
		// somebody's dev server, and a websocket for hot reload is the normal
		// case rather than the exception.
	}

	port = ln.Addr().(*net.TCPAddr).Port
	serve = func(cert, key string) {
		go func() {
			var err error
			if cert != "" {
				err = srv.ServeTLS(ln, cert, key)
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("the service viewer stopped", "err", err)
			}
		}()
	}
	return port, serve, func() { srv.Close() }
}
