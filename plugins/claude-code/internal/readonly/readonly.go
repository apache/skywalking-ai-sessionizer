// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package readonly classifies a shell command as one that cannot change
// the working tree, so the scans around it can be skipped.
//
// The classifier is small and fails toward scanning. It works on the whole
// text, never the first word, because most commands are compound and many
// read first and then write. It honours quotes: a pipe inside a grep
// pattern is not a segment boundary, a redirection sign inside single
// quotes is text, and a backtick inside double quotes still runs a command.
// A miss is not a loss: the next scan compares against the last manifest,
// so a write that slipped through lands as an unattributed change.
//
// The list is versioned as ReadonlyV1 and named in every record. The
// fixture in testdata holds real commands with the outcome each must get.
package readonly

import (
	"regexp"
	"strings"
)

// ReadonlyV1 names this list, for the record.
const ReadonlyV1 = "readonly-v1"

// plain are the programs that cannot write to the working tree.
var plain = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`cd grep rg cat ls head tail wc sort uniq cut tr diff cmp echo printf pwd which
		type stat file du df date env printenv jq tree basename dirname realpath readlink test [ true false sleep
		ps uname hostname whoami id nl column comm od xxd hexdump strings sha256sum shasum md5sum seq expr wait read`) {
		plain[w] = true
	}
}

// gitRead are the git subcommands that read, or that change only .git,
// which is excluded scope. commit is not here: a pre-commit hook can
// rewrite files.
var gitRead = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`status log diff show branch remote rev-parse describe blame ls-files ls-tree
		cat-file tag fetch grep shortlog for-each-ref check-ignore reflog version merge-base config count-objects
		name-rev symbolic-ref add --version --help`) {
		gitRead[w] = true
	}
}

// gitReadSub are two-word git subcommands that read.
var gitReadSub = map[string]bool{"stash list": true, "stash show": true, "worktree list": true, "worktree prune": true}

var goRead = map[string]bool{"version": true, "env": true, "list": true, "doc": true}

// keywordStart are words dropped from the front of a segment.
var keywordStart = map[string]bool{"if": true, "while": true, "until": true, "elif": true, "then": true, "else": true, "do": true, "time": true, "!": true}

// keywordOnly are segments that hold no command.
var keywordOnly = map[string]bool{"done": true, "fi": true, "esac": true, ";;": true, "}": true, ")": true, "{": true, "(": true}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
var devNull = regexp.MustCompile(`^>{1,2}\|?\s*/dev/null`)
var dupOut = regexp.MustCompile(`^>>?&(\d+|-)`)
var dupIn = regexp.MustCompile(`^<&(\d+|-)`)

// IsReadOnly reports whether the command cannot change the working tree.
func IsReadOnly(cmd string) bool {
	segs, redirect, subst := split(cmd)
	if redirect || subst {
		return false
	}
	for _, s := range segs {
		if !segmentReadOnly(s) {
			return false
		}
	}
	return true
}

// split cuts the command on unquoted separators and reports whether an
// unquoted redirection or a substitution was seen.
func split(text string) (segs []string, redirect, subst bool) {
	var cur strings.Builder
	var q byte
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(text); {
		c := text[i]
		if q == '\'' {
			if c == '\'' {
				q = 0
			}
			cur.WriteByte(c)
			i++
			continue
		}
		if c == '\\' && i+1 < len(text) {
			cur.WriteByte(c)
			cur.WriteByte(text[i+1])
			i += 2
			continue
		}
		if q == '"' {
			if c == '"' {
				q = 0
			} else if c == '`' || strings.HasPrefix(text[i:], "$(") {
				subst = true
			}
			cur.WriteByte(c)
			i++
			continue
		}
		switch {
		case c == '\'' || c == '"':
			q = c
			cur.WriteByte(c)
			i++
		case c == '`' || strings.HasPrefix(text[i:], "$(") || strings.HasPrefix(text[i:], "<(") || strings.HasPrefix(text[i:], ">("):
			subst = true
			cur.WriteByte(c)
			i++
		case c == '>':
			rest := text[i:]
			switch {
			case strings.HasPrefix(rest, ">="):
				cur.WriteString(">=")
				i += 2
			case devNull.MatchString(rest):
				m := devNull.FindString(rest)
				cur.WriteString(m)
				i += len(m)
			case dupOut.MatchString(rest):
				m := dupOut.FindString(rest)
				cur.WriteString(m)
				i += len(m)
			default:
				redirect = true
				cur.WriteByte(c)
				i++
			}
		case c == '<':
			rest := text[i:]
			switch {
			case dupIn.MatchString(rest):
				m := dupIn.FindString(rest)
				cur.WriteString(m)
				i += len(m)
			case strings.HasPrefix(rest, "<<") && !strings.HasPrefix(rest, "<<<"):
				redirect = true
				cur.WriteByte(c)
				i++
			default:
				cur.WriteByte(c)
				i++
			}
		case strings.HasPrefix(text[i:], "&>"):
			redirect = true
			cur.WriteByte(c)
			i++
		case strings.HasPrefix(text[i:], "||") || strings.HasPrefix(text[i:], "&&"):
			flush()
			i += 2
		case c == '|' || c == ';' || c == '&' || c == '\n':
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return segs, redirect, subst
}

// words splits a segment on whitespace, honouring quotes.
func words(seg string) []string {
	var out []string
	var cur strings.Builder
	var q byte
	has := false
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		switch {
		case q != 0:
			if ch == q {
				q = 0
			} else {
				cur.WriteByte(ch)
			}
		case ch == '\'' || ch == '"':
			q = ch
			has = true
		case ch == ' ' || ch == '\t':
			if has || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(ch)
			has = true
		}
	}
	if has || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// segmentReadOnly judges one segment by its first word.
func segmentReadOnly(seg string) bool {
	w := words(seg)
	for len(w) > 0 && keywordStart[w[0]] {
		w = w[1:]
	}
	for len(w) > 0 && assignment.MatchString(w[0]) {
		w = w[1:]
	}
	if len(w) == 0 {
		return true
	}
	first := w[0]
	switch {
	case keywordOnly[first]:
		return true
	case first == "for" || first == "case" || first == "select":
		return true // "for f in a b" and "case x in" hold no command
	case first == "xargs":
		rest := w[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return false
		}
		return segmentReadOnly(strings.Join(rest, " "))
	case first == "sed":
		for _, x := range w[1:] {
			if x == "--in-place" || strings.HasPrefix(x, "--in-place=") || (strings.HasPrefix(x, "-") && !strings.HasPrefix(x, "--") && strings.Contains(x, "i")) {
				return false
			}
		}
		return true
	case first == "find":
		for _, x := range w[1:] {
			switch x {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls":
				return false
			}
		}
		return true
	case first == "awk":
		return !strings.Contains(seg, "system(")
	case first == "git":
		args := w[1:]
		for len(args) > 0 && (args[0] == "-C" || args[0] == "--no-pager" || args[0] == "-c") {
			if args[0] == "--no-pager" {
				args = args[1:]
			} else {
				args = args[min(2, len(args)):]
			}
		}
		if len(args) == 0 {
			return true
		}
		if len(args) > 1 && gitReadSub[args[0]+" "+args[1]] {
			return true
		}
		return gitRead[args[0]]
	case first == "go":
		return len(w) > 1 && goRead[w[1]]
	}
	return plain[first]
}
