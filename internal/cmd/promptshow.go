// `fleet prompt show`: the texts fleet starts agents with, as the scope's
// config makes them, where each part comes from, and whether a replacement
// is behind the built-in text it started from.

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
		"Without NAME: for each of thread, lead, worker, revisit, unblock and screen_guidance, its " +
		"parts (the built-in text or the file replacing it, and the file following it) and, for a " +
		"replacement, whether its " +
		"`base` still is the built-in text. When it is not, the replacement is behind, and two commands " +
		"follow: one shows what changed in the built-in text, the other records the new base once the " +
		"replacement has taken the change in. A replacement without a base cannot be checked. A note " +
		"follows when you can write the config file, its directory, a file it names or that file's " +
		"directory: so can an agent running as you. No note does not make them safe from such an agent: " +
		"a directory can still be renamed away by whoever can write its parent, and another put in its " +
		"place. Run it after every change to the config and after " +
		"every upgrade of fleet.\n\n" +
		"With NAME: that text put together, each part under a line `==> <source> <==`. The model slots " +
		"are filled from the config's `models`; every other slot stays as written ({{job}}, say), since " +
		"its value is known only when an agent starts. Notes go to stderr: the built-in text's slots a " +
		"replacement does not use, and whether it is behind.\n\n" +
		"With NAME and --builtin: fleet's built-in text alone, byte for byte, and nothing else on " +
		"stdout, so it can be redirected to a base file. The config is not read.\n\n" +
		"Exit: 0; 1 for an unknown NAME, an invalid config, or a text that cannot be used (a file " +
		"missing, unreadable or empty, or with a slot its text does not have), each listed."
)

// PromptShowArgs are the arguments of `prompt show`.
type PromptShowArgs struct {
	// Name is the text to show, one of config.PromptNames; nil for all.
	Name *string
	// Builtin asks for the built-in text alone.
	Builtin bool
}

// PromptShow runs `prompt show`, writing the text or the overview to
// stdout and the notes on one text to stderr.
func PromptShow(stdout, stderr io.Writer, args PromptShowArgs) (exit.Code, error) {
	if args.Name != nil && !slices.Contains(config.PromptNames, *args.Name) {
		return 0, exit.Refusedf("no prompt %q; the names are %s", *args.Name, strings.Join(config.PromptNames, ", "))
	}
	if args.Builtin {
		if args.Name == nil {
			return 0, exit.Refusedf("--builtin needs a NAME")
		}
		_, err := io.WriteString(stdout, specOf(*args.Name).builtin)
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

// showAll writes, for each text, its parts and whether it is behind, then
// a note when the config can be written; with a text that cannot be used,
// it lists each and exits 1.
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
	if writable := writableBy(path, sc); len(writable) > 0 {
		fmt.Fprintf(&out, "note: you can write %s: so can an agent running as you, and change the rules of every agent started after it\n",
			strings.Join(writable, ", "))
	}
	if _, err := io.WriteString(stdout, out.String()); err != nil {
		return 0, err
	}
	if broken > 0 {
		return 0, exit.Refusedf("%d of the texts cannot be used, listed above: an agent that needs one is not started", broken)
	}
	return exit.Ok, nil
}

// partLabel says where a part comes from: `built-in`, or its config key
// and file.
func partLabel(sc *config.Scope, part promptPart) string {
	if part.Source == "built-in" {
		return "built-in"
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

// writableBy lists, as the prompts show them, the config file, the prompt
// files it names and their directories that the current user can write.
func writableBy(path string, sc *config.Scope) []string {
	paths := []string{path}
	for _, name := range config.PromptNames {
		conf := sc.Prompts[name]
		paths = append(paths, conf.Replace, conf.Append, conf.Base)
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
