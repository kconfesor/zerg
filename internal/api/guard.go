package api

import (
	"net"
	"net/http"
	"strings"
)

// maxBody is the largest request this API will read.
//
// Every body here is a form: a prompt, a team, a setting. A megabyte is far
// more than any of them and far less than a stream that would sit in memory
// until it finished. Without it a single request can grow the daemon until the
// machine decides which process to kill.
const maxBody = 1 << 20

// guard rejects requests a browser tab of this app would never make.
//
// There is no authentication yet, so the cockpit trusts whoever reaches the
// port — which is the point on a tailnet of your own devices. It is not the
// point when the request comes from a page you happened to visit: DNS
// rebinding resolves an attacker's hostname to 127.0.0.1 and then posts to
// this API from your own browser, and the tailnet cannot help, because the
// request originates inside it.
//
// That matters more here than in most apps. Agents run with permission prompts
// disabled, in worktrees of repositories you chose, so "create a task" is
// arbitrary code execution on this machine.
//
// Two checks, both of which a real cockpit tab passes and neither of which a
// cross-site request can forge:
//
//   - Sec-Fetch-Site, which the browser sets and script cannot. same-origin and
//     none (a typed URL) are allowed; cross-site is not.
//   - Origin, for anything that does not send Sec-Fetch-Site, compared against
//     the Host the request arrived on.
//
// Safe methods are left alone: a GET that leaks is a problem authentication
// solves, and refusing them would break linking to the cockpit.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		switch r.Header.Get("Sec-Fetch-Site") {
		case "same-origin", "same-site", "none":
			next.ServeHTTP(w, r)
			return
		case "cross-site":
			s.refuse(w, r, "cross-site")
			return
		}

		// No Sec-Fetch-Site: an older browser, or a non-browser client such as
		// curl. A non-browser sends no Origin either, and is allowed — it is
		// not the threat this is for. An Origin that disagrees with Host is.
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !sameHost(origin, r.Host) {
			s.refuse(w, r, "origin "+origin)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, why string) {
	s.log.Warn("refused a cross-origin request",
		"method", r.Method, "path", r.URL.Path, "reason", why, "remote", r.RemoteAddr)
	writeError(w, http.StatusForbidden,
		"this request did not come from the cockpit; cross-origin writes are refused")
}

// sameHost compares an Origin against the Host the request arrived on.
//
// Ports are compared, hosts case-insensitively. A default port is not inferred:
// the cockpit is always reached on an explicit one, and guessing here would
// accept an origin that differs in the one place that matters.
func sameHost(origin, host string) bool {
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	oh := origin[i+3:]
	if j := strings.IndexAny(oh, "/?#"); j >= 0 {
		oh = oh[:j]
	}
	return strings.EqualFold(normalizeHost(oh), normalizeHost(host))
}

func normalizeHost(h string) string {
	if hostOnly, port, err := net.SplitHostPort(h); err == nil {
		return hostOnly + ":" + port
	}
	return h
}
