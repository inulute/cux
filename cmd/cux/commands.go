package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// This file is cux's command surface, declared once.
//
// It used to live in three places that had to be kept in step by hand: the
// set of first-args cux claims rather than forwarding to claude, the switch
// that dispatched them, and the help text. A manifest for remote clients
// would have been a fourth, and one built from its own list would drift
// silently — a client auto-adapting to a command cux does not have, or
// missing one it does, with nothing to catch either. So the table below is
// the source: the gate, the dispatch and `cux commands --json` all derive
// from it, and a test asserts the help text covers it.

// ArgKind classifies a positional argument tightly enough that a client can
// validate one before handing it to cux. It is the same distinction a remote
// bridge has to make anyway — an identifier can be checked against a
// character class, a path cannot — so naming it here saves every consumer
// from inventing its own rule.
type ArgKind string

const (
	// ArgIdent is a slot number, email, alias or project name: cux's own
	// names for things, safe to constrain to a narrow character class.
	ArgIdent ArgKind = "ident"
	// ArgPath is a filesystem path, which cannot be narrowed the same way.
	ArgPath ArgKind = "path"
	// ArgText is a free-form value, e.g. a config value.
	ArgText ArgKind = "text"
)

// Arg is one positional argument of a command.
type Arg struct {
	Name     string  `json:"name"`
	Kind     ArgKind `json:"kind"`
	Required bool    `json:"required"`
	Variadic bool    `json:"variadic,omitempty"`
}

// Command is one verb in cux's surface, or one subcommand of another.
type Command struct {
	Name    string
	Aliases []string
	// Summary is one line, for the manifest. Help text is prose and stays
	// hand-written; see TestHelpCoversEveryPublicCommand.
	Summary string
	Args    []Arg
	// Mutates marks a command that changes state. A bridge that separates
	// read access from control access needs this per command, and deriving
	// it from the name is guesswork — `usage refresh` writes, `usage show`
	// does not.
	Mutates bool
	// Internal keeps a command out of the manifest: it exists for Claude
	// Code or for cux's own slash commands to invoke, and its shape is not
	// something external clients should bind to.
	Internal bool
	Children []*Command
	// Run receives the arguments after this command's own path. Nil on a
	// command that only groups children.
	Run func(args []string)
	// UsageErr prints the message a group shows when given no subcommand or
	// an unknown one. Only meaningful alongside Children.
	UsageErr func()
}

// commandTable is cux's whole command surface.
var commandTable = []*Command{
	{Name: "run", Summary: "run claude under the wrapper", Args: []Arg{{Name: "claude-args", Kind: ArgText, Variadic: true}}, Run: func(a []string) { runWrapper(a) }},
	{Name: "add", Summary: "add the currently logged-in account", Mutates: true, Run: cmdAdd},
	{Name: "list", Aliases: []string{"ls"}, Summary: "list managed accounts", Run: cmdList},
	{Name: "remove", Aliases: []string{"rm"}, Summary: "remove an account from cux", Mutates: true,
		Args: []Arg{{Name: "account", Kind: ArgIdent, Required: true}}, Run: cmdRemove},
	{Name: "alias", Summary: "set or clear an account's short alias", Mutates: true,
		Args: []Arg{{Name: "account", Kind: ArgIdent, Required: true}, {Name: "alias", Kind: ArgIdent}}, Run: cmdAlias},
	{Name: "switch", Summary: "swap the active account", Mutates: true,
		Args: []Arg{{Name: "account", Kind: ArgIdent, Required: true}}, Run: cmdSwitch},
	{Name: "force-switch", Aliases: []string{"rescue-switch"}, Summary: "emergency swap for an active cux session", Mutates: true,
		Args: []Arg{{Name: "account", Kind: ArgIdent}}, Run: cmdForceSwitch},
	{Name: "status", Summary: "show the live login and cux state", Run: cmdStatus},
	{Name: "sessions", Summary: "list running cux sessions", Run: cmdSessions},
	{Name: "attach", Summary: "attach to a running session", Mutates: true,
		Args: []Arg{{Name: "pid", Kind: ArgIdent}}, Run: func(a []string) { os.Exit(cmdAttach(a)) }},
	{Name: "history", Summary: "show or clear recent account swaps", Run: cmdHistory},
	{Name: "upgrade", Summary: "update cux using npm or the installer", Mutates: true, Run: cmdUpgrade},
	{Name: "setup", Summary: "install slash commands and Claude Code hooks", Mutates: true, Run: cmdSetup},
	{Name: "install-hooks", Summary: "install Claude Code hooks only", Mutates: true, Run: cmdInstallHooks},
	{Name: "uninstall-hooks", Summary: "remove cux's entries from settings.json", Mutates: true, Run: cmdUninstallHooks},
	{Name: "support", Summary: "show the support URL", Run: cmdSupport},
	{Name: "docs", Summary: "show the documentation URL", Run: cmdDocs},
	{Name: "version", Aliases: []string{"--version"}, Summary: "print the cux version", Run: cmdVersion},
	{Name: "help", Aliases: []string{"--help", "-h"}, Summary: "print help", Run: func([]string) { printHelp() }},

	{Name: "project", Summary: "scope directories to their own seat pools", UsageErr: printProjectUsage, Children: []*Command{
		{Name: "create", Summary: "scope a directory to its own seat pool", Mutates: true,
			Args: []Arg{{Name: "name", Kind: ArgIdent, Required: true}, {Name: "dir", Kind: ArgPath}}, Run: cmdProjectCreate},
		{Name: "assign", Summary: "add seats to a project", Mutates: true,
			Args: []Arg{{Name: "name", Kind: ArgIdent, Required: true}, {Name: "seat", Kind: ArgIdent, Required: true, Variadic: true}},
			Run:  func(a []string) { cmdProjectMutate(a, true) }},
		{Name: "unassign", Summary: "remove seats from a project", Mutates: true,
			Args: []Arg{{Name: "name", Kind: ArgIdent, Required: true}, {Name: "seat", Kind: ArgIdent, Required: true, Variadic: true}},
			Run:  func(a []string) { cmdProjectMutate(a, false) }},
		{Name: "list", Aliases: []string{"ls"}, Summary: "projects and the live usage of their seats", Run: cmdProjectList},
		{Name: "stats", Summary: "usage detail for one project",
			Args: []Arg{{Name: "name", Kind: ArgIdent, Required: true}}, Run: cmdProjectStats},
		{Name: "remove", Aliases: []string{"rm"}, Summary: "unbind a directory, leaving accounts untouched", Mutates: true,
			Args: []Arg{{Name: "name", Kind: ArgIdent, Required: true}}, Run: cmdProjectRemove},
	}},

	{Name: "usage", Summary: "the usage cache", Children: []*Command{
		{Name: "refresh", Summary: "fetch fresh usage for every account", Mutates: true, Run: cmdUsageRefresh},
		{Name: "show", Summary: "print the on-disk usage cache as JSON", Run: cmdUsageShow},
	}, UsageErr: func() { fmt.Fprintln(os.Stderr, "usage: cux usage refresh | cux usage show") }},

	{Name: "config", Summary: "cux configuration", Children: []*Command{
		{Name: "show", Summary: "print the current configuration as JSON", Run: cmdConfigShow},
		{Name: "keys", Summary: "list every settable key", Run: cmdConfigKeys},
		{Name: "edit", Summary: "interactive settings editor", Mutates: true, Run: cmdConfigEdit},
		{Name: "set", Summary: "update a single setting", Mutates: true,
			Args: []Arg{{Name: "key", Kind: ArgIdent, Required: true}, {Name: "value", Kind: ArgText, Required: true}}, Run: cmdConfigSet},
	}, UsageErr: func() {
		fmt.Fprintln(os.Stderr, "usage: cux config show | cux config keys | cux config edit | cux config set <key> <value>")
	}},

	// Run is wired in init(): this command reads the table it lives in, and
	// Go cannot order that as a package-level initialisation.
	{Name: "commands", Summary: "print cux's command surface as JSON"},

	// Internal: invoked by Claude Code hooks and cux's own slash commands.
	// Deliberately absent from the manifest — the shapes are cux's to change.
	{Name: "hook", Summary: "invoked by Claude Code", Internal: true, Run: cmdHook},
	{Name: "__slash-switch", Summary: "invoked by the /switch slash command", Internal: true, Run: cmdSlashSwitch},
}

func init() {
	c, ok := lookupTop("commands")
	if !ok {
		panic("cux: commands entry missing from the table")
	}
	c.Run = cmdCommands
}

// names returns every spelling that routes to c.
func (c *Command) names() []string {
	return append([]string{c.Name}, c.Aliases...)
}

// lookupTop finds the top-level command a first-arg routes to.
func lookupTop(arg string) (*Command, bool) {
	for _, c := range commandTable {
		for _, n := range c.names() {
			if n == arg {
				return c, true
			}
		}
	}
	return nil, false
}

// --- the manifest --------------------------------------------------------

// manifestSchema is the version of the *shape* below, not of cux.
//
// A client's whole reason to read this is to adapt when cux gains a command,
// so it needs to tell a new command apart from a changed contract. This
// integer bumps only when the shape changes in a way that could break a
// reader — a new command does not touch it. Additive fields do not either,
// since a consumer that ignores unknown keys keeps working.
const manifestSchema = 1

type manifestArg struct {
	Name     string  `json:"name"`
	Kind     ArgKind `json:"kind"`
	Required bool    `json:"required"`
	Variadic bool    `json:"variadic,omitempty"`
}

type manifestCommand struct {
	// Path is the argv prefix that invokes this command, e.g.
	// ["usage","refresh"]. Clients should build invocations from this rather
	// than from Name, so nesting stays transparent.
	Path    []string      `json:"path"`
	Aliases []string      `json:"aliases,omitempty"`
	Summary string        `json:"summary"`
	Args    []manifestArg `json:"args,omitempty"`
	Mutates bool          `json:"mutates"`
}

type manifest struct {
	Schema   int               `json:"schema"`
	Version  string            `json:"version"`
	Commands []manifestCommand `json:"commands"`
	// Notes records the boundaries of what this manifest describes, so a
	// consumer learns them from the document rather than by discovering them
	// the hard way.
	Notes manifestNotes `json:"notes"`
}

type manifestNotes struct {
	// Flags says plainly that flags are not described. Each command parses
	// its own FlagSet internally, so no table can see them; a client that
	// needs one must hard-code it and accept the coupling.
	Flags string `json:"flags"`
	// Internal says that commands cux reserves for itself are omitted.
	Internal string `json:"internal"`
}

// buildManifest flattens the table into the published surface.
func buildManifest() manifest {
	m := manifest{
		Schema:  manifestSchema,
		Version: version,
		Notes: manifestNotes{
			Flags:    "not described; every command parses its own flags, so a client that needs one must hard-code it",
			Internal: "commands cux reserves for its own hooks and slash commands are omitted",
		},
	}
	var walk func(prefix []string, cmds []*Command)
	walk = func(prefix []string, cmds []*Command) {
		for _, c := range cmds {
			if c.Internal {
				continue
			}
			path := append(append([]string{}, prefix...), c.Name)
			// A command that only groups children is not itself invocable,
			// so it is listed for discovery but carries no args.
			entry := manifestCommand{
				Path:    path,
				Aliases: c.Aliases,
				Summary: c.Summary,
				Mutates: c.Mutates,
			}
			for _, a := range c.Args {
				entry.Args = append(entry.Args, manifestArg{Name: a.Name, Kind: a.Kind, Required: a.Required, Variadic: a.Variadic})
			}
			m.Commands = append(m.Commands, entry)
			walk(path, c.Children)
		}
	}
	walk(nil, commandTable)
	sort.Slice(m.Commands, func(i, j int) bool {
		a, b := m.Commands[i].Path, m.Commands[j].Path
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	return m
}

func cmdCommands(args []string) {
	out, err := json.MarshalIndent(buildManifest(), "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
}

// --- dispatch ------------------------------------------------------------

// dispatch runs the command arg names, with rest as its arguments. It
// returns false when nothing in the table claims arg, leaving the caller to
// forward it to claude — `--resume`, `mcp`, `-c` and everything else that is
// claude's. The gate and the routing being the same lookup is the point: they
// cannot disagree about which is which.
func dispatch(arg string, rest []string) bool {
	c, ok := lookupTop(arg)
	if !ok {
		return false
	}
	runCommand(c, rest)
	return true
}

// runCommand executes c, descending through children when c is a group.
func runCommand(c *Command, args []string) {
	if len(c.Children) > 0 {
		// A group with a Run of its own would be ambiguous: the same argv
		// could mean "the group, with this argument" or "an unknown
		// subcommand". The table has none, and this keeps it that way.
		if len(args) == 0 {
			c.usageErr()
			return
		}
		for _, child := range c.Children {
			for _, n := range child.names() {
				if n == args[0] {
					runCommand(child, args[1:])
					return
				}
			}
		}
		c.usageErr()
		return
	}
	if c.Run == nil {
		// Unreachable via the table as written; a test asserts it stays so.
		fail(fmt.Errorf("cux: command %q has no implementation", c.Name))
	}
	c.Run(args)
}

func (c *Command) usageErr() {
	if c.UsageErr != nil {
		c.UsageErr()
	} else {
		fmt.Fprintf(os.Stderr, "usage: cux %s <subcommand>\n", c.Name)
	}
	os.Exit(2)
}
