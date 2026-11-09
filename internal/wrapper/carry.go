package wrapper

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/inulute/cux/internal/atomicfile"
)

// carryTranscripts copies each named session's transcript, and its sidecar
// directory when there is one, from one Claude Code configuration directory
// to another under the same project subdirectory. A terminal-scoped swap
// relaunches claude on the seat's own profile, and Claude Code can only
// --resume a conversation from the projects/ of the directory it runs on.
//
// The transcript is found by session id across projects/ rather than by
// re-encoding the working directory, so a difference between cux's encoding
// and Claude Code's cannot lose it. A destination copy that is at least as
// new and as large is left alone.
func carryTranscripts(fromDir, toDir string, w io.Writer, ids ...string) {
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		matches, _ := filepath.Glob(filepath.Join(fromDir, "projects", "*", id+".jsonl"))
		for _, src := range matches {
			srcProject := filepath.Dir(src)
			dstProject := filepath.Join(toDir, "projects", filepath.Base(srcProject))
			if err := copyIfNewer(src, filepath.Join(dstProject, id+".jsonl")); err != nil {
				fmt.Fprintf(w, "cux: could not carry transcript %s to %s: %v\n", id, toDir, err)
				continue
			}
			side := filepath.Join(srcProject, id)
			if fi, err := os.Stat(side); err != nil || !fi.IsDir() {
				continue
			}
			_ = filepath.Walk(side, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				rel, relErr := filepath.Rel(srcProject, path)
				if relErr != nil {
					return nil
				}
				if err := copyIfNewer(path, filepath.Join(dstProject, rel)); err != nil {
					fmt.Fprintf(w, "cux: could not carry %s: %v\n", rel, err)
				}
				return nil
			})
		}
	}
}

func copyIfNewer(src, dst string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if di, err := os.Stat(dst); err == nil && !di.ModTime().Before(si.ModTime()) && di.Size() >= si.Size() {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(dst, b, 0o600)
}
