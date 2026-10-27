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
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// remoteSelection is what -remote-conversation names: a pulled
// conversation, and which of its snapshots.
type remoteSelection struct {
	remote, conversation string
	// tag is a fixed snapshot, or empty for the newest unexpired one.
	tag string
	// suffixed says a tag or :latest was given, which narrows -list to
	// that one snapshot.
	suffixed bool
}

// parseRemoteSelection reads REMOTE/CONVERSATION, with :TAG or :latest
// after it. Neither a remote nor a conversation name can hold a colon or a
// slash, so the split is never in doubt.
func parseRemoteSelection(text string) (*remoteSelection, error) {
	remote, rest, ok := strings.Cut(text, "/")
	if !ok {
		return nil, fmt.Errorf("-remote-conversation %q is not REMOTE/CONVERSATION", text)
	}
	conversation, tag, suffixed := strings.Cut(rest, ":")
	if err := storage.CheckCopyNames(remote, conversation); err != nil {
		return nil, fmt.Errorf("-remote-conversation %q: %w", text, err)
	}
	s := &remoteSelection{remote: remote, conversation: conversation, suffixed: suffixed}
	switch {
	case !suffixed, tag == "latest":
	case tag == "":
		return nil, fmt.Errorf("-remote-conversation %q has an empty tag", text)
	default:
		if t, err := storage.ParseTag(tag); err != nil || storage.Stamp(t) != tag {
			return nil, fmt.Errorf("-remote-conversation %q: %q is not a snapshot's tag", text, tag)
		}
		s.tag = tag
	}
	return s, nil
}

func (s *remoteSelection) String() string { return s.remote + "/" + s.conversation }

// pulledLocal is the root's pulled copies, as a judge reads them.
func pulledLocal(cfg *config.Config, root string) (*client.Local, error) {
	ttl, err := cfg.RemoteSnapshotTTLDuration()
	if err != nil {
		return nil, err
	}
	return &client.Local{Root: root, TTL: ttl, Log: func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "asz: "+format+"\n", args...)
	}}, nil
}

// sweepPulled sweeps every pulled conversation, as a pull does at its start,
// and says what it could not settle: a conversation another command holds,
// and one whose controls failed. Neither stops the judging of another; a
// failure of the one selected fails the command, though no snapshot of it
// is left to admit.
func sweepPulled(ctx context.Context, local *client.Local, s *remoteSelection) error {
	swept, err := local.Sweep(ctx)
	if err != nil {
		return err
	}
	sweepFailed(swept)
	if err := swept.Failed[s.String()]; err != nil {
		return fmt.Errorf("%s: %w", s, err)
	}
	if interrupted(ctx) {
		return fmt.Errorf("%s: stopped while the copies here were swept; nothing is judged", s)
	}
	return nil
}

// admit selects the snapshot to judge and admits the command to it: with
// pull, the snapshot a pull of the head ended with, handed over admitted;
// else the one with the fixed tag, or the newest unexpired one. Selection
// reads only the snapshot's path and markers, so a remote no longer
// configured, or its credentials gone, does not stop judging what is here.
// Only a pull makes a request.
func (s *remoteSelection) admit(ctx context.Context, local *client.Local, cfg *config.Config, pull bool) (*storage.Admission, error) {
	if pull {
		r, err := openRemote(cfg, s.remote, &remoteSlots{})
		if err != nil {
			return nil, err
		}
		res, err := pullKeeping(ctx, local, r, s.conversation)
		if err != nil {
			return nil, fmt.Errorf("%s: %w; nothing is judged", s, err)
		}
		// A pull interrupted once its snapshot was published starts no
		// judging: the snapshot is let go, within the stop's grace.
		if interrupted(ctx) {
			return nil, errors.Join(fmt.Errorf("%s: the pull was stopped; nothing is judged", s), res.Admission.Release(ctx))
		}
		if res.Unchanged {
			fmt.Printf("%s:%s unchanged\n", s, res.Tag)
		} else {
			fmt.Printf("%s:%s\n", s, res.Tag)
		}
		return res.Admission, nil
	}
	var a *storage.Admission
	var err error
	if s.tag != "" {
		a, err = local.AdmitTag(ctx, s.remote, s.conversation, s.tag)
	} else {
		a, err = local.AdmitNewest(ctx, s.remote, s.conversation)
	}
	if errors.Is(err, client.ErrNoSnapshot) {
		what := "no unexpired snapshot of " + s.String()
		if s.tag != "" {
			what = "no snapshot " + s.String() + ":" + s.tag
		}
		return nil, fmt.Errorf("%s is here; run asz pull %s first", what, s)
	}
	if err == nil && interrupted(ctx) {
		return nil, errors.Join(fmt.Errorf("%s: stopped; nothing is judged", s), a.Release(ctx))
	}
	return a, err
}

// judgeListPulled lists the runs of every unexpired snapshot of a pulled
// conversation, newest tag first, each run named by its snapshot. Every
// snapshot read keeps its lease until the listing ends. Damage to one
// snapshot is said, and the others are still listed; damage to the
// conversation's own controls fails the listing.
func judgeListPulled(ctx context.Context, local *client.Local, s *remoteSelection) int {
	tags, err := local.Tags(s.remote, s.conversation)
	if err != nil {
		return fail(err)
	}
	var held []*storage.Admission
	defer func() {
		for _, a := range held {
			if err := a.Release(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "asz:", err)
			}
		}
	}()
	listed := 0
	for i := len(tags) - 1; i >= 0; i-- {
		tag := tags[i]
		a, err := local.AdmitTag(ctx, s.remote, s.conversation, tag)
		if err == nil {
			held = append(held, a)
		}
		// An interrupt lists nothing more, an admission it let through
		// included, and is never taken for damage.
		if interrupted(ctx) {
			return fail(fmt.Errorf("%s: stopped; nothing more is listed", s))
		}
		switch {
		case errors.Is(err, storage.ErrExpired), errors.Is(err, storage.ErrRemovedTag), errors.Is(err, storage.ErrPublishing):
			continue
		case errors.Is(err, storage.ErrConversationControl), errors.Is(err, storage.ErrLocalWrite):
			return fail(err)
		case err != nil:
			fmt.Fprintf(os.Stderr, "asz: %s:%s is damaged and is passed over: %v\n", s, tag, err)
			continue
		}
		runs, err := storage.JudgmentRuns(storage.NewZone(a.Root.Path), a.Marker.Session)
		if err != nil {
			return fail(err)
		}
		for _, run := range runs {
			fmt.Printf("%s:%s run %d  %s\n", s, tag, run.Run, runLine(run))
			listed++
		}
	}
	if listed == 0 {
		fmt.Printf("%s has no judge runs\n", s)
	}
	return 0
}
