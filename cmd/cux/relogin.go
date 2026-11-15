package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/inulute/cux/internal/switcher"
)

// cmdRelogin logs one managed slot in again: `cux relogin <slot|email|alias>`.
//
// It is the repair for a slot whose stored login expired or was revoked,
// which used to take `claude login` as that account followed by `cux add` —
// two steps that also replaced the live login and made the slot active.
func cmdRelogin(args []string) {
	fs := flag.NewFlagSet("relogin", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cux relogin <slot|email|alias>")
		os.Exit(2)
	}
	bin, err := claudeBinary()
	if err != nil {
		fail(err)
	}
	res, err := switcher.Relogin(fs.Arg(0), claudeLogin(bin), os.Stdout)
	if err != nil {
		fail(err)
	}
	acct := res.Account
	if res.Live {
		fmt.Printf("Logged in slot %d (%s) again. It is the live login, so running sessions use the new login too.\n", acct.Slot, acct.Email)
	} else {
		fmt.Printf("Logged in slot %d (%s) again. The active account did not change.\n", acct.Slot, acct.Email)
	}
	fmt.Println("Run `cux list --refresh` to see its usage.")
}

// claudeBinary resolves the real `claude` the way the wrapper does:
// CUX_CLAUDE_BIN, else PATH, refusing cux itself.
func claudeBinary() (string, error) {
	bin := os.Getenv("CUX_CLAUDE_BIN")
	if bin == "" {
		resolved, err := exec.LookPath("claude")
		if err != nil {
			return "", errors.New("`claude` not found on PATH — install Claude Code first, or set CUX_CLAUDE_BIN")
		}
		bin = resolved
	}
	if bin == os.Args[0] || strings.HasSuffix(bin, "/cux") {
		return "", errors.New("refusing to launch cux as the claude binary (loop)")
	}
	return bin, nil
}

// claudeLogin returns a switcher.LoginFunc that runs `claude auth login` on
// the terminal, with Claude Code's config directory pointed at dir.
func claudeLogin(bin string) switcher.LoginFunc {
	return func(dir, email string) error {
		cmd := exec.Command(bin, "auth", "login", "--email", email)
		cmd.Env = loginEnv(os.Environ(), dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
}

// loginEnvDropped are variables that would make the login child store its
// result somewhere other than dir, or authenticate with something other
// than the browser login being run.
var loginEnvDropped = []string{
	"CLAUDE_CONFIG_DIR",
	"CLAUDE_SECURESTORAGE_CONFIG_DIR",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
}

// loginEnv is environ with CLAUDE_CONFIG_DIR set to dir and the variables
// in loginEnvDropped removed.
func loginEnv(environ []string, dir string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, d := range loginEnvDropped {
			if name == d {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return append(out, "CLAUDE_CONFIG_DIR="+dir)
}
