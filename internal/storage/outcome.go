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

// Outcome is what one collection pass established of one session: whether
// every source of it was read to its end.
type Outcome string

// The outcomes of a session.
const (
	// OutcomeComplete is a session every source of which was read to its end.
	OutcomeComplete Outcome = "complete"
	// OutcomeIncomplete is a session something of which is left: a record
	// held for its turn, a line not finished at a source's end, a session
	// another collector held, a conflict, a read that failed.
	OutcomeIncomplete Outcome = "incomplete"
	// OutcomeUnknown is a session nothing can be said of, as when a failure
	// of the pass names no session.
	OutcomeUnknown Outcome = "unknown"
)

// Outcomes are what one adapter's pass established of the sessions it read.
// A session the adapter did not read has no outcome from it: the adapter is
// no source of it this pass.
type Outcomes struct {
	// Seen are the sessions the pass read.
	Seen map[string]bool
	// Left are the sessions the pass left something of.
	Left map[string]bool
	// Unattributed is a failure that names no session, such as a directory
	// discovery could not read, which can hide a source of any session.
	Unattributed bool
}

// Saw records that the pass read a session.
func (o *Outcomes) Saw(session string) {
	if o.Seen == nil {
		o.Seen = map[string]bool{}
	}
	o.Seen[session] = true
}

// Leave records that the pass left something of a session.
func (o *Outcomes) Leave(session string) {
	o.Saw(session)
	if o.Left == nil {
		o.Left = map[string]bool{}
	}
	o.Left[session] = true
}

// Of is the outcome of one session from this adapter, and false when the
// adapter read nothing of it this pass. A failure that names no session
// makes every session unknown.
func (o Outcomes) Of(session string) (Outcome, bool) {
	switch {
	case o.Unattributed:
		return OutcomeUnknown, true
	case o.Left[session]:
		return OutcomeIncomplete, true
	case o.Seen[session]:
		return OutcomeComplete, true
	}
	return "", false
}

// Combine joins the outcomes the sources of one session gave: complete only
// when every one is complete, incomplete when any is, and unknown otherwise.
// No outcome at all is unknown.
func Combine(outcomes ...Outcome) Outcome {
	if len(outcomes) == 0 {
		return OutcomeUnknown
	}
	all := true
	for _, o := range outcomes {
		if o == OutcomeIncomplete {
			return OutcomeIncomplete
		}
		all = all && o == OutcomeComplete
	}
	if all {
		return OutcomeComplete
	}
	return OutcomeUnknown
}
