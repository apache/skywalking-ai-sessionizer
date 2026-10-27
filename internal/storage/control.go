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

package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ControlBusyError preserves the final rooted rename error on Windows.
// The caller must release transition locks before retrying the whole decision.
type ControlBusyError struct{ Err error }

func (e *ControlBusyError) Error() string { return e.Err.Error() }
func (e *ControlBusyError) Unwrap() error { return e.Err }

// ReplaceControlDurable requires a checked parent and the owning transition lock.
// Each call writes a fresh file. A later sync cannot repair a failed replacement.
func ReplaceControlDurable(path string, mode os.FileMode, write func(io.Writer) error, files FileSystem) (err error) {
	files = writeFileSystem(files)
	parent := filepath.Dir(path)
	var temporary string
	var file WriteFile
	for {
		temporary = filepath.Join(parent, reservedPrefix+rand.Text())
		file, err = files.Create(temporary, 0o600)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	closed, renamed := false, false
	defer func() {
		var cleanup error
		if !closed {
			cleanup = file.Close()
		}
		if !renamed {
			cleanup = errors.Join(cleanup, files.Remove(temporary))
		}
		if cleanup != nil {
			var busy *ControlBusyError
			if errors.As(err, &busy) {
				err = busy.Err
			}
			err = errors.Join(err, cleanup)
		}
	}()
	if err = write(file); err != nil {
		return err
	}
	if err = file.Chmod(mode); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("storage: control file sync failed: %w", err)
	}
	closed = true
	if err = file.Close(); err != nil {
		return err
	}
	if err = files.Replace(parent, filepath.Base(temporary), filepath.Base(path)); err != nil {
		return err
	}
	renamed = true
	if err = files.SyncDir(parent); err != nil {
		return fmt.Errorf("storage: control replacement is visible but its directory sync failed: %w", err)
	}
	return nil
}

// CommitFactDurable keeps every field of an observed record, including fields the
// caller does not interpret. The caller checks identity and the fact under its lock.
// Dependent work may start only after this fresh replacement succeeds.
func CommitFactDurable(path string, check func(io.Reader) error, files FileSystem) (err error) {
	file, err := OpenShared(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("storage: control record %s is not a regular file", path)
	}
	if err := check(file); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return ReplaceControlDurable(path, info.Mode().Perm(), func(out io.Writer) error {
		_, err := io.Copy(out, file)
		return err
	}, files)
}

// RetryControl reruns the complete transition, including its locks and checks.
// The callback releases transition locks and new probes before returning. It uses
// the supplied context for reacquisition and retains only established ownership.
func RetryControl(ctx context.Context, transition func(context.Context) error) error {
	return retryControl(ctx, transition, time.Now, waitControl)
}

func retryControl(ctx context.Context, transition func(context.Context) error, now func() time.Time, wait func(context.Context, time.Duration) error) error {
	var last error
	var deadline time.Time
	attempt := ctx
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if last != nil && (!now().Before(deadline) || attempt.Err() != nil) {
			return last
		}
		err := transition(attempt)
		if err == nil {
			return nil
		}
		var busy *ControlBusyError
		if !errors.As(err, &busy) {
			if last != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return last
			}
			return err
		}
		last = busy.Err
		if deadline.IsZero() {
			deadline = now().Add(2 * time.Second)
			var cancel context.CancelFunc
			attempt, cancel = context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return last
		}
		if err := wait(attempt, min(50*time.Millisecond, remaining)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return last
		}
	}
}

func waitControl(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
