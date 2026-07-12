package store

import (
	"path/filepath"
	"testing"
)

func projectFixture(t *testing.T) *State {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := &State{
		Accounts: map[int]Account{
			1: {Slot: 1, Email: "a@x.test"},
			2: {Slot: 2, Email: "b@x.test"},
			3: {Slot: 3, Email: "c@x.test"},
		},
		Sequence: []int{1, 2, 3},
	}
	if err := s.AddProject("alpha", "/work/alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProject("beta", "/work/beta"); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []int{1, 2} {
		if err := s.AssignProjectSlot("alpha", slot); err != nil {
			t.Fatal(err)
		}
	}
	// Slot 2 is shared between the two projects; slot 3 belongs to beta.
	for _, slot := range []int{2, 3} {
		if err := s.AssignProjectSlot("beta", slot); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func poolSlots(pool map[int]Account) []int {
	out := []int{}
	for slot := range pool {
		out = append(out, slot)
	}
	return out
}

func TestPoolForScopesByDirectory(t *testing.T) {
	s := projectFixture(t)

	pool, p := s.PoolFor("/work/alpha/src")
	if p == nil || p.Name != "alpha" {
		t.Fatalf("project = %v, want alpha", p)
	}
	if len(pool) != 2 || pool[1].Email != "a@x.test" || pool[2].Email != "b@x.test" {
		t.Errorf("alpha pool = %v, want slots 1,2", poolSlots(pool))
	}

	// Shared seat: slot 2 must appear in beta's pool too.
	pool, p = s.PoolFor("/work/beta")
	if p == nil || p.Name != "beta" {
		t.Fatalf("project = %v, want beta", p)
	}
	if len(pool) != 2 || pool[2].Email != "b@x.test" || pool[3].Email != "c@x.test" {
		t.Errorf("beta pool = %v, want slots 2,3", poolSlots(pool))
	}

	// Unclaimed directory → full pool, today's behavior.
	pool, p = s.PoolFor("/somewhere/else")
	if p != nil || len(pool) != 3 {
		t.Errorf("unclaimed dir: got project %v, pool %v; want full pool", p, poolSlots(pool))
	}

	// Path-boundary awareness: /work/alphabet is NOT inside /work/alpha.
	if _, p := s.PoolFor("/work/alphabet"); p != nil {
		t.Errorf("/work/alphabet matched %s — prefix match must be path-boundary aware", p.Name)
	}
}

func TestPoolForNestedProjectsLongestWins(t *testing.T) {
	s := projectFixture(t)
	if err := s.AddProject("alpha-sub", filepath.Join("/work/alpha", "vendor")); err != nil {
		t.Fatal(err)
	}
	if err := s.AssignProjectSlot("alpha-sub", 3); err != nil {
		t.Fatal(err)
	}
	if _, p := s.PoolFor("/work/alpha/vendor/pkg"); p == nil || p.Name != "alpha-sub" {
		t.Errorf("nested dir resolved to %v, want alpha-sub (longest match)", p)
	}
	if _, p := s.PoolFor("/work/alpha/src"); p == nil || p.Name != "alpha" {
		t.Errorf("outer dir resolved to %v, want alpha", p)
	}
}

func TestProjectWithNoSeatsFallsBackToFullPool(t *testing.T) {
	s := projectFixture(t)
	if err := s.AddProject("empty", "/work/empty"); err != nil {
		t.Fatal(err)
	}
	pool, p := s.PoolFor("/work/empty")
	if p == nil || p.Name != "empty" {
		t.Fatalf("project = %v, want empty", p)
	}
	if len(pool) != 3 {
		t.Errorf("empty project pool = %v, want full pool until seats are assigned", poolSlots(pool))
	}
}

func TestRemoveAccountCleansProjectAssignments(t *testing.T) {
	s := projectFixture(t)
	if err := s.Remove(2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		for _, slot := range s.Projects[name].Slots {
			if slot == 2 {
				t.Errorf("removed slot 2 still assigned to %s", name)
			}
		}
	}
}

func TestProjectLifecycleValidation(t *testing.T) {
	s := projectFixture(t)
	if err := s.AddProject("alpha", "/elsewhere"); err == nil {
		t.Error("duplicate project name must be rejected")
	}
	if err := s.AddProject("dup-dir", "/work/alpha"); err == nil {
		t.Error("duplicate project dir must be rejected")
	}
	if err := s.AddProject("Bad Name", "/x"); err == nil {
		t.Error("invalid project name must be rejected")
	}
	if err := s.AddProject("rel", "relative/path"); err == nil {
		t.Error("relative dir must be rejected")
	}
	if err := s.AssignProjectSlot("alpha", 99); err == nil {
		t.Error("assigning an unknown slot must be rejected")
	}
	if err := s.AssignProjectSlot("alpha", 1); err != nil {
		t.Errorf("re-assigning an assigned slot must be idempotent, got %v", err)
	}
	if err := s.UnassignProjectSlot("alpha", 3); err != nil {
		t.Errorf("unassigning a not-assigned slot must be idempotent, got %v", err)
	}
	if err := s.RemoveProject("ghost"); err == nil {
		t.Error("removing an unknown project must be rejected")
	}
}
