// Package hostsettings reads the machine-wide defaults a user may adjust.
//
// These are host policy, not project policy. A repository can propose a project
// override where one is meaningful — see projectmanifest — but the ceiling, the
// floor, and everything that costs the machine resources are decided here, in a
// file outside any repository, for the same reason the registry is: a repository
// author must not be able to change how much of the machine LCTK uses.
//
// The file is optional. A missing settings document is the normal state and
// yields the shipped defaults, so nothing has to be created before LCTK works.
package hostsettings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

// FileName is the settings document inside the LCTK home directory.
const FileName = "settings.json"

// SchemaVersion is the layout version this build writes and understands.
const SchemaVersion = 1

// Bounds on the debounce window.
//
// The floor exists because an editor save is not one filesystem operation: a
// write to a temporary file and a rename over the original is the common pattern,
// and reacting between the two indexes a file that is about to be replaced. The
// ceiling exists because the whole point of watching is that an agent's next
// question sees the edit it just made; past a minute the user would be better
// served by asking for a reindex.
const (
	MinDebounce = 200 * time.Millisecond
	MaxDebounce = 60 * time.Second
)

// Bounds on how long a continuously edited project may defer its update. Without
// a ceiling, a save every two seconds would restart the timer forever and the
// index would never advance.
const (
	MinSettleCeiling = time.Second
	MaxSettleCeiling = 10 * time.Minute
)

// Watch is the host's watcher and freshness policy.
type Watch struct {
	// DebounceMS is how long to wait after the most recent change before an
	// update is considered settled. Each new change restarts the wait.
	DebounceMS int `json:"debounce_ms"`
	// MaxDebounceMS caps how long a batch may be deferred by continuous editing.
	MaxDebounceMS int `json:"max_debounce_ms"`
	// MaxWatchedDirectories bounds the native watches one project may hold. Past
	// the bound the watcher reports a gap and the project falls back to
	// reconciliation rather than exhausting the process's handles.
	MaxWatchedDirectories int `json:"max_watched_directories"`
	// IdleStopSeconds is how long a project may go without a change or a request
	// before its watcher is released. Watching costs a handle per directory, and
	// a project nobody is using should not hold them.
	IdleStopSeconds int `json:"idle_stop_seconds"`
}

// Mode is the background-load policy: how much of the machine LCTK may spend on
// work the user did not directly ask for.
type Mode string

const (
	// ModeQuiet keeps indexing out of the way of interactive work.
	ModeQuiet Mode = "quiet"
	// ModeNormal is the shipped balance.
	ModeNormal Mode = "normal"
	// ModeFast finishes indexing as quickly as the machine allows.
	ModeFast Mode = "fast"
)

// Valid reports whether a mode is one this build understands. The empty mode is
// valid and means "use the layer above": machine policy for a project, the
// shipped default for the machine.
func (m Mode) Valid() bool {
	switch m {
	case "", ModeQuiet, ModeNormal, ModeFast:
		return true
	}
	return false
}

// Resources is the host's background-load policy.
type Resources struct {
	Mode Mode `json:"mode"`
	// MemoryLimitMB caps the memory a project container may use. Zero means no
	// cap, which is the shipped default and a deliberate one: a memory limit does
	// not slow an indexer down, it kills it, and the right limit depends on the
	// project rather than on the mode. An operator who wants the guarantee can
	// set it; nobody gets it by accident.
	MemoryLimitMB int `json:"memory_limit_mb"`
}

// Budget is what a mode actually costs, in terms the container runtime and the
// index service understand.
type Budget struct {
	// CPUs limits the project container. Zero means no limit.
	CPUs float64
	// IndexParallelism bounds concurrent index work inside the service. Zero
	// leaves the engine's own default, which scales with the container's CPUs.
	IndexParallelism int
	// MemoryLimitMB caps container memory. Zero means no cap.
	MemoryLimitMB int
}

// Budget resolves the policy into numbers.
//
// CPU is limited rather than memory because the two fail differently. A CPU
// limit throttles: indexing takes longer and the machine stays usable. A memory
// limit kills: the container is terminated mid-build and the index is no better
// off than before. So the mode controls CPU, and memory is opt-in.
func (r Resources) Budget() Budget {
	budget := Budget{MemoryLimitMB: r.MemoryLimitMB}
	switch r.Mode {
	case ModeQuiet:
		budget.CPUs = 1
		budget.IndexParallelism = 1
	case ModeFast:
		// No CPU limit and no parallelism cap: the engine sizes itself to what
		// the container is allowed to use, which here is everything.
	default:
		budget.CPUs = 2
		budget.IndexParallelism = 2
	}
	return budget
}

// Settings is the whole document.
type Settings struct {
	SchemaVersion int       `json:"schema_version"`
	Watch         Watch     `json:"watch"`
	Resources     Resources `json:"resources"`
	// ExtendedCodeTools also serves the semantic and graph-traversal MCP tools.
	// Off by default: the default surface is the five exploration tools
	// ADR-0031 settled on, and every extra tool taxes every session's catalog.
	ExtendedCodeTools bool `json:"extended_code_tools,omitempty"`
}

// Defaults are the shipped policy.
//
// Three seconds is the accepted debounce default: it is long enough to absorb the
// write-then-rename an editor performs on save and the burst a formatter produces
// across a package, and short enough that an agent asking a question right after
// an edit does not notice the wait.
var Defaults = Settings{
	SchemaVersion: SchemaVersion,
	Watch: Watch{
		DebounceMS:            3000,
		MaxDebounceMS:         30000,
		MaxWatchedDirectories: 20000,
		IdleStopSeconds:       900,
	},
	Resources: Resources{Mode: ModeNormal},
}

// Path returns the settings document without creating anything.
func Path() (string, error) {
	dir, err := lctkhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the settings, returning the shipped defaults when no file exists.
//
// A malformed document is an error rather than a silent fallback. Unlike a change
// journal, this file is something a person wrote on purpose, and quietly ignoring
// it would leave them believing a setting applies when it does not.
func Load() (Settings, error) {
	path, err := Path()
	if err != nil {
		return Defaults, err
	}
	return LoadFrom(path)
}

// LoadFrom reads a specific settings document.
func LoadFrom(path string) (Settings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Defaults, nil
		}
		return Defaults, fmt.Errorf("read settings %q: %w", path, err)
	}

	var document Settings
	if err := json.Unmarshal(raw, &document); err != nil {
		return Defaults, fmt.Errorf("settings %q is not valid JSON: %w", path, err)
	}
	if document.SchemaVersion > SchemaVersion {
		return Defaults, fmt.Errorf("settings %q use schema version %d, this build understands %d",
			path, document.SchemaVersion, SchemaVersion)
	}

	merged := Defaults
	if document.Watch.DebounceMS != 0 {
		merged.Watch.DebounceMS = document.Watch.DebounceMS
	}
	if document.Watch.MaxDebounceMS != 0 {
		merged.Watch.MaxDebounceMS = document.Watch.MaxDebounceMS
	}
	if document.Watch.MaxWatchedDirectories != 0 {
		merged.Watch.MaxWatchedDirectories = document.Watch.MaxWatchedDirectories
	}
	if document.Watch.IdleStopSeconds != 0 {
		merged.Watch.IdleStopSeconds = document.Watch.IdleStopSeconds
	}
	if document.Resources.Mode != "" {
		if !document.Resources.Mode.Valid() {
			return Defaults, fmt.Errorf("settings %q name an unknown resource mode %q: expected quiet, normal, or fast",
				path, document.Resources.Mode)
		}
		merged.Resources.Mode = document.Resources.Mode
	}
	if document.Resources.MemoryLimitMB != 0 {
		if document.Resources.MemoryLimitMB < 0 {
			return Defaults, fmt.Errorf("settings %q set a negative memory limit", path)
		}
		merged.Resources.MemoryLimitMB = document.Resources.MemoryLimitMB
	}
	return merged, nil
}

// WithProjectMode applies a project's own mode on top of the machine policy.
//
// Unlike the debounce proposal, this does not come from the repository. It is set
// by the machine owner per project, because how much of their machine a project
// may use is theirs to decide and a repository author's to have no say in.
func (r Resources) WithProjectMode(mode Mode) Resources {
	if mode == "" || !mode.Valid() {
		return r
	}
	r.Mode = mode
	return r
}

// Debounce is the settled window, clamped into the supported range.
func (w Watch) Debounce() time.Duration {
	return clamp(time.Duration(w.DebounceMS)*time.Millisecond, MinDebounce, MaxDebounce)
}

// SettleCeiling is the longest a batch may be deferred by continuous editing. It
// is never shorter than the debounce window, because a ceiling below the window
// would settle every single change immediately and make the window meaningless.
func (w Watch) SettleCeiling() time.Duration {
	ceiling := clamp(time.Duration(w.MaxDebounceMS)*time.Millisecond, MinSettleCeiling, MaxSettleCeiling)
	if debounce := w.Debounce(); ceiling < debounce {
		return debounce
	}
	return ceiling
}

// IdleStop is how long a quiet project keeps its watcher.
func (w Watch) IdleStop() time.Duration {
	return clamp(time.Duration(w.IdleStopSeconds)*time.Second, 30*time.Second, 24*time.Hour)
}

// WithProjectDebounce applies a project's own debounce proposal.
//
// A project may ask for a different window because it knows its own editing
// shape: a generated-code-heavy repository wants a longer one, a small one wants
// a shorter one. It cannot escape the host's bounds, which is why this clamps
// rather than assigns.
func (w Watch) WithProjectDebounce(milliseconds int) Watch {
	if milliseconds <= 0 {
		return w
	}
	w.DebounceMS = int(clamp(
		time.Duration(milliseconds)*time.Millisecond, MinDebounce, MaxDebounce).Milliseconds())
	return w
}

func clamp(value, low, high time.Duration) time.Duration {
	switch {
	case value < low:
		return low
	case value > high:
		return high
	default:
		return value
	}
}
