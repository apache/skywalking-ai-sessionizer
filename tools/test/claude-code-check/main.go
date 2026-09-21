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
// would. It runs the commands as the pages write them, each in the shell
// the page gives it for this system:
//
//  1. Install script in docs/en/setup/install.md: the command that runs
//     install/asz.sh, or install/asz.ps1 on Windows, in every shell there is.
//  2. Install in docs/en/setup/claude-code-plugin.md: the command that runs
//     install/claude-code-plugin.sh, or .ps1. The plugin must be installed,
//     and Claude Code's cache must hold the plugin's directory alone.
//  3. A headless Claude Code session runs one shell command. The plugin must
//     record the file it wrote, and asz collect must land the record.
//  4. The same session without asz-changes on PATH must still run the
//     command, and leave no record.
//  5. The Upgrade commands, by hand, move the plugin to a second tag, and the
//     install script moves it to a third. Each time the plugin's data must
//     stay and the next session must be recorded. The script run once more
//     must change nothing.
//  6. The By hand commands install the plugin into a new configuration.
//
// Only download addresses change: the mirror selector, the download site,
// the archive, raw.githubusercontent.com and github.com all become a server on
// 127.0.0.1. It holds PACKAGE under three version names, this tree's install
// scripts, a git repository with the marketplace at the three tags, and a
// stand-in for the model's API. As on the real sites, the download site
// holds the first two versions and the archive holds all three, so the third
// comes from the archive. The Windows scripts add a directory to the user's
// Path, so on Windows this runs only in GitHub Actions. CI runs it on each
// binary package's own platform.
//
//	go run ./tools/test/claude-code-check PACKAGE VERSION
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
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
	plugin      = "file-changes"
	// The file the stand-in model asks the shell command to write.
	written = "claudecodecheck.txt"
	// A settings value the plugin reads, equal to its default, so the file
	// changes nothing and its survival across an upgrade can be checked.
	settings = "scan_timeout: 30s\n"

	rawBase = "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/"
	gitURL  = "https://github.com/apache/skywalking-ai-sessionizer.git"
)

var windows = runtime.GOOS == "windows"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/test/claude-code-check PACKAGE VERSION")
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
	_ = os.RemoveAll(c.work)
	fmt.Printf("\n%s works with Claude Code on this machine\n", filepath.Base(c.pkg))
}

type check struct {
	pkg, version string
	tree         string // the repository, the working directory
	work         string
	base         string // the address of the local server
	claude       string
	shells       []string // the shells the pages give for this system, that are here
	aszBin       string   // where install/asz put asz
	bin          string   // where install/claude-code-plugin put asz-changes
	config       string   // Claude Code's configuration directory for this run
	api          *stand
}

func step(format string, args ...any) { fmt.Printf("\n== "+format+"\n", args...) }

// The three versions the local server offers, and the marketplace's tags.
func (c *check) second() string { return c.version + "-next" }
func (c *check) third() string  { return c.version + "-last" }

func (c *check) run() error {
	var err error
	if c.tree, err = os.Getwd(); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.tree, "install", "asz.sh")); err != nil {
		return errors.New("run it from the root of the repository")
	}
	if windows && os.Getenv("GITHUB_ACTIONS") != "true" {
		return errors.New("the Windows install scripts add a directory to the user's Path, so on Windows this runs only in GitHub Actions")
	}
	if c.claude, err = exec.LookPath("claude"); err != nil {
		return errors.New("claude is not on PATH. Install Claude Code first")
	}
	if _, err := os.Stat(c.pkg + ".sha512"); err != nil {
		return fmt.Errorf("%s.sha512 must be beside the package: %w", c.pkg, err)
	}
	names := []string{"sh", "bash", "zsh"}
	if windows {
		names = []string{"powershell", "pwsh"}
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			c.shells = append(c.shells, path)
		}
	}
	if len(c.shells) == 0 {
		return fmt.Errorf("none of %v is here", names)
	}
	if c.work, err = os.MkdirTemp("", "claudecodecheck-"); err != nil {
		return err
	}
	// A Windows runner's TEMP is a short name, C:\Users\RUNNER~1\...
	// Claude Code 2.1.274 refused a shell command's write under it as a
	// suspicious Windows path, so the check works under the long name.
	if long, err := filepath.EvalSymlinks(c.work); err == nil {
		c.work = long
	}
	c.config = filepath.Join(c.work, "config")
	out, err := exec.Command(c.claude, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude --version: %w: %s", err, out)
	}
	fmt.Printf("claude: %s (%s)\n", strings.TrimSpace(string(out)), c.claude)

	for _, s := range []func() error{c.serve, c.installAsz, c.installPlugin, c.recorded, c.collect, c.missing, c.upgradeByHand, c.upgradeByScript, c.byHand} {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

// serve starts the local server. Claude Code refuses a file:// marketplace,
// and git cannot clone shallowly over plain file serving, so the repository
// is served by git http-backend.
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
	mux.HandleFunc("/skywalking/ai-sessionizer/", func(w http.ResponseWriter, r *http.Request) {
		c.site(w, r, "/skywalking/ai-sessionizer/", c.version, c.second())
	})
	mux.HandleFunc("/archive/dist/skywalking/ai-sessionizer/", func(w http.ResponseWriter, r *http.Request) {
		c.site(w, r, "/archive/dist/skywalking/ai-sessionizer/", c.version, c.second(), c.third())
	})
	mux.HandleFunc("/raw/", c.raw)
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
	// Every script may download only from addresses this check replaces,
	// which is known before any of it runs.
	for _, name := range []string{"asz.sh", "asz.ps1", "claude-code-plugin.sh", "claude-code-plugin.ps1"} {
		if _, err := c.script(name); err != nil {
			return err
		}
	}
	fmt.Printf("serving %s as %s, %s and %s, the install scripts, the marketplace at their tags, and the model on %s\n",
		filepath.Base(c.pkg), c.version, c.second(), c.third(), c.base)
	return nil
}

// site serves PACKAGE under a download site's path for each of versions.
// The first version's .sha512 is the one beside PACKAGE; the others name
// their own file, as make checksums writes them.
func (c *check) site(w http.ResponseWriter, r *http.Request, prefix string, versions ...string) {
	version, file, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	held := false
	for _, v := range versions {
		held = held || v == version
	}
	if !held {
		http.NotFound(w, r)
		return
	}
	name := strings.Replace(file, "-"+version+"-", "-"+c.version+"-", 1)
	if name != filepath.Base(c.pkg) && name != filepath.Base(c.pkg)+".sha512" {
		http.NotFound(w, r)
		return
	}
	if version == c.version || !strings.HasSuffix(file, ".sha512") {
		http.ServeFile(w, r, filepath.Join(filepath.Dir(c.pkg), name))
		return
	}
	b, err := os.ReadFile(c.pkg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha512.Sum512(b)
	fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), strings.TrimSuffix(file, ".sha512"))
}

// raw serves this tree's install scripts under any tag, with their download
// addresses pointed here.
func (c *check) raw(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if !strings.HasPrefix(strings.TrimPrefix(r.URL.Path, "/raw/"), "v") || !strings.Contains(r.URL.Path, "/install/") {
		http.NotFound(w, r)
		return
	}
	text, err := c.script(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, text)
}

var closer = regexp.MustCompile(`https://www\.apache\.org/dyn/closer\.lua\?path=([^"&]+)&action=download`)

// script reads an install script and points its download addresses at the
// local server. A script that downloads from anywhere else is refused, so
// a new address in a script cannot reach the network unnoticed.
func (c *check) script(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(c.tree, "install", filepath.Base(name)))
	if err != nil {
		return "", err
	}
	text := string(b)
	if len(closer.FindAllString(text, -1)) != 1 || strings.Count(text, "https://downloads.apache.org/skywalking/") != 1 ||
		strings.Count(text, "https://archive.apache.org/dist/skywalking/") != 1 {
		return "", fmt.Errorf("install/%s no longer names closer.lua, downloads.apache.org and archive.apache.org once each, which this check replaces", name)
	}
	text = closer.ReplaceAllString(text, c.base+"/${1}")
	text = strings.ReplaceAll(text, "https://downloads.apache.org/", c.base+"/")
	text = strings.ReplaceAll(text, "https://archive.apache.org/", c.base+"/archive/")
	text = strings.ReplaceAll(text, gitURL, c.base+"/git/asz.git")
	// Comments may name the script's own address; commands may not.
	for _, line := range strings.Split(text, "\n") {
		if l := strings.TrimSpace(line); !strings.HasPrefix(l, "#") && strings.Contains(l, "https://") && !strings.Contains(l, "https://skywalking.apache.org/downloads/") {
			return "", fmt.Errorf("install/%s downloads from an address this check does not replace: %s", name, l)
		}
	}
	return text, nil
}

// repository commits the marketplace and the plugin at the first tag, and a
// changed plugin at each later tag. It also commits a file outside the
// plugin's directory, which the plugin cache must not get.
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
	steps := []func() error{
		func() error { return gitIn(src, "init", "-q") },
		func() error { return gitIn(src, "add", "-A") },
		func() error { return gitIn(src, "commit", "-q", "-m", c.version) },
		func() error { return gitIn(src, "tag", "v"+c.version) },
	}
	for _, v := range []string{c.second(), c.third()} {
		steps = append(steps,
			func() error { return appendTo(manifest, "\n") },
			func() error { return gitIn(src, "commit", "-q", "-am", v) },
			func() error { return gitIn(src, "tag", "v"+v) })
	}
	steps = append(steps, func() error { return gitIn(c.work, "clone", "-q", "--bare", src, bare) })
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

// installAsz runs the command under Quick install in install.md in every
// shell, each into a home of its own, and keeps the last one's asz.
func (c *check) installAsz() error {
	line, err := c.pageBlock("install.md", "## Install script", "install/asz.", oneLiner())
	if err != nil {
		return err
	}
	for _, shell := range c.shells {
		step("install/asz, from install.md, in %s", filepath.Base(shell))
		home := filepath.Join(c.work, "home-asz-"+strings.TrimSuffix(filepath.Base(shell), ".exe"))
		bin := filepath.Join(home, ".local", "bin")
		out, err := c.run1(shell, line, c.version, c.homeEnv(home, nil))
		fmt.Print(out)
		if err != nil {
			return fmt.Errorf("the asz install command failed in %s: %w", shell, err)
		}
		if err := c.reports(filepath.Join(bin, "asz"+exe()), c.version); err != nil {
			return err
		}
		if !strings.Contains(out, "through the Apache mirrors") {
			return errors.New("install/asz did not take the version on the download site through the mirrors")
		}
		// From 0.5.0 the script installs both programs, as Homebrew and apt
		// do; the plugin installer then finds the recorder already there.
		if err := c.reports(filepath.Join(bin, "asz-changes"+exe()), c.version); err != nil {
			return fmt.Errorf("install/asz did not install asz-changes beside asz: %w", err)
		}
		c.aszBin = bin
	}
	step("install/asz at v%s, which only the archive holds, in %s", c.third(), filepath.Base(c.shells[0]))
	out, err := c.run1(c.shells[0], line, c.third(), c.homeEnv(filepath.Join(c.work, "home-asz-archive"), nil))
	fmt.Print(out)
	if err != nil {
		return fmt.Errorf("the asz install command failed for a version only the archive holds: %w", err)
	}
	if !strings.Contains(out, "from archive.apache.org") {
		return errors.New("install/asz did not take a version the download site does not hold from the archive")
	}
	return nil
}

// installPlugin runs the command under Install in claude-code-plugin.md,
// then checks the binary, the plugin, and what Claude Code copied.
func (c *check) installPlugin() error {
	step("install/claude-code-plugin, from claude-code-plugin.md, in %s", filepath.Base(c.shells[0]))
	line, err := c.pageBlock("claude-code-plugin.md", "## Install", "install/claude-code-plugin.", oneLiner())
	if err != nil {
		return err
	}
	home := filepath.Join(c.work, "home-plugin")
	c.bin = filepath.Join(home, ".local", "bin")
	out, err := c.run1(c.shells[0], line, c.version, c.homeEnv(home, c.claudeEnv(nil, c.config)))
	fmt.Print(out)
	if err != nil {
		return fmt.Errorf("the plugin install command failed: %w", err)
	}
	if err := c.reports(filepath.Join(c.bin, "asz-changes"+exe()), c.version); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.bin, "asz"+exe())); err == nil {
		return errors.New("install/claude-code-plugin installed asz too, which is its own install")
	}
	if err := c.installed(c.config); err != nil {
		return err
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

// recorded runs a session with asz-changes on PATH. The plugin must
// record the file the shell command wrote.
func (c *check) recorded() error {
	step("A session with asz-changes on PATH")
	id, err := c.session("with", []string{c.bin})
	if err != nil {
		return err
	}
	return c.hasRecord(id)
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
	cmd := exec.Command(filepath.Join(c.aszBin, "asz"+exe()), "collect", "-once", "-config", file)
	cmd.Env = c.claudeEnv(nil, c.config)
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

// missing runs a session without asz-changes on PATH. The shell
// command must still run, and no record may be written.
func (c *check) missing() error {
	step("A session without asz-changes on PATH")
	id, err := c.session("without", nil)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.data(), "output", id)); err == nil {
		return fmt.Errorf("the plugin wrote output for session %s with no asz-changes on PATH", id)
	}
	log, _ := os.ReadFile(filepath.Join(c.work, "session-without.log"))
	if bytes.Contains(log, []byte("Executable not found")) {
		fmt.Println("Claude Code reported the missing executable, the command ran, and nothing was recorded")
	} else {
		fmt.Println("the command ran and nothing was recorded; Claude Code's output does not say \"Executable not found\"")
	}
	return nil
}

// upgradeByHand runs the Upgrade commands to the second tag.
func (c *check) upgradeByHand() error {
	step("Upgrade by hand to v%s, in %s", c.second(), filepath.Base(c.shells[0]))
	block, err := c.pageBlock("claude-code-plugin.md", "## Upgrade", "--keep-data")
	if err != nil {
		return err
	}
	return c.moved("by hand", func() (string, error) {
		return c.run1(c.shells[0], block, c.second(), c.claudeEnv(nil, c.config))
	})
}

// upgradeByScript runs the plugin's install command again with the third
// version, then once more, which must change nothing.
func (c *check) upgradeByScript() error {
	shell := c.shells[len(c.shells)-1]
	step("Upgrade with install/claude-code-plugin to v%s, in %s", c.third(), filepath.Base(shell))
	line, err := c.pageBlock("claude-code-plugin.md", "## Install", "install/claude-code-plugin.", oneLiner())
	if err != nil {
		return err
	}
	home := filepath.Join(c.work, "home-plugin")
	env := c.homeEnv(home, c.claudeEnv(nil, c.config))
	if err := c.moved("by the script", func() (string, error) { return c.run1(shell, line, c.third(), env) }); err != nil {
		return err
	}
	step("install/claude-code-plugin at v%s again", c.third())
	out, err := c.run1(shell, line, c.third(), env)
	fmt.Print(out)
	if err != nil {
		return fmt.Errorf("running the plugin install command again failed: %w", err)
	}
	if !strings.Contains(out, "already") {
		return errors.New("running the plugin install command again did not say the plugin was installed already")
	}
	return nil
}

// moved runs an upgrade and checks that the plugin moved to a new version,
// kept its settings and output, and records the next session.
func (c *check) moved(how string, upgrade func() (string, error)) error {
	before, err := c.claudeRun(c.config, "plugin", "list")
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
	out, err := upgrade()
	fmt.Print(out)
	if err != nil {
		return fmt.Errorf("the upgrade %s failed: %w", how, err)
	}
	got, err := os.ReadFile(filepath.Join(c.data(), "settings.yaml"))
	if err != nil {
		return fmt.Errorf("the upgrade %s lost the plugin's settings.yaml: %w", how, err)
	}
	if string(got) != settings {
		return fmt.Errorf("the upgrade %s changed the plugin's settings.yaml to %q", how, got)
	}
	for _, f := range outputs {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("the upgrade %s lost %s: %w", how, f, err)
		}
	}
	after, err := c.claudeRun(c.config, "plugin", "list")
	if err != nil {
		return err
	}
	if !strings.Contains(after, plugin+"@"+marketplace) || versionOf(after) == versionOf(before) {
		return fmt.Errorf("the upgrade %s did not move the plugin:\nbefore:\n%s\nafter:\n%s", how, before, after)
	}
	fmt.Printf("moved from %s to %s, and kept settings.yaml and %d output files\n", versionOf(before), versionOf(after), len(outputs))
	id, err := c.session("after-"+strings.ReplaceAll(how, " ", "-"), []string{c.bin})
	if err != nil {
		return err
	}
	return c.hasRecord(id)
}

// byHand runs the By hand commands into a new configuration.
func (c *check) byHand() error {
	step("Install by hand, in %s", filepath.Base(c.shells[0]))
	block, err := c.pageBlock("claude-code-plugin.md", "### By hand", "claude plugin marketplace add")
	if err != nil {
		return err
	}
	config := filepath.Join(c.work, "config-by-hand")
	out, err := c.run1(c.shells[0], block, c.version, c.claudeEnv(nil, config))
	fmt.Print(out)
	if err != nil {
		return fmt.Errorf("the By hand commands failed: %w", err)
	}
	return c.installed(config)
}

func (c *check) installed(config string) error {
	list, err := c.claudeRun(config, "plugin", "list")
	if err != nil {
		return err
	}
	if !strings.Contains(list, plugin+"@"+marketplace) {
		return fmt.Errorf("claude plugin list does not show %s@%s:\n%s", plugin, marketplace, list)
	}
	return nil
}

func (c *check) reports(binary, version string) error {
	got, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version: %w: %s", binary, err, got)
	}
	if !strings.Contains(string(got), version) {
		return fmt.Errorf("%s reports %s, not %s", binary, got, version)
	}
	return nil
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

var versionRe = regexp.MustCompile(`Version:\s*(\S+)`)

func versionOf(list string) string {
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
	cmd.Env = c.claudeEnv(path, c.config)
	asked := c.api.requests()
	out, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(c.work, "session-"+name+".log"), out, 0o644)
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

// claudeRun runs one claude command in a configuration.
func (c *check) claudeRun(config string, args ...string) (string, error) {
	cmd := exec.Command(c.claude, args...)
	cmd.Env = c.claudeEnv(nil, config)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// claudeEnv is this process's environment without what would point Claude
// Code elsewhere, with Claude Code's directory and path in front of PATH.
// Run from inside Claude Code, the inherited CLAUDECODE and CLAUDE_CODE_*
// variables made a child claude -p ask for a login.
func (c *check) claudeEnv(path []string, config string) []string {
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
		"CLAUDE_CONFIG_DIR="+config,
		"ANTHROPIC_BASE_URL="+c.base,
		"ANTHROPIC_API_KEY=sk-ant-claudecodecheck",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
}

// homeEnv makes home the home of a script, with its .local/bin first on
// PATH, as it is for a person whose Claude Code came from its installer.
func (c *check) homeEnv(home string, env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	path := ""
	for _, kv := range env {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			path = kv[len("PATH="):]
		}
	}
	bin := filepath.Join(home, ".local", "bin")
	return withEnv(env, "HOME="+home, "USERPROFILE="+home, "PATH="+bin+string(os.PathListSeparator)+path,
		"NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
}

// pageBlock reads the one block under heading, in the shell language the
// page gives this system, whose text holds every one of contains, with raw
// GitHub addresses pointed at the local server.
func (c *check) pageBlock(page, heading string, contains ...string) (string, error) {
	lang := "sh"
	if windows {
		lang = "powershell"
	}
	b, err := os.ReadFile(filepath.Join(c.tree, "docs", "en", "setup", page))
	if err != nil {
		return "", err
	}
	found := blocks(section(string(b), heading), lang, contains...)
	if len(found) != 1 {
		return "", fmt.Errorf("%s has %d %s blocks under %q holding %q, want 1", page, len(found), lang, heading, contains)
	}
	// A page sets the version with a placeholder, VERSION=<version>, which this
	// check sets itself before the block.
	var lines []string
	for _, l := range strings.Split(found[0], "\n") {
		if !strings.Contains(l, "<version>") {
			lines = append(lines, l)
		}
	}
	block := strings.ReplaceAll(strings.Join(lines, "\n"), rawBase, c.base+"/raw/")
	return strings.ReplaceAll(block, gitURL, c.base+"/git/asz.git"), nil
}

// run1 runs a block from a page the way a person pastes it, with the version
// set first as the page says. A PowerShell block runs from a file, whose
// exit code is the last command's.
func (c *check) run1(shell, block, version string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if windows {
		file := filepath.Join(c.work, fmt.Sprintf("block-%d.ps1", time.Now().UnixNano()))
		text := "$Version = '" + version + "'\n" + block + "\nexit $LASTEXITCODE\n"
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			return "", err
		}
		cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
	} else {
		cmd = exec.CommandContext(ctx, shell, "-c", "VERSION='"+version+"'\n"+block)
	}
	cmd.Env = env
	cmd.Dir = c.work
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// section is the text under heading, up to the next heading of any level,
// so a subsection's blocks are not its parent's.
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

// blocks are the fenced blocks of lang in text whose text holds every one
// of contains. A block indented under a list item loses that indentation.
func blocks(text, lang string, contains ...string) []string {
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
		all := true
		for _, want := range contains {
			all = all && strings.Contains(b, want)
		}
		if all {
			found = append(found, b)
		}
	}
	return found
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

// oneLiner is what the command that fetches and runs an install script
// holds, apart from a block that downloads the script to read it first.
func oneLiner() string {
	if windows {
		return "Invoke-RestMethod"
	}
	return "| sh -s"
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
		key := strings.ToUpper(kv[:strings.Index(kv, "=")+1])
		out := env[:0:0]
		for _, e := range env {
			if !strings.HasPrefix(strings.ToUpper(e), key) {
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
