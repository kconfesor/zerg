// Package tailnet reads the local tailscaled, and gets certificates from it.
//
// Deliberately the CLI and not tsnet. Embedding tailscale.com/tsnet would make
// zerg its own tailnet device with automatic HTTPS, which is a genuinely nicer
// shape — and costs 547 modules against this project's ten, and roughly +30 MB
// of binary. The machine already runs tailscaled; asking it is enough, and the
// dependency budget is better spent elsewhere.
//
// Everything here degrades to "not available" rather than failing. A machine
// without Tailscale is the normal case, not an error.
package tailnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Status is what the local tailscaled reports about this machine.
type Status struct {
	// Available is false when tailscale is not installed or not running. Every
	// other field is then meaningless.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	// DNSName is the MagicDNS name, without the trailing dot. This is what a
	// certificate is issued for and what you type on a phone.
	DNSName string   `json:"dnsName,omitempty"`
	IPs     []string `json:"ips,omitempty"`

	// HTTPSEnabled reports whether the tailnet has HTTPS certificates turned
	// on. Without it `tailscale cert` fails, and it is switched on in the admin
	// console rather than anywhere zerg can reach — so it is surfaced as a
	// state with a remedy instead of an error at bind time.
	HTTPSEnabled bool `json:"httpsEnabled"`
}

const probeTimeout = 5 * time.Second

// Probe asks the local tailscaled what it knows.
func Probe(ctx context.Context) Status {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return Status{Reason: "the tailscale command is not installed"}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		return Status{Reason: "tailscaled is not running, or this machine is logged out"}
	}

	var doc struct {
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
		CertDomains []string `json:"CertDomains"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return Status{Reason: fmt.Sprintf("could not read tailscale status: %v", err)}
	}

	name := strings.TrimSuffix(doc.Self.DNSName, ".")
	if name == "" {
		return Status{Reason: "this machine has no MagicDNS name yet"}
	}
	return Status{
		Available: true,
		DNSName:   name,
		IPs:       doc.Self.TailscaleIPs,
		// CertDomains is empty until HTTPS is enabled for the tailnet.
		HTTPSEnabled: len(doc.CertDomains) > 0,
	}
}

// EnsureCert writes a certificate and key for host into dir and returns their
// paths.
//
// `tailscale cert` is idempotent and renews in place, so this is safe to call
// on every start: an existing valid certificate is reused rather than reissued,
// and one near expiry is replaced.
//
// The failure worth naming is HTTPS being switched off for the tailnet, since
// nothing about the resulting error says "go to the admin console".
func EnsureCert(ctx context.Context, host, dir string) (certFile, keyFile string, err error) {
	if host == "" {
		return "", "", fmt.Errorf("no MagicDNS name to request a certificate for")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("creating %s: %w", dir, err)
	}

	certFile = filepath.Join(dir, host+".crt")
	keyFile = filepath.Join(dir, host+".key")

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "tailscale", "cert",
		"--cert-file", certFile, "--key-file", keyFile, host)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// The message for a tailnet without certificates enabled says neither
		// "HTTPS" nor anything about where to switch it on — the observed text
		// is "your Tailscale account does not support getting TLS certs", which
		// reads like a billing problem and is usually a toggle. Match what it
		// actually says, and answer the question it raises.
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "does not support") ||
			strings.Contains(lower, "https") ||
			strings.Contains(lower, "not enabled") {
			return "", "", fmt.Errorf(
				"tailscale will not issue a certificate for %s yet: HTTPS Certificates are off "+
					"for this tailnet. Enable them at https://login.tailscale.com/admin/dns "+
					"(under HTTPS Certificates), then try again. tailscale said: %s",
				host, msg)
		}
		return "", "", fmt.Errorf("tailscale cert %s: %v (%s)", host, err, msg)
	}

	// A zero exit is the command's account of itself, not proof of a file. The
	// two agree in every observed case — a failure exits 1 and writes nothing —
	// but handing back paths to files that do not exist would surface as an
	// opaque TLS error at bind time, several steps from the cause.
	for _, f := range []string{certFile, keyFile} {
		if _, err := os.Stat(f); err != nil {
			return "", "", fmt.Errorf(
				"tailscale reported success but did not write %s: %w", f, err)
		}
	}
	return certFile, keyFile, nil
}
