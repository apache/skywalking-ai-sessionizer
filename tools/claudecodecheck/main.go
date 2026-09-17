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

// Command claudecodecheck follows the install pages on this machine, with
// the Claude Code that is on PATH, and fails where a person following them
// would:
//
//  1. The Quick install block of docs/en/setup/install.md installs asz and
//     asz-claude-plugin from PACKAGE. On Windows that is the PowerShell block,
//     in every PowerShell there is; elsewhere the shell block, in every
//     shell there is.
//  2. The commands under Install in docs/en/setup/claude-code-plugin.md add
//     the marketplace at the version's tag and install the plugin.
//  3. A headless Claude Code session runs one shell command. The plugin must
//     record the file it wrote, and asz collect must land the record.
//  4. The same session without asz-claude-plugin on PATH must still run the
//     command, and leave no record.
//  5. The Upgrade block moves the plugin to a second tag. The plugin's data
//     must stay, and the next session must be recorded again.
//
// Each block is read from the page, and only its download addresses are
// changed, to a server on 127.0.0.1 that holds PACKAGE, a git repository with
// the marketplace at two tags, and a stand-in for the model's API. The
// Windows block adds a directory to the user's Path, so on Windows this runs
// only in GitHub Actions. CI runs it on each binary package's own platform.
//
//	go run ./tools/claudecodecheck PACKAGE VERSION
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	marketplace = "skywalking-ai-sessionizer"
	plugin      = "asz-changes"
	// The file the stand-in model asks the shell command to write.
	written = "claudecodecheck.txt"
	// A settings value the plugin reads, equal to its default, so the file
	// changes nothing and its survival across the upgrade can be checked.
	settings = "scan_timeout: 30s\n"
)

var windows = runtime.GOOS == "windows"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/claudecodecheck PACKAGE VERSION")
		os.Exit(2)
	}
	c := &check{pkg: os.Args[1], version: os.Args[2]}
	if err := c.run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nclaudecodecheck: %v\n", err)
		if c.work != "" {
			fmt.Fprintf(os.Stderr, "claudecodecheck: the files of this run are kept in %s\n", c.work)
		}
		os.Exit(1)
	}
	os.RemoveAll(c.work)
	fmt.Printf("\n%s works with Claude Code on this machine\n", filepath.Base(c.pkg))
}

type check struct {
	pkg, version string
	tree         string // the repository, the working directory
	work         string
	base         string // the address of the local server
	claude       string
	bin          string // where the install block put the two binaries
	config       string // Claude Code's configuration directory for this run
	api          *stand
}

func step(format string, args ...any) { fmt.Printf("\n== "+format+"\n", args...) }

func (c *check) run() error {
	var err error
	if c.tree, err = os.Getwd(); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.tree, "docs", "en", "setup", "install.md")); err != nil {
		return errors.New("run it from the root of the repository")
	}
	if windows && os.Getenv("GITHUB_ACTIONS") != "true" {
		return errors.New("the Windows install block adds a directory to the user's Path, so on Windows this runs only in GitHub Actions")
	}
	if c.claude, err = exec.LookPath("claude"); err != nil {
		return errors.New("claude is not on PATH. Install Claude Code first")
	}
	if _, err := os.Stat(c.pkg + ".sha512"); err != nil {
		return fmt.Errorf("%s.sha512 must be beside the package: %w", c.pkg, err)
	}
	if c.work, err = os.MkdirTemp("", "claudecodecheck-"); err != nil {
		return err
	}
	c.config = filepath.Join(c.work, "config")
	out, err := exec.Command(c.claude, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude --version: %w: %s", err, out)
	}
	fmt.Printf("claude: %s (%s)\n", strings.TrimSpace(string(out)), c.claude)

	if err := c.serve(); err != nil {
		return err
	}
	for _, s := range []func() error{c.install, c.installPlugin, c.recorded, c.collect, c.missing, c.upgrade} {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

// serve starts the local server: the package under the path the download
// site uses, the marketplace repository over git's HTTP protocol, and the
// stand-in model. Claude Code refuses a file:// marketplace, and git cannot
// clone shallowly over plain file serving, so the repository needs
// git http-backend.
func (c *check) serve() error {
	step("The local server")
	git, err := exec.LookPath("git")
	if err != nil {
		return errors.New("git is not on PATH")
	}
	repos := filepath.Join(c.work, "git")
	if err := c.repository(git, filepath.Join(repos, "asz.git")); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	c.base = "http://" + ln.Addr().String()
	c.api = &stand{}
	mux := http.NewServeMux()
	site := "/skywalking/ai-sessionizer/" + c.version + "/"
	mux.Handle(site, http.StripPrefix(site, http.FileServer(http.Dir(filepath.Dir(c.pkg)))))
	mux.Handle("/git/", &cgi.Handler{
		Path:       git,
		Root:       "/git",
		Args:       []string{"http-backend"},
		Env:        []string{"GIT_PROJECT_ROOT=" + repos, "GIT_HTTP_EXPORT_ALL=1"},
		InheritEnv: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME", "USERPROFILE"},
	})
	mux.Handle("/v1/", c.api)
	// The server lives as long as the check, which ends the process.
	go func() { _ = http.Serve(ln, mux) }()
	fmt.Printf("serving %s, the marketplace at v%s and v%s, and the model on %s\n", filepath.Base(c.pkg), c.version, c.next(), c.base)
	return nil
}

func (c *check) next() string { return c.version + "-next" }

// repository commits the marketplace and the plugin at the version's tag,
// then a changed plugin at a second tag for the upgrade. It also commits a
// file outside the plugin's directory, which the plugin cache must not get.
func (c *check) repository(git, bare string) error {
	src := filepath.Join(c.work, "repository")
	for _, rel := range []string{".claude-plugin", filepath.Join("plugins", "claude-code", "plugin"), filepath.Join("plugins", "claude-code", "main.go")} {
		if err := copyPath(filepath.Join(c.tree, rel), filepath.Join(src, rel)); err != nil {
			return err
		}
	}
	gitIn := func(dir string, args ...string) error {
		cmd := exec.Command(git, append([]string{"-c", "user.name=claudecodecheck", "-c", "user.email=claudecodecheck@invalid", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	manifest := filepath.Join(src, "plugins", "claude-code", "plugin", ".claude-plugin", "plugin.json")
	for _, s := range []func() error{
		func() error { return gitIn(src, "init", "-q") },
		func() error { return gitIn(src, "add", "-A") },
		func() error { return gitIn(src, "commit", "-q", "-m", "the version") },
		func() error { return gitIn(src, "tag", "v"+c.version) },
		func() error { return appendTo(manifest, "\n") },
		func() error { return gitIn(src, "commit", "-q", "-am", "the next version") },
		func() error { return gitIn(src, "tag", "v"+c.next()) },
		func() error { return gitIn(c.work, "clone", "-q", "--bare", src, bare) },
	} {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

// install runs the Quick install block in every shell this machine has for
// it, each into a home of its own, and keeps the last one's binaries.
func (c *check) install() error {
	lang, shells := "sh", []string{"sh", "bash", "zsh"}
	if windows {
		lang, shells = "powershell", []string{"powershell", "pwsh"}
	}
	block, err := c.block(filepath.Join("docs", "en", "setup", "install.md"), "## Quick install", lang, "VERSION")
	if err != nil {
		return err
	}
	block, err = c.rewriteDownloads(block)
	if err != nil {
		return err
	}
	ran := 0
	for _, shell := range shells {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		step("Quick install in %s", shell)
		home := filepath.Join(c.work, "home-"+shell)
		if err := os.MkdirAll(home, 0o755); err != nil {
			return err
		}
		bin := filepath.Join(home, ".local", "bin")
		env := withEnv(os.Environ(), "HOME="+home, "USERPROFILE="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
		out, err := c.script(path, block, env, c.work)
		fmt.Print(out)
		if err != nil {
			return fmt.Errorf("the Quick install block failed in %s: %w", shell, err)
		}
		for _, name := range []string{"asz", "asz-claude-plugin"} {
			got, err := exec.Command(filepath.Join(bin, name+exe()), "version").CombinedOutput()
			if err != nil || !strings.Contains(string(got), c.version) {
				return fmt.Errorf("%s from the Quick install block in %s does not report %s: %w %s", name, shell, c.version, err, got)
			}
		}
		c.bin = bin
		ran++
	}
	if ran == 0 {
		return fmt.Errorf("no shell here runs the %s block", lang)
	}
	return nil
}

// installPlugin runs the commands under Install that add the marketplace
// and install the plugin, then checks what Claude Code copied.
func (c *check) installPlugin() error {
	step("Install the plugin")
	if err := c.pluginBlock("## Install", "claude plugin marketplace add", c.version); err != nil {
		return err
	}
	list, err := c.claudeRun(nil, "plugin", "list")
	if err != nil {
		return err
	}
	if !strings.Contains(list, plugin+"@"+marketplace) {
		return fmt.Errorf("claude plugin list does not show %s@%s:\n%s", plugin, marketplace, list)
	}
	dirs, _ := filepath.Glob(filepath.Join(c.config, "plugins", "cache", marketplace, plugin, "*"))
	if len(dirs) != 1 {
		return fmt.Errorf("the plugin cache holds %d versions of %s, want 1: %v", len(dirs), plugin, dirs)
	}
	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "LICENSE", "NOTICE"} {
		if _, err := os.Stat(filepath.Join(dirs[0], filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("the plugin cache lacks %s: %w", rel, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dirs[0], "*.go")); len(matches) > 0 {
		return fmt.Errorf("the plugin cache holds Go source: %v", matches)
	}
	fmt.Printf("installed; the cache holds the manifest, the hooks, LICENSE and NOTICE in %s\n", filepath.Base(dirs[0]))
	return nil
}

func (c *check) pluginBlock(heading, contains, version string) error {
	lang, shell := "sh", "sh"
	if windows {
		lang, shell = "powershell", "pwsh"
		if _, err := exec.LookPath(shell); err != nil {
			shell = "powershell"
		}
	}
	block, err := c.block(filepath.Join("docs", "en", "setup", "claude-code-plugin.md"), heading, lang, contains)
	if err != nil {
		return err
	}
	const github = "https://github.com/apache/skywalking-ai-sessionizer.git"
	if !strings.Contains(block, github) {
		return fmt.Errorf("the %s block under %s no longer names %s", lang, heading, github)
	}
	block = strings.ReplaceAll(block, github, c.base+"/git/asz.git")
	path, err := exec.LookPath(shell)
	if err != nil {
		return err
	}
	env := c.claudeEnv(nil)
	env = withEnv(env, "VERSION="+version)
	out, err := c.script(path, block, env, c.work, version)
	fmt.Print(out)
	if err != nil {
		return fmt.Errorf("the %s block under %s failed: %w", lang, heading, err)
	}
	return nil
}

// recorded runs a session with asz-claude-plugin on PATH. The plugin must
// record the file the shell command wrote.
func (c *check) recorded() error {
	step("A session with asz-claude-plugin on PATH")
	id, err := c.session("with", []string{c.bin})
	if err != nil {
		return err
	}
	return c.hasRecord(id)
}

func (c *check) hasRecord(id string) error {
	file := filepath.Join(c.data(), "output", id, "main.jsonl")
	f, err := os.Open(file)
	if err != nil {
		c.showPluginLog()
		return fmt.Errorf("the plugin wrote no record for session %s: %w", id, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var rec struct {
			Changes []struct {
				Path      string `json:"path"`
				Operation string `json:"operation"`
			} `json:"changes"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		for _, ch := range rec.Changes {
			if ch.Path == written && ch.Operation == "create" {
				fmt.Printf("the plugin recorded %s as created, in %s\n", written, file)
				return nil
			}
		}
	}
	c.showPluginLog()
	return fmt.Errorf("no record in %s names %s as created", file, written)
}

// collect lands the plugin's records with the installed asz.
func (c *check) collect() error {
	step("asz collect")
	cfg, err := os.ReadFile(filepath.Join(c.tree, "asz.yaml"))
	if err != nil {
		return err
	}
	root := filepath.Join(c.work, "root")
	text := string(cfg)
	if !strings.Contains(text, "  root: ./data\n") {
		return errors.New("asz.yaml no longer sets root: ./data, which this check replaces")
	}
	text = strings.Replace(text, "  root: ./data\n", "  root: '"+filepath.ToSlash(root)+"'\n", 1)
	// The default leaves out sessions under /private/tmp, where a macOS
	// temporary directory can resolve.
	text = strings.ReplaceAll(text, "    exclude:\n      - /private/tmp/**\n", "    exclude: []\n")
	file := filepath.Join(c.work, "asz.yaml")
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(c.bin, "asz"+exe()), "collect", "-once", "-config", file)
	cmd.Env = c.claudeEnv(nil)
	out, err := cmd.CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("asz collect: %w", err)
	}
	landed, _ := filepath.Glob(filepath.Join(root, "*", "streams", "main", "changes-*.sd"))
	if len(landed) == 0 {
		return fmt.Errorf("asz collect landed no changes file under %s", root)
	}
	fmt.Printf("asz landed %s\n", filepath.Base(landed[0]))
	return nil
}

// missing runs a session without asz-claude-plugin on PATH. The shell
// command must still run, and no record may be written.
func (c *check) missing() error {
	step("A session without asz-claude-plugin on PATH")
	id, err := c.session("without", nil)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.data(), "output", id)); err == nil {
		return fmt.Errorf("the plugin wrote output for session %s with no asz-claude-plugin on PATH", id)
	}
	log, _ := os.ReadFile(filepath.Join(c.work, "session-without.log"))
	if bytes.Contains(log, []byte("Executable not found")) {
		fmt.Println("Claude Code reported the missing executable, the command ran, and nothing was recorded")
	} else {
		fmt.Println("the command ran and nothing was recorded; Claude Code's output does not say \"Executable not found\"")
	}
	return nil
}

// upgrade runs the Upgrade block to the second tag. The settings and the
// output must stay, and the next session must be recorded.
func (c *check) upgrade() error {
	step("Upgrade to v%s", c.next())
	before, err := c.claudeRun(nil, "plugin", "list")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.data(), "settings.yaml"), []byte(settings), 0o644); err != nil {
		return err
	}
	outputs, _ := filepath.Glob(filepath.Join(c.data(), "output", "*", "main.jsonl"))
	if len(outputs) == 0 {
		return errors.New("there is no output to keep across the upgrade")
	}
	if err := c.pluginBlock("### Upgrade", "--keep-data", c.next()); err != nil {
		return err
	}
	got, err := os.ReadFile(filepath.Join(c.data(), "settings.yaml"))
	if err != nil {
		return fmt.Errorf("the upgrade lost the plugin's settings.yaml: %w", err)
	}
	if string(got) != settings {
		return fmt.Errorf("the upgrade changed the plugin's settings.yaml to %q", got)
	}
	for _, f := range outputs {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("the upgrade lost %s: %w", f, err)
		}
	}
	after, err := c.claudeRun(nil, "plugin", "list")
	if err != nil {
		return err
	}
	if !strings.Contains(after, plugin+"@"+marketplace) || versionLine(after) == versionLine(before) {
		return fmt.Errorf("the plugin did not move to v%s:\nbefore:\n%s\nafter:\n%s", c.next(), before, after)
	}
	fmt.Printf("moved from %s to %s, and kept settings.yaml and %d output files\n", versionLine(before), versionLine(after), len(outputs))
	step("A session after the upgrade")
	id, err := c.session("upgraded", []string{c.bin})
	if err != nil {
		return err
	}
	return c.hasRecord(id)
}

var versionRe = regexp.MustCompile(`Version:\s*(\S+)`)

func versionLine(list string) string {
	if m := versionRe.FindStringSubmatch(list); m != nil {
		return m[1]
	}
	return ""
}

func (c *check) data() string {
	return filepath.Join(c.config, "plugins", "data", plugin+"-"+marketplace)
}

func (c *check) showPluginLog() {
	if b, err := os.ReadFile(filepath.Join(c.data(), "log", "plugin.log")); err == nil && len(b) > 0 {
		fmt.Printf("-- the plugin's log\n%s\n", b)
	}
}

var sessionRe = regexp.MustCompile(`"session_id":"([0-9a-f-]{36})"`)

// session runs Claude Code headless in a new workspace, with the given
// directories in front of PATH. The stand-in model has it run one shell
// command that writes a file, and the file must be there afterwards.
func (c *check) session(name string, path []string) (string, error) {
	ws := filepath.Join(c.work, "workspace-"+name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.claude, "-p", "Write the file.", "--allowedTools", "Bash,PowerShell",
		"--output-format", "stream-json", "--verbose", "--model", "claude-opus-5")
	cmd.Dir = ws
	cmd.Env = c.claudeEnv(path)
	asked := c.api.requests()
	out, err := cmd.CombinedOutput()
	logFile := filepath.Join(c.work, "session-"+name+".log")
	_ = os.WriteFile(logFile, out, 0o644)
	if err != nil {
		return "", fmt.Errorf("claude -p: %w\n%s", err, tail(out, 40))
	}
	if _, err := os.Stat(filepath.Join(ws, written)); err != nil {
		return "", fmt.Errorf("the session's shell command did not write %s\n%s", written, tail(out, 40))
	}
	m := sessionRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("claude -p printed no session id\n%s", tail(out, 40))
	}
	fmt.Printf("session %s wrote %s in %d requests to the model\n", m[1], written, c.api.requests()-asked)
	return string(m[1]), nil
}

// claudeRun runs one claude command in this run's configuration.
func (c *check) claudeRun(path []string, args ...string) (string, error) {
	cmd := exec.Command(c.claude, args...)
	cmd.Env = c.claudeEnv(path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// claudeEnv is this process's environment without what would point Claude
// Code elsewhere. Run from inside Claude Code, the inherited CLAUDECODE and
// CLAUDE_CODE_* variables made a child claude -p ask for a login.
func (c *check) claudeEnv(path []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		i := strings.Index(kv, "=")
		key := strings.ToUpper(kv[:max(i, 0)])
		if i > 0 && (strings.HasPrefix(key, "CLAUDE") || strings.HasPrefix(key, "ANTHROPIC_") || key == "PATH") {
			continue
		}
		env = append(env, kv)
	}
	dirs := append(append([]string{}, path...), filepath.Dir(c.claude), os.Getenv("PATH"))
	return append(env,
		"PATH="+strings.Join(dirs, string(os.PathListSeparator)),
		"CLAUDE_CONFIG_DIR="+c.config,
		"ANTHROPIC_BASE_URL="+c.base,
		"ANTHROPIC_API_KEY=sk-ant-claudecodecheck",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
}

// script runs a block from a page the way a person pastes it, with the
// version set first. A PowerShell block runs from a file, whose exit code
// is the last command's.
func (c *check) script(shell, block string, env []string, dir string, version ...string) (string, error) {
	v := c.version
	if len(version) > 0 {
		v = version[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if windows {
		file := filepath.Join(c.work, fmt.Sprintf("block-%d.ps1", time.Now().UnixNano()))
		text := "$Version = '" + v + "'\n" + block + "\nexit $LASTEXITCODE\n"
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			return "", err
		}
		cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
	} else {
		cmd = exec.CommandContext(ctx, shell, "-c", "VERSION='"+v+"'\n"+block)
	}
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// block reads the one fenced block of lang under heading whose text holds
// contains. The section ends at the next heading of any level, so a
// subsection's blocks are not its parent's. A block indented under a list
// item loses that indentation.
func (c *check) block(page, heading, lang, contains string) (string, error) {
	b, err := os.ReadFile(filepath.Join(c.tree, page))
	if err != nil {
		return "", err
	}
	found := blocks(section(string(b), heading), lang, contains)
	if len(found) != 1 {
		return "", fmt.Errorf("%s has %d %s blocks under %q holding %q, want 1", page, len(found), lang, heading, contains)
	}
	return found[0], nil
}

func section(text, heading string) string {
	var out []string
	in, fence := false, false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(l, "#") {
			if in {
				break
			}
			if l == heading {
				in = true
				continue
			}
		}
		if in {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func blocks(text, lang, contains string) []string {
	var found []string
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		if strings.TrimSpace(lines[i]) != "```"+lang {
			continue
		}
		var body []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			body = append(body, strings.TrimPrefix(lines[i], indent))
		}
		b := strings.Join(body, "\n") + "\n"
		if strings.Contains(b, contains) {
			found = append(found, b)
		}
	}
	return found
}

var closer = regexp.MustCompile(`https://www\.apache\.org/dyn/closer\.lua\?path=([^"&]+)&action=download`)

// rewriteDownloads points a Quick install block at the local server: the
// mirror selector and the download site each become its address, and
// nothing else in the block may leave the machine.
func (c *check) rewriteDownloads(block string) (string, error) {
	if len(closer.FindAllString(block, -1)) != 1 || strings.Count(block, "https://downloads.apache.org/") != 1 {
		return "", errors.New("the Quick install block no longer downloads once through closer.lua and once from downloads.apache.org, which this check replaces")
	}
	block = closer.ReplaceAllString(block, c.base+"/${1}")
	block = strings.ReplaceAll(block, "https://downloads.apache.org/", c.base+"/")
	if strings.Contains(block, "https://") {
		return "", errors.New("the Quick install block downloads from an address this check does not replace")
	}
	return block, nil
}

// stand answers as the Messages API does, enough for one tool call: while
// no message holds a tool result, it asks for the shell tool the request
// offers, and then it ends the turn. Checking only the last message for a
// tool result looped, because Claude Code sends more after it.
type stand struct {
	mu sync.Mutex
	n  int
}

func (s *stand) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *stand) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		writeJSON(w, map[string]any{"input_tokens": 10})
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Model    string                  `json:"model"`
		Stream   bool                    `json:"stream"`
		Tools    []struct{ Name string } `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.n++
	id := s.n
	s.mu.Unlock()

	tool := ""
	for _, t := range req.Tools {
		if t.Name == "Bash" {
			tool = "Bash"
			break
		}
		if t.Name == "PowerShell" {
			tool = "PowerShell"
		}
	}
	answered := false
	for _, m := range req.Messages {
		var parts []struct{ Type string }
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				answered = answered || p.Type == "tool_result"
			}
		}
	}
	block := map[string]any{"type": "text", "text": "done"}
	stop := "end_turn"
	if tool != "" && !answered {
		command := "echo hello > " + written
		if tool == "PowerShell" {
			command = "Set-Content -Path " + written + " -Value hello"
		}
		block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_claudecodecheck_%d", id), "name": tool,
			"input": map[string]any{"command": command, "description": "Write the file"}}
		stop = "tool_use"
	}
	msg := map[string]any{"id": fmt.Sprintf("msg_claudecodecheck_%d", id), "type": "message", "role": "assistant",
		"model": req.Model, "content": []any{block}, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}}
	if !req.Stream {
		writeJSON(w, msg)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(name string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	}
	start := map[string]any{}
	for k, v := range msg {
		start[k] = v
	}
	start["content"], start["stop_reason"] = []any{}, nil
	event("message_start", map[string]any{"type": "message_start", "message": start})
	if block["type"] == "tool_use" {
		input, _ := json.Marshal(block["input"])
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
	} else {
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": block["text"]}})
	}
	event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 5}})
	event("message_stop", map[string]any{"type": "message_stop"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func exe() string {
	if windows {
		return ".exe"
	}
	return ""
}

// withEnv sets each key=value in env, replacing an earlier value of the key.
// Windows compares keys without case.
func withEnv(env []string, set ...string) []string {
	for _, kv := range set {
		key := kv[:strings.Index(kv, "=")+1]
		out := env[:0:0]
		for _, e := range env {
			if !strings.HasPrefix(strings.ToUpper(e), strings.ToUpper(key)) {
				out = append(out, e)
			}
		}
		env = append(out, kv)
	}
	return env
}

func copyPath(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func appendTo(file, text string) error {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
