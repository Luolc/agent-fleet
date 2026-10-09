// Package cliargs reproduces the clap behaviors the standard `flag` package
// lacks: flags after positionals, `--` ending the flags, and a string flag
// that knows whether it was given.
package cliargs

import "flag"

// Parse parses args with fs, accepting flags anywhere, and returns the
// positionals in order. Everything after `--` is positional.
func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
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
// given, even with an empty value.
type OptString struct {
	Given bool
	Value string
}

// String implements flag.Value.
func (o *OptString) String() string { return o.Value }

// Set implements flag.Value.
func (o *OptString) Set(value string) error {
	o.Given, o.Value = true, value
	return nil
}

// Ptr is the value as a pointer, nil when the flag was not given.
func (o *OptString) Ptr() *string {
	if !o.Given {
		return nil
	}
	return &o.Value
}
