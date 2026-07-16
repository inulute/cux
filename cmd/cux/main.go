// cux — Run multiple Claude Code Pro/Max accounts as one.
//
// Daily-driver entry point: run `cux` instead of `claude`. cux wraps
// the real `claude` binary and consumes the Stop / SessionStart /
// PostToolUseFailure hooks so a `/switch <account>` slash command —
// or a rate-limit error from the API — can swap the active account
// and re-launch claude on the same conversation, without restarting
// the terminal.
//
// Subcommands manage the local set of saved accounts. Anything that
// isn't a known subcommand is passed straight through to `claude`.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/inulute/cux/internal/branding"
	"github.com/inulute/cux/internal/config"
	"github.com/inulute/cux/internal/history"
	"github.com/inulute/cux/internal/hookinstall"
	"github.com/inulute/cux/internal/hooks"
	"github.com/inulute/cux/internal/lockfile"
	"github.com/inulute/cux/internal/monitor"
	"github.com/inulute/cux/internal/paths"
	"github.com/inulute/cux/internal/registry"
	"github.com/inulute/cux/internal/store"
	"github.com/inulute/cux/internal/switcher"
	"github.com/inulute/cux/internal/transcripts"
	"github.com/inulute/cux/internal/updater"
	"github.com/inulute/cux/internal/usage"
	"github.com/inulute/cux/internal/wrapper"
	"golang.org/x/term"
)

// version is overridden at build time by the release workflow via:
//
//	-ldflags "-X main.version=X.Y.Z"
//
// It must be a var (not const) so -ldflags can inject the real release
// tag. The fallback "0.2.6" is the development/unreleased default;
// released binaries always get the tag stamped in.
var version = "0.3.1"

const (
	// donateURL is shown only by `cux version --verbose`. Subtle by
	// design — never printed during normal use, never injected into
	// help output, never shown by the wrapper or the slash command.
	donateURL = "https://support.inulute.com"
)

// knownSubcommands is the set of first-args cux handles itself.
// Anything else (including `--resume`, `mcp`, `-c`, etc.) is forwarded
// to the real claude binary via the wrapper.
var knownSubcommands = map[string]bool{
	"add":             true,
	"project":         true,
	"alias":           true,
	"list":            true,
	"ls":              true,
	"remove":          true,
	"rm":              true,
	"status":          true,
	"sessions":        true,
	"attach":          true,
	"support":         true,
	"switch":          true,
	"force-switch":    true,
	"rescue-switch":   true,
	"setup":           true,
	"install-hooks":   true,
	"uninstall-hooks": true,
	"hook":            true,
	"history":         true,
	"config":          true,
	"usage":           true,
	"upgrade":         true,
	"run":             true,
	"docs":            true,
	"help":            true,
	"--help":          true,
	"-h":              true,
	"version":         true,
	"--version":       true,
	"__slash-switch":  true,
}

func main() {
	initTerminalOutput()

	if len(os.Args) < 2 {
		runWrapper(nil)
		return
	}

	cmd := os.Args[1]
	rest := os.Args[2:]

	if !knownSubcommands[cmd] {
		runWrapper(os.Args[1:])
		return
	}

	switch cmd {
	case "add":
		cmdAdd(rest)
	case "list", "ls":
		cmdList(rest)
	case "remove", "rm":
		cmdRemove(rest)
	case "alias":
		cmdAlias(rest)
	case "project":
		cmdProject(rest)
	case "status":
		cmdStatus(rest)
	case "attach":
		os.Exit(cmdAttach(rest))
	case "sessions":
		cmdSessions(rest)
	case "support":
		cmdSupport(rest)
	case "switch":
		cmdSwitch(rest)
	case "force-switch", "rescue-switch":
		cmdForceSwitch(rest)
	case "setup":
		cmdSetup(rest)
	case "install-hooks":
		cmdInstallHooks(rest)
	case "uninstall-hooks":
		cmdUninstallHooks(rest)
	case "hook":
		cmdHook(rest)
	case "history":
		cmdHistory(rest)
	case "config":
		cmdConfig(rest)
	case "usage":
		cmdUsage(rest)
	case "upgrade":
		cmdUpgrade(rest)
	case "run":
		runWrapper(rest)
	case "docs":
		cmdDocs(rest)
	case "help", "--help", "-h":
		printHelp()
	case "version", "--version":
		cmdVersion(rest)
	case "__slash-switch":
		cmdSlashSwitch(rest)
	}
}

// --- Account-management subcommands --------------------------------------

func cmdAdd(args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	slot := fs.Int("slot", 0, "specific slot number (default: next free)")
	alias := fs.String("alias", "", "short alias for this account (e.g. work, personal)")
	noAlias := fs.Bool("no-alias", false, "skip auto-alias from display name")
	_ = fs.Parse(args)

	acct, refreshed, err := switcher.AddCurrent(*slot, *alias, *noAlias)
	if err != nil {
		fail(err)
	}
	aliasStr := ""
	if acct.Alias != "" {
		aliasStr = fmt.Sprintf(" · %s", acct.Alias)
	}
	if refreshed {
		fmt.Printf("Refreshed slot %d (%s%s).\n", acct.Slot, acct.Email, aliasStr)
	} else {
		fmt.Printf("Added slot %d (%s%s).\n", acct.Slot, acct.Email, aliasStr)
	}
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	refresh := fs.Bool("refresh", false, "fetch fresh usage before listing")
	_ = fs.Parse(args)

	state, err := store.Load()
	if err != nil {
		fail(err)
	}
	if len(state.Accounts) == 0 {
		fmt.Println("No accounts managed yet. Run `cux add` while logged in.")
		return
	}

	if *refresh {
		_, errs := monitor.RefreshAll()
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "warning:", e)
		}
	}

	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}
	liveEmail, _ := switcher.CurrentLiveEmail()

	cfg, _ := config.Load()
	setTheme(cfg.Theme)

	printFancyHeader(os.Stdout, state, liveEmail)
	printAccountTable(os.Stdout, state, liveEmail, cache)

	if !*refresh && len(cache) == 0 {
		fmt.Printf("\n %s(No usage data — run `cux list --refresh` or `cux usage refresh` to fetch.)%s\n\n", colorGray, colorReset)
	}
}

func cmdProjectStats(args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		printProjectUsage()
		os.Exit(2)
	}
	name := args[0]
	fs := flag.NewFlagSet("project stats", flag.ExitOnError)
	days := fs.Int("days", 0, "limit to the last N days (0 = all time)")
	_ = fs.Parse(args[1:])

	state, err := store.Load()
	if err != nil {
		fail(err)
	}
	proj, ok := state.Projects[name]
	if !ok {
		fail(fmt.Errorf("project %q not found — see `cux project list`", name))
	}
	var since time.Time
	if *days > 0 {
		since = time.Now().Add(-time.Duration(*days) * 24 * time.Hour)
	}
	st, err := transcripts.ForDir(proj.Dir, since)
	if err != nil {
		fail(err)
	}

	window := "all time"
	if *days > 0 {
		window = fmt.Sprintf("last %d day(s)", *days)
	}
	fmt.Printf("%s  %s  (%s)\n", proj.Name, proj.Dir, window)
	if st.Sessions == 0 {
		fmt.Println("  no transcript activity found")
		return
	}
	fmt.Printf("  sessions      %d\n", st.Sessions)
	fmt.Printf("  active time   %s\n", formatDuration(st.ActiveTime))
	fmt.Printf("  turns         %d\n", st.Turns)
	fmt.Printf("  tokens in     %s\n", formatTokens(st.InputTokens))
	fmt.Printf("  tokens out    %s\n", formatTokens(st.OutputTokens))
	fmt.Printf("  cache write   %s\n", formatTokens(st.CacheCreationTokens))
	fmt.Printf("  cache read    %s\n", formatTokens(st.CacheReadTokens))
	fmt.Printf("  first / last  %s → %s\n",
		st.FirstAt.Local().Format("2006-01-02 15:04"),
		st.LastAt.Local().Format("2006-01-02 15:04"))
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func windowPct(w *usage.Window) string {
	if w == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", w.Utilization)
}

// nextReset returns the soonest reset time across the populated
// windows, formatted as a human-friendly relative duration. We render
// "1h32m", "2d", or "—" for missing data.
func nextReset(u usage.AccountUsage) string {
	var soonest *time.Time
	for _, w := range []*usage.Window{u.FiveHour, u.SevenDay} {
		if w == nil || w.ResetsAt == nil {
			continue
		}
		if soonest == nil || w.ResetsAt.Before(*soonest) {
			t := *w.ResetsAt
			soonest = &t
		}
	}
	if soonest == nil {
		return "—"
	}
	d := time.Until(*soonest)
	if d <= 0 {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func cmdRemove(args []string) {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	force := fs.Bool("force", false, "remove even if active")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cux remove [--force] <slot|email|alias>")
		os.Exit(2)
	}
	acct, err := switcher.Remove(fs.Arg(0), *force)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Removed slot %d (%s).\n", acct.Slot, acct.Email)
}

// cmdAlias sets or clears the alias for a managed account.
//
//	cux alias <slot|email|alias> <new-alias>   — set alias
//	cux alias <slot|email|alias> --clear       — remove alias
// --- Project subcommands ---------------------------------------------------

func cmdProject(args []string) {
	if len(args) == 0 {
		printProjectUsage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		cmdProjectCreate(rest)
	case "assign":
		cmdProjectMutate(rest, true)
	case "unassign":
		cmdProjectMutate(rest, false)
	case "list", "ls":
		cmdProjectList(rest)
	case "stats":
		cmdProjectStats(rest)
	case "remove", "rm":
		cmdProjectRemove(rest)
	default:
		printProjectUsage()
		os.Exit(2)
	}
}

func printProjectUsage() {
	fmt.Fprintln(os.Stderr, "usage: cux project create <name> [--dir PATH]     bind a directory (default: cwd)")
	fmt.Fprintln(os.Stderr, "       cux project assign <name> <slot|email|alias> [...]")
	fmt.Fprintln(os.Stderr, "       cux project unassign <name> <slot|email|alias> [...]")
	fmt.Fprintln(os.Stderr, "       cux project list [--refresh]")
	fmt.Fprintln(os.Stderr, "       cux project stats <name> [--days N]      tokens & time from Claude Code transcripts")
	fmt.Fprintln(os.Stderr, "       cux project remove <name>")
}

func withState(fn func(*store.State) error) {
	lk, err := lockfile.Acquire(paths.LockFile(), 10*time.Second)
	if err != nil {
		fail(err)
	}
	defer lk.Unlock() //nolint:errcheck
	state, err := store.Load()
	if err != nil {
		fail(err)
	}
	if err := fn(state); err != nil {
		fail(err)
	}
	if err := state.Save(); err != nil {
		fail(err)
	}
}

func cmdProjectCreate(args []string) {
	// Name first, flags after: `cux project create iht --dir ~/code/iht`.
	// Go's flag parser stops at the first positional, so split by hand.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		printProjectUsage()
		os.Exit(2)
	}
	name := args[0]
	fs := flag.NewFlagSet("project create", flag.ExitOnError)
	dir := fs.String("dir", "", "directory the project claims (default: current directory)")
	_ = fs.Parse(args[1:])
	if fs.NArg() != 0 {
		printProjectUsage()
		os.Exit(2)
	}
	d := *dir
	if d == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fail(err)
		}
		d = cwd
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		fail(err)
	}
	withState(func(state *store.State) error {
		return state.AddProject(name, abs)
	})
	fmt.Printf("Created project %s → %s. Assign seats with `cux project assign %s <slot|alias>`.\n", name, abs, name)
}

func cmdProjectMutate(args []string, assign bool) {
	if len(args) < 2 {
		printProjectUsage()
		os.Exit(2)
	}
	name, seats := args[0], args[1:]
	withState(func(state *store.State) error {
		for _, seat := range seats {
			acct, err := state.Resolve(seat)
			if err != nil {
				return err
			}
			if assign {
				if err := state.AssignProjectSlot(name, acct.Slot); err != nil {
					return err
				}
				fmt.Printf("Assigned slot %d (%s) to %s.\n", acct.Slot, acct.Email, name)
			} else {
				if err := state.UnassignProjectSlot(name, acct.Slot); err != nil {
					return err
				}
				fmt.Printf("Unassigned slot %d (%s) from %s.\n", acct.Slot, acct.Email, name)
			}
		}
		return nil
	})
}

func cmdProjectRemove(args []string) {
	if len(args) != 1 {
		printProjectUsage()
		os.Exit(2)
	}
	withState(func(state *store.State) error {
		return state.RemoveProject(args[0])
	})
	fmt.Printf("Removed project %s. Accounts are untouched.\n", args[0])
}

func cmdProjectList(args []string) {
	fs := flag.NewFlagSet("project list", flag.ExitOnError)
	refresh := fs.Bool("refresh", false, "fetch fresh usage before printing")
	_ = fs.Parse(args)

	if *refresh {
		if _, errs := monitor.RefreshAll(); len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "warning: %v\n", e)
			}
		}
	}
	state, err := store.Load()
	if err != nil {
		fail(err)
	}
	if len(state.Projects) == 0 {
		fmt.Println("No projects defined. Create one with `cux project create <name> [--dir PATH]`.")
		return
	}
	cache, _ := usage.LoadCache()

	names := make([]string, 0, len(state.Projects))
	for n := range state.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := state.Projects[n]
		fmt.Printf("%s  %s\n", p.Name, p.Dir)
		if len(p.Slots) == 0 {
			fmt.Println("  (no seats assigned — full pool applies here)")
			continue
		}
		for _, slot := range p.Slots {
			acct, ok := state.Accounts[slot]
			if !ok {
				continue
			}
			label := acct.Email
			if acct.Alias != "" {
				label = acct.Alias + " · " + acct.Email
			}
			line := fmt.Sprintf("  [%02d] %-42s", slot, label)
			if cache != nil {
				if u, ok := cache[acct.CacheKey()]; ok {
					line += "  5h:" + windowPct(u.FiveHour) + "  7d:" + windowPct(u.SevenDay)
					if u.TokenExpired {
						line += "  (needs login)"
					}
				} else {
					line += "  (no usage data — try --refresh)"
				}
			}
			fmt.Println(line)
		}
	}
}

func cmdAlias(args []string) {
	fs := flag.NewFlagSet("alias", flag.ExitOnError)
	clear := fs.Bool("clear", false, "remove the alias from this account")
	_ = fs.Parse(args)

	if fs.NArg() < 1 || (fs.NArg() < 2 && !*clear) {
		fmt.Fprintln(os.Stderr, "usage: cux alias <slot|email|alias> <new-alias>")
		fmt.Fprintln(os.Stderr, "       cux alias <slot|email|alias> --clear")
		os.Exit(2)
	}

	lk, err := lockfile.Acquire(paths.LockFile(), 10*time.Second)
	if err != nil {
		fail(err)
	}
	defer lk.Unlock() //nolint:errcheck

	state, err := store.Load()
	if err != nil {
		fail(err)
	}
	acct, err := state.Resolve(fs.Arg(0))
	if err != nil {
		fail(err)
	}

	newAlias := ""
	if !*clear {
		newAlias = strings.TrimSpace(fs.Arg(1))
	}
	if err := state.SetAlias(acct.Slot, newAlias); err != nil {
		fail(err)
	}
	if err := state.Save(); err != nil {
		fail(err)
	}

	if newAlias == "" {
		fmt.Printf("Cleared alias for slot %d (%s).\n", acct.Slot, acct.Email)
	} else {
		fmt.Printf("Set alias %q for slot %d (%s).\n", newAlias, acct.Slot, acct.Email)
	}
}

// cmdSessions lists every live cux wrapper on this machine from the
// heartbeat registry: which directory, which seat, what state. This is
// the first place N concurrent sessions become visible at all —
// `cux list` shows seats, this shows the sessions using them.
func cmdSessions(args []string) {
	_ = args
	entries := registry.List()
	if len(entries) == 0 {
		fmt.Println("No cux sessions are running.")
		return
	}
	now := time.Now()
	for _, e := range entries {
		state := e.State
		if e.Detail != "" {
			state += " (" + e.Detail + ")"
		}
		sid := e.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		if sid == "" {
			sid = "-"
		}
		name := transcripts.FirstPrompt(e.CWD, e.SessionID, 60)
		if name == "" {
			name = filepath.Base(e.CWD)
		}
		fmt.Printf("[%d] %s\n", e.PID, name)
		fmt.Printf("    %s\n", e.CWD)
		fmt.Printf("    seat %-28s session %-9s %s\n", e.Seat, sid, state)
		fmt.Printf("    up %s, last change %s ago\n",
			formatDuration(now.Sub(e.StartedAt)), formatDuration(now.Sub(e.UpdatedAt)))
	}
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	noRefresh := fs.Bool("no-refresh", false, "show cached usage without fetching fresh data")
	_ = fs.Parse(args)

	cfg, _ := config.Load()
	setTheme(cfg.Theme)

	state, err := store.Load()
	if err != nil {
		fail(err)
	}
	liveEmail, liveErr := switcher.CurrentLiveEmail()
	if liveErr != nil {
		liveEmail = ""
	}

	var warnings []error
	if !*noRefresh && len(state.Accounts) > 0 {
		_, warnings = monitor.RefreshAll()
	}

	cache, _ := usage.LoadCache()
	if cache == nil {
		cache = usage.Cache{}
	}

	printFancyHeader(os.Stdout, state, liveEmail)
	if len(state.Accounts) > 0 {
		printAccountTable(os.Stdout, state, liveEmail, cache)
		if len(cache) == 0 {
			fmt.Printf("\n %s(No usage data — run `cux usage refresh` to fetch.)%s\n\n", colorGray, colorReset)
		}
	} else {
		fmt.Printf(" %sNo accounts managed yet. Run `cux add` while logged in.%s\n\n", colorGray, colorReset)
	}
	for _, e := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", e)
	}
}

func cmdSwitch(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: cux switch <slot|email|alias>")
		os.Exit(2)
	}
	from, to, err := switcher.SwitchTo(args[0])
	if err != nil {
		fail(err)
	}
	fromLabel := from.Email
	if from.Alias != "" {
		fromLabel = from.Alias
	}
	toLabel := to.Email
	if to.Alias != "" {
		toLabel = to.Alias
	}
	if fromLabel != "" {
		fmt.Printf("Switched %s → %s.\n", fromLabel, toLabel)
	} else {
		fmt.Printf("Switched to %s.\n", toLabel)
	}
	if os.Getenv("CUX_WRAPPED") == "" {
		fmt.Println("Restart Claude Code to apply the change.")
		fmt.Println("(For inline switching from inside Claude, run `cux setup` and use `/switch` next time.)")
	}
}

func cmdSlashSwitch(args []string) {
	target := strings.TrimSpace(strings.Join(args, " "))
	if err := wrapper.SlashSwitch(target, os.Stdout); err != nil {
		fail(err)
	}
}

func cmdForceSwitch(args []string) {
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: cux force-switch [slot|email|alias]")
		os.Exit(2)
	}
	target := strings.TrimSpace(strings.Join(args, " "))
	if err := wrapper.ForceSwitch(target, os.Stdout); err != nil {
		fail(err)
	}
}

// --- Hook subcommands ----------------------------------------------------

// cmdHook dispatches `cux hook {prompt-submit|prompt-expansion|stop|session-start|rate-limit}`. These
// are invoked by Claude Code itself via entries in
// ~/.claude/settings.json. They read JSON from stdin and write a
// signal file under the cux runtime directory; everything else is
// done by the wrapper's poll loop.
func cmdHook(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cux hook {prompt-submit|prompt-expansion|stop|session-start|rate-limit}")
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "prompt-submit":
		err = hooks.UserPromptSubmit(os.Stdin, os.Stdout)
	case "prompt-expansion":
		err = hooks.UserPromptExpansion(os.Stdin, os.Stdout)
	case "stop":
		err = hooks.Stop(os.Stdin)
	case "session-start":
		err = hooks.SessionStart(os.Stdin, os.Stdout)
	case "rate-limit":
		err = hooks.RateLimit(os.Stdin)
	default:
		fmt.Fprintf(os.Stderr, "cux: unknown hook %q\n", args[0])
		os.Exit(2)
	}
	if err != nil {
		// Hooks should not break Claude Code on a transient failure;
		// log and exit 0 so the user's session keeps running.
		fmt.Fprintf(os.Stderr, "cux hook %s: %v\n", args[0], err)
	}
}

func cmdInstallHooks(args []string) {
	resolved, perr := hookinstall.VerifyOnPATH()
	if perr != nil {
		fmt.Fprintln(os.Stderr, "warning:", perr)
		fmt.Fprintln(os.Stderr, "         hooks installed in settings.json will only work once `cux` is on PATH.")
	} else {
		fmt.Println("cux on PATH:", resolved)
	}
	changed, err := hookinstall.Install()
	if err != nil {
		fail(err)
	}
	if len(changed) == 0 {
		fmt.Println("All cux hooks already installed in ~/.claude/settings.json.")
	} else {
		fmt.Printf("Installed/updated hooks in ~/.claude/settings.json: %s\n", strings.Join(changed, ", "))
	}
}

func cmdUninstallHooks(args []string) {
	removed, err := hookinstall.Uninstall()
	if err != nil {
		fail(err)
	}
	if len(removed) == 0 {
		fmt.Println("No cux hooks present in ~/.claude/settings.json.")
	} else {
		fmt.Printf("Removed cux hooks: %s\n", strings.Join(removed, ", "))
	}
}

// cmdVersion prints the version. With --verbose it appends a single
// gentle line about supporting development. We deliberately keep the
// donate hint out of the default `cux version` output and out of every
// other surface (help, list, status, slash command) — the user asked
// for "subtle, not invasive" and that is what this hits.
func cmdVersion(args []string) {
	verbose := false
	for _, a := range args {
		if a == "-v" || a == "--verbose" {
			verbose = true
		}
	}
	fmt.Println("cux", version)
	if verbose {
		fmt.Println()
		fmt.Println("If cux saves you time, you can support development at", donateURL)
	}
}

func cmdSupport(args []string) {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: cux support")
		os.Exit(2)
	}
	fmt.Print(renderSupport(ansiEnabled))
}

func cmdDocs(args []string) {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: cux docs")
		os.Exit(2)
	}
	fmt.Print(renderDocs(ansiEnabled))
}

func renderDocs(useANSI bool) string {
	var b strings.Builder
	b.WriteString(":: C U X   D O C S ::\n\n")
	b.WriteString("Full documentation:\n")
	b.WriteString("  https://cux.inulute.com/docs\n\n")
	b.WriteString("Topics covered:\n")
	b.WriteString("  · Installation (npm, shell, manual binary)\n")
	b.WriteString("  · Windows, macOS, Linux edge cases\n")
	b.WriteString("  · Troubleshooting (PATH fix, postinstall failures)\n")
	b.WriteString("  · Configuration reference\n")
	b.WriteString("  · Command reference\n")
	return b.String()
}

// --- History / Config / Usage --------------------------------------------

func cmdHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	n := fs.Int("n", 20, "show the last N entries")
	clear := fs.Bool("clear", false, "delete the entire swap history")
	jsonOut := fs.Bool("json", false, "emit entries as one JSON document")
	_ = fs.Parse(args)

	if *clear {
		if err := history.Clear(); err != nil {
			fail(err)
		}
		fmt.Println("Cleared swap history.")
		return
	}

	entries, err := history.Tail(*n)
	if err != nil {
		fail(err)
	}
	if len(entries) == 0 {
		fmt.Println("No swap history yet.")
		return
	}
	if *jsonOut {
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			fail(err)
		}
		fmt.Println(string(out))
		return
	}
	for _, e := range entries {
		fmt.Printf("%s  %s → %s  [%s]\n",
			e.Timestamp.Local().Format("2006-01-02 15:04:05"),
			e.From, e.To, e.Trigger)
		if e.Reason != "" {
			fmt.Printf("    reason: %s\n", e.Reason)
		}
		// Only show usage figures when at least one was nonzero —
		// fresh installs would otherwise print a row of zeros that
		// looks like real data.
		if e.FromUsage5h+e.FromUsage7d+e.ToUsage5h+e.ToUsage7d > 0 {
			fmt.Printf("    usage: %s 5h:%.0f%% 7d:%.0f%% → %s 5h:%.0f%% 7d:%.0f%%\n",
				e.From, e.FromUsage5h, e.FromUsage7d,
				e.To, e.ToUsage5h, e.ToUsage7d)
		}
		if e.SessionID != "" {
			fmt.Printf("    session: %s\n", e.SessionID)
		}
	}
}

func cmdConfig(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cux config show | cux config keys | cux config edit | cux config set <key> <value>")
		os.Exit(2)
	}
	switch args[0] {
	case "show":
		c, err := config.Load()
		if err != nil {
			fail(err)
		}
		out, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			fail(err)
		}
		fmt.Println(string(out))
	case "keys":
		c, err := config.Load()
		if err != nil {
			fail(err)
		}
		keys := config.Keys(c)
		// Compute column widths once so the output lines up.
		var keyW, curW int
		for _, k := range keys {
			if l := len(k.Key); l > keyW {
				keyW = l
			}
			if l := len(k.Current); l > curW {
				curW = l
			}
		}
		if curW > 30 {
			curW = 30
		}
		fmt.Printf("%-*s  %-*s  %s\n", keyW, "KEY", curW, "CURRENT", "DESCRIPTION (default)")
		for _, k := range keys {
			cur := k.Current
			if len(cur) > curW {
				cur = cur[:curW-1] + "…"
			}
			fmt.Printf("%-*s  %-*s  %s (default: %s)\n", keyW, k.Key, curW, cur, k.Description, k.Default)
		}
	case "edit":
		if err := editConfigInteractive(); err != nil {
			fail(err)
		}
	case "set":
		if len(args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: cux config set <key> <value>")
			os.Exit(2)
		}
		c, err := config.Load()
		if err != nil {
			fail(err)
		}
		c, err = config.Set(c, args[1], args[2])
		if err != nil {
			fail(err)
		}
		if err := config.Save(c); err != nil {
			fail(err)
		}
		fmt.Printf("Set %s.\n", args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: cux config show | cux config keys | cux config edit | cux config set <key> <value>")
		os.Exit(2)
	}
}

func editConfigInteractive() error {
	reader := bufio.NewReader(os.Stdin)
	raw, err := newRawMenu()
	if err != nil {
		return err
	}
	defer raw.Close()
	for {
		c, err := config.Load()
		if err != nil {
			return err
		}
		setTheme(c.Theme)
		printConfigEditor(c)
		fmt.Print("Select setting, Esc/q=exit: ")
		choice, err := readEditorChoice(raw, reader)
		if err != nil {
			return err
		}
		choice = normalizeEditorChoice(choice)
		switch choice {
		case "", "s", "save", "q", "quit":
			fmt.Println("Settings saved.")
			return nil
		case "1":
			v, ok, err := promptValue(raw, reader, "5h threshold %", strconv.Itoa(c.Thresholds.FiveHour))
			if err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if err := setAndSaveConfig("thresholds.five_hour", v); err != nil {
				fmt.Printf("error: %v\r\n", err)
				waitEnter(raw, reader)
			}
		case "2":
			v, ok, err := promptValue(raw, reader, "7d threshold %", strconv.Itoa(c.Thresholds.SevenDay))
			if err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if err := setAndSaveConfig("thresholds.seven_day", v); err != nil {
				fmt.Printf("error: %v\r\n", err)
				waitEnter(raw, reader)
			}
		case "3":
			next := map[string]string{"drain": "balanced", "balanced": "manual", "manual": "drain"}[strings.ToLower(c.Strategy.Kind)]
			if next == "" {
				next = "drain"
			}
			if err := setAndSaveConfig("strategy.kind", next); err != nil {
				return err
			}
		case "4":
			v, ok, err := promptValue(raw, reader, "drain order (comma-separated emails, blank = auto)", strings.Join(c.Strategy.Order, ","))
			if err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if err := setAndSaveConfig("strategy.order", v); err != nil {
				fmt.Printf("error: %v\r\n", err)
				waitEnter(raw, reader)
			}
		case "5":
			if err := setAndSaveConfig("auto_switch_on_threshold", strconv.FormatBool(!c.AutoSwitchOnThreshold)); err != nil {
				return err
			}
		case "6":
			if err := setAndSaveConfig("auto_switch_on_rate_limit", strconv.FormatBool(!c.AutoSwitchOnRateLimit)); err != nil {
				return err
			}
		case "7":
			if err := setAndSaveConfig("auto_resume", strconv.FormatBool(!c.AutoResume)); err != nil {
				return err
			}
		case "8":
			v, ok, err := promptValue(raw, reader, `resume message (blank = no auto prompt)`, c.AutoMessage)
			if err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if v == "" {
				v = `""`
			}
			if err := setAndSaveConfig("auto_message", v); err != nil {
				fmt.Printf("error: %v\r\n", err)
				waitEnter(raw, reader)
			}
		case "9":
			if err := setAndSaveConfig("update_check.enabled", strconv.FormatBool(!c.UpdateCheck.Enabled)); err != nil {
				return err
			}
		case "10":
			v, ok, err := promptValue(raw, reader, "update cadence hours", strconv.Itoa(c.UpdateCheck.CadenceHours))
			if err != nil || !ok {
				if err != nil {
					return err
				}
				continue
			}
			if err := setAndSaveConfig("update_check.cadence_hours", v); err != nil {
				fmt.Printf("error: %v\r\n", err)
				waitEnter(raw, reader)
			}
		case "11":
			if err := setAndSaveConfig("notify", strconv.FormatBool(!c.Notify)); err != nil {
				return err
			}
		case "12":
			next := "default"
			if c.Theme == "default" {
				next = "claude"
			}
			if err := setAndSaveConfig("theme", next); err != nil {
				return err
			}
		default:
			fmt.Print("Unknown selection.\r\n")
			waitEnter(raw, reader)
		}
	}
}

func printConfigEditor(c config.Config) {
	fmt.Print("\033[H\033[2J")
	fmt.Printf("%s:: C U X   S E T T I N G S ::%s\r\n\r\n", colorBold, colorReset)
	fmt.Printf("%sToggle booleans by selecting their number. Numeric/text settings prompt for a value.%s\r\n", colorGray, colorReset)
	fmt.Printf("%sPress Enter on an empty prompt to go back/cancel. Press Esc or q to exit.%s\r\n\r\n", colorGray, colorReset)

	g := colorGray
	r := colorReset
	t := colorTeal
	b := colorBold

	fmt.Printf("%s┌────┬────────────────────────────┬────────────────────────────┬────────────────────────────────────┐%s\r\n", g, r)
	fmt.Printf("%s│%s %sID%s %s│%s %sSETTING%s                    %s│%s %sVALUE%s                      %s│%s %sCONTROL%s                            %s│%s\r\n", g, r, b, r, g, r, b, r, g, r, b, r, g, r, b, r, g, r)
	fmt.Printf("%s├────┼────────────────────────────┼────────────────────────────┼────────────────────────────────────┤%s\r\n", g, r)

	row := func(id int, label, value, control string, isBool bool) {
		fmt.Printf("%s│%s ", g, r)
		fmt.Printf("%s%02d%s ", t, id, r)
		fmt.Printf("%s│%s ", g, r)
		fmt.Printf("%s%-26s%s ", t, label, r)
		fmt.Printf("%s│%s ", g, r)
		if isBool {
			padding := strings.Repeat(" ", 26-12)
			fmt.Printf("%s%s", value, padding)
		} else {
			fmt.Printf("%s%-26s%s", t, value, r)
		}
		fmt.Printf(" %s│%s ", g, r)
		fmt.Printf("%-34s ", control)
		fmt.Printf("%s│%s\r\n", g, r)
	}

	row(1, "5h threshold", strconv.Itoa(c.Thresholds.FiveHour)+"%", "edit percent", false)
	row(2, "7d threshold", strconv.Itoa(c.Thresholds.SevenDay)+"%", "edit percent", false)
	row(3, "strategy", c.Strategy.Kind, "cycle drain/balanced/manual", false)
	row(4, "drain order", clip(strings.Join(c.Strategy.Order, ","), 26), "edit comma-separated emails", false)
	row(5, "threshold auto-switch", checkbox(c.AutoSwitchOnThreshold), "toggle", true)
	row(6, "rate-limit auto-switch", checkbox(c.AutoSwitchOnRateLimit), "toggle", true)
	row(7, "auto resume", checkbox(c.AutoResume), "toggle", true)
	row(8, "resume message", clip(displayEmpty(c.AutoMessage), 26), "edit text", false)
	row(9, "update check", checkbox(c.UpdateCheck.Enabled), "toggle", true)
	row(10, "update cadence", strconv.Itoa(c.UpdateCheck.CadenceHours)+"h", "edit hours", false)
	row(11, "notifications", checkbox(c.Notify), "toggle", true)
	row(12, "theme", c.Theme, "cycle default/claude", false)

	fmt.Printf("%s└────┴────────────────────────────┴────────────────────────────┴────────────────────────────────────┘%s\r\n\r\n", g, r)
}

func setAndSaveConfig(key, value string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	c, err = config.Set(c, key, value)
	if err != nil {
		return err
	}
	return config.Save(c)
}

// ANSI colors (themed)
var (
	ansiEnabled = true
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorTeal   = "\033[36m"
	colorGray   = "\033[90m"
	colorYellow = "\033[33m"
	colorGreen  = "\033[32m"
)

func initTerminalOutput() {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		disableANSI()
		return
	}
	if runtime.GOOS == "windows" {
		enableUnicodeOutput()
		if !enableANSIOutput() {
			disableANSI()
		}
	}
}

func disableANSI() {
	ansiEnabled = false
	colorReset = ""
	colorBold = ""
	colorTeal = ""
	colorGray = ""
	colorYellow = ""
	colorGreen = ""
}

// UI layout geometry: visual character widths for the status/list view.
// The box border width (boxBorder) equals the sum of all column widths
// plus the inner dividers, so the status header box and the account table
// share exactly the same outer visual width (104 chars).
const (
	colSlotW  = 6   // SLOT column cell width
	colEmailW = 30  // ACCOUNT column cell width
	colStateW = 8   // STATE column cell width
	colBarW   = 22  // usage bar column cell width (1 + 15 blocks + " %3d%%" + 1)
	colResetW = 8   // RESET column cell width
	barBlocks = 15  // number of █/░ segments in a usage bar
	boxBorder = 101 // ─ count in the status box (= table inner width)
	boxInner  = 99  // content chars between the status box margin spaces
)

func setTheme(name string) {
	if !ansiEnabled {
		disableANSI()
		return
	}

	// Standard ANSI defaults
	colorReset = "\033[0m"
	colorBold = "\033[1m"
	colorTeal = "\033[36m"
	colorGray = "\033[90m"
	colorYellow = "\033[33m"
	colorGreen = "\033[32m"

	if name == "claude" {
		// Claude Brand Colors (Truecolor RGB)
		// Primary Orange: #C15F3C
		// Accent Orange: #FFB38A
		// Cloudy Neutral: #B1ADA1
		colorBold = "\033[1;38;2;193;95;60m"   // Bold Claude Orange
		colorTeal = "\033[38;2;255;179;138m"   // Light Orange Accent
		colorGray = "\033[38;2;177;173;161m"   // Cloudy Neutral
		colorYellow = "\033[38;2;255;179;138m" // Use accent for prompts too
		colorGreen = "\033[38;2;193;95;60m"    // Use primary for success/enabled
	}
}

func promptValue(raw *rawMenu, reader *bufio.Reader, label, current string) (string, bool, error) {
	fmt.Printf("%s%s%s\r\n", colorBold, label, colorReset)
	fmt.Printf("%sCurrent:%s %s%s%s\r\n", colorGray, colorReset, colorTeal, displayEmpty(current), colorReset)
	fmt.Printf("%sNew value:%s ", colorYellow, colorReset)
	val, ok, err := readRawInput(raw, reader)
	if err != nil || !ok {
		return "", false, err
	}
	if val == "" || strings.EqualFold(val, "cancel") {
		return "", false, nil
	}
	return val, true, nil
}

func normalizeEditorChoice(choice string) string {
	choice = strings.TrimSpace(choice)
	if choice == "\x1b" {
		return "q"
	}
	choice = strings.ToLower(choice)
	if n, err := strconv.Atoi(choice); err == nil {
		return strconv.Itoa(n)
	}
	return choice
}

type rawMenu struct {
	enabled bool
	state   *term.State
}

func newRawMenu() (*rawMenu, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return &rawMenu{}, nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return &rawMenu{enabled: true, state: state}, nil
}

func (r *rawMenu) Close() {
	r.Suspend()
}

func (r *rawMenu) Suspend() {
	if r == nil || !r.enabled || r.state == nil {
		return
	}
	_ = term.Restore(int(os.Stdin.Fd()), r.state)
}

func (r *rawMenu) Resume() {
	if r == nil || !r.enabled {
		return
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		r.state = state
	}
}

func readEditorChoice(raw *rawMenu, reader *bufio.Reader) (string, error) {
	val, ok, err := readRawInput(raw, reader)
	if err != nil {
		return "", err
	}
	if !ok {
		return "q", nil
	}
	return val, nil
}

func readRawInput(raw *rawMenu, reader *bufio.Reader) (string, bool, error) {
	if raw == nil || !raw.enabled {
		choice, err := reader.ReadString('\n')
		if err != nil && len(choice) == 0 {
			if errors.Is(err, io.EOF) {
				fmt.Print("\r\n")
				return "", false, nil
			}
			return "", false, err
		}
		return strings.TrimSpace(choice), true, nil
	}
	var buf []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Print("\r\n")
				return "", false, nil
			}
			return "", false, err
		}
		switch b {
		case 0x03, 0x04: // Ctrl-C, Ctrl-D
			fmt.Print("\r\n")
			return "", false, nil
		case 0x1b: // Esc
			// Check if there are more bytes in the buffer (escape sequence like arrows)
			if reader.Buffered() > 0 {
				peek, _ := reader.Peek(1)
				if peek[0] == '[' {
					// It's an escape sequence (e.g. arrow keys ^[[A).
					// Read until a terminator (usually a letter or ~).
					_, _ = reader.ReadByte() // consume '['
					for {
						next, err := reader.ReadByte()
						if err != nil || (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') || next == '~' {
							break
						}
					}
					continue // Ignore the sequence
				}
			}
			// Just a single Esc key
			fmt.Print("\r\n")
			return "", false, nil
		case '\r', '\n':
			fmt.Print("\r\n")
			return string(buf), true, nil
		case 0x7f, 0x08:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b")
			}
		default:
			if b >= 32 && b < 127 {
				buf = append(buf, b)
				fmt.Printf("%c", b)
			}
		}
	}
}

func waitEnter(raw *rawMenu, reader *bufio.Reader) {
	fmt.Print("Press Enter to continue...")
	if raw == nil || !raw.enabled {
		_, _ = reader.ReadString('\n')
		return
	}
	// In raw mode Enter sends \r, not \n — ReadString('\n') would block forever.
	for {
		b, err := reader.ReadByte()
		if err != nil || b == '\r' || b == '\n' || b == 0x03 || b == 0x04 {
			fmt.Print("\r\n")
			return
		}
	}
}

func checkbox(v bool) string {
	if v {
		return "[" + colorGreen + "x" + colorReset + "] enabled "
	}
	return "[" + colorGray + " " + colorReset + "] disabled"
}

func displayEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func cmdUsage(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cux usage refresh | cux usage show")
		os.Exit(2)
	}
	switch args[0] {
	case "refresh":
		_, errs := monitor.RefreshAll()
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "warning:", e)
		}
		// Re-display so the user sees what was fetched.
		cmdList(nil)
	case "show":
		cache, err := usage.LoadCache()
		if err != nil {
			fail(err)
		}
		out, err := json.MarshalIndent(cache, "", "  ")
		if err != nil {
			fail(err)
		}
		fmt.Println(string(out))
	default:
		fmt.Fprintln(os.Stderr, "usage: cux usage refresh | cux usage show")
		os.Exit(2)
	}
}

// --- Wrapper -------------------------------------------------------------

func runWrapper(argv []string) {
	updateDone := startUpdateCheck()
	printCachedUpdateNotice()
	warnIfSetupMissing()
	bin := os.Getenv("CUX_CLAUDE_BIN")
	if bin == "" {
		resolved, err := exec.LookPath("claude")
		if err != nil {
			fail(fmt.Errorf("`claude` not found on PATH — install Claude Code first, or set CUX_CLAUDE_BIN"))
		}
		bin = resolved
	}
	if bin == os.Args[0] || strings.HasSuffix(bin, "/cux") {
		fail(fmt.Errorf("refusing to launch cux as the claude binary (loop)"))
	}
	exitCode, err := wrapper.Run(bin, argv, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cux:", err)
	}
	printUpdateResult(updateDone)
	os.Exit(exitCode)
}

func warnIfSetupMissing() {
	installed, err := setupInstalled()
	if err != nil || installed {
		return
	}
	fmt.Fprintln(os.Stderr, "cux: setup is not complete.")
	fmt.Fprintln(os.Stderr, "     Run `cux setup` once to install /switch, /cux:* and Claude Code hooks.")
}

func setupInstalled() (bool, error) {
	hooksInstalled, err := hookinstall.Installed()
	if err != nil || !hooksInstalled {
		return hooksInstalled, err
	}
	for _, p := range setupSlashCommandPaths() {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func setupSlashCommandPaths() []string {
	dir := filepath.Join(paths.ClaudeDir(), "commands")
	cuxDir := filepath.Join(dir, "cux")
	return []string{
		filepath.Join(dir, "switch.md"),
		filepath.Join(cuxDir, "add.md"),
		filepath.Join(cuxDir, "config.md"),
		filepath.Join(cuxDir, "list.md"),
		filepath.Join(cuxDir, "remove.md"),
		filepath.Join(cuxDir, "status.md"),
		filepath.Join(cuxDir, "switch.md"),
		filepath.Join(cuxDir, "usage-refresh.md"),
	}
}

func startUpdateCheck() <-chan updater.Result {
	cfg, err := config.Load()
	if err != nil || !cfg.UpdateCheck.Enabled {
		return nil
	}
	cadenceHours := cfg.UpdateCheck.CadenceHours
	if cadenceHours < 1 {
		cadenceHours = 6
	}
	done := make(chan updater.Result, 1)
	go func() {
		r, fresh, err := updater.CachedCheck(version, time.Duration(cadenceHours)*time.Hour)
		if err == nil && fresh && r.HasUpdate() {
			done <- r
		}
		close(done)
	}()
	return done
}

func printCachedUpdateNotice() {
	cfg, err := config.Load()
	if err != nil || !cfg.UpdateCheck.Enabled {
		return
	}
	if r, ok := updater.CachedResult(version); ok && r.HasUpdate() {
		fmt.Fprintf(os.Stderr, "cux: %s available — run cux upgrade.\n", r.Latest)
	}
}

func printUpdateResult(done <-chan updater.Result) {
	if done == nil {
		return
	}
	select {
	case r, ok := <-done:
		if ok && r.HasUpdate() {
			fmt.Fprintf(os.Stderr, "cux: %s available — run cux upgrade.\n", r.Latest)
		}
	default:
	}
}

// cmdSetup is the one-time post-install ritual. It installs the
// /switch and /cux:* slash commands plus the Claude Code hooks. Both
// pieces are needed for the inline-switch flow to work.
func cmdSetup(args []string) {
	branding.Print(os.Stdout)
	if err := installSlashCommand(); err != nil {
		fail(err)
	}
	fmt.Println("✓ Installed /switch and /cux:* slash commands under ~/.claude/commands")

	if resolved, err := hookinstall.VerifyOnPATH(); err != nil {
		fmt.Fprintln(os.Stderr, "  warning:", err)
		fmt.Fprintln(os.Stderr, "          hooks will be inert until `cux` is on PATH.")
	} else {
		fmt.Println("✓ cux is on PATH:", resolved)
	}
	changed, err := hookinstall.Install()
	if err != nil {
		fail(err)
	}
	if len(changed) == 0 {
		fmt.Println("✓ Hooks already installed in ~/.claude/settings.json")
	} else {
		fmt.Printf("✓ Installed hooks: %s\n", strings.Join(changed, ", "))
	}
	if err := enableUpdateChecks(); err != nil {
		fail(err)
	}
	printSetupFooter()
}

// printSetupFooter prints the post-install next-steps, help hint,
// GitHub star ask, and support link.
func printSetupFooter() {
	g, r, b := colorGray, colorReset, colorBold

	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Run `cux add` or `/cux:add` while logged in to each account you want to manage.")
	fmt.Println("  2. Use `cux` instead of `claude` to start sessions.")
	fmt.Println("  3. Use /switch or /cux:* inside Claude to manage accounts.")
	fmt.Println()
	fmt.Printf("  %sRun `cux help` to see all available commands.%s\n", b, r)
	fmt.Println()
	fmt.Printf("%s────────────────────────────────────────────────%s\n", g, r)
	fmt.Printf("  %s⭐  Enjoying cux? Star the repo on GitHub:%s\n", b, r)
	fmt.Println("     https://github.com/inulute/cux")
	fmt.Println()
	fmt.Println("  💛  Support development:")
	fmt.Println("     https://support.inulute.com")
	fmt.Printf("%s────────────────────────────────────────────────%s\n", g, r)
	fmt.Println()
}

// enableUpdateChecks silently ensures update checks are on in the saved
// config. No output — it's a background concern, not a user-facing step.
func enableUpdateChecks() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.UpdateCheck.Enabled && cfg.UpdateCheck.CadenceHours >= 1 {
		return nil
	}
	cfg.UpdateCheck.Enabled = true
	if cfg.UpdateCheck.CadenceHours < 1 {
		cfg.UpdateCheck.CadenceHours = 6
	}
	return config.Save(cfg)
}

// printSetupConfigSummary renders a compact table of the most important
// settings so the user knows what's active without opening the full editor.
func printSetupConfigSummary(c config.Config) {
	g, r, t, b := colorGray, colorReset, colorTeal, colorBold

	width := 54 // inner content width between the border spaces

	hline := func(l, m, mid, rr string) {
		fmt.Printf(" %s%s%s%s%s%s%s\n", g, l, strings.Repeat(m, 18), mid, strings.Repeat(m, width-18), rr, r)
	}

	row := func(label, value string) {
		lpad := 18 - len(label) - 1
		if lpad < 0 {
			lpad = 0
		}
		vpad := width - 18 - len([]rune(stripANSI(value))) - 1
		if vpad < 0 {
			vpad = 0
		}
		fmt.Printf(" %s│%s %s%-*s%s %s│%s %s%s%s%s %s│%s\n",
			g, r,
			t, lpad, label, r,
			g, r,
			b, value, r,
			strings.Repeat(" ", vpad),
			g, r,
		)
	}

	boolVal := func(v bool, onLabel, offLabel string) string {
		if v {
			return "[" + colorGreen + "x" + colorReset + "] " + colorGreen + onLabel + colorReset
		}
		return "[" + colorGray + " " + colorReset + "] " + colorGray + offLabel + colorReset
	}

	fmt.Printf(" %s%s:: C O N F I G   P R E V I E W ::%s\n\n", g, b, r)
	hline("┌", "─", "┬", "┐")
	fmt.Printf(" %s│%s %-16s %s│%s %-*s %s│%s\n",
		g, r,
		b+"SETTING"+r,
		g, r,
		width-18, b+"VALUE"+r,
		g, r,
	)
	hline("├", "─", "┼", "┤")
	row("theme", c.Theme)
	row("strategy", c.Strategy.Kind)
	row("5h threshold", strconv.Itoa(c.Thresholds.FiveHour)+"%")
	row("7d threshold", strconv.Itoa(c.Thresholds.SevenDay)+"%")
	row("threshold switch", boolVal(c.AutoSwitchOnThreshold, "on", "off"))
	row("rate-limit switch", boolVal(c.AutoSwitchOnRateLimit, "on", "off"))
	row("auto resume", boolVal(c.AutoResume, "on", "off"))
	row("resume message", clip(displayEmpty(c.AutoMessage), width-20))
	row("update checks", boolVal(c.UpdateCheck.Enabled, "on", "off"))
	hline("└", "─", "┴", "┘")
	fmt.Println()
}

// stripANSI removes ANSI escape sequences so we can measure visual width.
func stripANSI(s string) string {
	var out []byte
	inEsc := false
	for i := 0; i < len(s); i++ {
		if inEsc {
			if (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z') {
				inEsc = false
			}
			continue
		}
		if s[i] == '\x1b' {
			inEsc = true
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func cmdUpgrade(args []string) {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: cux upgrade")
		os.Exit(2)
	}
	exe, err := os.Executable()
	if err != nil {
		fail(err)
	}

	if isNPMInstall(filepath.Dir(exe)) {
		// npm manages its own package metadata (package.json, bin shims,
		// postinstall scripts), so let npm do the upgrade itself.
		runUpgradeCommand("npm", "install", "-g", "@inulute/cux@latest")
	} else {
		// For every other install method — shell installer, manual binary
		// download, Homebrew tap, Windows, etc. — use the built-in Go
		// self-updater: no sh, no curl, no PowerShell required.
		if err := updater.SelfUpdate(exe); err != nil {
			fail(err)
		}
	}
	// Clear the on-disk cache so the next run re-fetches instead of
	// immediately showing "update available" for the version just installed.
	clearUpdateCache()
}

// clearUpdateCache removes the on-disk update cache so the next run
// re-fetches from GitHub instead of showing a stale "update available"
// notice for a release that was just installed.
func clearUpdateCache() {
	_ = os.Remove(updater.CachePath())
}

func runUpgradeCommand(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fail(err)
	}
}

func isNPMInstall(binDir string) bool {
	if filepath.Base(binDir) != "bin" {
		return false
	}
	pkgPath := filepath.Join(filepath.Dir(binDir), "package.json")
	b, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(b, &pkg) == nil && pkg.Name == "@inulute/cux"
}

func sameDir(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

// --- Helpers -------------------------------------------------------------

func fail(err error) {
	fmt.Fprintln(os.Stderr, "cux:", err)
	os.Exit(1)
}

// padTo right-pads s to exactly n rune-columns, truncating with "…" if needed.
func padTo(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		if n <= 1 {
			return string(runes[:n])
		}
		return string(runes[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(runes))
}

// renderColorBar returns a colorized usage bar of barW block-chars followed by
// a right-aligned percentage. Visual width: barW+5 (e.g. barW=15 → 20 chars).
func renderColorBar(pct float64, barW int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct / 100.0 * float64(barW))
	return colorGreen + strings.Repeat("█", filled) +
		colorGray + strings.Repeat("░", barW-filled) + colorReset +
		colorTeal + fmt.Sprintf(" %3.0f%%", pct) + colorReset
}

// accountState returns the display label for one row in the account table.
func accountState(email, liveEmail string, au usage.AccountUsage) string {
	if au.TokenExpired {
		return "EXPRD"
	}
	if email == liveEmail {
		return "ACTIVE"
	}
	if au.FiveHour != nil && au.FiveHour.Utilization >= 100 {
		return "FULL"
	}
	if au.FiveHour != nil {
		return "READY"
	}
	return "IDLE"
}

// tableSep builds one horizontal separator line for the account table.
// left/mid/right are the box-drawing corner/junction characters.
func tableSep(left, mid, right string) string {
	g, r := colorGray, colorReset
	cols := []int{colSlotW, colEmailW, colStateW, colBarW, colBarW, colResetW}
	var sb strings.Builder
	sb.WriteString(" " + g + left)
	for i, w := range cols {
		sb.WriteString(strings.Repeat("─", w))
		if i < len(cols)-1 {
			sb.WriteString(mid)
		}
	}
	sb.WriteString(right + r)
	return sb.String()
}

// printFancyHeader prints the ASCII art banner and the system status box.
func printFancyHeader(w io.Writer, st *store.State, liveEmail string) {
	g, r, t, b := colorGray, colorReset, colorTeal, colorBold

	fmt.Fprintf(w, "\n%s%s%s\n", t, branding.Banner, r)
	fmt.Fprintf(w, " %s:: A C C O U N T   P O O L ::%s\n\n", b, r)

	statusLabel := "NONE"
	slotStr := "--"
	if liveEmail != "" {
		statusLabel = "ACTIVE"
		if st.ActiveSlot != 0 {
			slotStr = fmt.Sprintf("%02d", st.ActiveSlot)
		}
	}
	managedStr := fmt.Sprintf("%02d", len(st.Accounts))

	liveDisp := liveEmail
	if liveDisp == "" {
		liveDisp = "(none — run `claude login`)"
	}

	// Compute visible widths for correct padding (all fields are ASCII except
	// liveDisp, which may contain an em-dash in the fallback string).
	l1Left := len("  SYSTEM STATUS : ") + len(statusLabel) + len(" [") + len(slotStr) + len("]")
	l1Right := len("MANAGED ACCOUNTS : ") + len(managedStr) + len("  ")
	l1Pad := boxInner - l1Left - l1Right
	if l1Pad < 1 {
		l1Pad = 1
	}

	l2Prefix := "  LIVE INSTANCE : "
	liveRunes := []rune(liveDisp)
	l2Pad := boxInner - len(l2Prefix) - len(liveRunes)
	if l2Pad < 0 {
		max := boxInner - len(l2Prefix) - 1
		if max > 0 {
			liveDisp = string(liveRunes[:max]) + "…"
		}
		l2Pad = 0
	}

	statusColor := t
	if statusLabel == "ACTIVE" {
		statusColor = colorGreen
	}

	line1 := g + "  SYSTEM STATUS : " + r +
		statusColor + statusLabel + r +
		g + " [" + slotStr + "]" + r +
		strings.Repeat(" ", l1Pad) +
		g + "MANAGED ACCOUNTS : " + r +
		b + managedStr + r + "  "

	line2 := g + l2Prefix + r +
		t + liveDisp + r +
		strings.Repeat(" ", l2Pad)

	fmt.Fprintf(w, " %s┌%s┐%s\n", g, strings.Repeat("─", boxBorder), r)
	fmt.Fprintf(w, " %s│%s %s %s│%s\n", g, r, line1, g, r)
	fmt.Fprintf(w, " %s│%s %s %s│%s\n", g, r, line2, g, r)
	fmt.Fprintf(w, " %s└%s┘%s\n\n", g, strings.Repeat("─", boxBorder), r)
}

// printAccountTable renders the full account usage table below the status box.
func printAccountTable(w io.Writer, st *store.State, liveEmail string, cache usage.Cache) {
	g, r, t, b := colorGray, colorReset, colorTeal, colorBold

	hCell := func(text string, cellW int) string {
		return " " + b + padTo(text, cellW-2) + r + " "
	}

	fmt.Fprintln(w, tableSep("┌", "┬", "┐"))
	fmt.Fprintf(w, " %s│%s%s%s│%s%s%s│%s%s%s│%s%s%s│%s%s%s│%s%s%s│%s\n",
		g, r, hCell("SLOT", colSlotW),
		g, r, hCell("ACCOUNT", colEmailW),
		g, r, hCell("STATE", colStateW),
		g, r, hCell("5H USAGE", colBarW),
		g, r, hCell("7D USAGE", colBarW),
		g, r, hCell("RESET", colResetW),
		g, r)
	fmt.Fprintln(w, tableSep("├", "┼", "┤"))

	slots := st.SortedSlots()
	sort.Ints(slots)

	noBar := strings.Repeat(" ", 10) + g + "─" + r + strings.Repeat(" ", 11)

	for _, slot := range slots {
		a := st.Accounts[slot]
		au := cachedAccountUsage(cache, a)
		sl := accountState(a.Email, liveEmail, au)

		var sc string
		switch sl {
		case "ACTIVE":
			sc = colorGreen
		case "FULL", "EXPRD":
			sc = colorYellow
		default:
			sc = t
		}

		resetStr := nextReset(au)

		var barFive, barSeven string
		if au.FiveHour != nil {
			barFive = " " + renderColorBar(au.FiveHour.Utilization, barBlocks) + " "
		} else {
			barFive = noBar
		}
		if au.SevenDay != nil {
			barSeven = " " + renderColorBar(au.SevenDay.Utilization, barBlocks) + " "
		} else {
			barSeven = noBar
		}

		slotCell := "  " + b + fmt.Sprintf("%02d", slot) + r + "  "
		// ACCOUNT cell: if an alias is set, show "alias · email" truncated to
		// fit colEmailW; otherwise show email as before.
		var accountLabel string
		if a.Alias != "" {
			full := a.Alias + " · " + a.Email
			accountLabel = padTo(full, colEmailW-2)
		} else {
			accountLabel = padTo(a.Email, colEmailW-2)
		}
		emailCell := " " + t + accountLabel + r + " "
		stateCell := " " + sc + padTo(sl, colStateW-2) + r + " "
		resetCell := " " + t + padTo(resetStr, colResetW-2) + r + " "

		fmt.Fprintf(w, " %s│%s%s%s│%s%s%s│%s%s%s│%s%s%s│%s%s%s│%s%s%s│%s\n",
			g, r, slotCell,
			g, r, emailCell,
			g, r, stateCell,
			g, r, barFive,
			g, r, barSeven,
			g, r, resetCell,
			g, r)
	}

	fmt.Fprintln(w, tableSep("└", "┴", "┘"))
}

func cachedAccountUsage(cache usage.Cache, acct store.Account) usage.AccountUsage {
	if cache == nil {
		return usage.AccountUsage{}
	}
	if u, ok := cache[acct.CacheKey()]; ok {
		return u
	}
	if acct.CacheKey() != acct.Email {
		if u, ok := cache[acct.Email]; ok {
			return u
		}
	}
	return usage.AccountUsage{}
}

func renderSupport(useANSI bool) string {
	var b strings.Builder
	b.WriteString(":: C U X   S U P P O R T ::\n\n")
	b.WriteString("Support cux development:\n")
	b.WriteString(donateURL)
	b.WriteString("\n")
	return b.String()
}

func printHelp() {
	fmt.Println(`cux — Run multiple Claude Code Pro/Max accounts as one

USAGE
  cux [claude-args...]                    run claude under the wrapper (default)
  cux run [claude-args...]                same, explicit
  cux add [--slot N] [--alias NAME] [--no-alias]  add the currently logged-in account
  cux list                                list managed accounts
  cux alias <slot|email|alias> <name>     set a short alias (e.g. work, personal)
  cux project create <name> [--dir PATH]  scope a directory to its own seat pool
  cux project assign <name> <seat> [...]  add seats to a project (seats can be shared)
  cux project unassign <name> <seat> [...]
  cux project list [--refresh]            projects + live usage of their seats
  cux project remove <name>               unbind a directory (accounts untouched)
  cux alias <slot|email|alias> --clear    remove alias
  cux switch <slot|email|alias>           swap the active account (manual; requires
                                          restart unless run from /switch inside)
  cux force-switch [slot|email|alias]     emergency swap for an active cux session
                                          when Claude will not run /switch
  cux remove [--force] <slot|email|alias> remove an account from cux
  cux status                              show live login + cux state
  cux sessions                            list running cux sessions (heartbeat registry)
  cux support                             show support URL
  cux docs                                show documentation URL
  cux setup                               install /switch, /cux:* + Claude Code hooks
  cux install-hooks                       install Claude Code hooks only
  cux uninstall-hooks                     remove cux's entries from settings.json
  cux history [-n N] [--json]             show recent account swaps
  cux history --clear                     delete the swap history
  cux config show                         print current configuration
  cux config keys                         list every settable key
  cux config edit                         interactive settings editor
  cux config set <key> <value>            update a single setting
  cux usage refresh                       fetch fresh usage for every account
  cux usage show                          print the on-disk usage cache (JSON)
  cux upgrade                             update cux using npm or the installer
  cux hook <event>                        internal: invoked by Claude Code
  cux version                             print version

INLINE SWITCHING
  Once set up, type /switch [<slot|email|alias>] from inside a Claude Code
  session started via cux. You can also use /cux:switch, /cux:add,
  /cux:list, /cux:status, /cux:support, /cux:config, /cux:remove,
  and /cux:usage-refresh from inside the
  session. Manual and rate-limit swaps reconnect with --resume.`)
}
