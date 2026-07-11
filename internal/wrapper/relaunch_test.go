package wrapper

import (
	"reflect"
	"testing"
)

func TestRelaunchFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "keeps boolean flag, drops bare resume",
			in:   []string{"--dangerously-skip-permissions", "--resume"},
			want: []string{"--dangerously-skip-permissions"},
		},
		{
			name: "drops resume with session id value",
			in:   []string{"-r", "abc-123", "--verbose"},
			want: []string{"--verbose"},
		},
		{
			name: "keeps value flags, drops positional prompt",
			in:   []string{"--model", "opus", "fix the bug"},
			want: []string{"--model", "opus"},
		},
		{
			name: "equals forms",
			in:   []string{"--resume=abc", "--model=opus"},
			want: []string{"--model=opus"},
		},
		{
			name: "drops continue and session-id",
			in:   []string{"--session-id", "uuid-1", "-c"},
			want: []string{},
		},
		{
			name: "drops fork-session without eating next flag",
			in:   []string{"--fork-session", "--verbose"},
			want: []string{"--verbose"},
		},
		{
			name: "bare resume followed by flag eats nothing",
			in:   []string{"--resume", "--dangerously-skip-permissions"},
			want: []string{"--dangerously-skip-permissions"},
		},
		{
			name: "leading positional prompt dropped",
			in:   []string{"explain this repo", "--verbose"},
			want: []string{"--verbose"},
		},
		{
			name: "everything after separator dropped",
			in:   []string{"--verbose", "--", "--resume"},
			want: []string{"--verbose"},
		},
		{
			name: "empty argv",
			in:   []string{},
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relaunchFlags(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("relaunchFlags(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
