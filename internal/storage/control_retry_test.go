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
	"errors"
	"sync"
	"testing"
	"time"
)

func TestControlRetryRechecksAfterUnlock(t *testing.T) {
	var lock sync.Mutex
	attempts, checks, writes := 0, 0, 0
	eligible := true
	now := time.Unix(100, 0)
	err := retryControl(context.Background(), func(context.Context) error {
		lock.Lock()
		defer lock.Unlock()
		attempts++
		checks++
		if !eligible {
			return nil
		}
		writes++
		return &ControlBusyError{Err: errors.New("busy")}
	}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
		if delay != 50*time.Millisecond {
			t.Fatalf("retry wait: %v", delay)
		}
		if !lock.TryLock() {
			t.Fatal("wait held the transition lock")
		}
		eligible = false
		lock.Unlock()
		now = now.Add(delay)
		return nil
	})
	if err != nil || attempts != 2 || checks != 2 || writes != 1 {
		t.Fatalf("retry: %v, attempts %d, checks %d, writes %d", err, attempts, checks, writes)
	}
}

func TestControlRetryOneBoundAndLastError(t *testing.T) {
	now := time.Unix(100, 0)
	start := now
	count := 0
	var last error
	err := retryControl(context.Background(), func(ctx context.Context) error {
		count++
		if count > 1 {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("reacquisition has no deadline")
			}
		}
		last = errors.New("last raw error")
		return &ControlBusyError{Err: last}
	}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
		now = now.Add(delay)
		if now.Sub(start) > 3*time.Second {
			t.Fatal("retry bound reset")
		}
		return nil
	})
	//nolint:errorlint // the same error value, not one that wraps it
	if err != last || now.Sub(start) != 2*time.Second || count != 40 {
		t.Fatalf("bound: %v, elapsed %v, attempts %d", err, now.Sub(start), count)
	}
}

func TestControlRetryReacquisitionUsesBound(t *testing.T) {
	raw := errors.New("busy")
	calls := 0
	start := time.Now()
	err := RetryControl(context.Background(), func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return &ControlBusyError{Err: raw}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			t.Fatal("reacquisition exceeded retry deadline")
			return nil
		}
	})
	//nolint:errorlint // the same error value, not one that wraps it
	if err != raw || calls != 2 || time.Since(start) > 3*time.Second {
		t.Fatalf("reacquisition: %v, calls %d", err, calls)
	}
}

func TestControlRetryStops(t *testing.T) {
	failure := errors.New("sync failed")
	count := 0
	//nolint:errorlint // the same error value, not one that wraps it
	if err := RetryControl(context.Background(), func(context.Context) error { count++; return failure }); err != failure || count != 1 {
		t.Fatalf("retried ordinary failure: %v, %d", err, count)
	}
	ctx, cancel := context.WithCancel(context.Background())
	count = 0
	err := RetryControl(ctx, func(context.Context) error {
		count++
		cancel()
		return &ControlBusyError{Err: failure}
	})
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancelled wait: %v, %d", err, count)
	}
	if err := RetryControl(ctx, func(context.Context) error { t.Fatal("called after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	count = 0
	if err := RetryControl(context.Background(), func(context.Context) error { count++; return nil }); err != nil || count != 1 {
		t.Fatalf("success: %v, %d", err, count)
	}
}
