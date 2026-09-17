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

// Command debpackage writes one Debian package from the files make binaries
// staged for a Linux platform. make binaries runs it for every package in
// DEB_PACKAGES and every Linux platform in PLATFORMS.
//
//	go run ./tools/debpackage -package asz -version 0.4.0 -arch amd64 \
//	  -from dist/build/linux-amd64 -time 1757000000 -out FILE.deb
//
// A package holds its binary in /usr/bin, and the LICENSE, the NOTICE and the
// dependency licenses under /usr/share/doc/PACKAGE, as the other binary
// packages hold them beside the binary.
//
// The tool writes the archive itself rather than run dpkg-deb, which macOS
// does not have. Two runs with the same files, version and time write the
// same bytes: every entry gets the time given, owner and group 0, a fixed
// mode, and one sorted order, and gzip records no name and no time.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5" //nolint:gosec // dpkg --verify reads md5sums; it is not a security check
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A package of this project: the binary it installs, and what apt shows.
type definition struct {
	binary      string
	summary     string
	description []string
}

var definitions = map[string]definition{
	"asz": {
		binary:  "asz",
		summary: "Conversation-level observability for long-lived AI agents",
		description: []string{
			"Apache SkyWalking AI Sessionizer assembles fragmented agent telemetry,",
			"such as transcripts, subagent streams and workflow journals, into one",
			"durable conversation structure.",
		},
	},
	"asz-claude-code": {
		binary:  "asz-claude-plugin",
		summary: "Claude Code plugin that records which files each tool call changed",
		description: []string{
			"The binary the hooks of the asz-changes plugin run. The plugin itself is",
			"installed into Claude Code with the claude plugin commands.",
		},
	},
}

var arches = map[string]bool{"amd64": true, "arm64": true}

func main() {
	pkg := flag.String("package", "", "the package: "+strings.Join(names(), " or "))
	version := flag.String("version", "", "the version the binaries report")
	arch := flag.String("arch", "", "the Debian architecture: amd64 or arm64")
	from := flag.String("from", "", "the directory make binaries staged the platform's files in")
	epoch := flag.Int64("time", -1, "the time of every entry, in seconds since 1970")
	out := flag.String("out", "", "the package file to write")
	flag.Parse()
	if err := run(*pkg, *version, *arch, *from, *epoch, *out); err != nil {
		fmt.Fprintln(os.Stderr, "debpackage:", err)
		os.Exit(1)
	}
}

func names() []string {
	var list []string
	for name := range definitions {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}

func run(pkg, version, arch, from string, epoch int64, out string) error {
	def, ok := definitions[pkg]
	if !ok {
		return fmt.Errorf("unknown package %q: the packages are %s", pkg, strings.Join(names(), ", "))
	}
	if !arches[arch] {
		return fmt.Errorf("unknown architecture %q: amd64 or arm64", arch)
	}
	if version == "" || from == "" || out == "" || epoch < 0 {
		return errors.New("-version, -from, -time and -out are all needed")
	}
	stamp := time.Unix(epoch, 0).UTC()
	files, err := stage(def, pkg, from)
	if err != nil {
		return err
	}
	data, installed, sums, err := dataArchive(files, stamp)
	if err != nil {
		return err
	}
	control := controlFile(pkg, def, DebianVersion(version), arch, installed)
	meta, err := controlArchive(control, sums, stamp)
	if err != nil {
		return err
	}
	var deb bytes.Buffer
	deb.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		body []byte
	}{{"debian-binary", []byte("2.0\n")}, {"control.tar.gz", meta}, {"data.tar.gz", data}} {
		arMember(&deb, m.name, m.body, epoch)
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, deb.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

// debianVersionPattern is what dpkg accepts as a version with no epoch: it
// starts with a digit, and holds letters, digits and . + ~ -.
var debianVersionPattern = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~-]*$`)

// DebianVersion is the version the package states. A release version is
// used as it is. A build of a commit that is not tagged reports the commit,
// which can start with a letter, and dpkg refuses such a version. It becomes
// 0~COMMIT, which dpkg accepts and orders before every release.
func DebianVersion(version string) string {
	if debianVersionPattern.MatchString(version) {
		return version
	}
	cleaned := regexp.MustCompile(`[^A-Za-z0-9.+~]`).ReplaceAllString(version, ".")
	return "0~" + cleaned
}

// A file of the package: where it goes, and where it comes from.
type file struct {
	target string // relative to /, with no leading slash
	source string
	mode   int64
}

func stage(def definition, pkg, from string) ([]file, error) {
	bin := filepath.Join(from, def.binary)
	if info, err := os.Stat(bin); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file that make binaries staged", bin)
	}
	files := []file{{target: "usr/bin/" + def.binary, source: bin, mode: 0o755}}
	doc := "usr/share/doc/" + pkg
	for _, name := range []string{"LICENSE", "NOTICE"} {
		source := filepath.Join(from, name)
		if _, err := os.Stat(source); err != nil {
			return nil, fmt.Errorf("%s is missing: every binary package carries it", source)
		}
		files = append(files, file{target: doc + "/" + name, source: source, mode: 0o644})
	}
	licenses := filepath.Join(from, "licenses")
	err := filepath.WalkDir(licenses, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// make binaries removes macOS metadata from the staged files. One
		// that is still there is refused, not packaged.
		if name := d.Name(); name == ".DS_Store" || name == "__MACOSX" || strings.HasPrefix(name, "._") {
			return fmt.Errorf("%s is macOS metadata, which no package may hold", p)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, err := filepath.Rel(licenses, p)
		if err != nil {
			return err
		}
		files = append(files, file{target: doc + "/licenses/" + filepath.ToSlash(rel), source: p, mode: 0o644})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("the dependency licenses in %s: %w", licenses, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].target < files[j].target })
	return files, nil
}

// dataArchive returns data.tar.gz, the installed size in KiB as dpkg counts
// it, and the md5sums file.
func dataArchive(files []file, stamp time.Time) ([]byte, int64, []byte, error) {
	var out bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
	var entries []string
	for _, f := range files {
		for d := path.Dir(f.target); d != "."; d = path.Dir(d) {
			if !dirs[d] {
				dirs[d] = true
				entries = append(entries, d+"/")
			}
		}
	}
	sort.Strings(entries)
	header := func(name string, mode, size int64, kind byte) *tar.Header {
		return &tar.Header{Name: "./" + name, Mode: mode, Size: size, Typeflag: kind, ModTime: stamp,
			Uname: "root", Gname: "root", Format: tar.FormatGNU}
	}
	if err := tw.WriteHeader(header("", 0o755, 0, tar.TypeDir)); err != nil {
		return nil, 0, nil, err
	}
	for _, d := range entries {
		if err := tw.WriteHeader(header(d, 0o755, 0, tar.TypeDir)); err != nil {
			return nil, 0, nil, err
		}
	}
	var installed int64
	var sums bytes.Buffer
	for _, f := range files {
		body, err := os.ReadFile(f.source)
		if err != nil {
			return nil, 0, nil, err
		}
		if err := tw.WriteHeader(header(f.target, f.mode, int64(len(body)), tar.TypeReg)); err != nil {
			return nil, 0, nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, 0, nil, err
		}
		installed += (int64(len(body)) + 1023) / 1024
		sum := md5.Sum(body) //nolint:gosec // see the import
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), f.target)
	}
	// dpkg-gencontrol counts each directory as 1 KiB too.
	installed += int64(len(entries))
	if err := tw.Close(); err != nil {
		return nil, 0, nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, nil, err
	}
	return out.Bytes(), installed, sums.Bytes(), nil
}

func controlFile(pkg string, def definition, version, arch string, installed int64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "Package: %s\n", pkg)
	fmt.Fprintf(&b, "Version: %s\n", version)
	fmt.Fprintf(&b, "Architecture: %s\n", arch)
	b.WriteString("Maintainer: Apache SkyWalking <dev@skywalking.apache.org>\n")
	fmt.Fprintf(&b, "Installed-Size: %s\n", strconv.FormatInt(installed, 10))
	b.WriteString("Section: devel\n")
	b.WriteString("Priority: optional\n")
	b.WriteString("Homepage: https://skywalking.apache.org/\n")
	fmt.Fprintf(&b, "Description: %s\n", def.summary)
	for _, line := range def.description {
		fmt.Fprintf(&b, " %s\n", line)
	}
	return []byte(b.String())
}

func controlArchive(control, sums []byte, stamp time.Time) ([]byte, error) {
	var out bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: stamp,
		Uname: "root", Gname: "root", Format: tar.FormatGNU}); err != nil {
		return nil, err
	}
	for _, m := range []struct {
		name string
		body []byte
	}{{"control", control}, {"md5sums", sums}} {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + m.name, Mode: 0o644, Size: int64(len(m.body)),
			Typeflag: tar.TypeReg, ModTime: stamp, Uname: "root", Gname: "root", Format: tar.FormatGNU}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(m.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// arMember writes one member the way dpkg-deb does: the name with no
// trailing slash, owner and group 0, mode 100644, and a newline after an odd
// size.
func arMember(w io.Writer, name string, body []byte, epoch int64) {
	fmt.Fprintf(w, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, epoch, 0, 0, "100644", len(body))
	_, _ = w.Write(body)
	if len(body)%2 == 1 {
		_, _ = w.Write([]byte("\n"))
	}
}
