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

// Package sessionjudge defines Session Judgment, the record of one judge run
// over a conversation: one file per stage of the run, written once.
//
// A judge run measures a conversation at one head of its chain. Its stages
// are the ledger asz computes, then what evaluators decide: the targets a
// person set, whether each was achieved, and what each piece of work was.
// Every stage is one file of JSON lines, framed as a round is: a header,
// frames, and a commit whose digest covers every line before it. The format
// is public. The exporter reads it to send a run, and a receiver reads it to
// store and list one, so nothing here depends on how a run is made.
package sessionjudge

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Schema names this version of the format.
const Schema = "sj/1"

// FrameType is the `t` of a line.
type FrameType string

// The frames a stage file holds. Which body frames appear depends on the
// stage, as the stage constants say.
const (
	FrameHeader      FrameType = "header"
	FramePrice       FrameType = "price"
	FrameCost        FrameType = "cost"
	FrameTarget      FrameType = "target"
	FrameInput       FrameType = "input"
	FrameGoal        FrameType = "goal"
	FrameFinding     FrameType = "finding"
	FrameOperation   FrameType = "operation"
	FrameLabelGoal   FrameType = "label_goal"
	FramePlacement   FrameType = "placement"
	FrameAgreement   FrameType = "agreement"
	FrameLabelText   FrameType = "label_text"
	FrameSource      FrameType = "source"
	FrameSourceText  FrameType = "source_text"
	FrameConclusion  FrameType = "conclusion"
	FrameDivergence  FrameType = "divergence"
	FrameRollback    FrameType = "rollback"
	FrameAchievement FrameType = "achievement"
	FrameLabel       FrameType = "label"
	FrameSubtask     FrameType = "subtask"
	FrameStage       FrameType = "stage"
	FrameOutcome     FrameType = "outcome"
	FrameCommit      FrameType = "commit"
)

// The stages of a run, in the order a run takes them.
const (
	// StageLedger is computed by asz with no model: price and cost frames.
	StageLedger = "ledger"
	// StageTargets is what the person asked for: target and input frames.
	StageTargets = "targets"
	// StageAchievement is whether each target was achieved: achievement
	// frames.
	StageAchievement = "achievement"
	// StageSubtasks is how each sub-task went, the work the agent gave to a
	// child agent or a helper stream: subtask frames.
	StageSubtasks = "subtasks"
	// StageEfficiency is what each piece of a target's work was: label
	// frames, and the cost frames asz priced them at.
	StageEfficiency = "efficiency"
	// StageReference is the sources a run is judged against, as asz saved
	// them, and the conclusions each holds: source, source_text and
	// conclusion frames. It runs only when the run is given a source.
	StageReference = "reference"
	// StageDivergence is how the conversation compares with each
	// conclusion: divergence, rollback and finding frames.
	StageDivergence = "divergence"
	// StageClose ends a run: a stage frame for every stage written, and the
	// outcome and cost of each target. A run is sent once it is closed.
	StageClose = "close"
)

// Stages lists the stages in run order.
var Stages = []string{StageLedger, StageTargets, StageAchievement, StageSubtasks, StageEfficiency, StageReference, StageDivergence, StageClose}

// StageRegroup is not a stage of a run: it is made over a run already
// closed, from a person's goals in their own words. An evaluator places each
// of the judge's targets in the person's goal it serves, and asz compares
// the two groupings. It writes a file of its own beside the run's, so the
// run is never rewritten.
const StageRegroup = "regroup"

// The status of a stage, and of a run on its close file.
const (
	StatusComplete = "complete"
	// StatusPartial is a stage that stopped before it covered everything,
	// such as one whose budget ran out. Its coverage says what it covered.
	StatusPartial = "partial"
	// StatusFailed is a stage whose evaluator gave nothing that could be
	// accepted. Its note says why.
	StatusFailed = "failed"
)

// The kinds of evaluator.
const (
	// EvaluatorComputed is asz itself, with no model.
	EvaluatorComputed = "computed"
	// EvaluatorAPI is asz calling a model provider's API.
	EvaluatorAPI = "api"
	// EvaluatorCLI is an agent CLI asz runs.
	EvaluatorCLI = "cli"
)

// Header is the first line of a stage file.
type Header struct {
	T      FrameType `json:"t"`
	Schema string    `json:"schema"`

	Conversation string `json:"conversation"`
	Session      string `json:"session"`
	// Run counts the runs of one conversation from 1. Two runs at the same
	// head are two runs.
	Run   int    `json:"run"`
	Stage string `json:"stage"`

	// Head is the round of the chain the run measured, and Parser and
	// Policy the chain's interpretation, so a reader knows what the
	// citations address.
	Head   Head   `json:"head"`
	Parser string `json:"parser"`
	Policy string `json:"policy"`

	// Window is the part of the conversation the run judges, absent for
	// the whole conversation.
	Window *Window `json:"window,omitempty"`

	// BuildsOn names the earlier stages of the run this one read, by
	// digest.
	BuildsOn []Dependency `json:"builds_on,omitempty"`
	// Labels is the label file a regroup read: its name, its SHA-256 and
	// the regroup's version, counted from 1 over the regroups of a run.
	Labels *LabelFile `json:"labels,omitempty"`

	Evaluator Evaluator `json:"evaluator"`
	// Kit is the prompts and schemas the evaluator was driven by. Absent
	// for a computed stage.
	Kit *Kit `json:"kit,omitempty"`

	// Started and Finished are when the stage ran, in RFC 3339 UTC. They
	// are the evaluation's own times, never the conversation's.
	Started  string `json:"started"`
	Finished string `json:"finished"`
	Status   string `json:"status"`
	// Note says why a stage is partial or failed.
	Note     string    `json:"note,omitempty"`
	Coverage *Coverage `json:"coverage,omitempty"`
	// Reading is what the evaluator read through the navigation, when the
	// stage was an evaluator's.
	Reading *Reading `json:"reading,omitempty"`
	// Requests is what the evaluator's model requests were, as the provider
	// counted them.
	Requests *Requests `json:"requests,omitempty"`
	// Mode is single when one evaluator call did the stage, and stream when
	// it was cut into Chunks; Attempts counts the calls made, retries
	// included.
	Mode     string `json:"mode,omitempty"`
	Chunks   int    `json:"chunks,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	// PromptBytes is the largest prompt one of the stage's calls was sent,
	// as assembled, the rows and the carried state included.
	PromptBytes int `json:"prompt_bytes,omitempty"`
	// Usage is what the evaluator itself consumed, when it reported it.
	// Absent means unknown, never zero.
	Usage *sessiondata.Usage `json:"usage,omitempty"`
	// Credits is what an evaluator that bills in credits reported.
	Credits *float64 `json:"credits,omitempty"`
	// ReportedCostUSD is what the evaluator itself said the stage cost, in
	// US dollars, as claude does. It is the evaluator's own figure, kept
	// apart from any price asz applies.
	ReportedCostUSD *float64 `json:"reported_cost_usd,omitempty"`

	// Summary is what a list of runs shows, on the close file only, so a
	// receiver lists runs without reading their frames.
	Summary *Summary `json:"summary,omitempty"`
}

// Window is the part of a conversation a run judges: whole main talks,
// selected by the time they start, from From up to but not including To, or
// by their ids, from FromTalk to ToTalk, both included.
type Window struct {
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	FromTalk string `json:"from_talk,omitempty"`
	ToTalk   string `json:"to_talk,omitempty"`
	// Talks are the main talks selected, and Before counts the earlier
	// ones an evaluator read as context. End is the last record time of
	// the selected work, in unix milliseconds; nothing after it was shown.
	Talks  []string `json:"talks"`
	Before int      `json:"before"`
	End    int64    `json:"end,omitempty"`
}

// Reading is what an evaluator read through the navigation in one stage:
// how many calls, how many bytes of answers, the budget it had, and how many
// answers were refused because the budget ran out.
type Reading struct {
	Calls   int `json:"calls"`
	Bytes   int `json:"bytes"`
	Budget  int `json:"budget,omitempty"`
	Refused int `json:"refused,omitempty"`
}

// Requests describes a stage's model requests. Count and Largest, the
// largest input side of one request, are present when the evaluator said
// what each request consumed; WholeOnly says it gave one total per call
// instead, as codex does. ContextWindow is the window of the model that did
// the work, when the evaluator said, so Largest can be read against it.
type Requests struct {
	Count         int  `json:"count,omitempty"`
	Largest       int  `json:"largest,omitempty"`
	WholeOnly     bool `json:"whole_only,omitempty"`
	ContextWindow int  `json:"context_window,omitempty"`
	// Unknown counts the requests whose usage the provider never said, as
	// one whose answer was lost on the way. The stage's usage holds the
	// others and says nothing of these.
	Unknown int `json:"unknown,omitempty"`
}

// Head names a round of the chain.
type Head struct {
	Round  uint64 `json:"round"`
	Digest string `json:"digest"`
}

// Dependency names an earlier stage file of the same run.
type Dependency struct {
	Stage  string `json:"stage"`
	Digest string `json:"digest"`
}

// Evaluator is who made a stage.
type Evaluator struct {
	// Name is the evaluator's name in the configuration, such as "claude".
	Name string `json:"name"`
	// Kind is computed, api or cli.
	Kind string `json:"kind"`
	// Program is what ran: "asz" for a computed stage, the API ("anthropic",
	// "openai") or the CLI ("claude", "codex", "kiro-cli").
	Program string `json:"program"`
	// Model is the model the evaluator was asked to use, when it was told
	// one, and Version the program's own version, when it reports one.
	Model   string `json:"model,omitempty"`
	Version string `json:"version,omitempty"`
}

// Kit identifies the prompts and schemas that drove a stage.
type Kit struct {
	// Version is the digest of every file of the kit, and Prompt the digest
	// of the stage's own prompt.
	Version string `json:"version"`
	Prompt  string `json:"prompt"`
}

// Coverage says how much of the conversation a stage covered. Each count is
// absent where the stage does not cover that kind of thing.
type Coverage struct {
	Inputs        *int `json:"inputs,omitempty"`
	InputsRead    *int `json:"inputs_read,omitempty"`
	Targets       *int `json:"targets,omitempty"`
	TargetsJudged *int `json:"targets_judged,omitempty"`
	// Goals counts the goals found, and Unplaced the targets no goal holds.
	Goals        *int `json:"goals,omitempty"`
	Unplaced     *int `json:"unplaced,omitempty"`
	Talks        *int `json:"talks,omitempty"`
	TalksLabeled *int `json:"talks_labeled,omitempty"`
	// Subtasks counts the sub-tasks a run could judge, and SubtasksJudged
	// those it did.
	Subtasks       *int `json:"subtasks,omitempty"`
	SubtasksJudged *int `json:"subtasks_judged,omitempty"`
	Calls          *int `json:"calls,omitempty"`
	// CallsWithoutUsage counts calls whose usage is unknown.
	CallsWithoutUsage *int `json:"calls_without_usage,omitempty"`
}

// Summary is what a list of runs shows.
type Summary struct {
	Targets      int `json:"targets"`
	Achieved     int `json:"achieved"`
	Partly       int `json:"partly"`
	NotAchieved  int `json:"not_achieved"`
	CannotVerify int `json:"cannot_verify"`
	// Usage is the conversation's tokens the run measured, and Avoidable
	// the part the efficiency stage labeled avoidable. Unknown counts the
	// calls whose usage is not in it, and OutputUnknown those whose output
	// is not, so a total that is not whole says so.
	Usage         sessiondata.Usage  `json:"usage"`
	Unknown       int                `json:"unknown,omitempty"`
	OutputUnknown int                `json:"output_unknown,omitempty"`
	Avoidable     *sessiondata.Usage `json:"avoidable,omitempty"`
	// AvoidableBySubtasks is the part of Avoidable sub-agents caused, and
	// AvoidableCauseUnknown the part whose cause the record does not show.
	AvoidableBySubtasks   *sessiondata.Usage `json:"avoidable_by_subtasks,omitempty"`
	AvoidableCauseUnknown *sessiondata.Usage `json:"avoidable_cause_unknown,omitempty"`
	// Money and Currency are the conversation's priced cost, when any call
	// was priced. Unpriced counts the calls that were not.
	Money    *float64 `json:"money,omitempty"`
	Currency string   `json:"currency,omitempty"`
	Unpriced int      `json:"unpriced,omitempty"`
	// Score is the run's two scores, and Judging what the run's evaluators
	// consumed.
	Score *Score `json:"score,omitempty"`
	// Goals is how each goal did, in the order of their ids.
	Goals []GoalOutcome `json:"goals,omitempty"`
	// UnassignedOperations are the file operations no target's calls made.
	UnassignedOperations []string `json:"unassigned_operations,omitempty"`
	Judging              *Amount  `json:"judging,omitempty"`
	// JudgingReportedUSD adds up what the evaluators said the run's stages
	// cost, present only when every stage that ran said.
	JudgingReportedUSD *float64 `json:"judging_reported_usd,omitempty"`
	// JudgingMs is the time from the first evaluator stage's start to the
	// last one's end, in milliseconds.
	JudgingMs int64 `json:"judging_ms,omitempty"`
}

// Score is how a conversation did, in two numbers that are never combined:
// whether the targets were achieved, and how much of their cost was
// avoidable. A number is absent when nothing could give it.
type Score struct {
	// Achievement is the mean over the Judged targets, those judged
	// achieved, partly or not achieved, counting achieved 1, not achieved
	// 0, and partly the share of its criteria met, a contradicted claim
	// counting as one more not met. Cancelled and replaced
	// targets, and targets that cannot be verified, are left out.
	Achievement *float64 `json:"achievement,omitempty"`
	// Final is the mean of the same targets' final scores, which apply the
	// contradictions and undoings of what each delivered.
	Final  *float64 `json:"final,omitempty"`
	Judged int      `json:"judged"`
	// Efficiency is 1 less the avoidable part of the reviewed cost, and
	// Reviewed the part of the targets' cost a label covers. Unit says which
	// cost: money when every call of the targets was priced, tokens if not.
	Efficiency *float64 `json:"efficiency,omitempty"`
	Reviewed   *float64 `json:"reviewed,omitempty"`
	// Delivered is the part of the targets' cost that went to targets
	// achieved.
	Delivered *float64 `json:"delivered,omitempty"`
	Unit      string   `json:"unit,omitempty"`
}

// The units of a score.
const (
	UnitMoney  = "money"
	UnitTokens = "tokens"
)

// Rates are prices per million tokens of each type.
type Rates struct {
	In         float64 `json:"in" yaml:"in"`
	Out        float64 `json:"out" yaml:"out"`
	CacheRead  float64 `json:"cache_read" yaml:"cache_read"`
	CacheWrite float64 `json:"cache_write" yaml:"cache_write"`
}

// Price is one rate the ledger applied, so a run can be checked and priced
// again without the file it came from.
type Price struct {
	T FrameType `json:"t,omitempty"`
	// Model is the name a call recorded, and Key the price file entry it
	// matched: the entry's model, through an alias or not.
	Model      string `json:"model"`
	Key        string `json:"key"`
	From       string `json:"from,omitempty"`
	Currency   string `json:"currency"`
	PerMillion Rates  `json:"per_million"`
}

// The scopes of a cost frame.
const (
	ScopeConversation = "conversation"
	ScopeTalk         = "talk"
	// ScopeLoose is the calls of one stream that no talk contains.
	ScopeLoose  = "loose"
	ScopeTarget = "target"
	// ScopeShared is a talk's cost split between several targets.
	ScopeShared = "shared"
	// ScopeUnassigned is the cost of talks no target claims.
	ScopeUnassigned = "unassigned"
	// ScopeUnattributed is the cost of child streams no single talk can be
	// shown to have started: a nested child no call names, or a child two
	// talks claim.
	ScopeUnattributed = "unattributed"
	// ScopeWork is the cost of one target's work under one label.
	ScopeWork = "work"
	// ScopeSubtask is a sub-task's cost with every sub-task under it. It is
	// a view over calls counted elsewhere, never added to a total.
	ScopeSubtask = "subtask"
)

// Cost is what a set of calls consumed, as recorded, and what it cost where
// a price applied.
type Cost struct {
	T     FrameType `json:"t,omitempty"`
	Scope string    `json:"scope"`
	// ID names what the cost is of: a talk, a stream for loose calls, or a
	// target.
	ID     string `json:"id,omitempty"`
	Target string `json:"target,omitempty"`
	Work   string `json:"work,omitempty"`
	// Cause says, for avoidable work, whose doing it was: CauseMain for the
	// main agent, the id of the sub-task whose own work or whose task given
	// to a sub-task of its own went wrong, or CauseUnknown.
	Cause string `json:"cause,omitempty"`
	// Share is the part of a talk given to a target, as a fraction, for a
	// shared cost.
	Share *float64 `json:"share,omitempty"`

	Calls int               `json:"calls"`
	Usage sessiondata.Usage `json:"usage"`
	// Unknown counts the calls whose usage is unavailable: their tokens
	// are not in Usage, and are not zero.
	Unknown int `json:"unknown,omitempty"`
	// OutputUnknown counts the calls that never finished: what they read is
	// in Usage, and their output is not.
	OutputUnknown int `json:"output_unknown,omitempty"`
	// Money is the priced part, and Unpriced counts the calls with usage
	// and no price.
	Money    *float64 `json:"money,omitempty"`
	Currency string   `json:"currency,omitempty"`
	Unpriced int      `json:"unpriced,omitempty"`
	// From and To are the first and last record time the cost covers, in
	// unix milliseconds, when any record carried one.
	From int64 `json:"from,omitempty"`
	To   int64 `json:"to,omitempty"`
	// Children counts the calls of child streams included.
	Children int `json:"children,omitempty"`
}

// Cite points at evidence in the conversation: a node of the fold, and the
// landed record the node stands on.
type Cite struct {
	Node string `json:"node"`
	Ref  *Ref   `json:"ref,omitempty"`
}

// Ref is a landed record's address, as a round's references write it: the
// file's sequence, the record's row in it counting from 1, and the part.
type Ref struct {
	Seq   uint64 `json:"seq"`
	Row   uint64 `json:"row"`
	Block *int   `json:"block,omitempty"`
}

// Target is one outcome a person asked for.
type Target struct {
	T         FrameType   `json:"t,omitempty"`
	ID        string      `json:"id"`
	Statement string      `json:"statement"`
	Criteria  []Criterion `json:"criteria,omitempty"`
	// Status is open, closed, cancelled or replaced, as of the head.
	Status string `json:"status"`
	// ReplacedBy names the target that replaced this one, when the person
	// changed what they wanted.
	ReplacedBy string `json:"replaced_by,omitempty"`
	// Talks are the talks whose cost goes to this target.
	Talks []string `json:"talks"`
	Cite  []Cite   `json:"cite"`
}

// Criterion is one thing that would show a target achieved. Its id is the
// target's id and its place, such as t2.c1, given by asz and never reused, so
// an achievement answers each criterion by name.
type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Dropped says the person dropped the criterion. It is kept, so the
	// history of what was asked stays whole, and never judged.
	Dropped bool `json:"dropped,omitempty"`
}

// The statuses of a target.
const (
	TargetOpen      = "open"
	TargetClosed    = "closed"
	TargetCancelled = "cancelled"
	TargetReplaced  = "replaced"
)

// Input is what one input from outside the agent did to the targets, and
// how it relates to the inputs before it.
type Input struct {
	T       FrameType `json:"t,omitempty"`
	Node    string    `json:"node"`
	Talk    string    `json:"talk"`
	Actions []Action  `json:"actions"`
	// Relations say which goal the input acts on and how. An input with
	// mixed requests has one for each; every input has at least one.
	Relations []Relation `json:"relations,omitempty"`
}

// Relation is how an input, or one request in it, relates to the inputs
// before it. New starts a goal; continue, adjust and return act on a goal
// and name the earlier input of it they follow; unclear and not from the
// person name no goal.
type Relation struct {
	Kind string `json:"kind"`
	Goal string `json:"goal,omitempty"`
	// Follows is the earlier input of the goal this one follows.
	Follows string `json:"follows,omitempty"`
	// Request says which part of a mixed input this relation concerns, in
	// the person's words.
	Request string `json:"request,omitempty"`
	// Statement is the goal's result as the person meant it after this
	// input, for new and adjust: the steps of the goal's evolution.
	Statement string `json:"statement,omitempty"`
}

// The relations an input can have.
const (
	RelationNew       = "new"
	RelationContinue  = "continue"
	RelationAdjust    = "adjust"
	RelationReturn    = "return"
	RelationUnclear   = "unclear"
	RelationNotPerson = "not_from_person"
)

// Relations lists every relation kind.
var Relations = []string{RelationNew, RelationContinue, RelationAdjust, RelationReturn, RelationUnclear, RelationNotPerson}

// Goal is the targets that serve one result the person asked for: a group
// for the report, with no criteria or score of its own. Its id, such as
// g2, is given by asz and never reused.
type Goal struct {
	T         FrameType `json:"t,omitempty"`
	ID        string    `json:"id"`
	Statement string    `json:"statement"`
	// Members are the targets in the goal, by id.
	Members []string `json:"members"`
	// Main is the member whose result the goal is for, or empty when it is
	// undetermined; MainReason says why either way, and MainCite holds the
	// person's words that make it the main one.
	Main       string `json:"main,omitempty"`
	MainReason string `json:"main_reason,omitempty"`
	MainCite   []Cite `json:"main_cite,omitempty"`
	// Evolution is the goal's result after each input that set or
	// adjusted it, in order.
	Evolution []Evolution `json:"evolution"`
	// Cite holds the inputs that show these targets serve one result.
	Cite []Cite `json:"cite"`
}

// Evolution is one step of a goal's history: the input and what the goal
// asked for after it.
type Evolution struct {
	Input     string `json:"input"`
	Statement string `json:"statement"`
}

// Action is one thing an input did. Target is empty for none, not from a
// person and unknown.
type Action struct {
	Target string `json:"target,omitempty"`
	Action string `json:"action"`
}

// The actions an input takes.
const (
	ActionCreate  = "create"
	ActionRefine  = "refine"
	ActionSteer   = "steer"
	ActionAccept  = "accept"
	ActionCorrect = "correct"
	ActionCancel  = "cancel"
	// ActionNone is an input that asks for nothing, such as thanks.
	ActionNone = "none"
	// ActionNotPerson is an input that did not come from a person, such as
	// a system notice a runtime records as one.
	ActionNotPerson = "not_from_person"
	ActionUnknown   = "unknown"
)

// Actions lists every action.
var Actions = []string{ActionCreate, ActionRefine, ActionSteer, ActionAccept, ActionCorrect, ActionCancel, ActionNone, ActionNotPerson, ActionUnknown}

// Achievement is whether one target was achieved.
type Achievement struct {
	T      FrameType `json:"t,omitempty"`
	Target string    `json:"target"`
	Level  string    `json:"level"`
	// Basis is what the level rests on.
	Basis    string  `json:"basis"`
	Criteria []Check `json:"criteria,omitempty"`
	Claims   []Check `json:"claims,omitempty"`
	// Events are the deliveries of the target's result and their undoing,
	// in recorded order.
	Events []Event `json:"events,omitempty"`
	// Contradictions are evidence in the conversation that a delivered
	// result is wrong, each tied to the delivery it concerns.
	Contradictions []Contradiction `json:"contradictions,omitempty"`
	// Rework is the rounds of redoing the target's work, in recorded order.
	Rework []Rework `json:"rework,omitempty"`
	// Account says what was done for the target, by what it was for.
	Account *Account `json:"account,omitempty"`
	// Findings are the problems the stage observed in the target's work.
	Findings []Finding `json:"findings,omitempty"`
	Note     string    `json:"note,omitempty"`
	Cite     []Cite    `json:"cite"`
}

// Operation is one file operation the record shows: a file a workspace change
// record captured, or a file a tool's request named. Its id, such as o3, is
// given in the order the operations landed, so a run at one head gives the
// same ids. The list is history, never a net diff: an edit repeated, two
// captures of one change and a later revert are operations of their own.
type Operation struct {
	T  FrameType `json:"t,omitempty"`
	ID string    `json:"id"`
	// Step is the tool step the operation joins, empty for a change no step
	// joins.
	Step string `json:"step,omitempty"`
	// Record is the change record's id, and Workspace the root its paths
	// are relative to.
	Record    string `json:"record,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// Path is the file, and Operation what happened to it: for a change
	// record, create, modify, delete, type_change, or unlisted when its file
	// list was taken out; for a request, write or edit.
	Path      string `json:"path,omitempty"`
	Operation string `json:"operation"`
	// Source says where the operation was found. A request shows only an
	// attempt, so Status says whether its tool returned, failed or never
	// returned.
	Source      string `json:"source"`
	Status      string `json:"status,omitempty"`
	Attribution string `json:"attribution,omitempty"`
	Added       *int   `json:"added,omitempty"`
	Removed     *int   `json:"removed,omitempty"`
	Ref         *Ref   `json:"ref,omitempty"`
}

// Where an operation was found: a workspace change record, or a tool's
// request that named the file.
const (
	SourceChangeRecord = "change_record"
	SourceToolRequest  = "tool_request"
)

// SourceASZ marks a finding asz recorded itself, from what the stages
// decided.
const SourceASZ = "asz"

// OperationUnlisted is a change record whose file list was taken out to fit
// its landed file: files changed, which are not named.
const OperationUnlisted = "unlisted"

// Source is one source a run is judged against, as asz saved it before any
// evaluator ran: where it came from, how it was fetched, and what was saved.
// Its content is data, never instructions. Captured says when asz took the
// copy, never when what it says was knowable. A source that could not be
// fetched is kept as failed, which leaves its comparison unavailable.
type Source struct {
	T  FrameType `json:"t,omitempty"`
	ID string    `json:"id"`
	// Source is the source as it is kept: a URL without its query and
	// fragment, which QueryRemoved says were there. A query is never a
	// place for a credential, and asz keeps none.
	Source       string `json:"source"`
	QueryRemoved bool   `json:"query_removed,omitempty"`
	Captured     string `json:"captured"`
	Method       string `json:"method"`
	Outcome      string `json:"outcome"`
	Error        string `json:"error,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256,omitempty"`
	// Cut says the source was larger than a saved copy may be, and only
	// its start was saved.
	Cut bool `json:"cut,omitempty"`
	// Revision, Published and Author are what the source itself says, when
	// it says.
	Revision  string `json:"revision,omitempty"`
	Published string `json:"published,omitempty"`
	Author    string `json:"author,omitempty"`
	// Text is the text an evaluator reads, when it was taken out of another
	// form such as HTML, beside the fetched bytes. Lines counts its lines.
	Text  *Extracted `json:"text,omitempty"`
	Lines int        `json:"lines"`
	// MCP is the tool call that fetched a source through an MCP server.
	MCP *SourceMCP `json:"mcp,omitempty"`
}

// SourceMCP is the tool call that fetched a source: the server, kept as
// its URL's scheme and host or its program's name; the era and protocol
// version it spoke, when it got that far; the tool and its arguments as
// sent, a secret one shown as [env NAME]; how many content blocks that are
// not text, such as images, were left out; whether structured content was
// left out beside the text; and how many secrets were redacted. The copy is
// then not all the server sent.
type SourceMCP struct {
	Server            string         `json:"server"`
	Era               string         `json:"era,omitempty"`
	Version           string         `json:"version,omitempty"`
	Tool              string         `json:"tool"`
	Arguments         map[string]any `json:"arguments"`
	BlocksOmitted     int            `json:"blocks_omitted,omitempty"`
	StructuredOmitted bool           `json:"structured_omitted,omitempty"`
	Redactions        int            `json:"redactions,omitempty"`
}

// Extracted is the size and SHA-256 of a text taken out of a source.
type Extracted struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// How a source was fetched, and whether it was saved.
const (
	MethodFile    = "file"
	MethodHTTP    = "http"
	MethodCommand = "command"
	MethodMCP     = "mcp"
	SourceSaved   = "saved"
	SourceFailed  = "failed"
)

// SourceText is a run of whole lines of a saved source's text, from line
// From, counting from 1. A source's text is these frames in order.
type SourceText struct {
	T      FrameType `json:"t,omitempty"`
	Source string    `json:"source"`
	From   int       `json:"from"`
	Text   string    `json:"text"`
}

// Conclusion is one conclusion of a person a source holds, and how far it
// can be trusted. Scope names the inputs of the conversation it is about; a
// conclusion that can change a score needs a scope of its own, never one it
// builds on. Context cites the lines of the source that show it. Support is
// saved when the source holds the evidence, linked when it only names or
// links it, and asserted when it is the person's word alone.
type Conclusion struct {
	T           FrameType    `json:"t,omitempty"`
	ID          string       `json:"id"`
	Source      string       `json:"source"`
	Kind        string       `json:"kind"`
	Statement   string       `json:"statement"`
	Expectation string       `json:"expectation"`
	Scope       []string     `json:"scope"`
	Context     []SourceCite `json:"context,omitempty"`
	BuildsOn    []string     `json:"builds_on,omitempty"`
	Support     string       `json:"support"`
	Author      string       `json:"author,omitempty"`
	Revision    string       `json:"revision,omitempty"`
	Environment string       `json:"environment,omitempty"`
	Conflicts   []string     `json:"conflicts,omitempty"`
	// SupersededBy names a later conclusion of the same source that
	// corrects this one, which then changes no score.
	SupersededBy string `json:"superseded_by,omitempty"`
	Ambiguity    string `json:"ambiguity,omitempty"`
	NotCovered   string `json:"not_covered,omitempty"`
	// Unbound lists the inputs its scope named that the conversation does
	// not hold.
	Unbound []string `json:"unbound,omitempty"`
}

// SourceCite is lines of a saved source, first and last included.
type SourceCite struct {
	Source string `json:"source"`
	Lines  [2]int `json:"lines"`
	Note   string `json:"note,omitempty"`
}

// The kinds, expectations and support of a conclusion.
const (
	ConclusionFinal        = "final"
	ConclusionIntermediate = "intermediate"
	ExpectRequired         = "required"
	ExpectExplanatory      = "explanatory"
	SupportSaved           = "saved"
	SupportLinked          = "linked"
	SupportAsserted        = "asserted"
)

// Divergence is how the conversation compares with one conclusion.
// Reached cites where the conversation reached it. Contradicted names the
// target, the delivery it disproves, and the criteria it shows not met, or
// Whole for the delivered conclusion itself. Asserted marks a verdict of
// absence, such as not observed, that the record cannot show was read in
// full: it is the evaluator's judgment, and Given says how much of the
// judged work the evaluator was given.
type Divergence struct {
	T          FrameType `json:"t,omitempty"`
	Conclusion string    `json:"conclusion"`
	Verdict    string    `json:"verdict"`
	Target     string    `json:"target,omitempty"`
	Event      string    `json:"event,omitempty"`
	Criteria   []string  `json:"criteria,omitempty"`
	Whole      bool      `json:"whole,omitempty"`
	Asserted   bool      `json:"asserted,omitempty"`
	Given      *Given    `json:"given,omitempty"`
	Note       string    `json:"note,omitempty"`
	Cite       []Cite    `json:"cite"`
}

// Given is how much of an absence claim's scope the evaluator was given: the
// judged work for a verdict of not observed or a goal nothing worked on, and
// the span of one stream from where a question or an assumption arose to
// the step the finding is at. Counts is absent when it could not be said,
// and Reason then says why. It reports what the evaluator was given, never
// that the claim holds.
type Given struct {
	Scope  string       `json:"scope"`
	Counts *GivenCounts `json:"counts,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

// GivenCounts counts the steps of a scope three ways that add up to Steps:
// those the evaluator asked for itself and was answered, those an answer or
// the prompt only named in a row, and those it was given neither way.
// Unavailable counts, among all of them, the steps some of whose evidence no
// one can be given, as a part too large to land or withheld.
type GivenCounts struct {
	Steps       int `json:"steps"`
	Opened      int `json:"opened"`
	Listed      int `json:"listed"`
	None        int `json:"none"`
	Unavailable int `json:"unavailable"`
}

// The scopes a Given is over.
const (
	GivenWindow = "window"
	GivenSpan   = "span"
)

// The verdicts of a divergence.
const (
	VerdictReached       = "reached"
	VerdictContradicted  = "contradicted"
	VerdictNotObserved   = "not_observed"
	VerdictUnknown       = "unknown"
	VerdictNotApplicable = "not_applicable"
)

// Verdicts lists every verdict of a divergence.
var Verdicts = []string{VerdictReached, VerdictContradicted, VerdictNotObserved, VerdictUnknown, VerdictNotApplicable}

// Rollback is a delivered result a source says was undone outside the
// conversation, such as a release rolled back. It acts as an undoing of the
// delivery it names, for the outcome and the final score: of the criteria
// it names, or of the whole result.
type Rollback struct {
	T          FrameType `json:"t,omitempty"`
	Conclusion string    `json:"conclusion"`
	Target     string    `json:"target"`
	Event      string    `json:"event"`
	Criteria   []string  `json:"criteria,omitempty"`
	Note       string    `json:"note,omitempty"`
	Cite       []Cite    `json:"cite"`
}

// The causes a divergence finding can name, each needing its own evidence:
// a tool result that misled, a result read wrong, a sub-task's untrue
// report used, context the agent could not have had, context it had and
// did not use, an ambiguity it acted on without asking, a premise it never
// checked; or none the record supports.
const (
	CauseMisleadingResult     = "misleading_tool_result"
	CauseMisread              = "wrong_interpretation"
	CauseMisledBySubtask      = "misled_by_subtask"
	CauseMissingContext       = "missing_context"
	CauseContextNotUsed       = "context_not_used"
	CauseQuestionNotAsked     = "question_not_asked"
	CauseAssumptionNotChecked = "assumption_not_checked"
	CauseUnavailable          = "unavailable"
)

// FindingCauses lists every cause a divergence finding can name.
var FindingCauses = []string{CauseMisleadingResult, CauseMisread, CauseMisledBySubtask, CauseMissingContext, CauseContextNotUsed,
	CauseQuestionNotAsked, CauseAssumptionNotChecked, CauseUnavailable}

// LabelFile names the label file a regroup read: the person's goals, in
// their own words.
type LabelFile struct {
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Version int    `json:"version"`
}

// LabelGoal is one of the person's goals, by an id such as P2 in the order
// the file gives them: its words, the judge's targets that serve it, its
// main target, the judge's goals those targets are in, and how the judge's
// grouping compares with it. Worked says the conversation worked on it,
// shown by the person's input it cites. A goal with no target is not
// scored.
type LabelGoal struct {
	T         FrameType `json:"t,omitempty"`
	ID        string    `json:"id"`
	Statement string    `json:"statement"`
	Members   []string  `json:"members"`
	Main      string    `json:"main,omitempty"`
	Judge     []string  `json:"judge"`
	Class     string    `json:"class"`
	Worked    bool      `json:"worked,omitempty"`
	// Asserted marks a goal classed absent: that nothing worked on it rests
	// on reading all of where it would show, so it is the evaluator's
	// judgment, and Given says how much of the judged work it was given.
	Asserted bool   `json:"asserted,omitempty"`
	Given    *Given `json:"given,omitempty"`
	Note     string `json:"note,omitempty"`
	Cite     []Cite `json:"cite"`
}

// Placement is where a regroup put one of the judge's targets: the person's
// goal it serves, shown by the person's input it cites, or none when it
// serves none of them. Unknown marks a target the answer left out, or put
// in a goal with nothing that shows it.
type Placement struct {
	T       FrameType `json:"t,omitempty"`
	Target  string    `json:"target"`
	Goal    string    `json:"goal,omitempty"`
	Unknown bool      `json:"unknown,omitempty"`
	Note    string    `json:"note,omitempty"`
	Cite    []Cite    `json:"cite"`
}

// The classes of one of the person's goals: its targets are in one goal of
// the judge, which holds no other goal's; in several of the judge's goals;
// in one goal of the judge that also holds another goal's targets; it has
// none, though the conversation worked on it; it has none, and nothing
// worked on it; or the regroup cannot tell.
const (
	CompareAgree   = "agree"
	CompareSplit   = "split"
	CompareMerged  = "merged"
	CompareMissed  = "missed"
	CompareAbsent  = "absent"
	CompareUnknown = "unknown"
)

// Compares lists every class of a person's goal, most severe first.
var Compares = []string{CompareMissed, CompareSplit, CompareMerged, CompareUnknown, CompareAbsent, CompareAgree}

// Agreement counts the person's goals by class, and lists the targets that
// serve none of them, Extra, and those the regroup could not place,
// Unknown.
type Agreement struct {
	T       FrameType      `json:"t,omitempty"`
	Goals   int            `json:"goals"`
	Classes map[string]int `json:"classes"`
	Extra   []string       `json:"extra,omitempty"`
	Unknown []string       `json:"unknown,omitempty"`
}

// LabelText is a run of the label file a regroup was made from, as the
// person gave it, cut as a source's text is: the frames in order are the
// file, so the comparison can be made again after the file changed or was
// removed.
type LabelText struct {
	T    FrameType `json:"t,omitempty"`
	From int       `json:"from"`
	Text string    `json:"text"`
}

// Finding is a problem a stage observed, anchored at the step or input where
// it happened. Its explanation and improvement are proposals: a finding
// never changes a level or a cost. An achievement holds its own; the
// efficiency stage writes each as a frame of its own, naming its target.
type Finding struct {
	T      FrameType `json:"t,omitempty"`
	Target string    `json:"target,omitempty"`
	// Source is the stage that recorded it, or asz, which records one at
	// each criterion not met, each contradiction and undoing of a standing
	// delivery, each correction and each round of rework.
	Source string `json:"source,omitempty"`
	// Conclusion and Cause are a divergence finding's: the conclusion the
	// conversation did not reach, and why, as the evidence allows.
	Conclusion string `json:"conclusion,omitempty"`
	Cause      string `json:"cause,omitempty"`
	// Asserted marks a cause that rests on something not happening, which
	// the record cannot show was looked for everywhere: the evaluator's
	// judgment. From is the step where the question or the assumption
	// arose, and Given how much of the span from it to At the evaluator was
	// given.
	Asserted    bool   `json:"asserted,omitempty"`
	From        string `json:"from,omitempty"`
	Given       *Given `json:"given,omitempty"`
	At          string `json:"at"`
	Ref         *Ref   `json:"ref,omitempty"`
	Problem     string `json:"problem"`
	Explanation string `json:"explanation,omitempty"`
	Improvement string `json:"improvement,omitempty"`
	Cite        []Cite `json:"cite"`
}

// Account is what was done for a target, as a reader asks it: for a
// question, what was checked and what was answered; for a change to another
// system, which operations were used and what they changed.
type Account struct {
	Answer *Answer     `json:"answer,omitempty"`
	Files  []FileGroup `json:"files,omitempty"`
	Remote []Remote    `json:"remote,omitempty"`
}

// FileGroup is file operations of a target grouped by what they were for:
// the area they touched, such as a package, the category, such as code,
// tests or documents, and what the group changed. The operations are named
// by id.
type FileGroup struct {
	Area       string   `json:"area"`
	Category   string   `json:"category"`
	Summary    string   `json:"summary"`
	Operations []string `json:"operations"`
}

// Answer is how a question was answered: what was asked, what was checked or
// tried, what was ruled out, and the answer given.
type Answer struct {
	Asked    Said    `json:"asked"`
	Checked  []Tried `json:"checked,omitempty"`
	RuledOut []Tried `json:"ruled_out,omitempty"`
	Answered Said    `json:"answered"`
}

// Said is something stated, with where it was said.
type Said struct {
	Text string `json:"text"`
	Cite []Cite `json:"cite,omitempty"`
}

// Tried is one check: what was checked or ruled out, what it found, and how
// far that holds.
type Tried struct {
	What   string `json:"what"`
	Found  string `json:"found"`
	Limits string `json:"limits,omitempty"`
	Cite   []Cite `json:"cite"`
}

// Remote is one operation on another system: the system, the operation and
// the tool or API it went through, the request and the response, and what
// later confirmed the change. A call shows neither that a change was
// accepted nor that it was completed.
type Remote struct {
	System    string `json:"system"`
	Operation string `json:"operation"`
	Via       string `json:"via"`
	Request   Said   `json:"request"`
	Response  *Said  `json:"response,omitempty"`
	Confirmed *Said  `json:"confirmed,omitempty"`
}

// Event is one delivery of a target's result, or its undoing. Its id, such
// as t1.e2, is given by asz in recorded order.
type Event struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Criteria are the criteria an undoing took back; none means the whole
	// delivered result.
	Criteria []string `json:"criteria,omitempty"`
	// Replaces are the earlier deliveries a delivery again corrects; none
	// means every one before it.
	Replaces []string `json:"replaces,omitempty"`
	// Undoes are the deliveries an undoing took back; none means every one
	// standing then.
	Undoes []string `json:"undoes,omitempty"`
	// Artifact is what the event concerns: an answer, a revision such as a
	// commit or a pull request, a file, or a change to a remote system.
	Artifact string `json:"artifact"`
	Text     string `json:"text"`
	Cite     []Cite `json:"cite"`
}

// The kinds of event.
const (
	EventDelivered      = "delivered"
	EventUndone         = "undone"
	EventDeliveredAgain = "delivered_again"
)

// EventKinds lists every kind of event.
var EventKinds = []string{EventDelivered, EventUndone, EventDeliveredAgain}

// The artifacts an event concerns.
const (
	ArtifactAnswer   = "answer"
	ArtifactRevision = "revision"
	ArtifactFile     = "file"
	ArtifactRemote   = "remote"
)

// Artifacts lists every artifact an event can concern.
var Artifacts = []string{ArtifactAnswer, ArtifactRevision, ArtifactFile, ArtifactRemote}

// Rework is one round of redoing a target's work: an earlier attempt, what
// showed it had to be redone, and the redo. Its id, such as t1.r1, is given
// by asz in recorded order.
type Rework struct {
	ID      string `json:"id"`
	Attempt []Cite `json:"attempt"`
	// Trigger says what showed the attempt had to be redone, and
	// TriggerCite where.
	Trigger     string `json:"trigger"`
	TriggerCite []Cite `json:"trigger_cite"`
	Cause       string `json:"cause"`
	Redo        []Cite `json:"redo"`
	// From and To bound the round: the step that triggered it and the last
	// step of the redo.
	From     string `json:"from"`
	To       string `json:"to"`
	Finished bool   `json:"finished"`
	Note     string `json:"note,omitempty"`
}

// What can trigger a round of rework: the person's input, a check such as a
// test or a build that failed, or another tool result.
const (
	TriggerPerson       = "person"
	TriggerVerification = "verification"
	TriggerTool         = "tool"
)

// Triggers lists every trigger of a round of rework.
var Triggers = []string{TriggerPerson, TriggerVerification, TriggerTool}

// The causes of a round of rework: the agent's own error, a requirement the
// person changed, or a cause the record does not show.
const (
	ReworkAgentError        = "agent_error"
	ReworkRequirementChange = "requirement_change"
	ReworkUnknown           = "unknown"
)

// ReworkCauses lists every cause of a round of rework.
var ReworkCauses = []string{ReworkAgentError, ReworkRequirementChange, ReworkUnknown}

// Contradiction is evidence that a delivered result is wrong. It disproves
// the delivered conclusion itself, such as the answer or the cause named, or
// shows some criteria not met.
type Contradiction struct {
	// Event is the delivery it concerns, by id.
	Event      string   `json:"event"`
	Conclusion bool     `json:"conclusion,omitempty"`
	Criteria   []string `json:"criteria,omitempty"`
	Text       string   `json:"text"`
	Cite       []Cite   `json:"cite"`
}

// Check is one criterion or one claim, and what the evidence says of it. ID
// names the criterion a check answers; a claim has none.
type Check struct {
	ID    string `json:"id,omitempty"`
	Text  string `json:"text"`
	State string `json:"state"`
	Cite  []Cite `json:"cite,omitempty"`
	// Reason says why the state holds. When asz changed the state, it is
	// asz's reason, and GivenReason keeps the evaluator's own.
	Reason       string        `json:"reason,omitempty"`
	GivenReason  string        `json:"given_reason,omitempty"`
	Observations []Observation `json:"observations,omitempty"`
}

// Observation is one thing a cited step shows: an excerpt of it, exactly as
// recorded, what the record says of the step, and what the evaluator reads
// in it. The status is a fact of the record; the interpretation is not, as a
// query that succeeded can report a run that failed.
type Observation struct {
	Node           string `json:"node"`
	Ref            *Ref   `json:"ref,omitempty"`
	Excerpt        string `json:"excerpt,omitempty"`
	Status         string `json:"status,omitempty"`
	Interpretation string `json:"interpretation"`
}

// What the record says of an observed step: a tool step that failed, that
// returned, or that never returned; an error of the model API; an interrupt.
const (
	ObservedFailed      = "failed"
	ObservedReturned    = "returned"
	ObservedNoResult    = "no_result"
	ObservedError       = "error"
	ObservedInterrupted = "interrupted"
)

// The levels of achievement.
const (
	LevelAchieved     = "achieved"
	LevelPartly       = "partly"
	LevelNotAchieved  = "not_achieved"
	LevelCannotVerify = "cannot_verify"
)

// Levels lists every level.
var Levels = []string{LevelAchieved, LevelPartly, LevelNotAchieved, LevelCannotVerify}

// The bases a level rests on.
const (
	BasisAccepted  = "person_accepted"
	BasisObserved  = "observed_result"
	BasisDelivered = "delivered_answer"
	BasisClaimOnly = "agent_claim_only"
	BasisNone      = "no_evidence"
)

// Bases lists every basis.
var Bases = []string{BasisAccepted, BasisObserved, BasisDelivered, BasisClaimOnly, BasisNone}

// The states of a criterion or a claim.
const (
	CheckMet          = "met"
	CheckNotMet       = "not_met"
	CheckSupported    = "supported"
	CheckContradicted = "contradicted"
	CheckNoEvidence   = "no_evidence"
	// CheckNotCaptured is evidence the runtime or the collector did not
	// record, so it cannot be looked at.
	CheckNotCaptured = "not_captured"
	CheckUnknown     = "unknown"
)

// Checks lists every state of a check.
var Checks = []string{CheckMet, CheckNotMet, CheckSupported, CheckContradicted, CheckNoEvidence, CheckNotCaptured, CheckUnknown}

// Label is what one piece of a target's work was: a whole talk, or the part
// of it from one step on.
type Label struct {
	T      FrameType `json:"t,omitempty"`
	Target string    `json:"target"`
	Talk   string    `json:"talk"`
	// From is the first step of the piece, when the label covers only part
	// of the talk. Absent means the whole talk.
	From  string `json:"from,omitempty"`
	Label string `json:"label"`
	// Subtask names the sub-task whose untrue report the work acted on, for
	// the label WorkMisled.
	Subtask string `json:"subtask,omitempty"`
	Note    string `json:"note,omitempty"`
	Cite    []Cite `json:"cite"`
}

// Whose doing avoidable work was. CauseMain is the main agent's: its own
// work, and what followed from a task it gave wrong or gave again. A
// sub-task's id names that sub-agent the same way. CauseUnknown is work done
// on a sub-task's untrue report when the record does not show whether the
// task it was given was right, so neither can be charged.
const (
	CauseMain    = "main"
	CauseUnknown = "unknown"
)

// The labels of work. Useful work, exploration and verification are not
// waste. Rework after the person changed the requirement is charged to the
// person. NotReviewed is work no evaluator looked at. Misdirected is a
// sub-task's own work when the task it was given was wrong; Misled is the
// asking agent's work done on a sub-task's untrue report.
const (
	WorkUseful       = "useful"
	WorkExploration  = "exploration"
	WorkVerification = "verification"
	WorkAvoidable    = "avoidable_attempt"
	WorkRepeated     = "repeated"
	WorkRedundant    = "redundant_delegation"
	WorkRepair       = "repair_own_mistake"
	WorkRework       = "rework_requirement_change"
	WorkMisdirected  = "misdirected_delegation"
	WorkMisled       = "misled_by_subtask"
	WorkNotReviewed  = "not_reviewed"
)

// Works lists every label of work.
var Works = []string{WorkUseful, WorkExploration, WorkVerification, WorkAvoidable, WorkRepeated, WorkRedundant, WorkRepair,
	WorkRework, WorkMisdirected, WorkMisled, WorkNotReviewed}

// TalkWorks are the labels efficiency gives a talk's work: a talk is not a
// sub-task given the wrong task, and work no label covers is not reviewed
// without a label saying so.
var TalkWorks = []string{WorkUseful, WorkExploration, WorkVerification, WorkAvoidable, WorkRepeated, WorkRedundant, WorkRepair,
	WorkRework, WorkMisled}

// SubtaskWorks are the labels a sub-task decision gives its own work. One
// label covers the whole sub-task, so it cannot name which nested sub-task
// misled it.
var SubtaskWorks = []string{WorkUseful, WorkExploration, WorkVerification, WorkAvoidable, WorkRepeated, WorkRedundant, WorkRepair,
	WorkRework, WorkMisdirected, WorkNotReviewed}

// Avoidable says whether a label of work counts as avoidable.
func Avoidable(work string) bool {
	switch work {
	case WorkAvoidable, WorkRepeated, WorkRedundant, WorkRepair, WorkMisdirected, WorkMisled:
		return true
	}
	return false
}

// Subtask is how one sub-task went: whether it did what it was asked,
// whether what it reported back was true to its own work, whether the agent
// that asked used it, whether giving it away was reasonable at the time, and
// what its own cost was. One sub-task is judged once, whatever targets its
// starting talk serves.
type Subtask struct {
	T         FrameType `json:"t,omitempty"`
	ID        string    `json:"id"`
	Parent    string    `json:"parent,omitempty"`
	StartedBy string    `json:"started_by,omitempty"`
	// Outcome takes the levels of an achievement.
	Outcome  string `json:"outcome"`
	Report   string `json:"report"`
	Use      string `json:"use"`
	Dispatch string `json:"dispatch"`
	// Work labels the sub-task's own calls, as efficiency labels a talk's.
	Work string `json:"work"`
	Note string `json:"note,omitempty"`
	Cite []Cite `json:"cite"`
}

// What a sub-task's report back was, against its own work.
const (
	ReportAccurate     = "accurate"
	ReportInaccurate   = "inaccurate"
	ReportUnverifiable = "unverifiable"
)

// Reports lists every value of a sub-task's report.
var Reports = []string{ReportAccurate, ReportInaccurate, ReportUnverifiable}

// Whether the agent that asked used a sub-task's result. Misled is used when
// the report was untrue: the agent acted on what the sub-task got wrong.
const (
	UseUsed    = "used"
	UseUnused  = "unused"
	UseMisled  = "misled"
	UseUnknown = "unknown"
)

// Uses lists every value of a sub-task's use.
var Uses = []string{UseUsed, UseUnused, UseMisled, UseUnknown}

// Whether giving the work away was reasonable when it was given. Misdirected
// is a task wrong when it was given: its scope, its instructions or its
// command, against what the record showed then.
const (
	DispatchReasonable  = "reasonable"
	DispatchRedundant   = "redundant"
	DispatchMisdirected = "misdirected"
	DispatchUnknown     = "unknown"
)

// Dispatches lists every value of a sub-task's dispatch.
var Dispatches = []string{DispatchReasonable, DispatchRedundant, DispatchMisdirected, DispatchUnknown}

// Stage names one stage file of a run, on its close file.
type Stage struct {
	T         FrameType `json:"t,omitempty"`
	Stage     string    `json:"stage"`
	File      string    `json:"file"`
	Digest    string    `json:"digest"`
	Status    string    `json:"status"`
	Evaluator string    `json:"evaluator"`
}

// Outcome is one target's result, on the close file: the verdict and what
// the target cost. Every number is asz's, from the ledger and the labels.
type Outcome struct {
	T         FrameType `json:"t,omitempty"`
	Target    string    `json:"target"`
	Statement string    `json:"statement"`
	Status    string    `json:"status"`
	// Level and Basis are absent when no achievement stage judged the
	// target. Met counts its criteria met, of Criteria.
	Level    string `json:"level,omitempty"`
	Basis    string `json:"basis,omitempty"`
	Met      *int   `json:"met,omitempty"`
	Criteria *int   `json:"criteria,omitempty"`
	// Unknown counts the criteria neither met nor not met: unknown, or in
	// what the record did not capture. Contradicted counts the claims about
	// the outcome the record contradicts.
	Unknown      *int `json:"unknown,omitempty"`
	Contradicted *int `json:"contradicted,omitempty"`
	// Cost is the target's share of the calls, Avoidable the part labeled
	// avoidable, and NotReviewed the part no label covers. The last two are
	// absent when no efficiency stage ran.
	Cost        Amount  `json:"cost"`
	Avoidable   *Amount `json:"avoidable,omitempty"`
	NotReviewed *Amount `json:"not_reviewed,omitempty"`
	// AvoidableBySubtasks is the part of Avoidable sub-agents caused, and
	// AvoidableCauseUnknown the part whose cause the record does not show.
	// The rest is the main agent's.
	AvoidableBySubtasks   *Amount `json:"avoidable_by_subtasks,omitempty"`
	AvoidableCauseUnknown *Amount `json:"avoidable_cause_unknown,omitempty"`
	// Span is the time the target's talks were observed working, with the
	// child agents they started, in milliseconds: the union of their spans,
	// so two talks at once count once. It is not time the agent was busy,
	// since a talk can wait inside itself. AvoidableSpan is the union of the
	// spans of the main stream's avoidable work.
	Span          int64  `json:"span_ms,omitempty"`
	AvoidableSpan *int64 `json:"avoidable_span_ms,omitempty"`
	// Class is the outcome, most severe first: contradicted, reverted,
	// not_achieved, partly, achieved; then cannot_verify; and cancelled or
	// replaced, which are never scored. Absent when no achievement stage
	// judged the target.
	Class string `json:"class,omitempty"`
	// Standing are the deliveries standing at the end, by event id: every
	// delivery, in parts or whole, except those a later delivery again
	// replaced. Their contradictions apply. Undone are the undoings that
	// apply: those that took back a delivery still standing.
	Standing []string `json:"standing,omitempty"`
	Undone   []string `json:"undone,omitempty"`
	// ScoreRecord is the score the record's criteria give, from 0 to 1, as
	// Score.Achievement counts the target. ScoreFinal applies each
	// contradiction of the deliveries standing, and the undoings that apply,
	// once. Both are absent when the target is not scored or cannot be
	// verified.
	ScoreRecord *float64 `json:"score_record,omitempty"`
	ScoreFinal  *float64 `json:"score_final,omitempty"`
	// Rework is what the target's rounds of rework cost, by how their calls
	// were labeled, each call counted once.
	Rework []ReworkCost `json:"rework,omitempty"`
	// Corrections are the person's inputs that corrected the target.
	Corrections []string `json:"corrections,omitempty"`
	// Operations are the file operations of the target's calls, each with
	// the target's share when the call was shared.
	Operations []OperationShare `json:"operations,omitempty"`
	// References are the conclusions of a source whose contradiction or
	// rollback applies to the target's final score.
	References []string `json:"references,omitempty"`
}

// The classes of an outcome.
const (
	ClassContradicted = "contradicted"
	ClassReverted     = "reverted"
	ClassNotAchieved  = LevelNotAchieved
	ClassPartly       = LevelPartly
	ClassAchieved     = LevelAchieved
	ClassCannotVerify = LevelCannotVerify
	ClassCancelled    = TargetCancelled
	ClassReplaced     = TargetReplaced
)

// Classes lists every class of an outcome, most severe first.
var Classes = []string{ClassContradicted, ClassReverted, ClassNotAchieved, ClassPartly, ClassAchieved, ClassCannotVerify, ClassCancelled, ClassReplaced}

// ReworkCost is what a target's rounds of rework cost of one label: their
// calls the stages gave that label, each counted once however many rounds
// hold it, and for avoidable work whose doing it was, as a work group names
// it. Work no stage labeled is not reviewed.
type ReworkCost struct {
	Work  string `json:"work"`
	Cause string `json:"cause,omitempty"`
	Cost  Amount `json:"cost"`
}

// OperationShare is a file operation of a target, by id, and the target's
// share of the call that made it, absent when the call was the target's
// alone.
type OperationShare struct {
	ID    string   `json:"id"`
	Share *float64 `json:"share,omitempty"`
}

// GoalOutcome is how one goal did: its main target at the end, empty when
// undetermined, the most severe class of its targets, the finding recorded
// first in it, the main target's final score, and the mean of its targets'
// final scores. First says nothing of work before it that no stage read.
// FirstOrder says how far the choice of First rests on recorded places:
// empty when every finding had one.
type GoalOutcome struct {
	Goal       string   `json:"goal"`
	Main       string   `json:"main,omitempty"`
	Worst      string   `json:"worst,omitempty"`
	First      *Finding `json:"first,omitempty"`
	FirstOrder string   `json:"first_order,omitempty"`
	ScoreMain  *float64 `json:"score_main,omitempty"`
	ScoreMean  *float64 `json:"score_mean,omitempty"`
}

// How far a goal's first finding rests on recorded places: some findings
// had no place, so First is the first among those with one; or none had
// one, so First is the first written.
const (
	FirstOrderPartial = "partial"
	FirstOrderNone    = "none"
)

// Amount is an amount of tokens and, where every call in it was priced,
// money. A share of a call is counted in it, so tokens are rounded.
type Amount struct {
	Usage sessiondata.Usage `json:"usage"`
	// Money is absent when no call in the amount was priced. Unpriced and
	// Unknown count the calls in it with no price, and with no usage.
	Money    *float64 `json:"money,omitempty"`
	Unpriced int      `json:"unpriced,omitempty"`
	Unknown  int      `json:"unknown,omitempty"`
	// OutputUnknown counts the calls in it that never finished: what they
	// read is in Usage, their output is not.
	OutputUnknown int `json:"output_unknown,omitempty"`
}

// Tokens is every token of an amount, of every type.
func (a Amount) Tokens() int {
	return a.Usage.Input + a.Usage.Output + a.Usage.CacheRead + a.Usage.CacheWrite
}

// Commit is the last line.
type Commit struct {
	T      FrameType      `json:"t"`
	Digest string         `json:"digest"`
	Counts map[string]int `json:"counts"`
}

// Writer builds one stage file in memory. The digest covers every line
// before the commit, so it is known only once the file is complete.
type Writer struct {
	buf    bytes.Buffer
	enc    *json.Encoder
	counts map[string]int
	closed bool
}

// NewWriter starts a stage file.
func NewWriter(h Header) (*Writer, error) {
	h.T, h.Schema = FrameHeader, Schema
	if err := h.Validate(); err != nil {
		return nil, err
	}
	w := &Writer{counts: map[string]int{}}
	w.enc = newEncoder(&w.buf)
	if err := w.enc.Encode(h); err != nil {
		return nil, err
	}
	return w, nil
}

// newEncoder writes a frame as one line. A frame keeps the text it carries as
// it is: no HTML escaping, so a statement quoting code reads the same in every
// reader.
func newEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

// Write appends one body frame. Its `t` is set from its type.
func (w *Writer) Write(frame any) error {
	if w.closed {
		return errors.New("sessionjudge: file already closed")
	}
	t, err := frameType(frame)
	if err != nil {
		return err
	}
	// A frame that fails to encode writes no line, so it is not counted:
	// counted, it would leave the file's counts wrong and the file unread.
	if err := w.enc.Encode(frame); err != nil {
		return err
	}
	w.counts[string(t)]++
	return nil
}

func frameType(frame any) (FrameType, error) {
	switch f := frame.(type) {
	case *Price:
		f.T = FramePrice
	case *Cost:
		f.T = FrameCost
	case *Target:
		f.T = FrameTarget
	case *Input:
		f.T = FrameInput
	case *Goal:
		f.T = FrameGoal
	case *Finding:
		f.T = FrameFinding
	case *Operation:
		f.T = FrameOperation
	case *LabelGoal:
		f.T = FrameLabelGoal
	case *Placement:
		f.T = FramePlacement
	case *Agreement:
		f.T = FrameAgreement
	case *LabelText:
		f.T = FrameLabelText
	case *Source:
		f.T = FrameSource
	case *SourceText:
		f.T = FrameSourceText
	case *Conclusion:
		f.T = FrameConclusion
	case *Divergence:
		f.T = FrameDivergence
	case *Rollback:
		f.T = FrameRollback
	case *Achievement:
		f.T = FrameAchievement
	case *Label:
		f.T = FrameLabel
	case *Subtask:
		f.T = FrameSubtask
	case *Stage:
		f.T = FrameStage
	case *Outcome:
		f.T = FrameOutcome
	default:
		return "", fmt.Errorf("sessionjudge: %T is not a body frame", frame)
	}
	return frameOf(frame), nil
}

// frameOf reads back the type a body frame was given.
func frameOf(frame any) FrameType {
	switch f := frame.(type) {
	case *Price:
		return f.T
	case *Cost:
		return f.T
	case *Target:
		return f.T
	case *Input:
		return f.T
	case *Goal:
		return f.T
	case *Finding:
		return f.T
	case *Operation:
		return f.T
	case *LabelGoal:
		return f.T
	case *Placement:
		return f.T
	case *Agreement:
		return f.T
	case *LabelText:
		return f.T
	case *Source:
		return f.T
	case *SourceText:
		return f.T
	case *Conclusion:
		return f.T
	case *Divergence:
		return f.T
	case *Rollback:
		return f.T
	case *Achievement:
		return f.T
	case *Label:
		return f.T
	case *Subtask:
		return f.T
	case *Stage:
		return f.T
	case *Outcome:
		return f.T
	}
	return ""
}

// Close writes the commit and returns the file's bytes and digest.
func (w *Writer) Close() (data []byte, digest string, err error) {
	if w.closed {
		return nil, "", errors.New("sessionjudge: file already closed")
	}
	sum := sha256.Sum256(w.buf.Bytes())
	digest = hex.EncodeToString(sum[:])
	if err := w.enc.Encode(Commit{T: FrameCommit, Digest: digest, Counts: w.counts}); err != nil {
		return nil, "", err
	}
	w.closed = true
	return w.buf.Bytes(), digest, nil
}

// Validate checks what every header must carry.
func (h *Header) Validate() error {
	switch {
	case h.Schema != Schema:
		return fmt.Errorf("sessionjudge: schema %q, want %q", h.Schema, Schema)
	case h.Conversation == "" || h.Session == "":
		return errors.New("sessionjudge: header has no conversation or session")
	case h.Run < 1:
		return fmt.Errorf("sessionjudge: run %d; runs count from 1", h.Run)
	case !known(h.Stage, Stages) && h.Stage != StageRegroup:
		return fmt.Errorf("sessionjudge: unknown stage %q", h.Stage)
	case h.Head.Round == 0 || h.Head.Digest == "":
		return errors.New("sessionjudge: header names no head round")
	case h.Evaluator.Name == "" || h.Evaluator.Kind == "":
		return errors.New("sessionjudge: header names no evaluator")
	case h.Status != StatusComplete && h.Status != StatusPartial && h.Status != StatusFailed:
		return fmt.Errorf("sessionjudge: unknown status %q", h.Status)
	case h.Started == "" || h.Finished == "":
		return errors.New("sessionjudge: header has no start or finish time")
	}
	return nil
}

func known(s string, list []string) bool {
	for _, v := range list {
		if s == v {
			return true
		}
	}
	return false
}

// File is a stage file read and verified.
type File struct {
	Header       Header
	Prices       []Price
	Costs        []Cost
	Targets      []Target
	Inputs       []Input
	Goals        []Goal
	Findings     []Finding
	Operations   []Operation
	LabelGoals   []LabelGoal
	Placements   []Placement
	Agreements   []Agreement
	LabelTexts   []LabelText
	Sources      []Source
	SourceTexts  []SourceText
	Conclusions  []Conclusion
	Divergences  []Divergence
	Rollbacks    []Rollback
	Achievements []Achievement
	Labels       []Label
	Subtasks     []Subtask
	Stages       []Stage
	Outcomes     []Outcome
	Commit       Commit
}

// Read parses a stage file and verifies it: a header first, known frames,
// one commit last, a digest that matches every line before it, and counts
// that match the frames. A reader that accepts what a correct writer never
// writes can disagree with another reader about what the file said.
func Read(r io.Reader) (*File, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	var (
		out       File
		hashed    bytes.Buffer
		sawCommit bool
		line      int
	)
	counts := map[string]int{}
	for {
		raw, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(raw) > 0 {
				return nil, errors.New("sessionjudge: last line has no newline")
			}
			break
		}
		if err != nil {
			return nil, err
		}
		raw = raw[:len(raw)-1]
		line++
		var probe struct {
			T FrameType `json:"t"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("sessionjudge: line %d: undecodable frame: %w", line, err)
		}
		if sawCommit {
			return nil, fmt.Errorf("sessionjudge: line %d: content after the commit", line)
		}
		if (line == 1) != (probe.T == FrameHeader) {
			return nil, fmt.Errorf("sessionjudge: line %d: a header must be the first line and only there", line)
		}
		var derr error
		switch probe.T {
		case FrameHeader:
			if derr = decode(raw, &out.Header); derr == nil {
				derr = out.Header.Validate()
			}
		case FramePrice:
			out.Prices = append(out.Prices, Price{})
			derr = decode(raw, &out.Prices[len(out.Prices)-1])
		case FrameCost:
			out.Costs = append(out.Costs, Cost{})
			derr = decode(raw, &out.Costs[len(out.Costs)-1])
		case FrameTarget:
			out.Targets = append(out.Targets, Target{})
			derr = decode(raw, &out.Targets[len(out.Targets)-1])
		case FrameInput:
			out.Inputs = append(out.Inputs, Input{})
			derr = decode(raw, &out.Inputs[len(out.Inputs)-1])
		case FrameGoal:
			out.Goals = append(out.Goals, Goal{})
			derr = decode(raw, &out.Goals[len(out.Goals)-1])
		case FrameFinding:
			out.Findings = append(out.Findings, Finding{})
			derr = decode(raw, &out.Findings[len(out.Findings)-1])
		case FrameOperation:
			out.Operations = append(out.Operations, Operation{})
			derr = decode(raw, &out.Operations[len(out.Operations)-1])
		case FrameLabelGoal:
			out.LabelGoals = append(out.LabelGoals, LabelGoal{})
			derr = decode(raw, &out.LabelGoals[len(out.LabelGoals)-1])
		case FramePlacement:
			out.Placements = append(out.Placements, Placement{})
			derr = decode(raw, &out.Placements[len(out.Placements)-1])
		case FrameAgreement:
			out.Agreements = append(out.Agreements, Agreement{})
			derr = decode(raw, &out.Agreements[len(out.Agreements)-1])
		case FrameLabelText:
			out.LabelTexts = append(out.LabelTexts, LabelText{})
			derr = decode(raw, &out.LabelTexts[len(out.LabelTexts)-1])
		case FrameSource:
			out.Sources = append(out.Sources, Source{})
			derr = decode(raw, &out.Sources[len(out.Sources)-1])
		case FrameSourceText:
			out.SourceTexts = append(out.SourceTexts, SourceText{})
			derr = decode(raw, &out.SourceTexts[len(out.SourceTexts)-1])
		case FrameConclusion:
			out.Conclusions = append(out.Conclusions, Conclusion{})
			derr = decode(raw, &out.Conclusions[len(out.Conclusions)-1])
		case FrameDivergence:
			out.Divergences = append(out.Divergences, Divergence{})
			derr = decode(raw, &out.Divergences[len(out.Divergences)-1])
		case FrameRollback:
			out.Rollbacks = append(out.Rollbacks, Rollback{})
			derr = decode(raw, &out.Rollbacks[len(out.Rollbacks)-1])
		case FrameAchievement:
			out.Achievements = append(out.Achievements, Achievement{})
			derr = decode(raw, &out.Achievements[len(out.Achievements)-1])
		case FrameLabel:
			out.Labels = append(out.Labels, Label{})
			derr = decode(raw, &out.Labels[len(out.Labels)-1])
		case FrameSubtask:
			out.Subtasks = append(out.Subtasks, Subtask{})
			derr = decode(raw, &out.Subtasks[len(out.Subtasks)-1])
		case FrameStage:
			out.Stages = append(out.Stages, Stage{})
			if derr = decode(raw, &out.Stages[len(out.Stages)-1]); derr == nil && !isDigest(out.Stages[len(out.Stages)-1].Digest) {
				derr = fmt.Errorf("stage frame digest %q is not a SHA-256 in lowercase hex", out.Stages[len(out.Stages)-1].Digest)
			}
		case FrameOutcome:
			out.Outcomes = append(out.Outcomes, Outcome{})
			derr = decode(raw, &out.Outcomes[len(out.Outcomes)-1])
		case FrameCommit:
			if derr = decode(raw, &out.Commit); derr == nil {
				sum := sha256.Sum256(hashed.Bytes())
				if got := hex.EncodeToString(sum[:]); got != out.Commit.Digest {
					derr = fmt.Errorf("digest %s does not match the file's %s", out.Commit.Digest, got)
				}
			}
			sawCommit = true
		default:
			return nil, fmt.Errorf("sessionjudge: line %d: unknown frame %q", line, probe.T)
		}
		if derr != nil {
			return nil, fmt.Errorf("sessionjudge: line %d: %w", line, derr)
		}
		if probe.T != FrameHeader && probe.T != FrameCommit {
			counts[string(probe.T)]++
		}
		hashed.Write(raw)
		hashed.WriteByte('\n')
	}
	if line == 0 {
		return nil, errors.New("sessionjudge: empty file")
	}
	if !sawCommit {
		return nil, errors.New("sessionjudge: no commit; the file is truncated")
	}
	if len(counts) != len(out.Commit.Counts) {
		return nil, fmt.Errorf("sessionjudge: counts %v do not match the frames %v", out.Commit.Counts, counts)
	}
	for k, n := range counts {
		if out.Commit.Counts[k] != n {
			return nil, fmt.Errorf("sessionjudge: counts %v do not match the frames %v", out.Commit.Counts, counts)
		}
	}
	return &out, nil
}

// isDigest says a string is a SHA-256 in lowercase hex, as every digest the
// format names is.
func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// decode reads one frame. A key the format does not define is passed
// over, as a round's reader passes over one, so a later writer can add keys
// without stopping an earlier reader.
func decode(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
