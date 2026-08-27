package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// walk visits every command in the table, depth-first, with its full path.
func walk(t *testing.T, visit func(path []string, c *Command)) {
	t.Helper()
	var rec func(prefix []string, cmds []*Command)
	rec = func(prefix []string, cmds []*Command) {
		for _, c := range cmds {
			path := append(append([]string{}, prefix...), c.Name)
			visit(path, c)
			rec(path, c.Children)
		}
	}
	rec(nil, commandTable)
}

// TestEveryCommandIsInvocable is the structural invariant the table exists
// for: anything reachable by dispatch runs something. A leaf with no Run
// would be a command cux advertises and then fails on.
func TestEveryCommandIsInvocable(t *testing.T) {
	walk(t, func(path []string, c *Command) {
		leaf := len(c.Children) == 0
		switch {
		case leaf && c.Run == nil:
			t.Errorf("%s is a leaf with no Run", strings.Join(path, " "))
		case !leaf && c.Run != nil:
			// Ambiguous: the same argv could mean the group with an argument
			// or an unknown subcommand. runCommand resolves it as the
			// latter, so a group's Run would be silently dead.
			t.Errorf("%s has children and a Run; the Run would never fire", strings.Join(path, " "))
		}
	})
}

// TestNoDuplicateNamesOrAliases guards the lookup: two entries answering to
// one spelling means the second is unreachable, and which one wins is just
// table order.
func TestNoDuplicateNamesOrAliases(t *testing.T) {
	seen := map[string]string{} // spelling at this level -> owner path
	check := func(scope string, cmds []*Command) {
		for _, c := range cmds {
			for _, n := range c.names() {
				key := scope + "\x00" + n
				if prev, dup := seen[key]; dup {
					t.Errorf("%q is claimed by both %s and %s", n, prev, scope+" "+c.Name)
				}
				seen[key] = scope + " " + c.Name
			}
		}
	}
	check("", commandTable)
	walk(t, func(path []string, c *Command) {
		if len(c.Children) > 0 {
			check(strings.Join(path, " "), c.Children)
		}
	})
}

// TestHelpCoversEveryPublicCommand is why the help text can stay
// hand-written prose. It cannot silently fall behind the table.
func TestHelpCoversEveryPublicCommand(t *testing.T) {
	walk(t, func(path []string, c *Command) {
		if c.Internal {
			return
		}
		// A group is documented by its children, not by itself.
		if len(c.Children) > 0 {
			return
		}
		want := "cux " + strings.Join(path, " ")
		if !strings.Contains(helpText, want) {
			t.Errorf("help text never mentions %q", want)
		}
	})
}

// TestInternalCommandsStayOutOfTheManifest keeps cux's own hook and
// slash-command entry points unpublished, so their shapes remain cux's to
// change without breaking a client that bound to them.
func TestInternalCommandsStayOutOfTheManifest(t *testing.T) {
	var internal []string
	walk(t, func(path []string, c *Command) {
		if c.Internal {
			internal = append(internal, strings.Join(path, " "))
		}
	})
	if len(internal) == 0 {
		t.Fatal("no internal commands in the table; this test is no longer meaningful")
	}
	m := buildManifest()
	for _, entry := range m.Commands {
		got := strings.Join(entry.Path, " ")
		for _, bad := range internal {
			if got == bad {
				t.Errorf("internal command %q is published in the manifest", bad)
			}
		}
	}
}

// TestManifestIsUsableByAClient asserts the properties a remote bridge
// actually depends on: a stable schema number, an invocable path per entry,
// and an argument kind it can validate against.
func TestManifestIsUsableByAClient(t *testing.T) {
	raw, err := json.Marshal(buildManifest())
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != manifestSchema {
		t.Errorf("schema = %d, want %d", m.Schema, manifestSchema)
	}
	if m.Version == "" {
		t.Error("manifest carries no cux version")
	}
	if len(m.Commands) == 0 {
		t.Fatal("manifest lists no commands")
	}
	valid := map[ArgKind]bool{ArgIdent: true, ArgPath: true, ArgText: true}
	for _, c := range m.Commands {
		if len(c.Path) == 0 {
			t.Error("a command has an empty path")
		}
		if c.Summary == "" {
			t.Errorf("%v has no summary", c.Path)
		}
		for _, a := range c.Args {
			if !valid[a.Kind] {
				t.Errorf("%v arg %q has unknown kind %q", c.Path, a.Name, a.Kind)
			}
		}
		// Every published path must actually route.
		cur := commandTable
		for i, seg := range c.Path {
			var found *Command
			for _, cand := range cur {
				for _, n := range cand.names() {
					if n == seg {
						found = cand
					}
				}
			}
			if found == nil {
				t.Fatalf("manifest publishes %v but segment %q (index %d) does not route", c.Path, seg, i)
			}
			cur = found.Children
		}
	}
}

// TestClaudeArgsAreNotClaimed protects the wrapper's whole premise: a
// first-arg cux does not own must reach claude untouched.
func TestClaudeArgsAreNotClaimed(t *testing.T) {
	for _, arg := range []string{"--resume", "-r", "mcp", "-c", "-p", "--model", "doctor", "update"} {
		if c, ok := lookupTop(arg); ok {
			t.Errorf("cux claims %q (as %s); it would never reach claude", arg, c.Name)
		}
	}
}

// TestMutatesIsSetDeliberately spot-checks the read/write split a bridge
// uses to separate view access from control. Getting it wrong either blocks
// a safe read or exposes a mutation.
func TestMutatesIsSetDeliberately(t *testing.T) {
	cases := map[string]bool{
		"list":           false,
		"status":         false,
		"sessions":       false,
		"commands":       false,
		"usage show":     false,
		"config show":    false,
		"config keys":    false,
		"switch":         true,
		"add":            true,
		"remove":         true,
		"usage refresh":  true,
		"config set":     true,
		"project create": true,
	}
	found := map[string]bool{}
	walk(t, func(path []string, c *Command) {
		key := strings.Join(path, " ")
		if want, ok := cases[key]; ok {
			found[key] = true
			if c.Mutates != want {
				t.Errorf("%s mutates = %v, want %v", key, c.Mutates, want)
			}
		}
	})
	for key := range cases {
		if !found[key] {
			t.Errorf("%s is no longer in the table; update this test", key)
		}
	}
}
