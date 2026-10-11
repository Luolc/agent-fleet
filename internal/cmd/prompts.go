// fleet's built-in texts as the scope's config changes them
// (docs/design.md): each text is the built-in one or the file replacing
// it, then the file following it, then, for a replaced thread, lead or
// worker prompt, fleet's safety default; its slots are filled when the
// agent starts.

package cmd

import (
	"crypto/sha256"
	_ "embed" // the safety default
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Luolc/agent-fleet/internal/config"
	"github.com/Luolc/agent-fleet/internal/exit"
)

// safetyPrompt follows a replaced thread, lead or worker prompt, so no
// replacement drops what needs a person's yes.
//
//go:embed safety_prompt.md
var safetyPrompt string

// slotPattern is a slot: lower-case letters and underscores in double
// braces. Anything else in braces is text.
var slotPattern = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)

// modelSlots are the slots of the models the thread prompt names for
// leads and the lead prompt for workers; the three prompts all have them.
var modelSlots = []string{"lead_model", "lead_effort", "worker_model", "worker_effort"}

// promptSpec is what fleet knows of one text: the built-in text, its
// slots, and whether a replacement is followed by the safety default.
type promptSpec struct {
	builtin string
	slots   []string
	safety  bool
}

// specOf is the spec of the text `name`, one of config.PromptNames.
func specOf(name string) promptSpec {
	role := append([]string{"job", "scope", "agent", "parent", "lead", "cwd", "repo_line", "issue", "issue_note",
		"linear", "agents_md", "cap", "scratch"}, modelSlots...)
	switch name {
	case "thread":
		return promptSpec{threadPrompt, append([]string{"scope", "agent", "mapping", "cwd", "rules", "xrepo", "checkouts",
			"ticket", "ticket_note", "post", "progress"}, modelSlots...), true}
	case "lead":
		return promptSpec{leadPrompt, role, true}
	case "worker":
		return promptSpec{workerPrompt, role, true}
	case "revisit":
		return promptSpec{revisitPrompt, []string{"agent", "scope", "dir", "issue", "unchanged", "create", "ask", "again"}, false}
	case "unblock":
		return promptSpec{unblockPrompt, []string{"agent", "scope", "dir", "blocked", "who", "cwd", "question_file", "ticket"}, false}
	case "screen_guidance":
		return promptSpec{screenGuidance, nil, false}
	}
	panic("no built-in text " + name)
}

// promptPart is one part of a text: fleet's own (Source `built-in` or
// `safety`) or a file the scope's config names (Source its key, as
// `prompts.lead.append`, and Path the file).
type promptPart struct {
	Source string
	Path   string
	Text   string
}

// promptText is a text put together, its slots checked, not yet filled.
type promptText struct {
	Name  string
	Parts []promptPart
}

// builtinText is the text `name` as fleet has it.
func builtinText(name string) *promptText {
	return &promptText{Name: name, Parts: []promptPart{{Source: "built-in", Text: specOf(name).builtin}}}
}

// fill is the text with its slots filled, its parts a blank line apart.
// A slot not in `slots` cannot be in a checked text.
func (p *promptText) fill(slots map[string]string) string {
	parts := make([]string, len(p.Parts))
	for i, part := range p.Parts {
		parts[i] = slotPattern.ReplaceAllStringFunc(part.Text, func(slot string) string {
			if value, ok := slots[slot[2:len(slot)-2]]; ok {
				return value
			}
			return slot
		})
	}
	return strings.Join(parts, "\n")
}

// promptError is a file the scope's config names for a text that cannot
// be used; exit 1.
type promptError struct {
	failure *exit.Failure
	// digest tells one mistake from another: the message and the file's
	// content.
	digest string
}

func (e *promptError) Error() string { return e.failure.Message }

func (e *promptError) Unwrap() error { return e.failure }

// brokenPrompt is the promptError of the file `path` given as `key`.
func brokenPrompt(key, path, content, format string, args ...any) *promptError {
	message := fmt.Sprintf("%s (%s): ", key, path) + fmt.Sprintf(format, args...)
	sum := sha256.Sum256([]byte(message + "\x00" + content))
	return &promptError{failure: exit.Refusedf("%s", message), digest: hex.EncodeToString(sum[:8])}
}

// loadPrompt puts the text `name` together from fleet's built-in text and
// the files the scope's config names for it: a file that is missing,
// unreadable or empty, or has a slot the text does not have, is a
// promptError. The base is only read, to refuse it the same way.
func loadPrompt(sc *config.Scope, name string) (*promptText, error) {
	spec, conf := specOf(name), sc.Prompts[name]
	p := builtinText(name)
	replace, err := readPromptFile(sc, name, "replace", conf.Replace, true)
	if err != nil {
		return nil, err
	}
	if replace != nil {
		p.Parts[0] = *replace
	}
	if _, err := readPromptFile(sc, name, "base", conf.Base, false); err != nil {
		return nil, err
	}
	addition, err := readPromptFile(sc, name, "append", conf.Append, true)
	if err != nil {
		return nil, err
	}
	if addition != nil {
		p.Parts = append(p.Parts, *addition)
	}
	if replace != nil && spec.safety {
		p.Parts = append(p.Parts, promptPart{Source: "safety", Text: safetyPrompt})
	}
	return p, nil
}

// readPromptFile is the part the file `path` makes of the text `name` as
// its `field` (nil when no path is given), its slots checked when
// `checked`: a base goes into no message.
func readPromptFile(sc *config.Scope, name, field, path string, checked bool) (*promptPart, error) {
	if path == "" {
		return nil, nil
	}
	part := &promptPart{Source: "prompts." + name + "." + field, Path: path}
	shown := sc.Tilde(path)
	data, err := os.ReadFile(path)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			err = pathErr.Err
		}
		return nil, brokenPrompt(part.Source, shown, "", "cannot read the file: %v", err)
	}
	part.Text = string(data)
	if strings.TrimSpace(part.Text) == "" {
		return nil, brokenPrompt(part.Source, shown, part.Text, "the file is empty")
	}
	if !strings.HasSuffix(part.Text, "\n") {
		part.Text += "\n"
	}
	if !checked {
		return part, nil
	}
	return part, checkSlots(part, shown, name, specOf(name).slots)
}

// checkSlots refuses the first slot in `part` that the text `name` does
// not have, at its line and column.
func checkSlots(part *promptPart, shown, name string, slots []string) error {
	for _, at := range slotPattern.FindAllStringSubmatchIndex(part.Text, -1) {
		slot := part.Text[at[2]:at[3]]
		if slices.Contains(slots, slot) {
			continue
		}
		before := part.Text[:at[0]]
		line := strings.Count(before, "\n") + 1
		column := utf8.RuneCountInString(before[strings.LastIndex(before, "\n")+1:]) + 1
		has := name + " has no slots"
		if len(slots) > 0 {
			sorted := slices.Sorted(slices.Values(slots))
			has = name + " has {{" + strings.Join(sorted, "}} {{") + "}}"
		}
		return brokenPrompt(part.Source, fmt.Sprintf("%s:%d:%d", shown, line, column), part.Text,
			"unknown slot {{%s}}; %s", slot, has)
	}
	return nil
}

// modelValues fill the model slots from the scope's config.
func modelValues(sc *config.Scope) map[string]string {
	return map[string]string{"lead_model": sc.Models.Lead.Model, "lead_effort": sc.Models.Lead.Effort,
		"worker_model": sc.Models.Worker.Model, "worker_effort": sc.Models.Worker.Effort}
}
