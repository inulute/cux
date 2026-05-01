package updater

import "testing"

func TestIsNewer(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "patch", a: "0.2.1", b: "0.2.0", want: true},
		{name: "minor", a: "0.3.0", b: "0.2.9", want: true},
		{name: "major", a: "1.0.0", b: "0.9.9", want: true},
		{name: "same", a: "0.2.0", b: "0.2.0", want: false},
		{name: "older", a: "0.1.9", b: "0.2.0", want: false},
		{name: "strip suffix", a: "0.3.0+build.1", b: "0.2.0", want: true},
		{name: "short semver", a: "0.3", b: "0.2.9", want: true},
		{name: "fallback", a: "beta", b: "alpha", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNewer(tc.a, tc.b); got != tc.want {
				t.Fatalf("IsNewer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestResultHasUpdateStripsV(t *testing.T) {
	r := Result{Current: "v0.2.0", Latest: "v0.3.0"}
	if !r.HasUpdate() {
		t.Fatal("expected v0.3.0 to be newer than v0.2.0")
	}
}
