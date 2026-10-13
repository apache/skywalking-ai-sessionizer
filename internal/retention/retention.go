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

// Package retention removes the landed files of conversations that have been
// idle for longer than storage.retention, and leaves a skeleton in the place
// of each. A skeleton keeps every field the index is built from, so the
// conversation's index, and so its chain, go on as if nothing was removed.
//
// It is collector side: it reads the round chain by its format alone, as the
// scenario remover does, because the chain's package is server side.
package retention

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// Remover removes idle conversations' landed files from one storage root.
type Remover struct {
	Zone *storage.Zone
	// Retention is how long a conversation must be idle before its landed
	// files are removed. Zero removes nothing, though Recover still finishes
	// a removal an earlier pass left in progress.
	Retention time.Duration
	// Metrics says the pipeline derives metrics, so every file's derivation
	// must be committed before the file is removed: a removed file keeps no
	// execution record to derive from.
	Metrics bool
	// Now is the clock idleness is measured with. Tests set it.
	Now func() time.Time
	// SyncDir makes a directory's names survive a power loss, and says when
	// it could not. Empty is storage.SyncDir. Tests make it fail.
	SyncDir func(dir string) error
	// SyncFile makes a file's data and metadata survive a power loss. Empty
	// is storage.SyncFile. Tests watch it.
	SyncFile func(path string) error
	// Stop is called at each point a removal reaches, and a removal stops
	// there when it returns an error, as a crash would stop it. Tests set it.
	Stop func(Point) error
}

// Point is a place in a removal: its phase, and how many files of the
// phase are done.
type Point struct {
	Phase string
	Done  int
}

func (r *Remover) stop(phase string, done int) error {
	if r.Stop == nil {
		return nil
	}
	return r.Stop(Point{Phase: phase, Done: done})
}

func (r *Remover) syncFile(path string) error {
	if r.SyncFile != nil {
		return r.SyncFile(path)
	}
	return storage.SyncFile(path)
}

func (r *Remover) syncDir(dir string) error {
	if r.SyncDir != nil {
		return r.SyncDir(dir)
	}
	return storage.SyncDir(dir)
}

// Input is what the pipeline knows about this pass.
type Input struct {
	// Busy names the sessions whose parse failed or was held this pass. Their
	// chain may not cover their files, so they wait.
	Busy map[string]bool
}

// Result is what one pass of the remover did.
type Result struct {
	// Removed names the sessions files were removed from this pass, and
	// Files counts the files.
	Removed []string
	Files   int
	Errors  []error
}

func (r *Remover) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Recover finishes every removal an earlier pass left in progress. A pass
// runs it before anything lands, so no collector writes into a session whose
// files are being removed. It runs whatever the retention: a removal that
// was started is finished.
func (r *Remover) Recover() Result {
	var res Result
	sessions, err := sessionDirs(r.Zone.Root())
	if err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	intents := map[string]*storage.Removing{}
	var ids []string
	for _, id := range sessions {
		intent, err := storage.LoadRemoving(r.Zone.RemovingPath(id))
		if err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}
		if intent != nil {
			intents[id] = intent
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return res
	}
	r.exported(&res, func(sent *otlp.Sent) {
		for _, id := range ids {
			n, err := r.locked(id, func() (int, error) { return r.finish(id, intents[id], sent) })
			if err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("%s: finishing the removal: %w", id, err))
				continue
			}
			res.Removed = append(res.Removed, id)
			res.Files += n
		}
	})
	return res
}

// Pass removes the landed files of every session that may lose them now.
// It runs at the end of a pipeline pass, after the send. It also takes away
// what metrics derived from a removed file once the receiver has it.
func (r *Remover) Pass(in Input) Result {
	var res Result
	if r.Retention <= 0 {
		return res
	}
	sessions, err := sessionDirs(r.Zone.Root())
	if err != nil {
		res.Errors = append(res.Errors, err)
		return res
	}
	r.exported(&res, func(sent *otlp.Sent) {
		requests, err := storage.NewSpool(r.Zone).List()
		if err != nil {
			res.Errors = append(res.Errors, err)
			return
		}
		for _, id := range sessions {
			// A session never sent is passed over before its locks are
			// taken; removable asks again under them.
			if in.Busy[id] || sent.Owner(id) == "" {
				continue
			}
			n, err := r.locked(id, func() (int, error) {
				if err := r.tidy(id, sent, requests); err != nil {
					return 0, err
				}
				why, err := r.removable(id, sent)
				if err != nil || why != "" {
					return 0, err
				}
				return r.remove(id, sent)
			})
			if errors.Is(err, errBusy) {
				continue
			}
			if err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("%s: %w", id, err))
				continue
			}
			if n > 0 {
				res.Removed = append(res.Removed, id)
				res.Files += n
			}
		}
	})
	return res
}

// errBusy is a session another process holds a lock of. It is tried again
// on the next pass.
var errBusy = errors.New("retention: the session is busy")

// exported runs fn under the export lock, with push.state read once. The
// pipeline holds the root lock, so no pusher changes push.state meanwhile,
// and what fn learns of the receiver holds for the whole pass. The export
// lock is taken before any session's, the order every command takes them in.
func (r *Remover) exported(res *Result, fn func(*otlp.Sent)) {
	lock, err := storage.LockExport(r.Zone.Root())
	if errors.Is(err, storage.ErrExportBusy) {
		// Another pass sends from this root right now; the next one removes.
		return
	}
	if err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	defer func() { _ = lock.Unlock() }()
	sent, err := otlp.LoadSent(r.Zone.Root())
	if err != nil {
		res.Errors = append(res.Errors, err)
		return
	}
	fn(sent)
}

// locked runs fn under a session's lock and its chain's lock, in that order.
func (r *Remover) locked(id string, fn func() (int, error)) (int, error) {
	sessionLock, err := storage.LockSessionToRemove(r.Zone.SessionDir(id))
	if errors.Is(err, storage.ErrSessionBusy) {
		return 0, errBusy
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = sessionLock.Unlock() }()
	chainLock, err := storage.LockChain(filepath.Join(r.Zone.Root(), conversationsDir, id))
	if errors.Is(err, storage.ErrChainBusy) {
		return 0, errBusy
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = chainLock.Unlock() }()
	return fn()
}

// removable says why a session keeps its files this pass, or "" when they go.
//
// A session goes once every file of it is parsed, its metrics are derived
// when the pipeline derives them, and nothing new has landed for the whole
// retention. Sending is not waited for: a receiver that is down longer than
// the retention misses the files removed meanwhile, and asz does not repair
// that. A session never sent is kept until a later change covers it: its
// removal would start it again from its sources.
func (r *Remover) removable(id string, sent *otlp.Sent) (string, error) {
	if sent.Owner(id) == "" {
		return "never sent", nil
	}
	files, err := storage.LandedFiles(r.Zone, id)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "no landed file", nil
	}
	landed := false
	for _, lf := range files {
		if !lf.Skeleton {
			landed = true
		}
	}
	if !landed {
		return "removed already", nil
	}
	last := files[len(files)-1]
	at, err := landedAt(last)
	if err != nil {
		return "", err
	}
	if r.now().Sub(at) < r.Retention {
		return "active", nil
	}
	through, ok, err := chainThrough(r.Zone.Root(), id)
	if err != nil {
		return "", err
	}
	if !ok || through < last.Seq {
		return "not parsed", nil
	}
	if r.Metrics {
		done, err := metrics.DerivedAll(r.Zone, id, files)
		if err != nil {
			return "", err
		}
		if !done {
			return "metrics not derived", nil
		}
	}
	return "", nil
}

// remove removes every landed file of a session, through its newest.
func (r *Remover) remove(id string, sent *otlp.Sent) (int, error) {
	files, err := storage.LandedFiles(r.Zone, id)
	if err != nil {
		return 0, err
	}
	generation := 1
	for _, lf := range files {
		if !lf.Skeleton {
			continue
		}
		hdr, err := header(lf.Path)
		if err != nil {
			return 0, err
		}
		if hdr.Removed != nil && hdr.Removed.Generation >= generation {
			generation = hdr.Removed.Generation + 1
		}
	}
	// The tips are kept before the intent is written. A tip that cannot be
	// kept then keeps the session's files, and its collectors go on, where a
	// removal in progress would hold them off until it finished.
	if err := r.keepTips(id, files, generation); err != nil {
		return 0, err
	}
	intent := &storage.Removing{Generation: generation, Through: files[len(files)-1].Seq, Phase: storage.RemovingSkeletons}
	if err := storage.SaveRemoving(r.Zone.RemovingPath(id), intent); err != nil {
		return 0, err
	}
	if err := r.stop(storage.RemovingSkeletons, 0); err != nil {
		return 0, err
	}
	return r.finish(id, intent, sent)
}

// finish carries a removal on from its phase to its end: a skeleton for
// every landed file through the intent's sequence, then the landed files go,
// then the metrics requests of them the receiver has, then the intent.
//
// A skeleton is durable, its directory synced and the error checked, and it
// is checked to be the skeleton of its file, before the file is deleted: a
// file must never be deleted while the one that takes its place may still be
// lost, or is not its own. The directories the deletions changed are synced
// before the intent goes, so a power loss never brings back a deleted file
// with no intent left to finish its removal.
func (r *Remover) finish(id string, intent *storage.Removing, sent *otlp.Sent) (int, error) {
	files, err := storage.LandedFiles(r.Zone, id)
	if err != nil {
		return 0, err
	}
	var landed []storage.LandedFile
	for _, lf := range files {
		if lf.Seq <= intent.Through && !lf.Skeleton {
			landed = append(landed, lf)
		}
	}
	path := r.Zone.RemovingPath(id)
	if intent.Phase == storage.RemovingSkeletons {
		for i, lf := range landed {
			if err := r.writeSkeleton(id, lf, intent.Generation, sent); err != nil {
				return 0, err
			}
			if err := r.stop(storage.RemovingSkeletons, i+1); err != nil {
				return 0, err
			}
		}
		intent.Phase = storage.RemovingDelete
		if err := storage.SaveRemoving(path, intent); err != nil {
			return 0, err
		}
		if err := r.stop(storage.RemovingDelete, 0); err != nil {
			return 0, err
		}
	}
	// A reader that kept what it read of the session learns from the
	// generation that its files were replaced by skeletons. It is written
	// before any file is deleted, each time the phase starts, so a recovery
	// writes it too.
	if err := storage.SaveRemovalGeneration(r.Zone, id, intent.Generation); err != nil {
		return 0, err
	}
	// Every directory of the removed range is synced before the intent
	// goes, not only the ones this attempt changed: an attempt that stopped
	// after a deletion and before its sync left one no later deletion
	// covers. The spool is synced for the same reason.
	changed := map[string]bool{storage.NewSpool(r.Zone).Dir(): true}
	for _, lf := range files {
		if lf.Seq <= intent.Through {
			changed[filepath.Dir(lf.Path)] = true
		}
	}
	for i, lf := range landed {
		sk := storage.SkeletonPath(lf.Path)
		if err := r.ownSkeleton(lf.Path, sk); err != nil {
			return 0, fmt.Errorf("%s is kept: %w", lf.Path, err)
		}
		if err := os.Remove(lf.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		if err := r.stop(storage.RemovingDelete, i+1); err != nil {
			return 0, err
		}
	}
	requests, err := storage.NewSpool(r.Zone).List()
	if err != nil {
		return 0, err
	}
	if _, err := r.tidyDirs(id, sent, requests); err != nil {
		return 0, err
	}
	if err := r.syncAll(changed, r.syncDir); err != nil {
		return 0, err
	}
	later := false
	for _, lf := range files {
		if lf.Seq > intent.Through && !lf.Skeleton && lf.Stream == "" && lf.RunID == "" {
			later = true
		}
	}
	if err := r.reclaim(id, intent.Generation, later); err != nil {
		return 0, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	// The intent's deletion is made durable before a collector may land
	// into the session again. A power loss that brought the intent back
	// after a body landed would finish the removal again over that body.
	if err := r.syncDir(r.Zone.SessionDir(id)); err != nil {
		return 0, fmt.Errorf("the end of the removal may not survive a power loss: %w", err)
	}
	return len(landed), nil
}

// keepTips keeps, in the session's store under this removal's generation,
// the bytes of every chain tip the session's provider bodies leave. A body
// cut after the removal is compared with the same tips as with nothing
// removed, so their bytes must outlive the files they are built from. The
// bodies are read as a collector reads them, landed files and skeletons in
// landing order, with the store.
//
// A tip that holds or cites a piece too large to land is not kept: it cannot
// be rebuilt, and a collector passes it over as a base. Any other tip that
// does not rebuild keeps the session's files.
func (r *Remover) keepTips(id string, files []storage.LandedFile, generation int) error {
	store := r.Zone.BodyStore(id)
	held := providerbody.NewSession()
	held.Use(store)
	for _, lf := range files {
		if lf.Stream != "" || lf.RunID != "" {
			continue
		}
		f, err := storage.OpenLanded(lf.Path)
		if err != nil {
			return err
		}
		_, recs, err := sessiondata.All(f)
		f.Close()
		// A file that does not read whole holds nothing a collector refers
		// to, and its skeleton cannot be written: the session keeps its
		// files.
		if err != nil {
			return fmt.Errorf("%s does not read whole: %w", lf.Path, err)
		}
		for _, rec := range recs {
			_ = held.Add(rec)
		}
	}
	for _, tip := range held.Tips() {
		b, err := held.Body(tip)
		if errors.Is(err, providerbody.ErrTooLarge) {
			continue
		}
		if err != nil {
			return fmt.Errorf("the chain tip %s does not rebuild, so its bytes cannot be kept: %w", tip, err)
		}
		if err := store.PutTip(generation, tip, b, r.syncDir); err != nil {
			return err
		}
	}
	return nil
}

// reclaim deletes what the session's store holds that nothing needs once its
// landed bodies are removed: the tips of earlier removals, which only bodies
// now removed were built from, and the pieces, which only bodies now removed
// cited. A later body that cites a removed piece keeps it again. The pieces
// stay when a body landed after the removed range: a removal finished again,
// after a power loss brought its intent back, must not take what that body
// cites.
func (r *Remover) reclaim(id string, generation int, later bool) error {
	store := r.Zone.BodyStore(id)
	generations, err := store.Generations()
	if err != nil {
		return err
	}
	for _, g := range generations {
		if g == generation {
			continue
		}
		if err := os.RemoveAll(store.TipsDir(g)); err != nil {
			return err
		}
	}
	if later {
		return nil
	}
	return os.RemoveAll(store.PiecesDir())
}

func (r *Remover) syncAll(dirs map[string]bool, sync func(string) error) error {
	for dir := range dirs {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := sync(dir); err != nil {
			return fmt.Errorf("%s may not survive a power loss, so the removal stops here: %w", dir, err)
		}
	}
	return nil
}

// tidy takes away the metrics requests of a session's removed files once
// the receiver has them: a removal keeps a request that was not sent, and a
// later pass that sent it takes it here.
func (r *Remover) tidy(id string, sent *otlp.Sent, requests []storage.SpoolFile) error {
	changed, err := r.tidyDirs(id, sent, requests)
	if err != nil {
		return err
	}
	return r.syncAll(changed, r.syncDir)
}

// tidyDirs deletes the metrics request of every removed file of a session
// the receiver has, and names the directories it changed.
//
// A file's receipt stays. It is read only for a file metrics.state does not
// record as derived, and then it makes the derivation come out as it did:
// after a power loss took back a metrics.state whose sync failed, a file
// derived again without its receipt could count a call that went on into a
// later file at once, and the later file would then count it never. A
// request deleted early is made again, with the same bytes, from the
// receipt, and the export passes over it as sent.
func (r *Remover) tidyDirs(id string, sent *otlp.Sent, requests []storage.SpoolFile) (map[string]bool, error) {
	files, err := storage.LandedFiles(r.Zone, id)
	if err != nil {
		return nil, err
	}
	removed := map[uint64]bool{}
	for _, lf := range files {
		if lf.Skeleton {
			removed[lf.Seq] = true
		}
	}
	changed := map[string]bool{}
	for _, f := range requests {
		if f.Session != id || !removed[f.Seq] {
			continue
		}
		if _, ok := sent.Digest(storage.SpoolDir + "/" + filepath.Base(f.Path)); !ok {
			continue
		}
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		changed[filepath.Dir(f.Path)] = true
	}
	return changed, nil
}

// writeSkeleton writes the skeleton of one landed file and makes it durable,
// the error of the first sync after it checked, then gives it the file's
// modification time: the metrics grace period and the export's order read a
// file's time, and a removal must change neither.
//
// A skeleton an earlier attempt left is written again. That attempt may have
// stopped on a failed sync, and a later sync that succeeds does not say the
// data reached the disk: after a failed writeback the cache can be clean with
// nothing written. The landed file is still there in this phase, so nothing
// is lost by writing its skeleton again, and an empty reservation a crash
// left goes with it.
func (r *Remover) writeSkeleton(id string, lf storage.LandedFile, generation int, sent *otlp.Sent) error {
	sk := storage.SkeletonPath(lf.Path)
	if err := os.Remove(sk); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(lf.Path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r.Zone.Root(), lf.Path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	removed := sessiondata.Removed{
		At:         r.now().UTC().Format(time.RFC3339Nano),
		Generation: generation,
		Receiver:   sessiondata.ReceiverUnconfirmed,
		Owner:      sent.Owner(id),
	}
	sum := sha256.Sum256(data)
	if digest, ok := sent.Digest(rel); ok && digest == hex.EncodeToString(sum[:]) {
		removed.Receiver = sessiondata.ReceiverAccepted
		if sent.Rejected(rel) {
			removed.Receiver = sessiondata.ReceiverRejected
		}
	}
	if err := storage.WriteExclusiveDurable(sk, storage.PermLanded, func(w io.Writer) error {
		return skeleton.Write(w, data, removed)
	}, r.syncDir); err != nil {
		return err
	}
	return r.ownSkeleton(lf.Path, sk)
}

// ownSkeleton checks that sk is the whole skeleton of the landed file at
// path, and gives it the file's modification time.
func (r *Remover) ownSkeleton(path, sk string) error {
	digest, err := storage.FileDigest(path)
	if err != nil {
		return err
	}
	f, err := sessiondata.Open(sk)
	if err != nil {
		return err
	}
	hdr, _, err := sessiondata.All(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s does not read: %w", sk, err)
	}
	if hdr.Removed == nil || hdr.Removed.Digest != digest {
		return fmt.Errorf("%s is not the skeleton of %s", sk, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.Chtimes(sk, fi.ModTime(), fi.ModTime()); err != nil {
		return err
	}
	// The time is the file's metadata, which a sync of its directory does
	// not cover.
	return r.syncFile(sk)
}

// header reads a landed file's or a skeleton's header.
func header(path string) (sessiondata.Header, error) {
	f, err := sessiondata.Open(path)
	if err != nil {
		return sessiondata.Header{}, err
	}
	defer f.Close()
	rd, err := sessiondata.NewReader(f)
	if err != nil {
		return sessiondata.Header{}, err
	}
	return rd.Header(), nil
}

// landedAt is when a file landed, as its header says. A skeleton keeps its
// file's header, so a removal does not make a session look active.
func landedAt(lf storage.LandedFile) (time.Time, error) {
	hdr, err := header(lf.Path)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, hdr.At)
}

// conversationsDir holds each conversation's rounds under a storage root.
const conversationsDir = "_conversations"

// roundName is the name a chain gives a round.
var roundName = regexp.MustCompile(`^r(\d{6,})-([0-9a-f]{12})\.sf$`)

// chainThrough reads the landed sequence a conversation's head round covers,
// and false when the conversation has no round yet.
func chainThrough(root, id string) (uint64, bool, error) {
	dir := filepath.Join(root, conversationsDir, id, "rounds")
	items, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var head string
	var headN uint64
	for _, it := range items {
		m := roundName.FindStringSubmatch(it.Name())
		if it.IsDir() || m == nil {
			continue
		}
		if n, _ := strconv.ParseUint(m[1], 10, 64); head == "" || n > headN {
			head, headN = filepath.Join(dir, it.Name()), n
		}
	}
	if head == "" {
		return 0, false, nil
	}
	f, err := os.Open(head)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	// The field name is Session Flow's, written out because its package is
	// server side.
	var hdr struct {
		ThroughSeq uint64 `json:"through_seq"`
	}
	if err := json.Unmarshal(bytes.TrimRight(line, "\n"), &hdr); err != nil {
		return 0, false, fmt.Errorf("the head round %s cannot be read: %w", head, err)
	}
	return hdr.ThroughSeq, true, nil
}

// sessionDirs lists the sessions of a root: every directory whose name does
// not start with an underscore or a dot.
func sessionDirs(root string) ([]string, error) {
	items, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, it := range items {
		if it.IsDir() && !strings.HasPrefix(it.Name(), "_") && !strings.HasPrefix(it.Name(), ".") {
			out = append(out, it.Name())
		}
	}
	return out, nil
}
