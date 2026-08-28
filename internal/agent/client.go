package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/kconfesor/zerg/internal/store"
)

// Env vars an agent is spawned with. Identity arrives here rather than being
// inferred from the working directory, so running a command from a
// subdirectory cannot silently mean something different.
const (
	EnvSocket = "ZERG_SOCKET"
	EnvToken  = "ZERG_TOKEN"
	EnvRole   = "ZERG_ROLE"
)

// ErrNoWork is returned by Next when the queue is empty. It is not a failure —
// an agent told there is nothing should stop, not invent a retry loop.
var ErrNoWork = errors.New("no work queued")

// Client is what the zerg CLI uses on behalf of an agent.
type Client struct {
	http  *http.Client
	token string
}

// NewClientFromEnv builds a client from the spawn environment.
func NewClientFromEnv() (*Client, error) {
	socket, token := os.Getenv(EnvSocket), os.Getenv(EnvToken)
	if socket == "" || token == "" {
		return nil, fmt.Errorf("%s and %s must be set; this command is meant to be run by an agent zerg spawned",
			EnvSocket, EnvToken)
	}
	return NewClient(socket, token), nil
}

func NewClient(socket, token string) *Client {
	return &Client{
		token: token,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				},
			},
			// No global timeout: next and ask legitimately wait minutes for
			// work or a human. Each call bounds itself with a context.
		},
	}
}

// Next claims work, waiting up to wait for some to appear.
func (c *Client) Next(ctx context.Context, wait time.Duration) (*NextResponse, error) {
	var out NextResponse
	status, err := c.call(ctx, "/agent/next", nextRequest{WaitSeconds: int(wait.Seconds())}, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, ErrNoWork
	}
	return &out, nil
}

// Done acknowledges a lease.
func (c *Client) Done(ctx context.Context, leaseID string) error {
	_, err := c.call(ctx, "/agent/done", doneRequest{LeaseID: leaseID}, nil)
	return err
}

// Send hands work to another role, or finishes the task when To is empty and
// the caller is the terminal role.
func (c *Client) Send(ctx context.Context, req SendArgs) (*json.RawMessage, error) {
	var out json.RawMessage
	_, err := c.call(ctx, "/agent/send", sendRequest(req), &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SendArgs mirrors the wire shape so callers do not need the private type.
type SendArgs struct {
	To       string `json:"to"`
	TaskID   string `json:"taskId"`
	Kind     string `json:"kind"`
	Commit   string `json:"commit"`
	Body     string `json:"body"`
	Priority int    `json:"priority"`
}

// Ask raises a question for the operator and waits up to wait for an answer.
func (c *Client) Ask(ctx context.Context, question, taskID string, wait time.Duration) (*askResponse, error) {
	var out askResponse
	_, err := c.call(ctx, "/agent/ask", askRequest{
		Question: question, TaskID: taskID, WaitSeconds: int(wait.Seconds()),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Artifact records something the agent produced: a file to keep, or a port
// something it started is listening on.
func (c *Client) Artifact(ctx context.Context, req ArtifactArgs) (*store.Artifact, error) {
	var out store.Artifact
	if _, err := c.call(ctx, "/agent/artifact", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Remember writes down what this agent learned about serving the project, for
// the next one to read.
func (c *Client) Remember(ctx context.Context, note string) error {
	_, err := c.call(ctx, "/agent/remember", map[string]string{"note": note}, nil)
	return err
}

func (c *Client) call(ctx context.Context, path string, body, out any) (int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	// The host is ignored for a unix socket but must be syntactically present.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://zerg"+path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Zerg-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reaching the overmind: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return resp.StatusCode, errors.New(e.Error)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("reading the reply: %w", err)
		}
	}
	return resp.StatusCode, nil
}
