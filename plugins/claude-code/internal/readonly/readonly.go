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
// The list is versioned as ReadonlyV2 and named in every record. The
// fixture in testdata holds real commands with the outcome each must get.
package readonly

import (
	"regexp"
	"strings"
)

// ReadonlyV2 names this list, for the record. Version 2 scans executable
// wrappers and commands whose arguments can write files or run programs.
const ReadonlyV2 = "readonly-v2"

// plain are the programs that cannot write to the working tree.
var plain = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`cd grep cat ls head tail wc cut tr diff cmp echo printf pwd which
		type stat du df date printenv jq basename dirname realpath readlink test [ true false sleep
		ps uname hostname whoami id nl column comm od hexdump strings sha256sum shasum md5sum seq expr wait read`) {
		plain[w] = true
	}
}

// gitRead are the git subcommands that read, or that change only .git,
// which is excluded scope. commit is not here: a pre-commit hook can
// rewrite files.
var gitRead = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`status log diff show rev-parse describe blame ls-files ls-tree
		cat-file grep shortlog for-each-ref check-ignore version merge-base count-objects
		name-rev --version --help`) {
		gitRead[w] = true
	}
}

// gitReadSub are two-word git subcommands that read.
var gitReadSub = map[string]bool{"stash list": true, "stash show": true, "worktree list": true, "worktree prune": true}

// keywordStart are words dropped from the front of a segment.
var keywordStart = map[string]bool{"if": true, "while": true, "until": true, "elif": true, "then": true, "else": true, "do": true, "time": true, "!": true}

// keywordOnly are segments that hold no command.
var keywordOnly = map[string]bool{"done": true, "fi": true, "esac": true, ";;": true, "}": true, ")": true, "{": true, "(": true}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// inertVariables change only how a program formats what it prints. Any other
// variable may choose a program the command runs, as GIT_EXTERNAL_DIFF,
// GIT_CONFIG_* and LD_PRELOAD do, so its assignment is scanned.
var inertVariables = map[string]bool{
	"LANG": true, "LANGUAGE": true, "LC_ALL": true, "LC_COLLATE": true, "LC_CTYPE": true, "LC_MESSAGES": true,
	"LC_NUMERIC": true, "LC_TIME": true, "TZ": true, "TERM": true, "COLUMNS": true, "LINES": true,
	"NO_COLOR": true, "CLICOLOR": true, "CLICOLOR_FORCE": true, "FORCE_COLOR": true,
}
var devNull = regexp.MustCompile(`^>{1,2}\|?\s*/dev/null`)
var dupOut = regexp.MustCompile(`^>>?&(\d+|-)`)
var dupIn = regexp.MustCompile(`^<&(\d+|-)`)

// IsReadOnly reports whether the command cannot change the working tree.
func IsReadOnly(cmd string) bool {
	// No command at all is not a read-only command: it is a tool that is not
	// a shell. Returning true here would skip the scan for every tool of
	// every runtime whose tools take arguments rather than a command line,
	// which is all of them but this one.
	if strings.TrimSpace(cmd) == "" {
		return false
	}
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
			case nullRedirect(rest):
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
	if q != 0 {
		redirect = true // malformed quoting is not a command we can classify
	}
	flush()
	return segs, redirect, subst
}

func nullRedirect(rest string) bool {
	m := devNull.FindString(rest)
	return m != "" && (len(m) == len(rest) || strings.ContainsRune(" \t\n;&|", rune(rest[len(m)])))
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
		// A bare assignment matters too: it updates a variable the shell
		// already exports to every later command.
		name, _, _ := strings.Cut(w[0], "=")
		if !inertVariables[name] {
			return false
		}
		w = w[1:]
	}
	if len(w) == 0 {
		return true
	}
	first := w[0]
	switch {
	case keywordOnly[first]:
		return len(w) == 1
	case first == "for" || first == "case" || first == "select":
		return false // compound grammar may put a command in this segment
	case first == "xargs":
		return false // standard input can supply a writing option or executable
	case first == "env":
		return len(w) == 1 // any arguments may select an executable
	case first == "sort":
		return !hasOption(w[1:], "o", "output", "compress-program")
	case first == "tree":
		return !hasOption(w[1:], "o", "output")
	case first == "rg":
		return !hasOption(w[1:], "", "pre", "hostname-bin")
	case first == "sed":
		// Only the common print-only form is recognized. Other scripts can
		// write with w, execute with e, or load more script text from a file.
		if len(w) < 3 || w[1] != "-n" || !sedPrint.MatchString(w[2]) {
			return false
		}
		for _, arg := range w[3:] {
			if strings.HasPrefix(arg, "-") {
				return false
			}
		}
		return true
	case first == "find":
		for _, x := range w[1:] {
			switch x {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls":
				return false
			}
		}
		return true
	case first == "awk":
		return awkReadOnly(w[1:])
	case first == "git":
		args := w[1:]
		for len(args) > 0 && (args[0] == "-C" || args[0] == "--no-pager") {
			if args[0] == "--no-pager" {
				args = args[1:]
			} else {
				args = args[min(2, len(args)):]
			}
		}
		if len(args) == 0 {
			return true
		}
		if hasOption(args, "O", "output", "ext-diff", "textconv", "filters", "exec", "upload-pack", "receive-pack", "open-files-in-pager") {
			return false
		}
		switch args[0] {
		case "branch":
			return len(args) == 1 || (len(args) == 2 && (args[1] == "--show-current" || args[1] == "-v" || args[1] == "-vv")) || listOnly(args[1:], "--list")
		case "remote":
			return len(args) == 1 || (len(args) == 2 && (args[1] == "-v" || args[1] == "--verbose"))
		case "tag":
			return len(args) == 1 || listOnly(args[1:], "--list") || listOnly(args[1:], "-l")
		case "config":
			return len(args) > 1 && (args[1] == "--get" || args[1] == "--get-all" || args[1] == "--get-regexp" || args[1] == "--list" || args[1] == "get" || args[1] == "list")
		case "symbolic-ref":
			return len(args) == 2 && !strings.HasPrefix(args[1], "-")
		case "reflog":
			return len(args) == 1 || args[1] == "show"
		}
		if len(args) > 1 && gitReadSub[args[0]+" "+args[1]] {
			return true
		}
		return gitRead[args[0]]
	case first == "go":
		return len(w) > 1 && (w[1] == "version" || (w[1] == "env" && !hasOption(w[2:], "wu")))
	}
	return plain[first]
}

func listOnly(args []string, option string) bool {
	if len(args) == 0 || args[0] != option {
		return false
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			return false
		}
	}
	return true
}

// hasOption accepts attached short values and long values after an equals
// sign. Unknown parsing cases may scan unnecessarily, but never skip writes.
func hasOption(args []string, short string, long ...string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			name, _, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			for _, option := range long {
				if name == option {
					return true
				}
			}
		} else if strings.HasPrefix(arg, "-") && strings.ContainsAny(arg[1:], short) {
			return true
		}
	}
	return false
}

var sedPrint = regexp.MustCompile(`^([0-9]+|/[^/]*\/)(,([0-9]+|\$|/[^/]*\/))?p$`)

func awkReadOnly(args []string) bool {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false
	}
	// A program file, a loaded extension, or another program can execute
	// arbitrary code. Keep the one inline program form only.
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			return false
		}
	}
	program := args[0]
	if strings.Contains(program, "system") || strings.ContainsAny(program, "|@") {
		return false
	}
	for i := 0; i < len(program); i++ {
		if program[i] == '>' && (i+1 == len(program) || program[i+1] != '=') {
			return false
		}
	}
	return true
}
