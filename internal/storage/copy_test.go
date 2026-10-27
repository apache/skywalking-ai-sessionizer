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

package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

var (
	digestA = strings.Repeat("a", 64)
	digestB = strings.Repeat("b", 64)
	buildID = strings.Repeat("c", 64)
)

func conversationDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, storage.RemoteDir, "prod", "c1")
	mkdirs(t, dir)
	return dir
}

func TestOuterCopyRoundTrip(t *testing.T) {
	model := controlModel(t)
	// The model sees only what is made through it, so the directories are too.
	dir := filepath.Join(model.Root, storage.RemoteDir, "prod", "c1")
	check(t, storage.MakeAncestorsDurable(model.Root, dir, 0o755, model))
	c := storage.OuterCopy{Remote: "prod", Conversation: "c1"}
	tag, err := c.Reserve(time.Date(2026, 10, 25, 10, 32, 0, 123456789, time.UTC), buildID, digestA)
	check(t, err)
	if tag != testTag {
		t.Fatalf("tag %s", tag)
	}
	check(t, storage.WriteOuterCopy(dir, c, tag, model))
	got, err := storage.ReadOuterCopy(dir, "prod", "c1")
	check(t, err)
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("read back %+v, want %+v", got, c)
	}
	// The history is written freshly and survives a power loss as written.
	recovered := powerLoss(t, model, faulttest.DropAll)
	again := filepath.Join(recovered.Root, storage.RemoteDir, "prod", "c1")
	got, err = storage.ReadOuterCopy(again, "prod", "c1")
	check(t, err)
	removed, err := storage.IsRemoved(again, "prod", "c1", tag)
	check(t, err)
	if got.GreatestTag != tag || !removed {
		t.Fatalf("after power loss: %+v, removed %v", got, removed)
	}
}

func removedTags(t *testing.T, dir string) []string {
	t.Helper()
	tags := []string{}
	check(t, storage.EachRemoved(dir, "prod", "c1", func(tag string) error {
		tags = append(tags, tag)
		return nil
	}))
	return tags
}

// A marker is never read as far as it goes, filled in or repaired.
func TestOuterCopyRefusals(t *testing.T) {
	dir := conversationDir(t, t.TempDir())
	later := "20261025T103201.000000000Z"
	for name, text := range map[string]string{
		"not an object":        `[]`,
		"empty":                ``,
		"no schema":            `{"remote":"prod","conversation":"c1","greatest_tag":"","removed":[]}`,
		"other schema":         `{"schema":"pull-copy/2","remote":"prod","conversation":"c1","greatest_tag":"","removed":[]}`,
		"unknown member":       `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"","removed":[],"note":1}`,
		"missing removed":      `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":""}`,
		"null removed":         `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"","removed":null}`,
		"duplicate member":     `{"schema":"pull-copy/1","remote":"prod","remote":"prod","conversation":"c1","greatest_tag":"","removed":[]}`,
		"other remote":         `{"schema":"pull-copy/1","remote":"stage","conversation":"c1","greatest_tag":"","removed":[]}`,
		"other conversation":   `{"schema":"pull-copy/1","remote":"prod","conversation":"c2","greatest_tag":"","removed":[]}`,
		"bad greatest tag":     `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"2026","removed":[]}`,
		"removed unreserved":   `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + testTag + `","removed":["` + later + `"]}`,
		"removed out of order": `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + later + `","removed":["` + later + `","` + testTag + `"]}`,
		"removed twice":        `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + testTag + `","removed":["` + testTag + `","` + testTag + `"]}`,
		"intent off history":   `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + later + `","removed":[],"publishing":{"build":"` + buildID + `","tag":"` + testTag + `","manifest_digest":"` + digestA + `"}}`,
		"intent bad build":     `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + testTag + `","removed":[],"publishing":{"build":"x","tag":"` + testTag + `","manifest_digest":"` + digestA + `"}}`,
		"intent outcome":       `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + testTag + `","removed":[],"publishing":{"build":"` + buildID + `","tag":"` + testTag + `","manifest_digest":"` + digestA + `","outcome":"failed"}}`,
		"trailing value":       `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"","removed":[]} {}`,
	} {
		check(t, os.WriteFile(filepath.Join(dir, storage.CopyMarker), []byte(text), 0o644))
		if _, err := storage.ReadOuterCopy(dir, "prod", "c1"); !errors.Is(err, storage.ErrMarker) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestRemovedAndReserve(t *testing.T) {
	dir := conversationDir(t, t.TempDir())
	c := storage.OuterCopy{Remote: "prod", Conversation: "c1"}
	first, err := c.Reserve(time.Date(2026, 10, 25, 10, 32, 1, 0, time.UTC), buildID, digestA)
	check(t, err)
	if _, err := c.Reserve(time.Now(), buildID, digestA); err == nil {
		t.Fatal("reserved a second tag while a publication is in progress")
	}
	c.Publishing = nil
	// A clock that went back still gives a later tag.
	second, err := c.Reserve(time.Date(2026, 10, 25, 10, 32, 0, 0, time.UTC), buildID, digestB)
	check(t, err)
	if second <= first {
		t.Fatalf("second tag %s is not after %s", second, first)
	}
	// Removal keeps tag order whatever order removals happen in, and a tag
	// recorded twice stays once.
	for _, tag := range []string{"", second, first, second} {
		check(t, storage.WriteOuterCopy(dir, c, tag, nil))
	}
	if got := removedTags(t, dir); !reflect.DeepEqual(got, []string{first, second}) {
		t.Fatalf("removed %v", got)
	}
	if err := storage.WriteOuterCopy(dir, c, "20991231T235959.000000000Z", nil); err == nil {
		t.Fatal("recorded a tag that was never reserved")
	}
	// The history never goes back past a tag it has removed.
	behind := c
	behind.GreatestTag, behind.Publishing = first, nil
	if err := storage.WriteOuterCopy(dir, behind, "", nil); err == nil {
		t.Fatal("wrote a history behind its own removal list")
	}
	if got := removedTags(t, dir); !reflect.DeepEqual(got, []string{first, second}) {
		t.Fatalf("a refused write changed the list: %v", got)
	}
}

// A long list is streamed through every rewrite: a tag goes into its place,
// first, between or last, and nothing else changes.
func TestALongRemovalListStreams(t *testing.T) {
	dir := conversationDir(t, t.TempDir())
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var tags []string
	for i := 0; i < 1000; i++ {
		tags = append(tags, storage.Stamp(start.Add(time.Duration(i)*time.Minute)))
	}
	c := storage.OuterCopy{Remote: "prod", Conversation: "c1", GreatestTag: tags[len(tags)-1]}
	// Every other tag, in increasing order, then the rest in an order that
	// puts each new one first, between, or last.
	for i := 1; i < len(tags); i += 2 {
		check(t, storage.WriteOuterCopy(dir, c, tags[i], nil))
	}
	for _, i := range []int{0, 500, len(tags) - 2, 200, 800} {
		check(t, storage.WriteOuterCopy(dir, c, tags[i], nil))
	}
	got := removedTags(t, dir)
	var want []string
	for i := range tags {
		if i%2 == 1 || i == 0 || i == 500 || i == len(tags)-2 || i == 200 || i == 800 {
			want = append(want, tags[i])
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%d tags read back, want %d", len(got), len(want))
	}
	for _, probe := range []struct {
		tag  string
		want bool
	}{{tags[500], true}, {tags[2], false}, {tags[3], true}} {
		removed, err := storage.IsRemoved(dir, "prod", "c1", probe.tag)
		if err != nil || removed != probe.want {
			t.Fatalf("%s: %v, %v", probe.tag, removed, err)
		}
	}
}

func snapshotCopy() storage.SnapshotCopy {
	return storage.SnapshotCopy{
		Remote: "prod", Conversation: "c1", Session: "s1",
		Round: 41, Digest: digestA, ManifestDigest: digestB,
		PublishedAt: time.Date(2026, 10, 25, 10, 32, 0, 123456789, time.UTC),
	}
}

func TestSnapshotCopy(t *testing.T) {
	model := controlModel(t)
	dir := filepath.Join(model.Root, "build")
	check(t, model.Mkdir(dir, 0o755))
	want := snapshotCopy()
	check(t, storage.WriteSnapshotCopy(dir, testTag, want, model))
	got, err := storage.ReadSnapshotCopy(dir, "prod", "c1", testTag)
	check(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %+v", got)
	}
	if _, err := storage.ReadSnapshotCopy(dir, "prod", "c1", "20261025T103200.123456780Z"); !errors.Is(err, storage.ErrMarker) {
		t.Fatalf("another tag's time: %v", err)
	}
	for name, change := range map[string]func(*storage.SnapshotCopy){
		"round zero":             func(c *storage.SnapshotCopy) { c.Round = 0 },
		"short digest":           func(c *storage.SnapshotCopy) { c.Digest = "abc" },
		"capital digest":         func(c *storage.SnapshotCopy) { c.ManifestDigest = strings.Repeat("A", 64) },
		"control session":        func(c *storage.SnapshotCopy) { c.Session = "_root" },
		"unportable session":     func(c *storage.SnapshotCopy) { c.Session = "a:b" },
		"other time":             func(c *storage.SnapshotCopy) { c.PublishedAt = c.PublishedAt.Add(time.Nanosecond) },
		"protected conversation": func(c *storage.SnapshotCopy) { c.Conversation = "_admit" },
	} {
		c := snapshotCopy()
		change(&c)
		if err := storage.WriteSnapshotCopy(dir, testTag, c, nil); err == nil {
			t.Fatalf("%s: written", name)
		}
	}
	for name, text := range map[string]string{
		"offset time": strings.Replace(markerText(t, dir), "10:32:00.123456789Z", "18:32:00.123456789+08:00", 1),
		"unknown":     strings.Replace(markerText(t, dir), `"round":41`, `"round":41,"extra":true`, 1),
		"negative":    strings.Replace(markerText(t, dir), `"round":41`, `"round":-41`, 1),
		"fraction":    strings.Replace(markerText(t, dir), `"round":41`, `"round":41.0`, 1),
	} {
		other := filepath.Join(t.TempDir(), "s")
		mkdirs(t, other)
		check(t, os.WriteFile(filepath.Join(other, storage.CopyMarker), []byte(text), 0o644))
		if _, err := storage.ReadSnapshotCopy(other, "prod", "c1", testTag); !errors.Is(err, storage.ErrMarker) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func markerText(t *testing.T, dir string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(dir, storage.CopyMarker))
	check(t, err)
	if !strings.Contains(string(text), `"round":41`) {
		t.Fatalf("marker text %s", text)
	}
	return string(text)
}

// A marker decides ownership, so one reached through a link is refused: a
// link to a matching marker elsewhere would lend a tree an owner it does not
// have. A collision check then leaves the tree to a person, and bootstrap
// refuses the conversation.
func TestALinkedMarkerIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, storage.RemoteDir, "prod", "c1")
	elsewhere := filepath.Join(root, "elsewhere")
	tree := filepath.Join(dir, testTag)
	mkdirs(t, tree, elsewhere)
	check(t, storage.WriteSnapshotCopy(elsewhere, testTag, snapshotCopy(), nil))
	symlinkOrSkip(t, filepath.Join(elsewhere, storage.CopyMarker), filepath.Join(tree, storage.CopyMarker))
	if _, err := storage.ReadSnapshotCopy(tree, "prod", "c1", testTag); !errors.Is(err, storage.ErrProtected) {
		t.Fatalf("a linked snapshot marker: %v", err)
	}
	if got, _ := storage.CheckCollision(dir, "prod", "c1", testTag, digestB); got != storage.TagUnowned {
		t.Fatalf("a tree with a linked marker is %v", got)
	}
	other := filepath.Join(root, storage.RemoteDir, "prod", "c2")
	mkdirs(t, other)
	check(t, storage.WriteOuterCopy(filepath.Join(root, storage.RemoteDir, "prod", "c2"), storage.OuterCopy{Remote: "prod", Conversation: "c2"}, "", nil))
	linked := filepath.Join(root, storage.RemoteDir, "prod", "c3")
	mkdirs(t, linked)
	symlinkOrSkip(t, filepath.Join(other, storage.CopyMarker), filepath.Join(linked, storage.CopyMarker))
	if _, _, err := storage.BootstrapConversation(context.Background(), root, "prod", "c3", nil); err == nil {
		t.Fatal("bootstrap admitted a conversation through a linked marker")
	}
}

// A publication time keeps all nine fractional digits in UTC, whatever its
// trailing zeros or the zone it was given in, and reads back as the same
// instant. A shortened fraction or an offset is refused.
func TestPublishedTimeKeepsNineDigits(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 10, 25, 10, 32, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 10, 32, 0, 123456780, time.UTC),
		time.Date(2026, 10, 25, 10, 32, 0, 100000000, time.UTC),
		time.Date(2026, 10, 25, 18, 32, 0, 123456789, time.FixedZone("east", 8*60*60)),
	} {
		dir := t.TempDir()
		c := snapshotCopy()
		c.PublishedAt = at
		tag := storage.Stamp(at)
		check(t, storage.WriteSnapshotCopy(dir, tag, c, nil))
		text, err := os.ReadFile(filepath.Join(dir, storage.CopyMarker))
		check(t, err)
		want := `"published_at":"` + at.UTC().Format("2006-01-02T15:04:05.000000000Z") + `"`
		if !strings.Contains(string(text), want) {
			t.Fatalf("written as %s, want %s", text, want)
		}
		got, err := storage.ReadSnapshotCopy(dir, "prod", "c1", tag)
		check(t, err)
		if !got.PublishedAt.Equal(at) || got.PublishedAt.Location() != time.UTC {
			t.Fatalf("read back %v, want %v", got.PublishedAt, at)
		}
	}
	dir := t.TempDir()
	check(t, storage.WriteSnapshotCopy(dir, testTag, snapshotCopy(), nil))
	text := markerText(t, dir)
	for name, spelling := range map[string]string{
		"short fraction": "2026-10-25T10:32:00.12345678Z\"",
		"no fraction":    "2026-10-25T10:32:00Z\"",
		"offset":         "2026-10-25T10:32:00.123456789+00:00\"",
	} {
		other := filepath.Join(t.TempDir(), "s")
		mkdirs(t, other)
		bad := strings.Replace(text, "2026-10-25T10:32:00.123456789Z\"", spelling, 1)
		check(t, os.WriteFile(filepath.Join(other, storage.CopyMarker), []byte(bad), 0o644))
		if _, err := storage.ReadSnapshotCopy(other, "prod", "c1", testTag); !errors.Is(err, storage.ErrMarker) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A sweep deletes the trees of removed tags, so it never sees a tag from a
// marker that fails its checks, wherever in the marker the failure is.
func TestEachRemovedChecksTheWholeMarkerFirst(t *testing.T) {
	dir := conversationDir(t, t.TempDir())
	for name, text := range map[string]string{
		"members missing":    `{"schema":"pull-copy/1","removed":["` + testTag + `"]}`,
		"other conversation": `{"schema":"pull-copy/1","removed":["` + testTag + `"],"remote":"prod","conversation":"c2","greatest_tag":"` + testTag + `"}`,
		"unreserved":         `{"schema":"pull-copy/1","removed":["` + testTag + `"],"remote":"prod","conversation":"c1","greatest_tag":""}`,
		"cut short":          `{"schema":"pull-copy/1","remote":"prod","conversation":"c1","greatest_tag":"` + testTag + `","removed":["` + testTag + `"`,
	} {
		check(t, os.WriteFile(filepath.Join(dir, storage.CopyMarker), []byte(text), 0o644))
		visited := 0
		err := storage.EachRemoved(dir, "prod", "c1", func(string) error { visited++; return nil })
		if err == nil || visited != 0 {
			t.Fatalf("%s: visited %d tags, %v", name, visited, err)
		}
	}
}
