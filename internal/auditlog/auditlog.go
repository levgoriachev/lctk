// Package auditlog records what LCTK executed on the user's behalf.
//
// It exists because the runner is the one part of LCTK that runs somebody's code
// rather than reading it. When a command has done something surprising, the
// question is always "what actually ran, when, and what happened" — and the
// answer has to survive the daemon that produced it.
//
// One line per run, appended, never rewritten. Append-only is not tamper
// resistance, which a local file cannot offer against its owner; it is the much
// smaller property that a later entry does not disturb an earlier one, so the
// record reads in the order things happened.
package auditlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

// DirName is the LCTK home subdirectory holding one log per project.
const DirName = "audit"

// MaxOutputExcerpt bounds what of a command's output is kept.
//
// The log is a record of what happened, not a copy of every build. The tail is
// what is kept, because a command that failed says why at the end.
const MaxOutputExcerpt = 4 << 10

// Entry is one executed command.
type Entry struct {
	At        time.Time `json:"at"`
	ProjectID string    `json:"project_id"`
	// Name is the approved command's name, and Command the text that ran. Both
	// are recorded: the name is what the client asked for, and the text is what
	// actually happened, and a reader wants to see that the two agree.
	Name    string `json:"name"`
	Command string `json:"command"`
	Image   string `json:"image"`
	Network string `json:"network"`
	// Client is the OAuth authorization the request arrived on, so a run can be traced back to
	// which client asked for it.
	Client          string  `json:"client,omitempty"`
	AuthorizationID string  `json:"authorization_id,omitempty"`
	ExitCode        int     `json:"exit_code"`
	TimedOut        bool    `json:"timed_out,omitempty"`
	Seconds         float64 `json:"seconds"`
	// Output is the tail of what the command printed, bounded. Nothing else from
	// the run is stored: no environment, and no credential, because the runner
	// passes neither.
	Output string `json:"output,omitempty"`
	// Refused records a run that never happened and why, which is as much a part
	// of the record as one that did.
	Refused string `json:"refused,omitempty"`
}

// Log appends entries for one machine.
type Log struct {
	dir string

	mu sync.Mutex
}

// New returns a log writing under the LCTK home.
func New() (*Log, error) {
	home, err := lctkhome.Dir()
	if err != nil {
		return nil, err
	}
	return &Log{dir: filepath.Join(home, DirName)}, nil
}

// NewAt returns a log writing to a specific directory.
func NewAt(dir string) *Log { return &Log{dir: dir} }

// Path is where a project's entries are written.
func (l *Log) Path(projectID string) string {
	return filepath.Join(l.dir, projectID+".jsonl")
}

// Append records one entry.
//
// A failure to write is returned rather than swallowed, but the caller is
// expected to report it and carry on: losing the log is bad, and refusing to run
// anything because the log is unwritable would be worse.
func (l *Log) Append(entry Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("create %q: %w", l.dir, err)
	}
	if entry.At.IsZero() {
		entry.At = time.Now()
	}
	entry.At = entry.At.UTC()
	entry.Output = tail(entry.Output, MaxOutputExcerpt)

	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode the audit entry: %w", err)
	}

	path := l.Path(entry.ProjectID)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open the audit log %q: %w", path, err)
	}
	defer file.Close()

	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write the audit log %q: %w", path, err)
	}
	return nil
}

// Recent reads the last entries for a project, oldest first.
func (l *Log) Recent(projectID string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	raw, err := os.ReadFile(l.Path(projectID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the audit log: %w", err)
	}

	var entries []Entry
	for line := range splitLines(string(raw)) {
		var entry Entry
		if json.Unmarshal([]byte(line), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// splitLines yields non-empty lines. A truncated final line, which a crash
// mid-write could leave, simply fails to parse and is skipped.
func splitLines(text string) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(text); i++ {
			if text[i] != '\n' {
				continue
			}
			if line := trim(text[start:i]); line != "" && !yield(line) {
				return
			}
			start = i + 1
		}
		if line := trim(text[start:]); line != "" {
			yield(line)
		}
	}
}

func trim(line string) string {
	for len(line) > 0 && (line[len(line)-1] == '\r' || line[len(line)-1] == ' ') {
		line = line[:len(line)-1]
	}
	return line
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}
