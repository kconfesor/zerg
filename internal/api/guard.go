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

// maxUpload is the largest file that can be attached to a chat message.
//
// Twenty-five megabytes: a screenshot is under one, a phone photograph is a
// few, and a log worth reading is smaller than either. Beyond that the useful
// answer is "point the agent at the path", not "wait while a browser posts a
// video".
const maxUpload = 25 << 20

// uploadPath reports whether a request carries a file rather than JSON.
//
// Matched on the suffix rather than parsed, because the project id sits in the
// middle and this runs on every request.
func uploadPath(path string) bool {
	return strings.HasSuffix(path, "/attachments")
}

// expectedHost reports whether a request arrived addressed to a name this
// daemon actually serves.
//
// This is the check that stops DNS rebinding, and the one the comment below
// used to imply the other two were doing. They are not: rebinding resolves the
// attacker's own hostname to this machine, so by the time the request is made
// the browser considers it same-origin and sends Sec-Fetch-Site: same-origin
// with a matching Origin. Both checks pass. What does not match is the Host,
// which is still the attacker's name, because that is the name the victim's
// browser was pointed at.
//
// Allowed: loopback in any spelling, whatever address the daemon bound, and the
// tailnet name it serves TLS for. Those are the ways the cockpit is reached,
// and they are the URLs the daemon prints at startup.
//
// An unrecognised Host is refused for every method, safe ones included, because
// a read is the whole of the attack here: GET /api/browse enumerates the
// filesystem, and the answer is worth taking from someone whose page is
// pretending to be this daemon.
func (s *Server) expectedHost(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		// HTTP/1.0 without a Host header. Not a browser, and not what rebinding
		// looks like.
		return true
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}

	for _, known := range []string{s.applied.Addr, s.applied.TailnetHost, s.tailnetHost} {
		if known == "" {
			continue
		}
		name := known
		if h, _, err := net.SplitHostPort(name); err == nil {
			name = h
		}
		if name == "" || name == "0.0.0.0" || name == "::" {
			// Bound to everything, so the Host is whatever route was taken to
			// get here and there is nothing to compare it against. Saying so
			// out loud rather than silently allowing anything: this is the
			// configuration the startup warning is about.
			return true
		}
		if strings.EqualFold(name, host) {
			return true
		}
	}
	return false
}

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
// Safe methods are left alone by the cross-site checks: refusing them would
// break linking to the cockpit, and a cross-site page cannot read the response
// anyway, since nothing here sends CORS headers. The Host check above applies
// to them, because rebinding defeats exactly that reasoning.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One megabyte is right for JSON written by a person or an agent, and
		// wrong for the one route that carries a file somebody picked. That
		// route has its own limit, enforced where the bytes are read.
		if uploadPath(r.URL.Path) {
			r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		} else {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}

		if !s.expectedHost(r) {
			s.refuseHost(w, r)
			return
		}

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

// refuseHost answers a request addressed to a name this daemon does not serve.
//
// Named separately from refuse because the remedy is different: this is not a
// page misbehaving, it is a request that arrived under the wrong name, and the
// operator's fix is to use the URL the daemon printed.
func (s *Server) refuseHost(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("refused a request addressed to an unknown host",
		"method", r.Method, "path", r.URL.Path, "host", r.Host, "remote", r.RemoteAddr)
	writeError(w, http.StatusForbidden,
		"this daemon does not answer to "+r.Host+"; reach it at the address it printed at startup")
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
