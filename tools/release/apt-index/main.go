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

// Command aptindex adds released Debian packages to the apt repository that
// the SkyWalking website serves at https://skywalking.apache.org/apt, from
// static/apt in apache/skywalking-website.
//
//	go run ./tools/release/apt-index -dir APT_DIR PACKAGE.deb...
//
// The repository holds no package. It holds the index, which lists every
// version of every package, and a .htaccess that redirects each package's
// address in the index to the file on the Apache download sites. apt follows
// the redirect and checks the file against the index. The newest version is
// on the download mirrors, and archive.apache.org keeps every version, so the
// newest redirects to the mirrors and every older one to the archive.
//
// It writes, under APT_DIR:
//
//	dists/stable/main/binary-ARCH/Packages and Packages.gz, for amd64 and arm64
//	dists/stable/Release, which names the size and hashes of each of them
//	.htaccess, the redirects
//
// A package already in the index stays as it is. The same package given again
// changes nothing, and a package of the same name, version and architecture
// with other bytes is refused, because apt would reject the published file.
// When an index changes, Release is written with a new date, and InRelease and
// Release.gpg, its signatures, are removed. The release manager signs Release
// again: tools/release/apt-repository.sh does both.
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	suite     = "stable"
	component = "main"
	// The name make binaries gives a Debian package in the release.
	fileTemplate = "apache-skywalking-ai-sessionizer-{version}-bin-{package}-{arch}.deb"
)

var arches = []string{"amd64", "arm64"}

func main() {
	dir := flag.String("dir", "", "the apt directory: static/apt in apache/skywalking-website")
	current := flag.String("current", "https://www.apache.org/dyn/closer.lua/skywalking/ai-sessionizer/{version}/{file}?action=download",
		"where the newest version's packages are")
	archive := flag.String("archive", "https://archive.apache.org/dist/skywalking/ai-sessionizer/{version}/{file}",
		"where every other version's packages are")
	date := flag.String("date", "", "the date of Release, in RFC 3339; now when empty")
	flag.Parse()
	if err := run(*dir, *current, *archive, *date, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "aptindex:", err)
		os.Exit(1)
	}
}

// A field of a stanza, in the order it is written.
type field struct{ name, value string }

type stanza []field

func (s stanza) get(name string) string {
	for _, f := range s {
		if strings.EqualFold(f.name, name) {
			return f.value
		}
	}
	return ""
}

func (s stanza) key() string {
	return s.get("Package") + " " + s.get("Version") + " " + s.get("Architecture")
}

var releaseVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

func run(dir, current, archive, date string, debs []string) error {
	if dir == "" {
		return errors.New("-dir is needed")
	}
	for _, t := range []string{current, archive} {
		if !strings.Contains(t, "{version}") || !strings.Contains(t, "{file}") {
			return fmt.Errorf("%q must hold {version} and {file}", t)
		}
	}
	stamp := time.Now().UTC()
	if date != "" {
		var err error
		if stamp, err = time.Parse(time.RFC3339, date); err != nil {
			return fmt.Errorf("-date: %w", err)
		}
		stamp = stamp.UTC()
	}
	index := map[string][]stanza{}
	for _, arch := range arches {
		entries, err := readPackages(filepath.Join(dir, packagesPath(arch)))
		if err != nil {
			return err
		}
		index[arch] = entries
	}
	for _, deb := range debs {
		entry, err := describe(deb)
		if err != nil {
			return fmt.Errorf("%s: %w", deb, err)
		}
		arch := entry.get("Architecture")
		added := true
		for _, known := range index[arch] {
			if known.key() != entry.key() {
				continue
			}
			if known.get("SHA256") != entry.get("SHA256") || known.get("Size") != entry.get("Size") {
				return fmt.Errorf("%s: the index lists %s already, with other bytes. A published package never changes", deb, entry.key())
			}
			added = false
		}
		if added {
			index[arch] = append(index[arch], entry)
			fmt.Printf("aptindex: added %s\n", entry.key())
		} else {
			fmt.Printf("aptindex: %s is in the index already\n", entry.key())
		}
	}

	changed := false
	newest := ""
	for _, arch := range arches {
		entries := index[arch]
		sort.SliceStable(entries, func(i, j int) bool {
			if a, b := entries[i].get("Package"), entries[j].get("Package"); a != b {
				return a < b
			}
			return compareVersions(entries[i].get("Version"), entries[j].get("Version")) < 0
		})
		for _, e := range entries {
			if newest == "" || compareVersions(e.get("Version"), newest) > 0 {
				newest = e.get("Version")
			}
		}
		text := writeStanzas(entries)
		wrote, err := update(filepath.Join(dir, packagesPath(arch)), text)
		if err != nil {
			return err
		}
		changed = changed || wrote
		packed, err := gzipped(text)
		if err != nil {
			return err
		}
		// A Packages.gz from another gzip differs in bytes and not in what
		// it holds, so it is written again only when Packages changed or it
		// is missing.
		gzPath := filepath.Join(dir, packagesPath(arch)+".gz")
		if _, err := os.Stat(gzPath); wrote || errors.Is(err, os.ErrNotExist) {
			if err := write(gzPath, packed); err != nil {
				return err
			}
			changed = true
		}
	}

	releasePath := filepath.Join(dir, "dists", suite, "Release")
	if _, err := os.Stat(releasePath); changed || errors.Is(err, os.ErrNotExist) {
		release, err := releaseFile(dir, stamp)
		if err != nil {
			return err
		}
		if err := write(releasePath, release); err != nil {
			return err
		}
		for _, signature := range []string{"InRelease", "Release.gpg"} {
			if err := os.Remove(filepath.Join(dir, "dists", suite, signature)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		fmt.Println("aptindex: wrote dists/stable/Release, which needs to be signed again")
	} else {
		fmt.Println("aptindex: the index did not change")
	}

	wrote, err := update(filepath.Join(dir, ".htaccess"), redirects(index, newest, current, archive))
	if err != nil {
		return err
	}
	if wrote {
		fmt.Printf("aptindex: wrote .htaccess: %s from the download mirrors, every older version from the archive\n", newest)
	}
	return nil
}

func packagesPath(arch string) string {
	return filepath.Join("dists", suite, component, "binary-"+arch, "Packages")
}

// describe reads a package's control file and returns its index entry.
func describe(deb string) (stanza, error) {
	body, err := os.ReadFile(deb)
	if err != nil {
		return nil, err
	}
	control, err := controlOf(body)
	if err != nil {
		return nil, err
	}
	fields, err := parseStanzas(control)
	if err != nil || len(fields) != 1 {
		return nil, fmt.Errorf("the control file is not one stanza")
	}
	entry := fields[0]
	pkg, version, arch := entry.get("Package"), entry.get("Version"), entry.get("Architecture")
	if !releaseVersion.MatchString(version) {
		return nil, fmt.Errorf("version %q is not a release version: digits and dots", version)
	}
	known := false
	for _, a := range arches {
		known = known || a == arch
	}
	if !known {
		return nil, fmt.Errorf("architecture %q is not one of %s", arch, strings.Join(arches, " "))
	}
	if want := distName(pkg, version, arch); filepath.Base(deb) != want {
		return nil, fmt.Errorf("the release names this package %s", want)
	}
	s256 := sha256.Sum256(body)
	s512 := sha512.Sum512(body)
	var out stanza
	for _, f := range entry {
		if f.name != "Description" {
			out = append(out, f)
		}
	}
	out = append(out,
		field{"Filename", "pool/" + pkg + "_" + version + "_" + arch + ".deb"},
		field{"Size", strconv.Itoa(len(body))},
		field{"SHA256", hex.EncodeToString(s256[:])},
		field{"SHA512", hex.EncodeToString(s512[:])},
		field{"Description", entry.get("Description")},
	)
	return out, nil
}

// distName is the name of a package on the download sites.
func distName(pkg, version, arch string) string {
	return strings.NewReplacer("{version}", version, "{package}", pkg, "{arch}", arch).Replace(fileTemplate)
}

// controlOf takes the control file out of a .deb: an ar archive whose second
// member is control.tar.gz.
func controlOf(deb []byte) ([]byte, error) {
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(deb, []byte(magic)) {
		return nil, errors.New("not a Debian package: no ar header")
	}
	rest := deb[len(magic):]
	for len(rest) >= 60 {
		header := rest[:60]
		name := strings.TrimRight(strings.TrimSpace(string(header[:16])), "/")
		size, err := strconv.Atoi(strings.TrimSpace(string(header[48:58])))
		if err != nil || size < 0 || 60+size > len(rest) {
			return nil, errors.New("a damaged ar member header")
		}
		body := rest[60 : 60+size]
		if name == "control.tar.gz" {
			gz, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			tr := tar.NewReader(gz)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}
				if strings.TrimPrefix(h.Name, "./") == "control" {
					return io.ReadAll(tr)
				}
			}
			return nil, errors.New("control.tar.gz holds no control file")
		}
		rest = rest[60+size+size%2:]
	}
	return nil, errors.New("no control.tar.gz member")
}

func readPackages(path string) ([]stanza, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := parseStanzas(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}

func parseStanzas(body []byte) ([]stanza, error) {
	var out []stanza
	var cur stanza
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.TrimSpace(line) == "":
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
		case line[0] == ' ' || line[0] == '\t':
			if len(cur) == 0 {
				return nil, fmt.Errorf("a continuation line with no field: %q", line)
			}
			cur[len(cur)-1].value += "\n" + line
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				return nil, fmt.Errorf("a line with no field name: %q", line)
			}
			cur = append(cur, field{name, strings.TrimSpace(value)})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

func writeStanzas(entries []stanza) []byte {
	var b bytes.Buffer
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, f := range e {
			fmt.Fprintf(&b, "%s: %s\n", f.name, f.value)
		}
	}
	return b.Bytes()
}

func gzipped(body []byte) ([]byte, error) {
	var out bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if _, err := gz.Write(body); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func releaseFile(dir string, stamp time.Time) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("Origin: Apache SkyWalking\n")
	b.WriteString("Label: Apache SkyWalking AI Sessionizer\n")
	fmt.Fprintf(&b, "Suite: %s\nCodename: %s\n", suite, suite)
	fmt.Fprintf(&b, "Date: %s\n", stamp.Format("Mon, 02 Jan 2006 15:04:05 UTC"))
	fmt.Fprintf(&b, "Architectures: %s\n", strings.Join(arches, " "))
	fmt.Fprintf(&b, "Components: %s\n", component)
	b.WriteString("Description: Apache SkyWalking AI Sessionizer, the released Debian packages\n")
	type sums struct{ s256, s512 string }
	var names []string
	hashes := map[string]sums{}
	sizes := map[string]int{}
	for _, arch := range arches {
		for _, suffix := range []string{"", ".gz"} {
			name := component + "/binary-" + arch + "/Packages" + suffix
			body, err := os.ReadFile(filepath.Join(dir, "dists", suite, filepath.FromSlash(name)))
			if err != nil {
				return nil, err
			}
			a, c := sha256.Sum256(body), sha512.Sum512(body)
			names = append(names, name)
			hashes[name] = sums{hex.EncodeToString(a[:]), hex.EncodeToString(c[:])}
			sizes[name] = len(body)
		}
	}
	b.WriteString("SHA256:\n")
	for _, n := range names {
		fmt.Fprintf(&b, " %s %d %s\n", hashes[n].s256, sizes[n], n)
	}
	b.WriteString("SHA512:\n")
	for _, n := range names {
		fmt.Fprintf(&b, " %s %d %s\n", hashes[n].s512, sizes[n], n)
	}
	return b.Bytes(), nil
}

func redirects(index map[string][]stanza, newest, current, archive string) []byte {
	var b bytes.Buffer
	b.WriteString(`# Written by tools/release/apt-index in apache/skywalking-ai-sessionizer. Do not edit it
# by hand: run tools/release/apt-repository.sh there.
#
# apt reads the index under dists/ and asks for each package under pool/.
# These rules send it to the file on the Apache download sites. The newest
# version is on the download mirrors, and archive.apache.org keeps every
# version.
RewriteEngine On
`)
	for _, arch := range arches {
		for _, e := range index[arch] {
			pkg, version := e.get("Package"), e.get("Version")
			target := archive
			if version == newest {
				target = current
			}
			url := strings.NewReplacer("{version}", version, "{file}", distName(pkg, version, arch)).Replace(target)
			fmt.Fprintf(&b, "RewriteRule ^%s$ %s [R=302,L]\n", regexp.QuoteMeta(e.get("Filename")), url)
		}
	}
	return b.Bytes()
}

// compareVersions orders release versions, digits and dots, by number.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// update writes body to path when the file does not hold it already, and
// says whether it wrote.
func update(path string, body []byte) (bool, error) {
	old, err := os.ReadFile(path)
	if err == nil && bytes.Equal(old, body) {
		return false, nil
	}
	return true, write(path, body)
}

func write(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
