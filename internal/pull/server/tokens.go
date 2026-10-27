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

// Package server serves pulls from a production storage root: the tokens and
// TLS that protect every route on its address, the bounded writes of every
// answer, the frozen snapshots a pull copies, and the jobs that freeze and
// archive them.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TokenPrefix starts every token asz makes, so one is known for what it is
// wherever it turns up.
const TokenPrefix = "asz_"

// maxTokensFile bounds a tokens file. A line takes under 330 bytes, so this
// holds thousands of tokens.
const maxTokensFile = 4 << 20

// reloadEvery is how often a server looks at its tokens file again.
const reloadEvery = 5 * time.Second

// Tokens is the set a tokens file names: one name and one SHA-256 per token.
type Tokens struct {
	entries []tokenEntry
}

type tokenEntry struct {
	name string
	hash [sha256.Size]byte
}

// Len is how many tokens the set holds.
func (t *Tokens) Len() int { return len(t.entries) }

// ParseTokens reads a tokens file: one line per token, a name, one space and
// the token's SHA-256 in lower-case hexadecimal. Blank lines and lines that
// start with # are passed over. Any other line, or a name or a hash given
// twice, refuses the whole file: a server never runs on part of one.
func ParseTokens(data []byte) (*Tokens, error) {
	t := &Tokens{}
	names := map[string]int{}
	hashes := map[[sha256.Size]byte]int{}
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hash, ok := strings.Cut(line, " ")
		if !ok || strings.Contains(hash, " ") {
			return nil, fmt.Errorf("tokens: line %d is not a name, a space and a hash", n)
		}
		if err := storage.CheckRemoteName(name); err != nil {
			return nil, fmt.Errorf("tokens: line %d: %w", n, err)
		}
		if !storage.Digest64(hash) {
			return nil, fmt.Errorf("tokens: line %d: the hash is not 64 lower-case hexadecimal characters", n)
		}
		var sum [sha256.Size]byte
		_, _ = hex.Decode(sum[:], []byte(hash))
		// Names are ASCII, so their case aliases fold by ASCII alone.
		if at, seen := names[strings.ToLower(name)]; seen {
			return nil, fmt.Errorf("tokens: line %d names %q again, as line %d does", n, name, at)
		}
		if at, seen := hashes[sum]; seen {
			return nil, fmt.Errorf("tokens: line %d gives line %d's token again", n, at)
		}
		names[strings.ToLower(name)], hashes[sum] = n, n
		t.entries = append(t.entries, tokenEntry{name: name, hash: sum})
	}
	return t, nil
}

// Lookup finds the token whose text was sent. The text is hashed exactly as
// it came, and the hash is compared with every line in constant time, so how
// long a refusal takes says nothing of which tokens exist.
func (t *Tokens) Lookup(sent string) (name, hash string, ok bool) {
	sum := sha256.Sum256([]byte(sent))
	found := -1
	for i := range t.entries {
		if subtle.ConstantTimeCompare(sum[:], t.entries[i].hash[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return "", "", false
	}
	return t.entries[found].name, hex.EncodeToString(sum[:]), true
}

// has says whether the set names a token by name, case aside, or by hash.
func (t *Tokens) has(name string, hash [sha256.Size]byte) (string, bool) {
	for _, e := range t.entries {
		if strings.EqualFold(e.name, name) {
			return fmt.Sprintf("the name %q is taken by %q", name, e.name), true
		}
		if e.hash == hash {
			return fmt.Sprintf("the token is %q's already", e.name), true
		}
	}
	return "", false
}

// NewToken makes a token: 32 random bytes, written as asz_ and 43
// characters of unpadded base64url. It returns the token and its line's hash.
func NewToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// TokenLine is a tokens file's line for a name and a token's hash.
func TokenLine(name, hash string) string { return name + " " + hash }

// tokensLock is the lock beside a tokens file that its writers hold.
func tokensLock(file string) string {
	return filepath.Join(filepath.Dir(file), "."+filepath.Base(file)+".lock")
}

// readTokensFile reads a tokens file a person named, checked as a supplied
// path and read through its checked parent, with the sharing a control read
// takes on Windows. A file a link leads to, as a mounted secret is, is read
// where the link leads.
func readTokensFile(file string) ([]byte, error) {
	f, err := storage.OpenSuppliedRead(file, "")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTokensFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxTokensFile {
		return nil, fmt.Errorf("tokens: %s is larger than %d MiB", file, maxTokensFile>>20)
	}
	return b, nil
}

// AddToken adds a token's line to a tokens file, under a lock beside it that
// every writer holds through its read, its checks and its replacement. A
// name already there, in any case, or a token already there, is refused,
// and so is a file that does not read as a tokens file. A file not there yet
// is made. The whole file is written again, never appended to.
func AddToken(ctx context.Context, file, name, hash string, files storage.FileSystem) error {
	if err := storage.CheckRemoteName(name); err != nil {
		return err
	}
	var sum [sha256.Size]byte
	if !storage.Digest64(hash) {
		return errors.New("tokens: the hash is not 64 lower-case hexadecimal characters")
	}
	_, _ = hex.Decode(sum[:], []byte(hash))
	// The file is written where the check found it, by the path its links
	// resolve to, and the lock is the one beside that file. Under the lock
	// the path is checked again: an ancestor swapped since must not take
	// the write somewhere the check never saw.
	resolved, err := storage.CheckSuppliedPath(file, "")
	if err != nil {
		return err
	}
	lock := tokensLock(resolved)
	if _, err := storage.CheckSuppliedPath(lock, ""); err != nil {
		return err
	}
	// A replacement Windows finds busy runs the whole of it again, the
	// lock let go and taken again and the file read again.
	return storage.RetryControl(ctx, func(ctx context.Context) error {
		l, err := storage.LockFileWait(ctx, lock)
		if err != nil {
			return err
		}
		defer func() { _ = l.Unlock() }()
		if again, err := storage.CheckSuppliedPath(file, ""); err != nil || again != resolved {
			return errors.Join(fmt.Errorf("tokens: %s no longer leads to %s", file, resolved), err)
		}
		data, err := readTokensFile(resolved)
		switch {
		case errors.Is(err, os.ErrNotExist):
			data = nil
		case err != nil:
			return err
		}
		set, err := ParseTokens(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if why, taken := set.has(name, sum); taken {
			return fmt.Errorf("tokens: %s: %s", file, why)
		}
		next := data
		if len(next) > 0 && !bytes.HasSuffix(next, []byte("\n")) {
			next = append(next, '\n')
		}
		next = append(next, TokenLine(name, hash)+"\n"...)
		// Written through the parent the check resolved, bound once: an
		// ancestor swapped for a link after this check is refused there.
		return storage.ReplaceSuppliedDurable(resolved, 0o600, func(w io.Writer) error {
			_, err := w.Write(next)
			return err
		}, files)
	})
}

// Keeper holds the tokens a serving process checks every request with. It
// looks at the file again at most every five seconds, and takes its content
// when it changed: a mounted secret swapped by a link is followed. A
// replacement that does not read as a tokens file is logged, and the last
// good set kept. A file that cannot be read refuses every request until it
// can be.
type Keeper struct {
	file string
	now  func() time.Time
	log  func(format string, args ...any)

	mu        sync.Mutex
	set       *Tokens
	content   [sha256.Size]byte
	checked   time.Time
	unread    error
	malformed [sha256.Size]byte
}

// LoadKeeper reads a tokens file to serve with. A file that cannot be read,
// or does not read as a tokens file, stops the server from starting.
func LoadKeeper(file string, now func() time.Time, log func(format string, args ...any)) (*Keeper, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	data, err := readTokensFile(file)
	if err != nil {
		return nil, err
	}
	set, err := ParseTokens(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	k := &Keeper{file: file, now: now, log: log, set: set, content: sha256.Sum256(data), checked: now()}
	if set.Len() == 0 {
		log("tokens: %s names no token: every request is refused", file)
	}
	return k, nil
}

// Lookup finds the token sent, or says why none is taken.
func (k *Keeper) Lookup(sent string) (name, hash string, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if now := k.now(); now.Sub(k.checked) >= reloadEvery || now.Before(k.checked) {
		k.checked = now
		k.reload()
	}
	if k.unread != nil {
		return "", "", fmt.Errorf("tokens: %s cannot be read", k.file)
	}
	name, hash, ok := k.set.Lookup(sent)
	if !ok {
		return "", "", errUnknownToken
	}
	return name, hash, nil
}

var errUnknownToken = errors.New("tokens: no such token")

// reload reads the file again and takes its content when it changed.
// Holds says whether the tokens file still names the token of this hash,
// looking at the file again when it is due: a request that runs on checks
// it was not revoked meanwhile.
func (k *Keeper) Holds(hash string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if now := k.now(); now.Sub(k.checked) >= reloadEvery || now.Before(k.checked) {
		k.checked = now
		k.reload()
	}
	if k.unread != nil {
		return false
	}
	var sum [sha256.Size]byte
	if _, err := hex.Decode(sum[:], []byte(hash)); err != nil {
		return false
	}
	for _, e := range k.set.entries {
		if subtle.ConstantTimeCompare(sum[:], e.hash[:]) == 1 {
			return true
		}
	}
	return false
}

func (k *Keeper) reload() {
	data, err := readTokensFile(k.file)
	if err != nil {
		if k.unread == nil {
			k.log("tokens: %s cannot be read, and every request is refused until it can: %v", k.file, err)
		}
		k.unread = err
		return
	}
	if k.unread != nil {
		k.log("tokens: %s can be read again", k.file)
		k.unread = nil
	}
	sum := sha256.Sum256(data)
	if sum == k.content {
		return
	}
	set, err := ParseTokens(data)
	if err != nil {
		if sum != k.malformed {
			k.log("tokens: %s changed and does not read as a tokens file, so the last good one is kept: %v", k.file, err)
			k.malformed = sum
		}
		return
	}
	k.set, k.content = set, sum
	if set.Len() == 0 {
		k.log("tokens: %s now names no token: every request is refused", k.file)
	}
}
