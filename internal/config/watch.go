package config

import (
	"errors"
	"time"
)

// Watch is when `fleet watch` acts. A repo's `.fleet/config.json` sets
// the job's two (WorkerStale, LeadQuestion); a scope's settings set all of
// them, the job's two for its cross-repo jobs.
type Watch struct {
	// WorkerStale is how long a worker's status, seq and screen stay
	// unchanged before it is a suspect.
	WorkerStale time.Duration
	// LeadQuestion is how long a lead's question waits before its job is
	// ended by force.
	LeadQuestion time.Duration
	// ThreadIdle is how long a thread goes without a message before its
	// live thread agent is reclaimed.
	ThreadIdle time.Duration
	// ThreadQuiet is how long a thread goes without a message before
	// watch asks its thread agent about it.
	ThreadQuiet time.Duration
	// ThreadQuestion is how long a thread agent's own question waits
	// before its session is reclaimed.
	ThreadQuestion time.Duration
	// Reminders are when the people are reminded of a thread's pending
	// questions, counted from the oldest, in increasing order.
	Reminders []time.Duration
	// ParentStale is how long a parent issue of the scope's jobs stays
	// unchanged before watch starts an agent to look at it; ParentAgents
	// bounds those starts in one run.
	ParentStale  time.Duration
	ParentAgents int
}

// DefaultWatch is what an absent key means.
var DefaultWatch = Watch{
	WorkerStale:    10 * time.Minute,
	LeadQuestion:   72 * time.Hour,
	ThreadIdle:     72 * time.Hour,
	ThreadQuiet:    30 * time.Minute,
	ThreadQuestion: 72 * time.Hour,
	Reminders:      []time.Duration{30 * time.Minute, 3 * time.Hour, 24 * time.Hour},
	ParentStale:    72 * time.Hour,
	ParentAgents:   1,
}

// jobWatchFile is the `watch` section of a repo's config, as written:
// durations in Go's notation (`10m`, `72h`).
type jobWatchFile struct {
	WorkerStale  *string `json:"worker_stale"`
	LeadQuestion *string `json:"lead_question"`
}

// scopeWatchFile is the `watch` section of a scope's settings.
type scopeWatchFile struct {
	jobWatchFile
	ThreadIdle     *string   `json:"thread_idle"`
	ThreadQuiet    *string   `json:"thread_quiet"`
	ThreadQuestion *string   `json:"thread_question"`
	Reminders      *[]string `json:"reminders"`
	ParentStale    *string   `json:"parent_stale"`
	ParentAgents   *int      `json:"parent_agents"`
}

// duration parses one positive duration, naming the key on an error.
func duration(key, text string) (time.Duration, error) {
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 {
		return 0, errors.New("watch." + key + ": must be a positive duration such as 10m or 72h")
	}
	return d, nil
}

// apply sets each key given in f onto w.
func (f *jobWatchFile) apply(w *Watch) error {
	for _, s := range []struct {
		key   string
		value *string
		into  *time.Duration
	}{{"worker_stale", f.WorkerStale, &w.WorkerStale}, {"lead_question", f.LeadQuestion, &w.LeadQuestion}} {
		if s.value == nil {
			continue
		}
		d, err := duration(s.key, *s.value)
		if err != nil {
			return err
		}
		*s.into = d
	}
	return nil
}

// apply sets each key given in f onto w; a nil f (no `watch` section)
// sets none.
func (f *scopeWatchFile) apply(w *Watch) error {
	if f == nil {
		return nil
	}
	if err := f.jobWatchFile.apply(w); err != nil {
		return err
	}
	for _, s := range []struct {
		key   string
		value *string
		into  *time.Duration
	}{{"thread_idle", f.ThreadIdle, &w.ThreadIdle}, {"thread_quiet", f.ThreadQuiet, &w.ThreadQuiet},
		{"thread_question", f.ThreadQuestion, &w.ThreadQuestion}, {"parent_stale", f.ParentStale, &w.ParentStale}} {
		if s.value == nil {
			continue
		}
		d, err := duration(s.key, *s.value)
		if err != nil {
			return err
		}
		*s.into = d
	}
	if f.Reminders != nil {
		w.Reminders = nil
		for _, text := range *f.Reminders {
			d, err := duration("reminders", text)
			if err != nil {
				return err
			}
			if n := len(w.Reminders); n > 0 && d <= w.Reminders[n-1] {
				return errors.New("watch.reminders: must be in increasing order")
			}
			w.Reminders = append(w.Reminders, d)
		}
	}
	if f.ParentAgents != nil {
		if *f.ParentAgents <= 0 {
			return errors.New("watch.parent_agents: must be a positive number")
		}
		w.ParentAgents = *f.ParentAgents
	}
	return nil
}
