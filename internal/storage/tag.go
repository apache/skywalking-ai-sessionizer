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
	"fmt"
	"time"
)

// ParseTag requires Stamp's spelling so byte order also orders publications.
func ParseTag(tag string) (time.Time, error) {
	parsed, err := time.Parse("20060102T150405.000000000Z", tag)
	if err != nil {
		return time.Time{}, fmt.Errorf("storage: parse publication tag %q: %w", tag, err)
	}
	if Stamp(parsed) != tag {
		return time.Time{}, fmt.Errorf("storage: publication tag %q is not canonical", tag)
	}
	return parsed, nil
}

// NextTag keeps publication order when the clock repeats or moves backward.
func NextTag(now time.Time, greatest string) (string, error) {
	if greatest != "" {
		previous, err := ParseTag(greatest)
		if err != nil {
			return "", err
		}
		next := previous.Add(time.Nanosecond)
		if now.Before(next) {
			now = next
		}
	}
	tag := Stamp(now)
	// Adding to the last supported year must not produce an unreadable tag.
	if _, err := ParseTag(tag); err != nil {
		return "", fmt.Errorf("storage: next publication tag: %w", err)
	}
	return tag, nil
}
