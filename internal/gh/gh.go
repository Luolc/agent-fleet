// Package gh runs `gh api` for `fleet ci wait`, the one command that asks
// GitHub. gh holds the credential: fleet passes it only gh's own
// environment variables and keeps of the reply the HTTP status, the
// headers that pace the next request and the body.
package gh

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Luolc/agent-fleet/internal/herdr"
)

// envNames are the variables gh may see beyond herdr's allow-list, so gh
// acts as the account the caller's environment selects.
var envNames = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR"}

// Response is one HTTP reply as `gh api --include` printed it.
type Response struct {
	Status int
	// Header holds the reply's headers by lower-cased name.
	Header map[string]string
	Body   []byte
}

// NoResponse is a call gh made without printing an HTTP reply (no
// network, gh's own failure): worth trying again later.
type NoResponse struct {
	Message string
}

func (e *NoResponse) Error() string { return e.Message }

// Get runs `gh api --include --method GET <path>`. A reply of any status is
// a Response; a call that printed none is a *NoResponse; gh missing or
// timing out is the *exit.Failure herdr.Exec returns.
func Get(path string) (*Response, error) {
	stdout, stderr, err := herdr.Exec("gh api", envNames, "gh", "api", "--include", "--method", "GET",
		"-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28", path)
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return nil, err
		}
	}
	resp, ok := parse(stdout)
	if !ok {
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return nil, &NoResponse{fmt.Sprintf("gh api %s exited %d with no HTTP reply: %s", path, code, firstLine(stderr))}
	}
	return resp, nil
}

// statusLine is the first line `--include` prints, such as `HTTP/2.0 200 OK`.
var statusLine = regexp.MustCompile(`^HTTP/[0-9.]+ ([0-9]{3})`)

// parse splits `gh api --include` output into status, headers and body.
func parse(out []byte) (*Response, bool) {
	head, body, found := bytes.Cut(out, []byte("\r\n\r\n"))
	if !found {
		head, body, found = bytes.Cut(out, []byte("\n\n"))
	}
	if !found {
		head, body = out, nil
	}
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	m := statusLine.FindStringSubmatch(lines[0])
	if m == nil {
		return nil, false
	}
	status, _ := strconv.Atoi(m[1])
	header := map[string]string{}
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if ok {
			header[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	return &Response{Status: status, Header: header, Body: body}, true
}

// Message is GitHub's `message` in an error reply's body, or "" when it
// has none.
func (r *Response) Message() string {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(r.Body, &body)
	return body.Message
}

// RateLimited says whether the reply is GitHub's rate limit, primary or
// secondary, and how long it asks to wait: Retry-After when given, else
// until X-RateLimit-Reset when no requests remain, else 0 (not said).
func (r *Response) RateLimited(now time.Time) (bool, time.Duration) {
	if r.Status != 403 && r.Status != 429 {
		return false, 0
	}
	if s, ok := r.Header["retry-after"]; ok {
		if secs, err := strconv.Atoi(s); err == nil && secs >= 0 {
			return true, time.Duration(secs) * time.Second
		}
	}
	if r.Header["x-ratelimit-remaining"] == "0" {
		if reset, err := strconv.ParseInt(r.Header["x-ratelimit-reset"], 10, 64); err == nil {
			return true, max(time.Unix(reset, 0).Sub(now)+time.Second, 0)
		}
		return true, 0
	}
	if r.Status == 429 || strings.Contains(strings.ToLower(r.Message()), "rate limit") {
		return true, 0
	}
	return false, 0
}

// githubToken is the shape of a GitHub token, kept out of what fleet
// prints of gh's stderr.
var githubToken = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]+|github_pat_[A-Za-z0-9_]+`)

// firstLine is the first line of gh's stderr, at most 200 bytes, with
// anything shaped like a token taken out.
func firstLine(stderr []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(stderr)), "\n")
	line = githubToken.ReplaceAllString(line, "[redacted]")
	if len(line) > 200 {
		line = line[:200]
	}
	if line == "" {
		return "(nothing on stderr)"
	}
	return line
}
