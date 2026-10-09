// Command fleet runs and coordinates coding agents.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// version is the fleet release version.
const version = "0.0.0"

const usage = `usage: fleet <command>

commands:
  version    print the version (also: fleet --version)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches on the first argument and returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fs := flag.NewFlagSet("version", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		fmt.Fprintln(stdout, "fleet", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "fleet: unknown command %q\n", args[0])
		fmt.Fprint(stderr, usage)
		return 2
	}
}
