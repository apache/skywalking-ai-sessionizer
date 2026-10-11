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

// Package kiro implements the kiro-local adapter: a pull collector that reads
// the sessions Kiro's V3 agent engine writes on disk.
//
// The V3 engine is what kiro-cli runs with --v3 or with chat.agentEngine set
// to v3, what Kiro Crew runs on its kas backend, and what the Kiro IDE runs.
// Its store was measured on kiro-cli 2.28.0 with engine @kiro/agent 0.66.26:
// one directory per session under ~/.kiro/sessions, holding the session's
// header, its records as JSON lines, and one file of records per subagent.
//
// The older stores are not read. kiro-cli's V2 engine writes
// sessions/cli/<id>.jsonl, and its V1 engine a SQLite database beside the
// login. Discovery counts the V2 sessions so a person is told they are there
// and why they are not collected.
package kiro

import (
	"os"
	"path/filepath"
	"regexp"
)

// Name is the adapter identifier used in configuration and in landed headers.
const Name = "kiro-local"

// Version is the adapter's contract version, recorded in landed headers so a
// bundle can be interpreted against the adapter that produced it.
const Version = "0.1.0"

// Dialect names the schema the records were read as.
const Dialect = "kiro/1"

// RuntimeName is the runtime a landed Kiro session came from, the service a
// push names it under.
const RuntimeName = "Kiro"

// Schema is the only session schema this adapter reads. It is the one the
// engine writes in every measured session. A session that names another is
// reported and left alone until that schema is measured.
const Schema = "1.0.0"

// sessionIDRe matches a V3 session directory, which is also the session's own
// id in its header.
var sessionIDRe = regexp.MustCompile(`^sess_[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsSessionID reports whether name is a V3 session id.
func IsSessionID(name string) bool { return sessionIDRe.MatchString(name) }

// childIDRe matches a subagent's file name without its extension. The engine
// names the file by the child's session id, a UUID in every measured case. The
// pattern is wider than that, so an id of another shape is still collected,
// and narrow enough that the name is safe as a directory.
var childIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ResolveSourceRoot determines where Kiro keeps its sessions.
//
// The engine writes under the home directory unless it is started with
// --home-dir, and it does not read KIRO_HOME. kiro-cli does not say how it
// starts the engine, so nothing here guesses: the root is the override, or
// the default.
func ResolveSourceRoot(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kiro"), nil
}
