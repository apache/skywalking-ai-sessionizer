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

package view

import "net/http"

// ModeStatic is the status mode of a server nothing refreshes.
const ModeStatic = "static"

// Status reports how the data behind the page is kept current.
//
// A reader looking at a list needs to know how old it can be. When the server
// runs beside a local source it lands new data and assembles rounds on an
// interval, and the page shows when that last happened and when it happens
// next. In static mode nothing refreshes and both times are zero.
type Status struct {
	// Mode is "watch", "once" or "static".
	Mode    string `json:"mode"`
	Adapter string `json:"adapter,omitempty"`
	Source  string `json:"source,omitempty"`
	// IntervalMS is the watch interval. Zero outside watch mode.
	IntervalMS int64 `json:"interval_ms,omitempty"`
	// LastRefresh and NextRefresh are unix milliseconds. Zero means never
	// refreshed, and none scheduled.
	LastRefresh int64 `json:"last_refresh"`
	NextRefresh int64 `json:"next_refresh"`
	Refreshing  bool  `json:"refreshing"`
	// LastError is empty when the last pass was clean.
	LastError string `json:"last_error,omitempty"`
	// Counts from the last completed pass.
	Landed  int   `json:"landed"`
	Records int   `json:"records"`
	Rounds  int   `json:"rounds"`
	TookMS  int64 `json:"took_ms"`
}

// SetStatus records the state of whatever keeps the zone current.
func (s *Server) SetStatus(st Status) {
	s.statusMu.Lock()
	s.status = st
	s.statusMu.Unlock()
}

// Status returns the last recorded status.
func (s *Server) Status() Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Status())
}
