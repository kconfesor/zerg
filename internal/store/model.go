package store

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Receive modes. A task role takes one unit at a time; a batch role takes
// every queued item sharing the head item's priority, bounded by its policy.
const (
	ReceiveTask  = "task"
	ReceiveBatch = "batch"
)

// Gates. An approval gate holds this role's outbound handoffs until a human
// decides. It is a field rather than a role type, so it composes: a planner
// uses it to get its spec signed off, and an architect can use it too.
const (
	GateNone     = "none"
	GateApproval = "approval"
)

// RoleTemplate is an entry in the global library — the idea of a role,
// independent of any project. Projects select templates; see ProjectRole.
type RoleTemplate struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Harness        string    `json:"harness"`
	Model          string    `json:"model"`
	Args           []string  `json:"args"`
	Receive        string    `json:"receive"`
	BatchMaxItems  int       `json:"batchMaxItems"`
	BatchMaxAgeSec int       `json:"batchMaxAgeSec"`
	Prompt         string    `json:"prompt"`
	Gate           string    `json:"gate"`
	Builtin        bool      `json:"builtin"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Project is a git repository zerg orchestrates.
type Project struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	BaseBranch string `json:"baseBranch"`

	// Integration is how finished work reaches the base branch: merge, branch
	// or pr. See the constants in config.go for what each means and why this
	// is a property of the project rather than of a role.
	Integration  string     `json:"integration"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastOpenedAt *time.Time `json:"lastOpenedAt,omitempty"`
}

// ProjectRole is one template's membership in one project's pipeline. Position
// and enablement live here, and so do overrides — an override is a property of
// the pairing, not a patch applied on the side.
type ProjectRole struct {
	TemplateID    string   `json:"templateId"`
	Position      int      `json:"position"`
	Enabled       bool     `json:"enabled"`
	ModelOverride *string  `json:"modelOverride,omitempty"`
	ArgsOverride  []string `json:"argsOverride,omitempty"`
}

// ResolvedRole is a template with its project overrides applied — what the
// cerebrate is actually asked to run. Overridden reports whether anything
// diverged from the library, so the UI can badge it.
type ResolvedRole struct {
	RoleTemplate
	Position   int  `json:"position"`
	Enabled    bool `json:"enabled"`
	Overridden bool `json:"overridden"`
	Terminal   bool `json:"terminal"`

	// ModelOverride and ArgsOverride are what this project set, as opposed to
	// what it ended up with. Both are needed to round-trip a team edit: with
	// only the resolved values and one Overridden flag, a reorder had to guess,
	// and it guessed by sending the resolved model as an override and dropping
	// the argument override entirely — so changing a role's position silently
	// erased its arguments and pinned a model nobody had pinned.
	ModelOverride *string  `json:"modelOverride,omitempty"`
	ArgsOverride  []string `json:"argsOverride,omitempty"`
}

// ValidationError marks a caller mistake — something a user can fix by
// changing what they sent. It exists so the API layer can map errors by type
// rather than by matching on message text, which drifts the moment a message
// is reworded.
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

// Validation is a marker so errors.As can find this through a wrapped chain.
func (e *ValidationError) Validation() {}

func invalid(format string, args ...any) error {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// nameRe constrains a role name because the name becomes a worktree directory:
// lowercase, digits and dashes, starting with a letter. That rules out path
// traversal, shell surprises and case-insensitive filesystem collisions in one
// stroke, without needing to sanitise at every use site.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// Validate checks a template's structure. It deliberately does not check that
// Harness is a registered adapter or that Model exists — those need the adapter
// registry and a live harness, and belong to preflight (ARCHITECTURE.md §8),
// which can report them with a remedy.
func (t *RoleTemplate) Validate() error {
	if !nameRe.MatchString(t.Name) {
		return invalid("role name %q must be lowercase letters, digits and dashes, starting with a letter", t.Name)
	}
	if strings.TrimSpace(t.Harness) == "" {
		return invalid("role %q needs a harness", t.Name)
	}
	if t.Receive != ReceiveTask && t.Receive != ReceiveBatch {
		return invalid("role %q has receive %q, want %q or %q", t.Name, t.Receive, ReceiveTask, ReceiveBatch)
	}
	if t.Gate != GateNone && t.Gate != GateApproval {
		return invalid("role %q has gate %q, want %q or %q", t.Name, t.Gate, GateNone, GateApproval)
	}
	if t.Receive == ReceiveBatch {
		if t.BatchMaxItems < 1 {
			return invalid("role %q batches, so batchMaxItems must be at least 1", t.Name)
		}
		if t.BatchMaxAgeSec < 1 {
			return invalid("role %q batches, so batchMaxAgeSec must be at least 1", t.Name)
		}
	}
	return nil
}
