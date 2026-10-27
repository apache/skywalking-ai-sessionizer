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

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// A token is asz_ and 43 characters of unpadded base64url over 32 random
// bytes, and its line's hash is the SHA-256 of that text.
func TestATokenIsThirtyTwoRandomBytes(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		token, hash, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		body, ok := strings.CutPrefix(token, TokenPrefix)
		if !ok || len(body) != 43 {
			t.Fatalf("token %q", token)
		}
		if b, err := base64.RawURLEncoding.DecodeString(body); err != nil || len(b) != 32 {
			t.Fatalf("token %q: %v", token, err)
		}
		if hash != hashOf(token) || seen[token] {
			t.Fatalf("token %q hash %q, seen %v", token, hash, seen[token])
		}
		seen[token] = true
	}
}

// A tokens file is read whole or not at all: every line is a name, one space
// and a hash, apart from blank lines and comments, and no name, case aside,
// or hash comes twice.
func TestATokensFileIsReadWholeOrNotAtAll(t *testing.T) {
	a, b := hashOf("a"), hashOf("b")
	good := "# the team\n\nprod " + a + "\r\nci.bot-2_x " + b + "\n"
	set, err := ParseTokens([]byte(good))
	if err != nil || set.Len() != 2 {
		t.Fatalf("%v %v", set, err)
	}
	for name, body := range map[string]string{
		"no space":           "prod" + a,
		"two spaces":         "prod  " + a,
		"a space after":      "prod " + a + " ",
		"an upper-case hash": "prod " + strings.ToUpper(a),
		"a short hash":       "prod " + a[:63],
		"a bad name":         "pro/d " + a,
		"an empty name":      " " + a,
		"a device name":      "nul " + a,
		"a name twice":       "prod " + a + "\nprod " + b,
		"a case alias":       "prod " + a + "\nPROD " + b,
		"a hash twice":       "prod " + a + "\nci " + a,
		"an indented line":   " # not a comment",
	} {
		if _, err := ParseTokens([]byte(body)); err == nil {
			t.Errorf("%s was read", name)
		}
	}
	if set, err := ParseTokens(nil); err != nil || set.Len() != 0 {
		t.Fatalf("an empty file: %v %v", set, err)
	}
}

// A token is found by the hash of its text exactly as sent: a changed byte,
// added space or other case is another token.
func TestATokenIsFoundByItsExactText(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseTokens([]byte("prod " + hash + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if name, got, ok := set.Lookup(token); !ok || name != "prod" || got != hash {
		t.Fatalf("%q %q %v", name, got, ok)
	}
	changed := []byte(token)
	changed[len(changed)-1] ^= 1
	for _, sent := range []string{string(changed), token + " ", " " + token, strings.ToUpper(token), ""} {
		if _, _, ok := set.Lookup(sent); ok {
			t.Errorf("%q was taken", sent)
		}
	}
}

func tokensFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A line is added under the lock beside the file: a new file is made, a
// name or token already there is refused, and a file that does not read as
// a tokens file is left as it is.
func TestAddingATokenRefusesWhatIsThere(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tokens")
	a, b := hashOf("a"), hashOf("b")
	if err := AddToken(ctx, path, "prod", a, nil); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != "prod "+a+"\n" {
		t.Fatalf("the file: %q", body)
	}
	for name, add := range map[string][2]string{
		"the same name": {"prod", b},
		"a case alias":  {"Prod", b},
		"the same hash": {"ci", a},
		"a bad name":    {"c i", b},
		"a bad hash":    {"ci", "x"},
	} {
		if err := AddToken(ctx, path, add[0], add[1], nil); err == nil {
			t.Errorf("%s was added", name)
		}
	}
	if err := AddToken(ctx, path, "ci", b, nil); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(path); string(body) != "prod "+a+"\nci "+b+"\n" {
		t.Fatalf("the file: %q", body)
	}
	broken := tokensFile(t, "not a line\n")
	if err := AddToken(ctx, broken, "prod", a, nil); err == nil {
		t.Fatal("a line was added to a file that is not a tokens file")
	}
	if body, _ := os.ReadFile(broken); string(body) != "not a line\n" {
		t.Fatalf("the broken file became %q", body)
	}
	// A comment and a missing last line end are kept.
	kept := tokensFile(t, "# keep\nprod "+a)
	if err := AddToken(ctx, kept, "ci", b, nil); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(kept); string(body) != "# keep\nprod "+a+"\nci "+b+"\n" {
		t.Fatalf("the file: %q", body)
	}
}

// Creators that race each hold the lock through their read and replacement,
// so none loses another's line, and of equal names, case aliases or equal
// tokens exactly one lands.
func TestCreatorsThatRaceLoseNoLine(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tokens")
	var wg sync.WaitGroup
	const n = 12
	errs := make([]error, 3*n)
	for i := range n {
		wg.Add(3)
		go func() {
			defer wg.Done()
			errs[i] = AddToken(ctx, path, fmt.Sprintf("bot-%d", i), hashOf(fmt.Sprint(i)), nil)
		}()
		go func() {
			defer wg.Done()
			errs[n+i] = AddToken(ctx, path, []string{"same", "SAME", "Same"}[i%3], hashOf("x"+fmt.Sprint(i)), nil)
		}()
		go func() {
			defer wg.Done()
			errs[2*n+i] = AddToken(ctx, path, fmt.Sprintf("dup-%d", i), hashOf("one"), nil)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("a distinct creator failed: %v", errs[i])
		}
	}
	landed := func(errs []error) (ok int) {
		for _, err := range errs {
			if err == nil {
				ok++
			}
		}
		return ok
	}
	if landed(errs[n:2*n]) != 1 || landed(errs[2*n:]) != 1 {
		t.Fatalf("equal names landed %d times, equal tokens %d times", landed(errs[n:2*n]), landed(errs[2*n:]))
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseTokens(body)
	if err != nil || set.Len() != n+2 {
		t.Fatalf("%d tokens: %v\n%s", set.Len(), err, body)
	}
}

// failingWrites fails every create, as a full or read-only disk does.
type failingWrites struct{ storage.FileSystem }

func (failingWrites) Create(string, os.FileMode) (storage.WriteFile, error) {
	return nil, errors.New("no space left on device")
}

func (failingWrites) CreateIn(*storage.Dir, string, os.FileMode) (storage.WriteFile, error) {
	return nil, errors.New("no space left on device")
}

// A replacement that fails leaves the file as it was.
func TestAFailedAdditionLeavesTheFile(t *testing.T) {
	a := hashOf("a")
	path := tokensFile(t, "prod "+a+"\n")
	if err := AddToken(context.Background(), path, "ci", hashOf("b"), failingWrites{storage.DiskFileSystem{}}); err == nil {
		t.Fatal("a failed write was taken for an addition")
	}
	if body, _ := os.ReadFile(path); string(body) != "prod "+a+"\n" {
		t.Fatalf("the file became %q", body)
	}
}

// A tokens file in a tree pull owns is refused before anything is opened or
// made beside it, a held lock included.
func TestATokensFileInAPulledTreeIsRefused(t *testing.T) {
	root := t.TempDir()
	conversation := filepath.Join(root, storage.RemoteDir, "prod", "c1")
	held := filepath.Join(conversation, storage.PullDir)
	if err := os.MkdirAll(held, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := storage.LockOwnedCreate(held, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	targets := []string{filepath.Join(held, ".lock"), filepath.Join(root, storage.PullDir, "tokens")}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "tokens")
		if err := os.Symlink(filepath.Join(held, ".lock"), link); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, link)
	}
	for _, target := range targets {
		before, _ := os.ReadDir(filepath.Dir(target))
		if err := AddToken(context.Background(), target, "prod", hashOf("a"), nil); !errors.Is(err, storage.ErrProtected) {
			t.Errorf("%s: %v", target, err)
		}
		after, _ := os.ReadDir(filepath.Dir(target))
		if len(after) != len(before) {
			t.Errorf("%s: something was made beside it", target)
		}
	}
	if info, err := os.Stat(filepath.Join(held, ".lock")); err != nil || info.Size() != 0 {
		t.Fatalf("the held lock: %v %v", info, err)
	}
	if _, err := storage.LockOwned(held); !errors.Is(err, storage.ErrBusy) {
		t.Fatalf("the lock is no longer held: %v", err)
	}
}

// fakeClock is a clock a test moves.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// A server reads its tokens file again at most every five seconds: a
// revoked token stops working then, a replacement that does not read keeps
// the last good set, an unreadable file refuses everything until it reads,
// and an empty one refuses everything.
func TestTheKeeperFollowsItsFile(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	var logged []string
	log := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	path := tokensFile(t, "prod "+hashOf("a")+"\nci "+hashOf("b")+"\n")
	k, err := LoadKeeper(path, clock.now, log)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(token string) bool { _, _, err := k.Lookup(token); return err == nil }
	if !lookup("a") || !lookup("b") || lookup("c") {
		t.Fatal("the first set")
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("prod " + hashOf("a") + "\n")
	clock.add(4 * time.Second)
	if !lookup("b") {
		t.Fatal("the file was read again before five seconds")
	}
	clock.add(time.Second)
	if lookup("b") || !lookup("a") {
		t.Fatal("a revoked token still works")
	}
	write("broken\n")
	clock.add(5 * time.Second)
	if !lookup("a") || len(logged) == 0 || !strings.Contains(logged[len(logged)-1], "last good") {
		t.Fatalf("a malformed replacement: %v", logged)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clock.add(5 * time.Second)
	if lookup("a") {
		t.Fatal("a token worked while the file could not be read")
	}
	write("prod " + hashOf("a") + "\n")
	clock.add(5 * time.Second)
	if !lookup("a") {
		t.Fatal("the file was not read once it could be")
	}
	write("# nobody\n")
	clock.add(5 * time.Second)
	if lookup("a") {
		t.Fatal("an empty set took a token")
	}
	if _, err := LoadKeeper(tokensFile(t, "broken\n"), clock.now, log); err == nil {
		t.Fatal("a server started on a malformed file")
	}
	if _, err := LoadKeeper(filepath.Join(t.TempDir(), "none"), clock.now, log); err == nil {
		t.Fatal("a server started without its file")
	}
}

// A mounted secret is a link swapped to new content: the keeper follows it.
func TestTheKeeperFollowsASwappedLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a mounted secret's links are a Linux arrangement")
	}
	dir := t.TempDir()
	for name, token := range map[string]string{"v1": "a", "v2": "b"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "tokens"), []byte("prod "+hashOf(token)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "tokens"), filepath.Join(dir, "tokens")); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Unix(0, 0)}
	k, err := LoadKeeper(filepath.Join(dir, "tokens"), clock.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.Lookup("a"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v2", filepath.Join(dir, "..data.new")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data.new"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	clock.add(reloadEvery)
	if _, _, err := k.Lookup("a"); err == nil {
		t.Fatal("the old secret still works")
	}
	if _, _, err := k.Lookup("b"); err != nil {
		t.Fatal(err)
	}
}

// A tokens file is written where its path led when it was checked: a link
// on the way that leads elsewhere by the time the writer holds the lock
// stops the write, and nothing is written where it now leads.
func TestATokensPathThatChangesStopsTheWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	base := t.TempDir()
	first, second := filepath.Join(base, "first"), filepath.Join(base, "second")
	for _, d := range []string{first, second} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	// The writer waits for the lock beside the file the path leads to.
	held, err := storage.LockFileWait(context.Background(), tokensLock(filepath.Join(first, "tokens")))
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- AddToken(context.Background(), filepath.Join(alias, "tokens"), "prod", hash, nil) }()
	time.Sleep(200 * time.Millisecond)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	_ = held.Unlock()
	if err := <-done; err == nil {
		t.Fatal("the write went on after its path changed")
	}
	for _, d := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(d, "tokens")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a tokens file in %s: %v", d, err)
		}
	}
}

// swapsAtCreate swaps an ancestor of a tokens file for a link at the first
// file the write creates, after every check the write makes by path.
type swapsAtCreate struct {
	storage.DiskFileSystem
	once sync.Once
	swap func()
}

func (s *swapsAtCreate) Create(path string, mode os.FileMode) (storage.WriteFile, error) {
	s.once.Do(s.swap)
	return s.DiskFileSystem.Create(path, mode)
}

func (s *swapsAtCreate) CreateIn(dir *storage.Dir, name string, mode os.FileMode) (storage.WriteFile, error) {
	s.once.Do(s.swap)
	return s.DiskFileSystem.CreateIn(dir, name, mode)
}

// A token is written in the directory its path was checked to lead to: an
// ancestor swapped for a link while the write runs never takes it where the
// link leads.
func TestATokenWriteStaysInTheCheckedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need privileges on Windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"safe/sub", "elsewhere/sub"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	files := &swapsAtCreate{swap: func() {
		if err := os.Rename(filepath.Join(base, "safe"), filepath.Join(base, "moved")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(base, "safe")); err != nil {
			t.Error(err)
		}
	}}
	_ = AddToken(context.Background(), filepath.Join(base, "safe", "sub", "tokens"), "prod", hash, files)
	if _, err := os.Stat(filepath.Join(base, "elsewhere", "sub", "tokens")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a tokens file where the link leads: %v", err)
	}
}

// busyOnce refuses the first replacement as Windows does a busy one.
type busyOnce struct {
	storage.DiskFileSystem
	once sync.Once
}

func (b *busyOnce) ReplaceIn(dir *storage.Dir, from, to string) error {
	var err error
	b.once.Do(func() { err = &storage.ControlBusyError{Err: errors.New("the replacement is busy")} })
	if err != nil {
		return err
	}
	return b.DiskFileSystem.ReplaceIn(dir, from, to)
}

// A token whose file's replacement is busy once is added on the next try
// of the whole write.
func TestATokenIsAddedPastABusyReplacement(t *testing.T) {
	path := tokensFile(t, "")
	_, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := AddToken(context.Background(), path, "prod", hash, &busyOnce{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "prod "+hash) {
		t.Fatalf("%q %v", b, err)
	}
}
