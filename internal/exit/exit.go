// Package exit holds the exit codes shared by every command (listed in
// `fleet --help`).
package exit

import "fmt"

// Code is the exit code contract. The numeric values are part of the
// interface.
type Code int

const (
	// Ok means done; for `send`, herdr reported `agent_prompted`.
	Ok Code = 0
	// Refused is a usage error or a precondition refused (role, cap,
	// resources, empty body).
	Refused Code = 1
	// Unknown means herdr gave no clear signal (timeout, stalled): the
	// outcome is unknown.
	Unknown Code = 2
	// Blocked means the target is blocked, or `spawn` stopped at an unknown
	// screen.
	Blocked Code = 3
	// NotFound means the target was not found.
	NotFound Code = 4
	// Environment is an environment error: herdr, git or the database failed.
	Environment Code = 5
)

// Failure is a command failure: the exit code plus the message printed to
// stderr.
type Failure struct {
	Code    Code
	Message string
}

// New makes a Failure with the given code.
func New(code Code, message string) *Failure {
	return &Failure{Code: code, Message: message}
}

// Refusedf makes an exit 1 failure; the arguments are fmt.Sprintf's.
func Refusedf(format string, args ...any) *Failure {
	return New(Refused, fmt.Sprintf(format, args...))
}

// Environmentf makes an exit 5 failure; the arguments are fmt.Sprintf's.
func Environmentf(format string, args ...any) *Failure {
	return New(Environment, fmt.Sprintf(format, args...))
}

// Database wraps a database error as an exit 5 failure.
func Database(err error) *Failure {
	return Environmentf("database: %v", err)
}

// IO wraps an I/O error as an exit 5 failure.
func IO(err error) *Failure {
	return New(Environment, err.Error())
}

func (f *Failure) Error() string {
	return f.Message
}
