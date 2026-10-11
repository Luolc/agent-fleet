package config

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

// PromptNames are fleet's built-in texts a scope's config can change: the
// prompts of the five roles and the screen helpers' guidance.
var PromptNames = []string{"thread", "lead", "worker", "revisit", "unblock", "screen_guidance"}

// Prompt is how a scope changes one built-in text: the files that replace
// it, follow it, and record the built-in text the replacement started
// from. An empty path is a file not given. LoadScope makes every path
// absolute.
type Prompt struct {
	Replace string
	Append  string
	Base    string
}

// Model is the model and effort a prompt names for the agents its agent
// starts.
type Model struct {
	Model  string
	Effort string
}

// Models are the model and effort the thread prompt names for leads, and
// the lead prompt for workers.
type Models struct {
	Lead   Model
	Worker Model
}

// DefaultModels are what the built-in prompts named before they could be
// configured.
var DefaultModels = Models{Lead: Model{Model: "opus", Effort: "high"}, Worker: Model{Model: "opus", Effort: "high"}}

// promptFile is one `prompts.<name>` as written.
type promptFile struct {
	Replace *string `json:"replace"`
	Append  *string `json:"append"`
	Base    *string `json:"base"`
}

// modelFile is `models.lead` or `models.worker` as written.
type modelFile struct {
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
}

// modelsFile is `models` as written.
type modelsFile struct {
	Lead   *modelFile `json:"lead"`
	Worker *modelFile `json:"worker"`
}

// prompts reads `prompts`: only the known names, each with a replacement
// or an addition, a base only beside a replacement. `~/` is expanded to
// `home`; a relative path stays relative until LoadScope resolves it
// against the config file's directory.
func prompts(files map[string]*promptFile, home string) (map[string]Prompt, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	out := map[string]Prompt{}
	for _, name := range names {
		key := "prompts." + name
		if !slices.Contains(PromptNames, name) {
			return nil, errors.New(key + ": unknown name; the names are " + strings.Join(PromptNames, ", "))
		}
		f := files[name]
		if f.Replace == nil && f.Append == nil {
			return nil, errors.New(key + ": must have replace or append")
		}
		if f.Base != nil && f.Replace == nil {
			return nil, errors.New(key + ".base: only goes with replace")
		}
		var p Prompt
		if err := apply([]setting{{key + ".replace", f.Replace, &p.Replace}, {key + ".append", f.Append, &p.Append},
			{key + ".base", f.Base, &p.Base}}); err != nil {
			return nil, err
		}
		for _, path := range []*string{&p.Replace, &p.Append, &p.Base} {
			if rest, ok := strings.CutPrefix(*path, "~/"); ok {
				*path = filepath.Join(home, rest)
			} else if *path != "" {
				*path = filepath.Clean(*path)
			}
		}
		out[name] = p
	}
	return out, nil
}

// resolve makes the relative paths of `prompts` relative to `dir`.
func resolve(prompts map[string]Prompt, dir string) {
	for name, p := range prompts {
		for _, path := range []*string{&p.Replace, &p.Append, &p.Base} {
			if *path != "" && !filepath.IsAbs(*path) {
				*path = filepath.Join(dir, *path)
			}
		}
		prompts[name] = p
	}
}

// apply sets each key given in f onto m, refusing empty values.
func (f *modelsFile) apply(m *Models) error {
	if f == nil {
		return nil
	}
	for _, s := range []struct {
		key  string
		file *modelFile
		into *Model
	}{{"models.lead", f.Lead, &m.Lead}, {"models.worker", f.Worker, &m.Worker}} {
		if s.file == nil {
			continue
		}
		if err := apply([]setting{{s.key + ".model", s.file.Model, &s.into.Model},
			{s.key + ".effort", s.file.Effort, &s.into.Effort}}); err != nil {
			return err
		}
	}
	return nil
}
