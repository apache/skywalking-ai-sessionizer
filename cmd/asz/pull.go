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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

const pullUsage = `Usage:
  asz pull [-config FILE] REMOTE/CONVERSATION ...
  asz pull [-config FILE] -list [-remote NAME ...] [-since T] [-until T] [-runtime R] [-match TEXT]
           [-min-talks N] [-limit N] [-after CURSOR]

Copies conversations from the asz servers configured under remotes, each
into a snapshot of its own under _remote/ in the storage root, to judge
locally. -list lists what the remotes hold, newest activity first, with the
round of the newest snapshot here. A pull always takes a conversation's head.
`

// listFlags are the flags only -list takes.
var listFlags = []string{"remote", "since", "until", "runtime", "match", "min-talks", "limit", "after"}

// cmdPull pulls conversations, or lists what the remotes hold.
func cmdPull(args []string) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, pullUsage) }
	cfgPath := fs.String("config", "", "configuration file")
	list := fs.Bool("list", false, "list what the remotes hold")
	var remotes repeated
	fs.Var(&remotes, "remote", "a remote to list; repeat for several")
	since := fs.String("since", "", "last activity at or after: a duration back from now, or an RFC 3339 time")
	until := fs.String("until", "", "last activity before")
	runtime := fs.String("runtime", "", "the session's dialect")
	match := fs.String("match", "", "text in the title, case ignored")
	minTalks := fs.String("min-talks", "", "at least this many talks")
	limit := fs.String("limit", "", "rows per remote, 1 to 500")
	after := fs.String("after", "", "the next page of one -remote")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	pairs := fs.Args()
	switch {
	case *list && len(pairs) > 0:
		fmt.Fprintln(os.Stderr, "asz pull: -list takes no REMOTE/CONVERSATION")
		return 2
	case !*list && len(pairs) == 0:
		fs.Usage()
		return 2
	case !*list:
		for _, name := range listFlags {
			if set[name] {
				fmt.Fprintf(os.Stderr, "asz pull: -%s is for -list\n", name)
				return 2
			}
		}
	case set["after"] && len(remotes) != 1:
		fmt.Fprintln(os.Stderr, "asz pull: -after needs exactly one -remote")
		return 2
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return fail(err)
	}
	var targets []target
	if !*list {
		if targets, err = pullTargets(cfg, pairs); err != nil {
			fmt.Fprintln(os.Stderr, "asz pull:", err)
			return 2
		}
	} else {
		for _, name := range remotes {
			if _, ok := cfg.Remote(name); !ok {
				fmt.Fprintf(os.Stderr, "asz pull: no remote %q is configured\n", name)
				return 2
			}
		}
	}
	root, err := cfg.ResolvedRoot()
	if err != nil {
		return fail(err)
	}
	if err := refuseCopyRoot("pull", root); err != nil {
		return fail(err)
	}
	ttl, err := cfg.RemoteSnapshotTTLDuration()
	if err != nil {
		return fail(err)
	}
	ctx, stop := stopping()
	defer stop()
	local := &client.Local{Root: root, TTL: ttl, Log: func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "asz: "+format+"\n", args...)
	}}
	swept, err := local.Sweep(ctx)
	if err != nil {
		return fail(err)
	}
	code := 0
	if sweepFailed(swept) {
		code = 1
	}
	if *list {
		query := url.Values{}
		// The next page is asked for with every filter this one had: a
		// cursor carries them, and the server refuses it with others.
		again := []string{}
		if *cfgPath != "" {
			again = append(again, "-config", *cfgPath)
		}
		for _, f := range []struct {
			name, value string
		}{{"since", *since}, {"until", *until}, {"runtime", *runtime}, {"match", *match},
			{"min_talks", *minTalks}, {"limit", *limit}, {"after", *after}} {
			flagName := strings.ReplaceAll(f.name, "_", "-")
			if set[flagName] {
				query.Set(f.name, f.value)
				if f.name != "after" {
					again = append(again, "-"+flagName, f.value)
				}
			}
		}
		names := []string(remotes)
		if len(names) == 0 {
			for _, r := range cfg.Remotes {
				names = append(names, r.Name)
			}
		}
		return max(code, pullList(ctx, cfg, local, names, query, again))
	}
	return max(code, pullConversations(ctx, cfg, local, targets))
}

// sweepFailed says what the startup sweep could not settle, and whether
// anything failed: damage it found is the command's failure, though the
// rest of its work goes on. A conversation another command held is said
// and passed over.
func sweepFailed(swept *client.Swept) bool {
	for _, held := range swept.Busy {
		fmt.Fprintf(os.Stderr, "asz: %s is held by another command, and was not swept\n", held)
	}
	names := make([]string, 0, len(swept.Failed))
	for name := range swept.Failed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "asz: %s: %v\n", name, swept.Failed[name])
	}
	return len(names) > 0
}

// stopGrace is how long an interrupted pull keeps waiting, in all: for its
// requests, its request slots and its leases' release.
const stopGrace = 5 * time.Second

// stopping is the context a pull works under. An interrupt starts one
// shared grace of five seconds, and the context ends with it: a wait or a
// release still running then stops, and a lease it leaves expires on the
// server. No new conversation starts once the interrupt came, which
// interrupted says. A writer is never cut off: the local build checks the
// context between file operations and finishes the one it is in.
func stopping() (context.Context, func()) {
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), interruptKey{}, signals))
	go func() {
		select {
		case <-signals.Done():
		case <-ctx.Done():
			return
		}
		t := time.NewTimer(stopGrace)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
		cancel()
	}()
	return ctx, func() {
		cancel()
		stopSignals()
	}
}

type interruptKey struct{}

// interrupted says whether the pull was interrupted, so no new work starts.
func interrupted(ctx context.Context) bool {
	signals, ok := ctx.Value(interruptKey{}).(context.Context)
	return ok && signals.Err() != nil || ctx.Err() != nil
}

// target is one REMOTE/CONVERSATION a pull was given.
type target struct{ remote, conversation string }

// pullTargets reads the pairs: each a configured remote, a slash and a
// conversation, with no tag; equal pairs are one.
func pullTargets(cfg *config.Config, pairs []string) ([]target, error) {
	seen := map[target]bool{}
	var out []target
	for _, p := range pairs {
		remote, conversation, ok := strings.Cut(p, "/")
		if !ok {
			return nil, fmt.Errorf("%q is not REMOTE/CONVERSATION", p)
		}
		if strings.Contains(conversation, ":") {
			return nil, fmt.Errorf("%q names a tag; a pull always takes the head", p)
		}
		if _, ok := cfg.Remote(remote); !ok {
			return nil, fmt.Errorf("no remote %q is configured", remote)
		}
		if err := storage.CheckCopyNames(remote, conversation); err != nil {
			return nil, err
		}
		t := target{remote, conversation}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// remoteSlots are the request slots of one origin and token, shared by
// every remote that reaches it with that token.
type remoteSlots struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

func (r *remoteSlots) of(origin, token string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slots == nil {
		r.slots = map[string]chan struct{}{}
	}
	key := origin + "\x00" + token
	if r.slots[key] == nil {
		r.slots[key] = make(chan struct{}, 2)
	}
	return r.slots[key]
}

// openRemote reads a remote's credentials, which only a command that
// contacts it reads, and makes its client.
func openRemote(cfg *config.Config, name string, slots *remoteSlots) (*client.Remote, error) {
	rc, ok := cfg.Remote(name)
	if !ok {
		return nil, fmt.Errorf("no remote %q is configured", name)
	}
	base, err := rc.BaseURL()
	if err != nil {
		return nil, err
	}
	origin, err := rc.Origin()
	if err != nil {
		return nil, err
	}
	token, err := remoteToken(rc)
	if err != nil {
		return nil, err
	}
	o := client.RemoteOptions{Name: name, Base: base, Token: token, ServerName: rc.ServerName, Slots: slots.of(origin, token)}
	if rc.CAFile != "" {
		if o.CA, err = config.ReadSupplied(rc.CAFile, credentialBytes); err != nil {
			return nil, fmt.Errorf("remote %s: ca_file: %w", name, err)
		}
	}
	if rc.ClientCertFile != "" {
		cert, err := config.ReadSupplied(rc.ClientCertFile, credentialBytes)
		if err != nil {
			return nil, fmt.Errorf("remote %s: client_cert_file: %w", name, err)
		}
		key, err := config.ReadSupplied(rc.ClientKeyFile, credentialBytes)
		if err != nil {
			return nil, fmt.Errorf("remote %s: client_key_file: %w", name, err)
		}
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("remote %s: the client certificate: %w", name, err)
		}
		o.ClientCert = &pair
	}
	return client.NewRemote(o)
}

// remoteToken is a remote's token: its file's or its variable's text, with
// spaces and line ends taken off both ends. An empty one is refused.
func remoteToken(rc config.Remote) (string, error) {
	var text string
	if rc.TokenFile != "" {
		b, err := config.ReadSupplied(rc.TokenFile, credentialBytes)
		if err != nil {
			return "", fmt.Errorf("remote %s: token_file: %w", rc.Name, err)
		}
		text = string(b)
	} else {
		text = os.Getenv(rc.TokenEnv)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("remote %s: its token is empty", rc.Name)
	}
	return text, nil
}

// pullConversations pulls every target, the remotes at once and each
// remote's conversations in turn, and says what each ended with.
func pullConversations(ctx context.Context, cfg *config.Config, local *client.Local, targets []target) int {
	slots := &remoteSlots{}
	byRemote := map[string][]string{}
	var order []string
	for _, t := range targets {
		if byRemote[t.remote] == nil {
			order = append(order, t.remote)
		}
		byRemote[t.remote] = append(byRemote[t.remote], t.conversation)
	}
	var mu sync.Mutex
	failed := false
	say := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Printf(format+"\n", args...)
	}
	var wg sync.WaitGroup
	for _, name := range order {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			r, err := openRemote(cfg, name, slots)
			if err != nil {
				mu.Lock()
				failed = true
				mu.Unlock()
				say("%s: %v; not pulled: %s", name, err, strings.Join(byRemote[name], " "))
				return
			}
			for i, conversation := range byRemote[name] {
				if interrupted(ctx) {
					mu.Lock()
					failed = true
					mu.Unlock()
					say("%s: interrupted; not pulled: %s", name, strings.Join(byRemote[name][i:], " "))
					return
				}
				tag, unchanged, err := pullOne(ctx, local, r, conversation)
				switch {
				case err != nil:
					mu.Lock()
					failed = true
					mu.Unlock()
					say("%s/%s: %v", name, conversation, err)
					if errors.Is(err, client.ErrRemote) {
						say("%s: stopped; not pulled: %s", name, strings.Join(byRemote[name][i+1:], " "))
						return
					}
				case unchanged:
					say("%s/%s:%s unchanged", name, conversation, tag)
				default:
					say("%s/%s:%s", name, conversation, tag)
				}
			}
		}(name)
	}
	wg.Wait()
	if failed {
		return 1
	}
	return 0
}

// pullOne pulls one conversation and lets go of the snapshot it ended
// with: within the shared grace, past which the lease is left for a sweep.
func pullOne(ctx context.Context, local *client.Local, r *client.Remote, conversation string) (string, bool, error) {
	res, err := pullKeeping(ctx, local, r, conversation)
	if err != nil {
		return "", false, err
	}
	if res.Admission != nil {
		if err := res.Admission.Release(ctx); err != nil {
			return "", false, err
		}
	}
	return res.Tag, res.Unchanged, nil
}

// pullKeeping pulls one conversation: a manifest of its head with a lease,
// a build that reuses what the newest snapshot here holds and fetches the
// rest as one archive, publication, and the lease's release. A snapshot
// that went away meanwhile is fetched again under a fresh manifest, three
// times at most, and a build whose files changed is made again under the
// same hold of the conversation. The result is handed over with the
// admission of the snapshot it ended with still held, an unchanged one
// included: the caller reads that exact snapshot, then releases it, and a
// later pull cannot switch it.
func pullKeeping(ctx context.Context, local *client.Local, r *client.Remote, conversation string) (*client.Result, error) {
	tries := &client.Tries{}
	m, resp, err := r.Manifest(ctx, conversation, tries)
	if err != nil {
		return nil, err
	}
	src := r.Source(conversation, m, resp, tries)
	res, err := local.Pull(ctx, r.Name, conversation, m, src)
	// A release that fails only warns: the lease expires on the server.
	// The lease released is the one the source holds now, the freshest.
	if rerr := r.Done(ctx, conversation, src.Response()); rerr != nil {
		fmt.Fprintf(os.Stderr, "asz: %s/%s: the lease was not released, and expires on its own: %v\n", r.Name, conversation, rerr)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// pullList lists what each remote holds, at once, merged newest first, with
// the round of the newest snapshot here.
func pullList(ctx context.Context, cfg *config.Config, local *client.Local, names []string, query url.Values, again []string) int {
	slots := &remoteSlots{}
	type result struct {
		remote  string
		listing *format.Listing
		err     error
	}
	results := make([]result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i].remote = name
			r, err := openRemote(cfg, name, slots)
			if err != nil {
				results[i].err = err
				return
			}
			results[i].listing, results[i].err = r.List(ctx, query)
		}(i, name)
	}
	wg.Wait()
	var rows []listedRow
	code := 0
	for _, res := range results {
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "asz: %s: %v\n", res.remote, res.err)
			code = 1
			continue
		}
		for _, r := range res.listing.Rows {
			rows = append(rows, listedRow{res.remote, r})
		}
		for _, k := range res.listing.Skipped {
			fmt.Fprintf(os.Stderr, "asz: %s: %s was not read: %s\n", res.remote, printable(k.ID), printable(k.Reason))
		}
	}
	newestFirst(rows)
	// The local column holds a lease on each snapshot it read, until the
	// listing ends.
	heads := local.NewLocalHeads()
	defer func() {
		if err := heads.Release(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "asz:", err)
		}
	}()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REMOTE/CONVERSATION\tROUND\tLOCAL\tTALKS\tLAST ACTIVITY\tRUNTIME\tTITLE")
	for _, r := range rows {
		// Damage to a conversation's copies here fails the listing, and its
		// local column says it could not be read.
		localRound := "-"
		round, ok, err := heads.Head(ctx, r.remote, r.ID)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "asz: %s/%s: the copies here: %v\n", r.remote, r.ID, err)
			localRound, code = "?", 1
		case ok:
			localRound = fmt.Sprint(round)
		}
		fmt.Fprintf(tw, "%s/%s\t%d\t%s\t%d\t%s\t%s\t%s\n", r.remote, r.ID, r.Round, localRound, r.Talks,
			r.SessionThroughTime, printable(r.Runtime), printable(r.Title))
	}
	if err := tw.Flush(); err != nil {
		return fail(err)
	}
	for _, res := range results {
		if res.listing != nil && res.listing.Next != "" {
			words := append([]string{"asz", "pull", "-list", "-remote", res.remote}, again...)
			words = append(words, "-after", res.listing.Next)
			for i, w := range words {
				words[i] = shellWord(w)
			}
			fmt.Printf("\nmore from %s: %s\n", res.remote, strings.Join(words, " "))
		}
	}
	return code
}

// listedRow is one row of a merged listing, with the remote that listed it.
type listedRow struct {
	remote string
	format.Row
}

// newestFirst orders a merged listing: newest activity first, by the
// instant each time names, whatever its offset or fraction; no time last;
// then by remote and identifier.
func newestFirst(rows []listedRow) {
	at := func(r listedRow) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, r.SessionThroughTime)
		return t, err == nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		ta, oka := at(a)
		tb, okb := at(b)
		switch {
		case oka != okb:
			return oka
		case oka && !ta.Equal(tb):
			return ta.After(tb)
		case a.remote != b.remote:
			return a.remote < b.remote
		}
		return a.ID < b.ID
	})
}

// shellWord writes a word a shell reads back as it is: quoted when it holds
// anything but letters, digits and a few safe marks.
func shellWord(w string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=+@,", r)
	}
	if w != "" && strings.IndexFunc(w, func(r rune) bool { return !safe(r) }) < 0 {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// printable escapes control characters for a terminal.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
