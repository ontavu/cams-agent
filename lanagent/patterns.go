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
	"regexp"
	"strings"
	"unicode"

	"github.com/juju/errors"
)

// interfacePattern is one of Config.DiscoverPatterns, compiled.
type interfacePattern struct {
	source  string
	exclude bool
	re      *regexp.Regexp
}

// compilePatterns turns the discover patterns into what selectInterface matches
// against, and refuses the first one that is not a pattern.
//
// Each is anchored here rather than left to whoever writes the configuration,
// because a partial match is never what an interface pattern means. Unanchored,
// "!lo" excluded wlo1, and "eth*" -- "et" followed by any number of "h" --
// selected every veth. A pattern that is anchored already stays valid.
//
// Refused at construction rather than skipped at discovery: a pattern that is
// skipped selects nothing and says so once, in a log line, while the agent goes
// on to find no camera.
func compilePatterns(patterns []string) ([]interfacePattern, error) {
	out := make([]interfacePattern, 0, len(patterns))
	for _, source := range patterns {
		expr, exclude := strings.CutPrefix(source, "!")
		if expr == "" {
			return nil, errors.NotValidf("interface pattern %q", source)
		}
		if suggestion, shaped := globShaped(expr); shaped {
			if exclude {
				suggestion = "!" + suggestion
			}
			return nil, errors.Errorf("interface pattern %q reads as a glob, and patterns are "+
				"regular expressions matching the whole name: write %q", source, suggestion)
		}
		// Checked as written first, so that the error quotes what the operator
		// wrote rather than the anchored form.
		if _, err := regexp.Compile(expr); err != nil {
			return nil, errors.Annotatef(err, "interface pattern %q", source)
		}
		re, err := regexp.Compile("^(?:" + expr + ")$")
		if err != nil {
			return nil, errors.Annotatef(err, "interface pattern %q", source)
		}
		out = append(out, interfacePattern{source: source, exclude: exclude, re: re})
	}
	return out, nil
}

// globShaped reports whether a pattern reads as a shell glob: a "*" right after
// a plain character, as in "eth*", where a regular expression repeats that one
// character. Anchored, such a pattern selects next to nothing -- "eth*" no
// longer takes eth0 -- so an agent still configured the way the README once
// showed would start cleanly and probe no interface at all. Refusing it costs
// the regular expressions that really mean to repeat a letter, which no
// interface name calls for.
//
// suggestion is the pattern with each such "*" turned into ".*". An escaped
// character and a character class are not plain: "\d*" and "[0-9]*" pass.
func globShaped(expr string) (suggestion string, shaped bool) {
	var out strings.Builder
	escaped, inClass, afterPlain := false, false, false
	for _, r := range expr {
		plain := false
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case inClass:
			inClass = r != ']'
		case r == '[':
			inClass = true
		case r == '*' && afterPlain:
			shaped = true
			out.WriteRune('.')
		default:
			plain = unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_'
		}
		afterPlain = plain
		out.WriteRune(r)
	}
	return out.String(), shaped
}

// selectInterface reports whether discovery probes on itf, and the pattern that
// decided, empty when none did. The first pattern that matches decides, and an
// interface that no pattern matches is left out.
func selectInterface(patterns []interfacePattern, itf string) (selected bool, decidedBy string) {
	for _, p := range patterns {
		if p.re.MatchString(itf) {
			return !p.exclude, p.source
		}
	}
	return false, ""
}
