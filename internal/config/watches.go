package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Entry is one watch of `watches.<name>`: where its subjects come from
// and what `watch` does about each.
type Entry struct {
	Name string
	// Source is the built-in source (one of BuiltinSources), or "" for a
	// command source.
	Source string
	// Command is the program and its arguments of a command source; none
	// for a `worker` watch without a source, a timer whose one subject is
	// the watch itself.
	Command []string
	// Every is the least time between two runs of the source; 0 is each
	// run of `watch`.
	Every time.Duration
	// Do is what is done about a subject: `tell` or `worker`.
	Do string
	// Prompt is the file that goes with the text; "" for the built-in
	// one of a default, none for a command source.
	Prompt string
	// Model and Effort are the --model and --effort of a `worker`
	// watch's starts; "" passes none.
	Model  string
	Effort string
	// Env are the variables a command source gets beyond the runner's
	// allow-list, by name.
	Env []string
	// PerRun bounds the acts of one run; Again is the least time between
	// two acts on one subject (0: act again only when its text changed);
	// Max bounds the acts on one subject, after which the people are asked
	// once instead (0: no bound).
	PerRun int
	Again  time.Duration
	Max    int
	Off    bool
}

// The built-in sources, and the defaults that use them: the defaults are
// the only entries a built-in source may have, since each keeps its state
// in the ledger's own columns and rows.
const (
	StaleAgents    = "stale-agents"
	QuietThreads   = "quiet-threads"
	BlockedScreens = "blocked-screens"
)

// BuiltinSources maps each built-in source to its default entry.
var BuiltinSources = map[string]string{StaleAgents: "stale-workers", QuietThreads: "quiet-threads",
	BlockedScreens: "unblock"}

// DefaultWatches are the watches of a scope without `watches`.
func DefaultWatches() []Entry {
	return []Entry{{Name: "quiet-threads", Source: QuietThreads, Do: "tell"},
		{Name: "stale-workers", Source: StaleAgents, Do: "tell"},
		{Name: "unblock", Source: BlockedScreens, Do: "worker"}}
}

// watchFile is one `watches.<name>` as written. Source is a string or an
// object, so it is read apart.
type watchFile struct {
	Source json.RawMessage `json:"source"`
	Every  *string         `json:"every"`
	Do     *string         `json:"do"`
	Prompt *string         `json:"prompt"`
	Env    *[]string       `json:"env"`
	PerRun *int            `json:"per_run"`
	Again  *string         `json:"again"`
	Max    *int            `json:"max"`
	Off    *bool           `json:"off"`
	Model  *string         `json:"model"`
	Effort *string         `json:"effort"`
}

var (
	watchName = regexp.MustCompile(`^[a-z0-9-]+$`)
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// watches reads `watches` onto the defaults, in name order: an entry named
// like a default changes only the keys it gives, which for a built-in
// source are `every`, `prompt` and `off`. `~/` in a prompt path is
// expanded to `home`; a relative one stays relative until LoadScope
// resolves it.
func watches(files map[string]*watchFile, home string) ([]Entry, error) {
	out := DefaultWatches()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		key := "watches." + name
		if !watchName.MatchString(name) {
			return nil, errors.New(key + ": a name is made of [a-z0-9-]")
		}
		i := slices.IndexFunc(out, func(e Entry) bool { return e.Name == name })
		if i < 0 {
			out = append(out, Entry{Name: name})
			i = len(out) - 1
		}
		if err := files[name].apply(key, &out[i], home); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// apply sets the keys given in f onto e.
func (f *watchFile) apply(key string, e *Entry, home string) error {
	builtin := e.Source != ""
	if err := f.source(key, e); err != nil {
		return err
	}
	if f.Every != nil {
		d, err := positive(key+".every", *f.Every)
		if err != nil {
			return err
		}
		e.Every = d
	}
	if f.Prompt != nil {
		if *f.Prompt == "" {
			return errors.New(key + ".prompt: must not be empty; leave the key out for the default")
		}
		e.Prompt = *f.Prompt
		if rest, ok := strings.CutPrefix(e.Prompt, "~/"); ok {
			e.Prompt = filepath.Join(home, rest)
		}
	}
	if f.Off != nil {
		e.Off = *f.Off
	}
	if builtin {
		for _, given := range []struct {
			name string
			set  bool
		}{{"do", f.Do != nil}, {"env", f.Env != nil}, {"per_run", f.PerRun != nil}, {"again", f.Again != nil},
			{"max", f.Max != nil}, {"model", f.Model != nil}, {"effort", f.Effort != nil}} {
			if given.set {
				return fmt.Errorf("%s.%s: a built-in source takes only every, prompt and off", key, given.name)
			}
		}
		return nil
	}
	return f.command(key, e)
}

// source reads `source`: a default's own built-in source may be named
// again, any other built-in one is refused, and an object is a command.
// None is a command watch's only with `do: worker` (command checks).
func (f *watchFile) source(key string, e *Entry) error {
	if f.Source == nil {
		return nil
	}
	var name string
	if json.Unmarshal(f.Source, &name) == nil {
		switch {
		case name == e.Source:
			return nil
		case BuiltinSources[name] != "":
			return fmt.Errorf("%s.source: %s is the source of the default %s; change that entry instead", key, name,
				BuiltinSources[name])
		}
		return fmt.Errorf("%s.source: unknown source %q; the built-in ones are stale-agents, quiet-threads and "+
			"blocked-screens, or give {\"command\": [...]}", key, name)
	}
	if e.Source != "" {
		return fmt.Errorf("%s.source: the default %s keeps its source %s", key, e.Name, e.Source)
	}
	var command struct {
		Command []string `json:"command"`
	}
	if err := decodeStrict(f.Source, &command); err != nil {
		return fmt.Errorf("%s.source: must be a built-in source's name or {\"command\": [...]}: %v", key, err)
	}
	if len(command.Command) == 0 || command.Command[0] == "" {
		return errors.New(key + ".source.command: must name a program, then its arguments")
	}
	e.Command = command.Command
	return nil
}

// command reads the keys of a command source's entry.
func (f *watchFile) command(key string, e *Entry) error {
	if f.Do == nil {
		return errors.New(key + ".do: must be given: tell or worker")
	}
	switch *f.Do {
	case "tell", "worker":
		e.Do = *f.Do
	default:
		return fmt.Errorf("%s.do: unknown %q; tell and worker are the two", key, *f.Do)
	}
	if e.Command == nil && e.Do == "tell" {
		return errors.New(key + ".source: must be given: a watch with `do: tell` needs subjects to tell about")
	}
	if err := f.models(key, e); err != nil {
		return err
	}
	if f.Env != nil {
		for _, name := range *f.Env {
			if !envName.MatchString(name) {
				return fmt.Errorf("%s.env: %q is not a variable's name", key, name)
			}
		}
		e.Env = *f.Env
	}
	e.PerRun = 1
	for _, n := range []struct {
		name  string
		value *int
		into  *int
	}{{"per_run", f.PerRun, &e.PerRun}, {"max", f.Max, &e.Max}} {
		if n.value == nil {
			continue
		}
		if *n.value <= 0 {
			return fmt.Errorf("%s.%s: must be a positive number", key, n.name)
		}
		*n.into = *n.value
	}
	if f.Again != nil {
		d, err := positive(key+".again", *f.Again)
		if err != nil {
			return err
		}
		e.Again = d
	}
	return nil
}

// models reads `model` and `effort`, which only a `worker` watch takes.
func (f *watchFile) models(key string, e *Entry) error {
	for _, s := range []struct {
		name  string
		value *string
		into  *string
	}{{"model", f.Model, &e.Model}, {"effort", f.Effort, &e.Effort}} {
		if s.value == nil {
			continue
		}
		if e.Do != "worker" {
			return fmt.Errorf("%s.%s: only a watch with `do: worker` starts an agent", key, s.name)
		}
		if *s.value == "" {
			return fmt.Errorf("%s.%s: must not be empty; leave the key out for the default", key, s.name)
		}
		*s.into = *s.value
	}
	return nil
}

// positive parses one positive duration, naming the key on an error.
func positive(key, text string) (time.Duration, error) {
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 {
		return 0, errors.New(key + ": must be a positive duration such as 10m or 72h")
	}
	return d, nil
}

// resolveWatches makes the relative prompt paths of `entries` relative to
// `dir`.
func resolveWatches(entries []Entry, dir string) {
	for i := range entries {
		if p := entries[i].Prompt; p != "" && !filepath.IsAbs(p) {
			entries[i].Prompt = filepath.Join(dir, p)
		}
	}
}
