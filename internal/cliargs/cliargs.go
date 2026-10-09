// Package cliargs reproduces the clap behaviors the standard `flag` package
// lacks: flags after positionals, `--` ending the flags, and a string flag
// that knows whether it was given.
package cliargs

import (
	"flag"
	"fmt"
	"strconv"
)

// repeated is a flag value that refuses a second occurrence, as clap does.
type repeated interface {
	repeatedError() error
}

// Parse parses args with fs, accepting flags anywhere, and returns the
// positionals in order. Everything after `--` is positional. A flag given
// twice is an error in clap's words.
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			var dup error
			fs.VisitAll(func(f *flag.Flag) {
				if r, ok := f.Value.(repeated); ok && r.repeatedError() != nil {
					dup = r.repeatedError()
				}
			})
			if dup != nil {
				return nil, dup
			}
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(positionals, rest...), nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

// OptString is an `Option<String>` flag: Given is true once the flag was
// given, even with an empty value. Name and Placeholder are for the error
// on a repeat (`--file <PATH>`).
type OptString struct {
	Name, Placeholder string
	Given             bool
	Value             string
	repeat            error
}

// String implements flag.Value.
func (o *OptString) String() string { return o.Value }

// Set implements flag.Value.
func (o *OptString) Set(value string) error {
	if o.Given {
		o.repeat = fmt.Errorf("the argument '--%s <%s>' cannot be used multiple times", o.Name, o.Placeholder)
		return o.repeat
	}
	o.Given, o.Value = true, value
	return nil
}

func (o *OptString) repeatedError() error { return o.repeat }

// Bool is a `bool` flag (clap `ArgAction::SetTrue`): refused when repeated.
type Bool struct {
	Name   string
	Value  bool
	repeat error
}

// String implements flag.Value.
func (b *Bool) String() string { return strconv.FormatBool(b.Value) }

// IsBoolFlag implements flag.boolFlag, so the flag takes no value.
func (b *Bool) IsBoolFlag() bool { return true }

// Set implements flag.Value.
func (b *Bool) Set(value string) error {
	if b.Value {
		b.repeat = fmt.Errorf("the argument '--%s' cannot be used multiple times", b.Name)
		return b.repeat
	}
	v, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	b.Value = v
	return nil
}

func (b *Bool) repeatedError() error { return b.repeat }

// Ptr is the value as a pointer, nil when the flag was not given.
func (o *OptString) Ptr() *string {
	if !o.Given {
		return nil
	}
	return &o.Value
}
