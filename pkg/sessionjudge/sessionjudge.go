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
	// StageClose ends a run: a stage frame for every stage written, and the
	// outcome and cost of each target. A run is sent once it is closed.
	StageClose = "close"
)

// Stages lists the stages in run order.
var Stages = []string{StageLedger, StageTargets, StageAchievement, StageSubtasks, StageEfficiency, StageClose}

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
	Talks         *int `json:"talks,omitempty"`
	TalksLabeled  *int `json:"talks_labeled,omitempty"`
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
	Score   *Score  `json:"score,omitempty"`
	Judging *Amount `json:"judging,omitempty"`
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
	Judged      int      `json:"judged"`
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

// Input is what one input from outside the agent did to the targets.
type Input struct {
	T       FrameType `json:"t,omitempty"`
	Node    string    `json:"node"`
	Talk    string    `json:"talk"`
	Actions []Action  `json:"actions"`
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
	Note     string  `json:"note,omitempty"`
	Cite     []Cite  `json:"cite"`
}

// Check is one criterion or one claim, and what the evidence says of it. ID
// names the criterion a check answers; a claim has none.
type Check struct {
	ID    string `json:"id,omitempty"`
	Text  string `json:"text"`
	State string `json:"state"`
	Cite  []Cite `json:"cite,omitempty"`
}

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
}

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
	case !known(h.Stage, Stages):
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
