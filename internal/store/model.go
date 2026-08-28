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
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Harness string   `json:"harness"`
	Model   string   `json:"model"`
	Args    []string `json:"args"`
	// Finisher marks a role that ends a pipeline: a reviewer or a cleaner is
	// the last step wherever it appears, and a planner never is. Added to a
	// team, such a role goes to the end and the roles added after it go in
	// front, which is how the pipeline keeps delivering through the same role
	// as it grows. It is not the same field as ResolvedRole.Terminal, which is
	// which role is finishing *this* pipeline.
	Finisher bool `json:"finisher"`

	// Purpose says whether this role is part of the pipeline or a job the
	// daemon starts.
	//
	// PurposePipeline is a role that claims work, appears on the board and can
	// be put in a team. PurposeRunner is the agent that works out how a
	// project serves itself and starts it: never routed to, never on the
	// board, never in a team, and started by the daemon when somebody asks to
	// see the app. Everything else about it -- harness, model, thinking level,
	// prompt -- is configured exactly where every other role's is, which is
	// the whole reason it is a role rather than a special case in the binary.
	Purpose string `json:"purpose"`
	// Thinking is how hard the harness reasons before answering, in that
	// harness's own vocabulary: claude spends it as --effort, pi as --thinking,
	// and their level sets are not the same. Empty leaves the harness's default
	// alone, which is what every role had before this existed.
	Thinking       string    `json:"thinking"`
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
	PRDraft      bool       `json:"prDraft"`
	CreatedAt    time.Time  `json:"createdAt"`
	LastOpenedAt *time.Time `json:"lastOpenedAt,omitempty"`

	// TeamPresetID is the team this project runs. Every project has one: a
	// pipeline of a project's own is a team belonging to that project, not a
	// layer over somebody else's team. It is a pointer because a project is
	// written before its team is chosen, and because the foreign key sets it
	// null if that team is deleted out from under it.
	TeamPresetID *string `json:"teamPresetId,omitempty"`

	// ChatHarness and ChatModel override what answers questions in Chat.
	// Empty means inherit from the terminal role, which is the default.
	ChatHarness string `json:"chatHarness,omitempty"`
	ChatModel   string `json:"chatModel,omitempty"`

	// Icon is one emoji, or empty. The switcher derives initials and a colour
	// when it is empty, so nothing has to be set for a project to be
	// recognisable — this is for when the derived mark is not the one you want.
	Icon string `json:"icon"`

	// AutoRun starts a preview of a task when it finishes. Off by default,
	// because every run is an agent turn and therefore money.
	AutoRun bool `json:"autoRun"`
}

// What a role is for; see RoleTemplate.Purpose.
const (
	PurposePipeline = "pipeline"
	PurposeRunner   = "runner"
)

// RoleOverrides is the nullable layer shared by reusable-team roles and a
// project's local role settings. Nil means inherit. Args deliberately keeps
// nil distinct from an explicit empty slice.
type RoleOverrides struct {
	HarnessOverride        *string  `json:"harnessOverride,omitempty"`
	ModelOverride          *string  `json:"modelOverride,omitempty"`
	ArgsOverride           []string `json:"argsOverride"`
	ThinkingOverride       *string  `json:"thinkingOverride,omitempty"`
	ReceiveOverride        *string  `json:"receiveOverride,omitempty"`
	BatchMaxItemsOverride  *int     `json:"batchMaxItemsOverride,omitempty"`
	BatchMaxAgeSecOverride *int     `json:"batchMaxAgeSecOverride,omitempty"`
	PromptOverride         *string  `json:"promptOverride,omitempty"`
	GateOverride           *string  `json:"gateOverride,omitempty"`
}

// ProjectRole is one role's settings for one project: this repository's coder
// on a stronger model, without a team of its own for it.
//
// Position and Enabled are the team's to say. They were this type's too, back
// when a project could freeze a pipeline's shape while naming a team it was not
// running (schema 16 is where that went), and a project that wants its own
// shape now has its own team.
type ProjectRole struct {
	TemplateID string `json:"templateId"`
	RoleOverrides
}

// TeamPreset is a named, reusable pipeline. Its role settings are themselves
// overrides over the role library, keeping the library as the common baseline.
type TeamPreset struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	// ProjectID is the project this team belongs to, or nil when it is shared
	// by every project. A team carries prompts, models and arguments chosen for
	// one repository as often as not, and those have no business in another
	// repository's picker, let alone changing under it when the first one is
	// edited.
	ProjectID *string          `json:"projectId"`
	Roles     []TeamPresetRole `json:"roles"`
	CreatedAt time.Time        `json:"createdAt"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

type TeamPresetRole struct {
	TemplateID string `json:"templateId"`
	Position   int    `json:"position"`
	Enabled    bool   `json:"enabled"`
	RoleOverrides
}

// ProjectTeam is the resolved team plus enough source information for a client
// to reset either topology or fields back to live preset defaults.
type ProjectTeam struct {
	PresetID *string        `json:"presetId"`
	Roles    []ResolvedRole `json:"roles"`
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
	RoleOverrides
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
	// Empty is the pipeline, so every role that existed before purpose did
	// keeps working without being rewritten.
	if t.Purpose == "" {
		t.Purpose = PurposePipeline
	}
	if t.Purpose != PurposePipeline && t.Purpose != PurposeRunner {
		return invalid("role %q has purpose %q, want %q or %q",
			t.Name, t.Purpose, PurposePipeline, PurposeRunner)
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
