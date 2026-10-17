// Copyright (c) 2022-2024 The authors (see the AUTHORS file)
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package lanagent

import (
	"errors"
	"strings"
	"testing"

	"github.com/ontavu/cams-agent/errkind"
)

// The default of bin/cams-agent, and the example of README.md. Copied rather
// than imported, since a main package cannot be.
var (
	defaultPatterns = []string{"!lo", "!docker.*", ".*"}
	readmePatterns  = []string{"!lo", "!docker.*", "!veth.*", "!br-.*", "en.*", "eth.*"}
)

func TestSelectInterface(t *testing.T) {
	for _, tc := range []struct {
		name      string
		patterns  []string
		itf       string
		selected  bool
		decidedBy string
	}{
		{"the default takes an ordinary interface", defaultPatterns, "enp3s0", true, ".*"},
		{"the default leaves lo out", defaultPatterns, "lo", false, "!lo"},
		{"the default leaves docker out", defaultPatterns, "docker0", false, "!docker.*"},
		// Unanchored, "!lo" matched inside the name of the onboard Wi-Fi.
		{"the default takes wlo1", defaultPatterns, "wlo1", true, ".*"},

		{"the example takes enp", readmePatterns, "enp3s0", true, "en.*"},
		{"the example takes eth", readmePatterns, "eth0", true, "eth.*"},
		{"the example leaves veth out", readmePatterns, "veth1a2b", false, "!veth.*"},
		{"the example leaves a bridge out", readmePatterns, "br-1234", false, "!br-.*"},
		{"the example leaves what it does not name", readmePatterns, "wlp2s0", false, ""},

		// Unanchored, "eth*" -- "et" then any number of "h" -- matched veth.
		{"a pattern covers the whole name", []string{"eth.*"}, "veth0", false, ""},
		{"a pattern is not a prefix", []string{"eth"}, "eth0", false, ""},
		{"an anchored pattern still works", []string{"^lo$"}, "lo", true, "^lo$"},
		{"an alternation is anchored as a whole", []string{"eth0|wlan0"}, "xeth0", false, ""},

		{"the first match decides", []string{"eth0", "!eth.*"}, "eth0", true, "eth0"},
		{"the first match decides, excluding", []string{"!eth0", "eth.*"}, "eth0", false, "!eth0"},
		{"no pattern selects nothing", nil, "eth0", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patterns, err := compilePatterns(tc.patterns)
			if err != nil {
				t.Fatalf("compilePatterns(%q): %v", tc.patterns, err)
			}
			selected, decidedBy := selectInterface(patterns, tc.itf)
			if selected != tc.selected || decidedBy != tc.decidedBy {
				t.Fatalf("selectInterface(%q, %q) = %v, %q; want %v, %q",
					tc.patterns, tc.itf, selected, decidedBy, tc.selected, tc.decidedBy)
			}
		})
	}
}

func TestCompilePatterns_RefusesWhatIsNotAPattern(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
	}{
		{"empty", []string{""}},
		{"a bare exclusion", []string{"!"}},
		{"an invalid expression", []string{"eth("}},
		{"an invalid exclusion", []string{"![a-"}},
		{"a glob", []string{"*"}},
		{"after a valid one", []string{"eth.*", "("}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compilePatterns(tc.patterns); err == nil {
				t.Fatalf("compilePatterns(%q) accepted it", tc.patterns)
			}
		})
	}
}

// A glob compiles as a regular expression, and anchored it selects next to
// nothing: refused, with the pattern it was meant to be.
func TestCompilePatterns_RefusesAGlob(t *testing.T) {
	for _, tc := range []struct{ pattern, suggestion string }{
		{"eth*", `"eth.*"`},
		{"enp*", `"enp.*"`},
		{"!veth*", `"!veth.*"`},
		{"!br-*", `"!br-.*"`},
		{"!wl*", `"!wl.*"`},
		{"eth*|en*", `"eth.*|en.*"`},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			_, err := compilePatterns([]string{tc.pattern})
			if err == nil {
				t.Fatalf("compilePatterns(%q) accepted a glob", tc.pattern)
			}
			if !strings.Contains(err.Error(), tc.suggestion) {
				t.Fatalf("error %q does not suggest %s", err, tc.suggestion)
			}
		})
	}
}

// What only looks like a glob to a careless eye is a regular expression, and
// passes.
func TestCompilePatterns_AcceptsRepetitionsThatAreNotGlobs(t *testing.T) {
	for _, pattern := range []string{"en.*", "eth[0-9]*", `eth\d*`, "(eth|en).*", "!docker.*", "^lo$"} {
		if _, err := compilePatterns([]string{pattern}); err != nil {
			t.Errorf("compilePatterns(%q): %v", pattern, err)
		}
	}
}

// A pattern that cannot work stops the agent at construction, and leaves its
// endpoint unbound: the same URL can be bound right after.
func TestNew_RefusesAnInvalidPattern(t *testing.T) {
	url := testURL(t)
	if lan, err := New(Config{DiscoverPatterns: []string{"eth("}}, nil, url); err == nil {
		if cerr := lan.Close(); cerr != nil {
			t.Errorf("agent close: %v", cerr)
		}
		t.Fatal("New accepted an invalid pattern")
	}

	lan, err := New(Config{}, nil, url)
	if err != nil {
		t.Fatalf("the refused agent left its endpoint bound: %v", err)
	}
	if err := lan.Close(); err != nil {
		t.Errorf("agent close: %v", err)
	}
}

// An interface forced by the configuration used to be registered by New and
// again by discoverNics, and the registry keeps duplicates: two rescan loops
// probed the same link.
func TestDiscoverNics_ForcedInterfaceRegisteredOnce(t *testing.T) {
	lan, err := New(Config{Interfaces: []string{"cams-test0"}}, nil, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := lan.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	})

	if err := lan.discoverNics(); err != nil {
		t.Fatalf("discoverNics: %v", err)
	}

	count := 0
	for _, nic := range lan.interfaces {
		if nic.ItfName == "cams-test0" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cams-test0 registered %d times, want once", count)
	}
}

// An empty pattern is refused as not valid, so that a caller of New can tell
// a configuration mistake from a failure to bind.
func TestCompilePatterns_EmptyIsNotValid(t *testing.T) {
	for _, pattern := range []string{"", "!"} {
		if _, err := compilePatterns([]string{pattern}); !errors.Is(err, errkind.ErrNotValid) {
			t.Fatalf("compilePatterns(%q): %v, want ErrNotValid", pattern, err)
		}
	}
}
