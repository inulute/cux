package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Projects scope the account pool per directory. A machine that hosts
// several codebases often wants them on distinct seats — different
// clients billed to different orgs, a personal side project kept off
// the company pool — while still sharing some accounts between related
// projects. A project binds a directory to a subset of managed slots;
// the wrapper resolves the project from its working directory and every
// automatic decision (threshold swap, rate-limit rotation,
// wait-for-reset) draws candidates from that subset only.
//
// Backward compatible by construction: with no projects defined, or in
// a directory no project claims, the pool is the full account list —
// exactly today's behavior. Explicit targets (`/switch <seat>`,
// `cux switch <seat>`) are never restricted: a human naming a seat
// outranks the project boundary.

// Project binds a directory to a subset of managed slots.
type Project struct {
	Name  string `json:"name"`
	Dir   string `json:"dir"`             // absolute, cleaned
	Slots []int  `json:"slots,omitempty"` // empty = full pool until seats are assigned
}

var projectNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ErrProjectExists / ErrProjectMissing mirror the account errors.
var (
	ErrProjectExists  = errors.New("store: project already exists")
	ErrProjectMissing = errors.New("store: project not found")
)

// ValidateProjectName returns an error if s is not a valid project name.
func ValidateProjectName(s string) error {
	if !projectNameRE.MatchString(s) {
		return fmt.Errorf("store: project name %q must start with a letter, contain only lowercase letters, digits, or hyphens, and be at most 32 chars", s)
	}
	return nil
}

// AddProject registers a project. dir must be absolute; it is cleaned
// before storage so prefix matching stays canonical.
func (s *State) AddProject(name, dir string) error {
	if err := ValidateProjectName(name); err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("store: project dir must be absolute, got %q", dir)
	}
	if s.Projects == nil {
		s.Projects = map[string]Project{}
	}
	if _, ok := s.Projects[name]; ok {
		return fmt.Errorf("%w: %s", ErrProjectExists, name)
	}
	dir = filepath.Clean(dir)
	for _, p := range s.Projects {
		if p.Dir == dir {
			return fmt.Errorf("store: project %q already claims %s", p.Name, dir)
		}
	}
	s.Projects[name] = Project{Name: name, Dir: dir}
	return nil
}

// RemoveProject unregisters a project. Accounts themselves are untouched.
func (s *State) RemoveProject(name string) error {
	if _, ok := s.Projects[name]; !ok {
		return fmt.Errorf("%w: %s", ErrProjectMissing, name)
	}
	delete(s.Projects, name)
	return nil
}

// AssignProjectSlot adds a seat to a project's pool. A slot may belong
// to any number of projects — shared accounts are the point.
func (s *State) AssignProjectSlot(name string, slot int) error {
	p, ok := s.Projects[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProjectMissing, name)
	}
	if _, ok := s.Accounts[slot]; !ok {
		return fmt.Errorf("%w: slot %d", ErrAccountMissing, slot)
	}
	for _, existing := range p.Slots {
		if existing == slot {
			return nil // idempotent
		}
	}
	p.Slots = append(p.Slots, slot)
	sort.Ints(p.Slots)
	s.Projects[name] = p
	return nil
}

// UnassignProjectSlot removes a seat from a project's pool. Removal is
// idempotent — unassigning a seat that is not there is not an error.
func (s *State) UnassignProjectSlot(name string, slot int) error {
	p, ok := s.Projects[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrProjectMissing, name)
	}
	out := p.Slots[:0]
	for _, existing := range p.Slots {
		if existing != slot {
			out = append(out, existing)
		}
	}
	p.Slots = out
	s.Projects[name] = p
	return nil
}

// ProjectFor returns the project whose directory contains cwd, choosing
// the longest (most specific) match so nested projects work. Matching
// is path-boundary aware: /a/b claims /a/b and /a/b/c, never /a/bc.
// Returns nil when no project claims cwd.
func (s *State) ProjectFor(cwd string) *Project {
	cwd = filepath.Clean(cwd)
	var best *Project
	for name := range s.Projects {
		p := s.Projects[name]
		if !dirContains(p.Dir, cwd) {
			continue
		}
		if best == nil || len(p.Dir) > len(best.Dir) {
			best = &p
		}
	}
	return best
}

// PoolFor returns the accounts automatic decisions may draw from in
// cwd, plus the project that scoped them (nil = unrestricted). The
// full pool comes back when no project claims cwd, when the matching
// project has no seats assigned yet, or when none of its assigned
// seats still exist.
func (s *State) PoolFor(cwd string) (map[int]Account, *Project) {
	p := s.ProjectFor(cwd)
	if p == nil || len(p.Slots) == 0 {
		return s.Accounts, p
	}
	pool := make(map[int]Account, len(p.Slots))
	for _, slot := range p.Slots {
		if a, ok := s.Accounts[slot]; ok {
			pool[slot] = a
		}
	}
	if len(pool) == 0 {
		// Every assigned seat has since been removed — falling back to
		// the full pool beats stranding the session.
		return s.Accounts, p
	}
	return pool, p
}

// dirContains reports whether path lives inside root (or is root).
func dirContains(root, path string) bool {
	if root == path {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// PoolForCwd is PoolFor for the current working directory — the shape
// the wrapper and hooks always want, since decisions are scoped to the
// directory claude runs in. Falls back to the full pool when the
// working directory cannot be determined.
func (s *State) PoolForCwd() map[int]Account {
	cwd, err := os.Getwd()
	if err != nil {
		return s.Accounts
	}
	pool, _ := s.PoolFor(cwd)
	return pool
}
