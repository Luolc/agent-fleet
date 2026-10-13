// `fleet prompt show`: the texts fleet starts agents with and the prompts
// of the watches, as the scope's config makes them, where each part comes
// from, and whether a replacement is behind the built-in text it started
// from.

package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
	"github.com/Luolc/agent-fleet/internal/identity"
)

// PromptAbout, PromptShowAbout and PromptShowLongAbout are the help texts
// of `prompt` and `prompt show`.
const (
	PromptAbout         = "Show the prompts fleet starts agents with (read-only)"
	PromptShowAbout     = "Show the prompts as the scope's config makes them, one or all (read-only)"
	PromptShowLongAbout = "Show the prompts fleet starts agents with, as the scope's config makes them (read-only).\n\n" +
		"Without NAME: for each of common, thread, lead and worker, its " +
		"parts (the built-in text or the file replacing it, and the file following it) and, for a " +
		"replacement, whether its " +
		"`base` still is the built-in text. When it is not, the replacement is behind, and two commands " +
		"follow: one shows what changed in the built-in text, the other records the new base once the " +
		"replacement has taken the change in. A replacement without a base cannot be checked. Then, for " +
		"each of the scope's watches (the defaults and those of `watches`), a line `watch:<name>`: its " +
		"prompt's source (the built-in text, the file `watches.<name>.prompt` names, or none), or `off`. A note " +
		"follows when you can write the config file, its directory, a file it names or that file's " +
		"directory: so can an agent running as you. No note does not make them safe from such an agent: " +
		"a directory can still be renamed away by whoever can write its parent, and another put in its " +
		"place. Run it after every change to the config and after " +
		"every upgrade of fleet.\n\n" +
		"With NAME: that text put together, each part under a line `==> <source> <==`; the texts of thread " +
		"agents, leads and workers have `common` and its addition between their own text and its addition. " +
		"The model slots ({{lead_model}}, say), which only files written for an older fleet use, are filled " +
		"from the config's `models` (`opus` and `high` where it gives none); every other slot stays as " +
		"written ({{job}}, say), since its value is known only when an agent starts. Notes go to stderr: " +
		"the model slots the files use, the built-in text's slots a replacement does not use, and whether " +
		"it is behind.\n\n" +
		"With NAME watch:<name>: that watch's prompt under its `==> <source> <==` line, its slots as " +
		"written ({{key}}, {{text}}, {{question_file}}), since they are filled for each subject; on stderr, " +
		"a note when the watch is off or has no prompt.\n\n" +
		"With NAME and --builtin: fleet's built-in text alone, byte for byte, and nothing else on " +
		"stdout, so it can be redirected to a base file; for watch:<name>, a default watch's. The config " +
		"is not read.\n\n" +
		"Exit: 0; 1 for an unknown NAME, an invalid config, or a text that cannot be used (a file " +
		"missing, unreadable or empty, or with a slot its text does not have), each listed."
)

// watchPrefix starts the NAME of a watch's prompt, `watch:<name>`, as the
// ledger keys the watch's runs; no name of config.PromptNames has it.
const watchPrefix = "watch:"

// PromptShowArgs are the arguments of `prompt show`.
type PromptShowArgs struct {
	// Name is the text to show, one of config.PromptNames or a watch's
	// `watch:<name>`; nil for all.
	Name *string
	// Builtin asks for the built-in text alone.
	Builtin bool
}

// PromptShow runs `prompt show`, writing the text or the overview to
// stdout and the notes on one text to stderr.
func PromptShow(stdout, stderr io.Writer, args PromptShowArgs) (exit.Code, error) {
	watch, isWatch := "", false
	if args.Name != nil {
		watch, isWatch = strings.CutPrefix(*args.Name, watchPrefix)
		if !isWatch && !slices.Contains(config.PromptNames, *args.Name) {
			return 0, exit.Refusedf("no prompt %q; the names are %s, and watch:<name> for a watch's", *args.Name,
				strings.Join(config.PromptNames, ", "))
		}
	}
	if args.Builtin {
		if args.Name == nil {
			return 0, exit.Refusedf("--builtin needs a NAME")
		}
		if !isWatch {
			_, err := io.WriteString(stdout, specOf(*args.Name).builtin)
			return exit.Ok, err
		}
		text, err := builtinWatch(watch)
		if err != nil {
			return 0, err
		}
		_, err = io.WriteString(stdout, text)
		return exit.Ok, err
	}
	scope, err := identity.Scope()
	if err != nil {
		return 0, err
	}
	sc, err := config.LoadScope(scope)
	if err != nil {
		return 0, err
	}
	if isWatch {
		return showWatch(stdout, stderr, sc, watch)
	}
	if args.Name != nil {
		return showOne(stdout, stderr, sc, *args.Name)
	}
	path, err := config.ScopePath(scope)
	if err != nil {
		return 0, err
	}
	return showAll(stdout, sc, path)
}

// showOne writes the text `name` with its parts labelled.
func showOne(stdout, stderr io.Writer, sc *config.Scope, name string) (exit.Code, error) {
	p, err := loadPrompt(sc, name)
	if err != nil {
		return 0, err
	}
	var out strings.Builder
	for i, part := range p.Parts {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "==> %s <==\n", partLabel(sc, part))
		out.WriteString((&promptText{Parts: []promptPart{part}}).fill(modelValues(sc)))
	}
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return 0, err
	}
	if used := usedModelSlots(p); len(used) > 0 {
		fmt.Fprintf(stderr, "note: {{%s}} only keeps a file written for an older fleet rendering as before; "+
			"`job start` and `spawn` pass `models` as their default --model and --effort\n", strings.Join(used, "}} {{"))
	}
	conf := sc.Prompts[name]
	if conf.Replace == "" {
		return exit.Ok, nil
	}
	if unused := unusedSlots(p); len(unused) > 0 {
		fmt.Fprintf(stderr, "note: the replacement does not use {{%s}}, which the built-in text has\n",
			strings.Join(unused, "}} {{"))
	}
	fmt.Fprint(stderr, "note: "+baseState(sc, name, conf.Base))
	return exit.Ok, nil
}

// builtinWatch is the built-in prompt of the default watch `name`.
func builtinWatch(name string) (string, error) {
	var defaults []string
	for _, e := range config.DefaultWatches() {
		if e.Name == name {
			return builtinWatchPrompt(e.Source), nil
		}
		defaults = append(defaults, watchPrefix+e.Name)
	}
	return "", exit.Refusedf("%s%s has no built-in prompt; the default watches' are %s", watchPrefix, name,
		strings.Join(defaults, ", "))
}

// showWatch writes the prompt of the scope's watch `name` under its
// source, its slots as written, noting on stderr a watch that is off or
// has no prompt.
func showWatch(stdout, stderr io.Writer, sc *config.Scope, name string) (exit.Code, error) {
	i := slices.IndexFunc(sc.Watches, func(e config.Entry) bool { return e.Name == name })
	if i < 0 {
		names := make([]string, len(sc.Watches))
		for i, e := range sc.Watches {
			names[i] = watchPrefix + e.Name
		}
		return 0, exit.Refusedf("no watch %q in the scope; its watches are %s", name, strings.Join(names, ", "))
	}
	e := sc.Watches[i]
	p, err := watchPrompt(sc, e)
	if err != nil {
		return 0, err
	}
	if e.Off {
		fmt.Fprintln(stderr, "note: the watch is off: `watch` does not run it")
	}
	if p == nil {
		fmt.Fprintln(stderr, "note: the watch has no prompt: its messages carry the subject's text without one")
		return exit.Ok, nil
	}
	_, err = fmt.Fprintf(stdout, "==> %s <==\n%s", partLabel(sc, p.Parts[0]), p.Parts[0].Text)
	return exit.Ok, err
}

// showAll writes, for each text, its parts and whether it is behind, then
// for each watch where its prompt comes from, then a note when the config
// can be written; with a text that cannot be used, it lists each and
// exits 1.
func showAll(stdout io.Writer, sc *config.Scope, path string) (exit.Code, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "config: %s", sc.Tilde(path))
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		out.WriteString(" (no such file: every text is built-in)")
	}
	out.WriteString("\n")
	broken := 0
	for _, name := range config.PromptNames {
		p, err := loadPrompt(sc, name)
		if err != nil {
			var bad *promptError
			if !errors.As(err, &bad) {
				return 0, err
			}
			broken++
			fmt.Fprintf(&out, "%s: error: %s\n", name, bad.Error())
			continue
		}
		labels := make([]string, len(p.Parts))
		for i, part := range p.Parts {
			labels[i] = partLabel(sc, part)
		}
		fmt.Fprintf(&out, "%s: %s\n", name, strings.Join(labels, " + "))
		if conf := sc.Prompts[name]; conf.Replace != "" {
			out.WriteString("  " + strings.ReplaceAll(baseState(sc, name, conf.Base), "\n  ", "\n    "))
		}
	}
	for _, e := range sc.Watches {
		line := "off"
		if !e.Off {
			p, err := watchPrompt(sc, e)
			var bad *promptError
			switch {
			case errors.As(err, &bad):
				broken++
				line = "error: " + bad.Error()
			case err != nil:
				return 0, err
			case p == nil:
				line = "no prompt"
			default:
				line = partLabel(sc, p.Parts[0])
			}
		}
		fmt.Fprintf(&out, "%s%s: %s\n", watchPrefix, e.Name, line)
	}
	if writable := writableBy(path, sc); len(writable) > 0 {
		fmt.Fprintf(&out, "note: you can write %s: so can an agent running as you, and change the rules of every agent started after it\n",
			strings.Join(writable, ", "))
	}
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return 0, err
	}
	if broken > 0 {
		return 0, exit.Refusedf("%d of the texts cannot be used, listed above: an agent that needs one is not started, "+
			"a watch that needs one is skipped", broken)
	}
	return exit.Ok, nil
}

// partLabel says where a part comes from: `built-in` (`built-in common`
// for the shared text in a role's), or its config key and file.
func partLabel(sc *config.Scope, part promptPart) string {
	if part.Path == "" {
		return part.Source
	}
	return fmt.Sprintf("%s (%s)", part.Source, sc.Tilde(part.Path))
}

// baseState says whether the replacement of `name` is behind: its base,
// the copy of the built-in text it started from, compared byte for byte
// with the built-in text now. Behind, two indented commands follow. A
// loaded text's base, when given, is readable.
func baseState(sc *config.Scope, name, base string) string {
	if base == "" {
		return "no base: which built-in text the replacement started from is unknown\n"
	}
	data, err := os.ReadFile(base)
	if err != nil {
		return fmt.Sprintf("base (%s) cannot be read: %v\n", sc.Tilde(base), err)
	}
	if bytes.Equal(data, []byte(specOf(name).builtin)) {
		return fmt.Sprintf("up to date: base (%s) is the built-in text\n", sc.Tilde(base))
	}
	word := shellWord(sc, base)
	return fmt.Sprintf("behind: the built-in text has changed since base (%s). See what changed, take it into the replacement, then record the new base:\n"+
		"  diff -u %s <(fleet prompt show %s --builtin)\n"+
		"  fleet prompt show %s --builtin > %s\n", sc.Tilde(base), word, name, name, word)
}

// safeWord is a path a shell takes as one word, `~/` expanded.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9_./~+@%:,=-]+$`)

// shellWord is `path` as one shell word: as the prompts show it when that
// needs no quotes, else absolute and single-quoted.
func shellWord(sc *config.Scope, path string) string {
	if shown := sc.Tilde(path); safeWord.MatchString(shown) {
		return shown
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// unusedSlots are the slots of the built-in text `p` replaces that the
// replacement does not use, sorted; an addition does not count.
func unusedSlots(p *promptText) []string {
	used := map[string]bool{}
	for _, m := range slotPattern.FindAllStringSubmatch(p.Parts[0].Text, -1) {
		used[m[1]] = true
	}
	var unused []string
	for _, m := range slotPattern.FindAllStringSubmatch(specOf(p.Name).builtin, -1) {
		if !used[m[1]] && !slices.Contains(unused, m[1]) {
			unused = append(unused, m[1])
		}
	}
	slices.Sort(unused)
	return unused
}

// usedModelSlots are the model slots the config's files for `p` use, in
// the order of modelSlots.
func usedModelSlots(p *promptText) []string {
	var used []string
	for _, slot := range modelSlots {
		for _, part := range p.Parts {
			if part.Path != "" && strings.Contains(part.Text, "{{"+slot+"}}") {
				used = append(used, slot)
				break
			}
		}
	}
	return used
}

// writableBy lists, as the prompts show them, the config file, the prompt
// files it names (the watches' too) and their directories that the
// current user can write.
func writableBy(path string, sc *config.Scope) []string {
	paths := []string{path}
	for _, name := range config.PromptNames {
		conf := sc.Prompts[name]
		paths = append(paths, conf.Replace, conf.Append, conf.Base)
	}
	for _, e := range sc.Watches {
		paths = append(paths, e.Prompt)
	}
	var seen, writable []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		for _, q := range []string{filepath.Dir(p), p} {
			if slices.Contains(seen, q) {
				continue
			}
			seen = append(seen, q)
			// W_OK; a path that does not exist is not listed.
			if syscall.Access(q, 0x2) == nil {
				writable = append(writable, sc.Tilde(q))
			}
		}
	}
	return writable
}
