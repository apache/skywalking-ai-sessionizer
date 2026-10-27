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

package client_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/pull/client"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/format"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/pulltest"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage/faulttest"
)

var errStopped = errors.New("the process stopped")

// sampled says which steps of a pull a fault is put at: every rename,
// replacement and removal, every fourth directory made, and every
// twenty-fifth other step; under the race detector, the renames,
// replacements and removals alone. Every step takes four and a half
// minutes, past what the suite can hold for one test.
func sampled(ops []faulttest.Operation) []int {
	var out []int
	made, other := 0, 0
	for i, op := range ops {
		switch op.Kind {
		case "replace", "rename-noreplace", "rename", "unlink":
			out = append(out, i+1)
		case "mkdir":
			if made++; !raceDetector && made%4 == 1 {
				out = append(out, i+1)
			}
		default:
			if other++; !raceDetector && other%25 == 1 {
				out = append(out, i+1)
			}
		}
	}
	return out
}

// A pull stopped at any step, by a returned error, by the process dying
// there, or by dying once the step took effect, leaves what the next pull
// finishes: after every power loss, and on what stayed visible, the next
// pull publishes, nothing private or unfinished is left, and every snapshot
// left is whole evidence.
func TestAPullSurvivesAFaultAtEveryStep(t *testing.T) {
	if testing.Short() {
		t.Skip("a pull per fault")
	}
	src, conversation := source(t, "three-rounds")
	m, err := pulltest.Manifest(src, conversation)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	run := func(model *faulttest.Model) error {
		local := &client.Local{Root: model.Root, TTL: 720 * time.Hour, Files: model, Now: func() time.Time { return at }}
		res, err := local.Pull(context.Background(), "prod", conversation, m, &pulltest.Source{Root: src})
		if err == nil {
			err = res.Admission.Release(context.Background())
		}
		return err
	}
	clean, err := faulttest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := run(clean); err != nil {
		t.Fatal(err)
	}
	final := func(t *testing.T, root, where string) {
		t.Helper()
		local := &client.Local{Root: root, TTL: 720 * time.Hour, Now: func() time.Time { return at.Add(time.Hour) }}
		res, err := local.Pull(context.Background(), "prod", conversation, m, &pulltest.Source{Root: src})
		if err != nil {
			t.Fatalf("%s: the next pull: %v", where, err)
		}
		if err := res.Admission.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, storage.RemoteDir, "prod", conversation)
		outer, err := storage.ReadOuterCopy(dir, "prod", conversation)
		if err != nil || outer.Publishing != nil {
			t.Fatalf("%s: the history: %+v %v", where, outer, err)
		}
		for _, private := range []string{storage.BuildingDir, storage.RemovingDir} {
			if left := entries(t, filepath.Join(dir, private)); len(left) != 0 {
				t.Fatalf("%s: left in %s: %v", where, private, left)
			}
		}
		for _, name := range entries(t, dir) {
			if _, err := storage.ParseTag(name); err != nil {
				continue
			}
			marker, err := storage.ReadSnapshotCopy(filepath.Join(dir, name), "prod", conversation, name)
			if err != nil {
				t.Fatalf("%s: %s: %v", where, name, err)
			}
			if _, err := format.Accept(filepath.Join(dir, name), format.Published, format.Want{Conversation: conversation,
				ManifestDigest: format.ManifestDigest(marker.ManifestDigest), Session: marker.Session, Round: marker.Round,
				Digest: format.CommitDigest(marker.Digest), Remote: "prod", Tag: name}); err != nil {
				t.Fatalf("%s: %s is not whole: %v", where, name, err)
			}
		}
	}
	for _, mode := range []string{"returned", "died", "unknown"} {
		for _, step := range sampled(clean.Operations()) {
			model, err := faulttest.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var failed faulttest.Operation
			stopped := false
			model.Fault = func(op faulttest.Operation) error {
				switch {
				case stopped:
					return errStopped
				case op.Index != step:
					return nil
				case mode == "returned":
					failed = op
					return syscall.EIO
				case mode == "died":
					failed, stopped = op, true
					return errStopped
				}
				failed, stopped = op, true
				return nil
			}
			model.After = func(op faulttest.Operation) error {
				if mode == "unknown" && op.Index == step {
					return errStopped
				}
				return nil
			}
			err = run(model)
			if err == nil {
				t.Fatalf("%s at step %d (%s): the pull succeeded", mode, step, failed.Kind)
			}
			model.Fault, model.After = nil, nil
			for _, cutoff := range []int{faulttest.DropAll, failed.Index} {
				recovered, err := model.PowerLoss(t.TempDir(), cutoff)
				if err != nil {
					t.Fatal(err)
				}
				final(t, recovered.Root, mode+" at "+failed.Kind+" then power loss")
			}
			final(t, model.Root, mode+" at "+failed.Kind)
		}
	}
}

// A sweep removing an expired snapshot, stopped at any step, leaves what
// the next sweep finishes: the tag recorded removed for good, its tree gone,
// and nothing left in _removing, after every power loss and on what stayed
// visible.
func TestASweepSurvivesAFaultAtEveryStep(t *testing.T) {
	if testing.Short() {
		t.Skip("a pull and a sweep per fault")
	}
	src, conversation := source(t, "three-rounds")
	m, err := pulltest.Manifest(src, conversation)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	published := func(t *testing.T) (*faulttest.Model, string) {
		t.Helper()
		model, err := faulttest.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		local := &client.Local{Root: model.Root, TTL: time.Hour, Files: model, Now: func() time.Time { return at }}
		res, err := local.Pull(context.Background(), "prod", conversation, m, &pulltest.Source{Root: src})
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Admission.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		return model, res.Tag
	}
	sweep := func(root string, files storage.FileSystem) error {
		local := &client.Local{Root: root, TTL: time.Hour, Files: files, Now: func() time.Time { return at.Add(2 * time.Hour) }}
		swept, err := local.Sweep(context.Background())
		if err == nil {
			for _, failed := range swept.Failed {
				err = errors.Join(err, failed)
			}
		}
		return err
	}
	clean, _ := published(t)
	start := clean.Point()
	if err := sweep(clean.Root, clean); err != nil {
		t.Fatal(err)
	}
	ops := clean.Operations()[start:]
	final := func(t *testing.T, root, tag, where string) {
		t.Helper()
		if err := sweep(root, nil); err != nil {
			t.Fatalf("%s: the next sweep: %v", where, err)
		}
		dir := filepath.Join(root, storage.RemoteDir, "prod", conversation)
		if removed, err := storage.IsRemoved(dir, "prod", conversation, tag); err != nil || !removed {
			t.Fatalf("%s: %s is not recorded removed: %v %v", where, tag, removed, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, tag)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %s is still there: %v", where, tag, err)
		}
		if left := entries(t, filepath.Join(dir, storage.RemovingDir)); len(left) != 0 {
			t.Fatalf("%s: left in _removing: %v", where, left)
		}
	}
	for _, mode := range []string{"returned", "died", "unknown"} {
		for i := range ops {
			if raceDetector && i%3 != 0 {
				continue
			}
			model, tag := published(t)
			step := model.Point() + i + 1
			var failed faulttest.Operation
			stopped := false
			model.Fault = func(op faulttest.Operation) error {
				switch {
				case stopped:
					return errStopped
				case op.Index != step:
					return nil
				case mode == "returned":
					failed = op
					return syscall.EIO
				case mode == "died":
					failed, stopped = op, true
					return errStopped
				}
				failed, stopped = op, true
				return nil
			}
			model.After = func(op faulttest.Operation) error {
				if mode == "unknown" && op.Index == step {
					return errStopped
				}
				return nil
			}
			if err := sweep(model.Root, model); err == nil && failed.Kind != "" && mode != "unknown" {
				t.Fatalf("%s at step %d (%s): the sweep succeeded", mode, step, failed.Kind)
			}
			model.Fault, model.After = nil, nil
			for _, cutoff := range []int{faulttest.DropAll, failed.Index} {
				recovered, err := model.PowerLoss(t.TempDir(), cutoff)
				if err != nil {
					t.Fatal(err)
				}
				final(t, recovered.Root, tag, mode+" at "+failed.Kind+" then power loss")
			}
			final(t, model.Root, tag, mode+" at "+failed.Kind)
		}
	}
}
