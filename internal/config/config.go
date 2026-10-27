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

// Package config loads SkyWalking AI Sessionizer configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Config is the top-level configuration document.
type Config struct {
	Storage  Storage   `yaml:"storage"`
	Adapters []Adapter `yaml:"adapters"`
	Parse    Parse     `yaml:"parse"`
	Metrics  Metrics   `yaml:"metrics"`
	Export   Export    `yaml:"export"`
	View     View      `yaml:"view"`
	Judge    Judge     `yaml:"judge"`
	// RemoteSnapshotTTL is how long a snapshot asz pull made, and the judge
	// runs in it, stay after publication. The newest one expires too.
	RemoteSnapshotTTL string `yaml:"remote_snapshot_ttl"`
	// Remotes are the asz servers asz pull copies conversations from.
	Remotes []Remote `yaml:"remotes"`
	// RemoteServer is what this instance needs to answer pulls.
	RemoteServer RemoteServer `yaml:"remote_server"`
}

// Judge configures asz judge and asz measure. A judge run happens only when a
// person runs asz judge: nothing in collect, server or push starts one.
type Judge struct {
	// Pricing is the optional price file. Empty prices nothing, and the
	// tokens still count.
	Pricing string `yaml:"pricing"`
	// Hide lists the flags whose records the evaluators are never shown,
	// as view.hide lists them for the page. A provider body is then never
	// shown either, since a request carries both again.
	Hide []string `yaml:"hide"`
	// Timeout bounds one evaluator call, as a duration such as 30m. A stage
	// cut into calls runs several, and a failed call is tried once more.
	Timeout string `yaml:"timeout"`
	// ReadBytes bounds what one evaluator call reads through the
	// navigation, in bytes of answers. Zero is no bound.
	ReadBytes int `yaml:"read_bytes"`
	// Mode is how a stage is given to its evaluator: single, one call;
	// stream, cut into chunks; auto, one call when the rows it is given fit
	// SingleBytes and chunks of ChunkBytes beyond. Concurrency bounds the
	// chunks run at once.
	Mode        string `yaml:"mode"`
	SingleBytes int    `yaml:"single_bytes"`
	ChunkBytes  int    `yaml:"chunk_bytes"`
	Concurrency int    `yaml:"concurrency"`
	// StateBytes bounds what a call is given of the targets found so far
	// in a targets scan; the targets tool gives the rest whole.
	StateBytes int `yaml:"state_bytes"`
	// Progress is how often a running stage says what it has consumed so
	// far, as a duration such as 30s; 0 says it only when the stage ends.
	Progress string `yaml:"progress"`
	// Evaluators are the models and agent CLIs a stage can be given to, by
	// name.
	Evaluators map[string]Evaluator `yaml:"evaluators"`
	// Plan is what asz judge runs when it is not told one stage: the
	// stages after the ledger, in order, each with its evaluator.
	Plan []JudgeStage `yaml:"plan"`
	// References are how the sources a run is judged against are fetched.
	References References `yaml:"references"`
}

// References are how asz fetches a source a run is judged against. A file
// and a plain http or https URL need no setting. A source that needs a
// login, such as a ticket system, goes through a command.
type References struct {
	// MaxBytes bounds a saved copy. A larger source is saved cut, and its
	// manifest says so.
	MaxBytes int `yaml:"max_bytes"`
	// Timeout bounds one fetch, as a duration such as 1m.
	Timeout string `yaml:"timeout"`
	// Fetch are the commands that fetch sources, the first whose prefix a
	// source starts with taking it.
	Fetch []Fetcher `yaml:"fetch"`
}

// Fetcher is a command that fetches the sources that start with a prefix.
// It is given the source as its last argument and writes the content on
// its standard output. It never sees the conversation.
type Fetcher struct {
	Prefix  string   `yaml:"prefix"`
	Command []string `yaml:"command"`
	// ContentType is what the command writes, text/plain when empty.
	ContentType string `yaml:"content_type"`
}

// Evaluator is one model or agent CLI a stage is given to.
type Evaluator struct {
	// Kind is cli, an agent CLI asz runs, or api, a provider's API asz
	// calls itself.
	Kind string `yaml:"kind"`
	// Program is claude, codex or kiro-cli for a CLI, and anthropic,
	// openai or bedrock for an API. openai is any endpoint that speaks the
	// OpenAI chat completions API; bedrock is Amazon Bedrock's Converse
	// API.
	Program string `yaml:"program"`
	// Command is the CLI's path, when it is not the program's name on PATH.
	Command string `yaml:"command"`
	// Args are added to the CLI's command line.
	Args []string `yaml:"args"`
	// Model is the model to ask for. Empty leaves the CLI's own default.
	Model string `yaml:"model"`
	// Endpoint is the API's base URL, and APIKeyEnv the environment
	// variable holding its key.
	Endpoint  string `yaml:"endpoint"`
	APIKeyEnv string `yaml:"api_key_env"`
	// Region and Profile are Bedrock's: the AWS region, us-east-1 when
	// empty, and the AWS CLI profile whose credentials sign the requests,
	// when they are not in the environment. A Bedrock evaluator with
	// APIKeyEnv sends that Bedrock API key instead and signs nothing.
	Region  string `yaml:"region"`
	Profile string `yaml:"profile"`
	// MaxTurns bounds the turns an evaluator takes in one call: model
	// calls for an API, agent turns for a CLI that has the limit.
	MaxTurns int `yaml:"max_turns"`
	// MaxTokens bounds what one model call of an API evaluator may answer.
	// 8000 when unset.
	MaxTokens int `yaml:"max_tokens"`
}

// JudgeStage is one stage of a plan.
type JudgeStage struct {
	Stage     string `yaml:"stage"`
	Evaluator string `yaml:"evaluator"`
}

// check refuses judge settings a run could not use.
func (j Judge) check() error {
	switch j.Mode {
	case "", "auto", "single", "stream":
	default:
		return fmt.Errorf("config: judge.mode %q is not auto, single or stream", j.Mode)
	}
	if j.Concurrency < 1 {
		return fmt.Errorf("config: judge.concurrency %d must be at least 1", j.Concurrency)
	}
	for name, n := range map[string]int{"read_bytes": j.ReadBytes, "single_bytes": j.SingleBytes, "chunk_bytes": j.ChunkBytes, "state_bytes": j.StateBytes} {
		if n < 0 {
			return fmt.Errorf("config: judge.%s %d must not be negative", name, n)
		}
	}
	if j.Progress != "" {
		if d, err := time.ParseDuration(j.Progress); err != nil || d < 0 {
			return fmt.Errorf("config: judge.progress %q is not a duration", j.Progress)
		}
	}
	r := j.References
	if r.MaxBytes < 0 {
		return fmt.Errorf("config: judge.references.max_bytes %d must not be negative", r.MaxBytes)
	}
	if r.Timeout != "" {
		if d, err := time.ParseDuration(r.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("config: judge.references.timeout %q is not a positive duration", r.Timeout)
		}
	}
	for i, f := range r.Fetch {
		if f.Prefix == "" || len(f.Command) == 0 || f.Command[0] == "" {
			return fmt.Errorf("config: judge.references.fetch[%d] needs a prefix and a command", i)
		}
	}
	return nil
}

// TimeoutDuration is the timeout of one evaluator call: 30m when unset.
func (j Judge) TimeoutDuration() (time.Duration, error) {
	if j.Timeout == "" {
		return 30 * time.Minute, nil
	}
	d, err := time.ParseDuration(j.Timeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("judge.timeout: %q is not a positive duration", j.Timeout)
	}
	return d, nil
}

// View configures what asz view and asz server serve.
type View struct {
	// Hide lists the flags whose steps the page withholds from every
	// reader: system_prompt, the system prompt a runtime sent, and
	// tool_schemas, the schemas of the tools it advertised. A withheld step
	// keeps its place, its flags and its size, loses its text, and says so
	// with the state omitted. The provider bodies go with it, since a
	// request carries both again and a body is served whole or not at all.
	// asz knows nothing about who is reading: one instance per audience,
	// each behind the deployment's own authentication, is how two audiences
	// are served, unless HideOverride lets a host decide per reader.
	// Empty hides nothing.
	Hide []string `yaml:"hide"`
	// HideOverride lets a request's hide parameter replace Hide for that
	// request, so a host that serves the API through its own route decides
	// what each reader is kept from. It is off by default: Hide is then the
	// whole answer, and a request that carries the parameter is refused,
	// since serving it anything else would not be what it asked for.
	HideOverride bool `yaml:"hide_override"`
}

// Metrics configures what asz derives from the landed files. It is one
// switch for the root, whichever adapters landed the files: every metric
// asz defines is derived from every landed file it applies to, the token
// usage of a Claude Code conversation and the calls the plugin saw reach an
// MCP server alike. export.otlp.metrics says whether a push sends them.
type Metrics struct {
	// Enabled derives the metrics. On unless set to false.
	Enabled *bool `yaml:"enabled"`
	// Lookback bounds the first derivation over a root, the one pass that
	// runs before the root has any metrics state: a minute older than this
	// is not derived, so a new deployment over months of history does not
	// send all of it at once. Every later pass derives each new file whole.
	// A duration such as 72h or 3d; empty means 72h; 0 or none means
	// everything.
	Lookback string `yaml:"lookback"`
}

// DefaultLookback is three days. A new deployment usually starts cold over a
// source that already holds history, and three days is enough to fill a
// dashboard without sending everything the source keeps.
const DefaultLookback = 72 * time.Hour

// On reports whether metrics are derived.
func (m Metrics) On() bool { return m.Enabled == nil || *m.Enabled }

// Parse configures assembly into rounds.
type Parse struct {
	// MaxRoundBytes caps a round file. A round travels whole as one log
	// record, so it is cut at the same budget as a landed file: the parser
	// narrows a round's input window until the round fits, and the rest of
	// the evidence goes to the next round. A round covering a single landed
	// file is published whole even when larger. The default is 2 MiB.
	MaxRoundBytes int64 `yaml:"max_round_bytes"`
}

// Export configures where collected data is sent.
type Export struct {
	OTLP OTLP `yaml:"otlp"`
}

// OTLP configures the OpenTelemetry logs push: every landed file and every
// round, one log record per file, over OTLP/gRPC or OTLP/HTTP.
type OTLP struct {
	// Protocol is the transport: grpc, the default, or http.
	Protocol string `yaml:"protocol"`
	// Endpoint is where the receiver listens. For grpc it is host:port, the
	// OAP's gRPC port 11800 by default; for http it is the receiver's base
	// URL, the OAP's REST port 12800, and the logs path is appended. Empty
	// means asz push has nowhere to send and refuses to run.
	Endpoint string `yaml:"endpoint"`
	// TLS makes the gRPC connection a TLS one, verified against the
	// system's roots. Over HTTP the scheme of the endpoint decides.
	TLS bool `yaml:"tls"`
	// ServiceName is the service every record is attributed to. Empty means
	// the runtime the adapter reads, Claude Code for claude-code-local.
	ServiceName string `yaml:"service_name"`
	// InstanceID is sent as service.instance.id and says who is pushing, in
	// words the people reading the receiver recognise: a mailbox, a name, a
	// machine. Empty means user@host of the machine running asz push.
	InstanceID string `yaml:"instance_id"`
	// Layer is sent as service.layer, which the SkyWalking OAP uses to place
	// the service.
	Layer string `yaml:"layer"`
	// Headers are added to every request, for example an authorization token.
	Headers map[string]string `yaml:"headers"`
	// BatchBytes is how many file bytes one request carries at most. A file
	// larger than this is sent alone. The default, 8 MiB, keeps a request
	// under the 10 MiB the OAP's HTTP server accepts, and well under the
	// 50 MB its gRPC server accepts.
	BatchBytes int64 `yaml:"batch_bytes"`
	// MaxBytesPerMinute caps what asz push puts on the wire per minute. A
	// pass waits before a request until a minute's budget holds it. Zero,
	// the default, is no limit.
	MaxBytesPerMinute int64 `yaml:"max_bytes_per_minute"`
	// Logs and Metrics switch the two things a push sends: the landed
	// files and rounds as OTLP logs, and the metrics spool. Both are on
	// unless set false, so a receiver that takes one and not the other is
	// sent what it takes. A push with both off is refused.
	Logs    *bool `yaml:"logs"`
	Metrics *bool `yaml:"metrics"`
}

// SendLogs says whether a push sends the landed files and rounds.
func (o OTLP) SendLogs() bool { return o.Logs == nil || *o.Logs }

// SendMetrics says whether a push sends the metrics spool.
func (o OTLP) SendMetrics() bool { return o.Metrics == nil || *o.Metrics }

// MetricsSent says whether derived metrics have somewhere to go: an endpoint
// is set and export.otlp.metrics is on. A pipeline derives only then. Every
// request it derives waits in the spool until it is sent, so a root that
// never sends would keep each one for ever.
func (c *Config) MetricsSent() bool {
	return c.Metrics.On() && c.Export.OTLP.Endpoint != "" && c.Export.OTLP.SendMetrics()
}

func boolPtr(b bool) *bool { return &b }

// LookbackDuration is the look-back as a duration: 72h when unset, zero for
// 0 or none, and a d suffix counts days, since a look-back is spoken of in
// days.
func (m Metrics) LookbackDuration() (time.Duration, error) {
	s := strings.TrimSpace(m.Lookback)
	switch s {
	case "":
		return DefaultLookback, nil
	case "0", "none":
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("config: metrics.lookback %q is not a number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("config: metrics.lookback %q is not a duration such as 24h or 7d", s)
	}
	return d, nil
}

// Storage locates the landing zone.
type Storage struct {
	// Root is where landed data is written. Default "./data", i.e. beside the
	// binary; tests point it beside their fixtures so a case is self-contained.
	Root string `yaml:"root"`
	// Retention is how long an idle conversation keeps what in the root.
	Retention Retention `yaml:"retention"`
}

// Retention is how long a conversation may be idle before the root keeps
// less of it. Idle is measured from the newest file landed for it, whether
// it was sent or not, and whether an endpoint is set or not. Every value is
// -1, off, by default, and only off keeps everything.
type Retention struct {
	// MetadataOnlyAfter is how long a conversation may be idle before only
	// its metadata is kept, to save disk: each landed file is replaced by
	// its skeleton, which keeps who did what, when and in what order, and
	// none of what was said or run. See MetadataOnlyAfterDuration.
	MetadataOnlyAfter string `yaml:"metadata_only_after"`
	// RetireAfter is how long a conversation may be idle before everything
	// of it is removed, its metadata too, and it is retired: it is never
	// collected again, even when its runtime resumes it. See
	// RetireAfterDuration.
	RetireAfter string `yaml:"retire_after"`
}

// MetadataOnlyAfterDuration is metadata_only_after as a duration: zero for -1
// or empty, which keep everything, and a d suffix counts days, as a retention
// is spoken of in days.
func (r Retention) MetadataOnlyAfterDuration() (time.Duration, error) {
	return retentionDuration("storage.retention.metadata_only_after", r.MetadataOnlyAfter)
}

// RetireAfterDuration is retire_after as a duration, read as
// MetadataOnlyAfterDuration reads its own.
func (r Retention) RetireAfterDuration() (time.Duration, error) {
	return retentionDuration("storage.retention.retire_after", r.RetireAfter)
}

// Durations reads both levels and checks that they go together. Retirement
// removes the metadata level 1 keeps, so it comes no earlier than level 1,
// and it needs level 1 set: a root that keeps the metadata for ever would
// otherwise retire what it was told to keep. Equal levels retire a
// conversation with no level 1 before it.
func (r Retention) Durations() (metadataOnly, retire time.Duration, err error) {
	if metadataOnly, err = r.MetadataOnlyAfterDuration(); err != nil {
		return 0, 0, err
	}
	if retire, err = r.RetireAfterDuration(); err != nil {
		return 0, 0, err
	}
	switch {
	case retire > 0 && metadataOnly == 0:
		return 0, 0, fmt.Errorf("config: storage.retention.retire_after is %s and metadata_only_after is off; set metadata_only_after too, at most %s",
			strings.TrimSpace(r.RetireAfter), strings.TrimSpace(r.RetireAfter))
	case retire > 0 && retire < metadataOnly:
		return 0, 0, fmt.Errorf("config: storage.retention.retire_after %s is shorter than metadata_only_after %s; it must be at least as long",
			strings.TrimSpace(r.RetireAfter), strings.TrimSpace(r.MetadataOnlyAfter))
	}
	return metadataOnly, retire, nil
}

func retentionDuration(name, value string) (time.Duration, error) {
	v := strings.TrimSpace(value)
	switch v {
	case "", "-1":
		return 0, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		// A number of days past what a duration holds would wrap around to
		// a short one, and remove what was meant to be kept for years.
		if err != nil || n <= 0 || int64(n) > int64(math.MaxInt64/(24*time.Hour)) {
			return 0, fmt.Errorf("config: %s %q is not a positive number of days a duration can hold", name, v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("config: %s %q is not -1 or a duration such as 72h or 7d", name, v)
	}
	return d, nil
}

// Adapter configures one collection source. The name identifies both the
// runtime and the collection posture, so a future push adapter for the same
// runtime is a separate entry rather than a mode flag.
type Adapter struct {
	Name    string `yaml:"name"`
	Enabled bool   `yaml:"enabled"`

	// SourceRoot overrides source discovery. When empty the adapter resolves
	// its own default.
	SourceRoot string `yaml:"source_root"`

	// Include and Exclude are evaluated per SESSION, not per directory: a
	// session's files can be spread across several source directories, so
	// matching per directory would silently cut streams out of a session
	// that is otherwise being collected.
	//
	// An entry starting with "/" is a real path, slugified forward before
	// matching. Anything else is a glob against the source directory name.
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`

	// Listen, on langsmith-ingest, is the address the receiver listens on,
	// the one LANGSMITH_ENDPOINT is pointed at, such as 127.0.0.1:1985.
	Listen string `yaml:"listen"`
	// Token, on langsmith-ingest, is required in the x-api-key header.
	// Empty accepts any key, which is what a local collector wants: the
	// client insists on sending one and has nothing to prove.
	Token string `yaml:"token"`
	// ThreadKeys, on langsmith-ingest, are the run metadata keys that may
	// carry the thread, in the order they are tried. An application that
	// names its own says so here.
	ThreadKeys []string `yaml:"thread_keys"`
	// Scope, on langsmith-ingest, names the dimensions that together own a
	// conversation, in order: "project" is the client's project name and
	// anything else is a metadata key. A bare thread key is not an
	// identity, because two applications can both use the project
	// "production" and the thread "123". Dropping "project" merges every
	// project that shares a thread key, which is a choice to make rather
	// than one to discover.
	Scope []string `yaml:"scope"`
	// ProviderBodies lands what each model call was sent and what came
	// back, beside the conversation, for the langsmith-ingest receiver. It
	// is what the continuity check between calls runs on, and what shows a
	// nested agent's own prompt. Cut against what the session already
	// holds; measured, a long conversation lands under half of what it was
	// on the wire, and a conversation of a few short calls lands more than
	// it was. The Claude Code provider adapter is its own adapter and does
	// not take this.
	ProviderBodies bool `yaml:"provider_bodies"`

	Collector Collector `yaml:"collector"`
}

// UnmarshalYAML starts each named adapter with its own defaults. Decoding
// over those values preserves an explicit false or empty exclusion list.
func (a *Adapter) UnmarshalYAML(node *yaml.Node) error {
	var name struct {
		Name string `yaml:"name"`
	}
	if err := node.Decode(&name); err != nil {
		return err
	}
	type plain Adapter
	value := plain{Name: name.Name, Enabled: true}
	if name.Name == RemovedClaudeCodeOTLP {
		// It was off unless turned on, and an entry written for it still
		// means that. Without this, an entry with no enabled key would turn
		// on, and print a skip line where it used to print nothing.
		value.Enabled = false
	}
	for _, def := range Default().Adapters {
		if sameAdapter(def.Name, name.Name) {
			value = plain(def)
			value.Name = name.Name
			break
		}
	}
	if err := node.Decode(&value); err != nil {
		return err
	}
	*a = Adapter(value)
	return nil
}

// DefaultInterval is the period between passes in watch mode. Every pass
// lands what moved as new files, so a short period lands many small files,
// and each file is one log record when it is sent. Tests and scenario feeds
// set their own shorter period.
const DefaultInterval = 10 * time.Minute

// Collector controls collection cadence for one adapter.
type Collector struct {
	// Mode is "watch" (poll continuously) or "once" (single pass, then exit).
	// "once" is the backfill path over history that already exists.
	Mode string `yaml:"mode"`

	Interval time.Duration `yaml:"interval"`

	// MaxDeltaBytes caps how much of a source is landed in a single file, so a
	// large catch-up is split rather than producing one enormous delta. A
	// file travels whole as one log record, so this is also the largest
	// record a receiver has to accept. A single source record larger than the
	// budget lands alone, with its largest payloads omitted until it fits.
	// The default is 2 MiB.
	MaxDeltaBytes int64 `yaml:"max_delta_bytes"`
}

const (
	ModeWatch = "watch"
	ModeOnce  = "once"

	// AdapterClaudeCodeLocal reads Claude Code's local files. Pull posture.
	AdapterClaudeCodeLocal = "claude-code-local"
	// RemovedClaudeCodeOTLP names the receiver for Claude Code's own
	// OpenTelemetry exporter that 0.5.0 and earlier had. It is gone because
	// every metric asz sends is derived from the landed files. The exporter
	// is pointed at the receiver that reads it, such as the SkyWalking OAP.
	// A configuration written then still names it. Such an entry lands
	// nothing. When it is enabled, it is skipped with a line that says so.
	RemovedClaudeCodeOTLP = "claude-code-otlp"
	// AdapterClaudeCodeChanges reads the workspace change records the asz
	// Claude Code plugin writes beside Claude Code's own files. Pull
	// posture, like the local adapter, and the plugin needs nothing from
	// it: it discovers and tails what the plugin has already written.
	AdapterChanges = "changes"
	// AdapterClaudeCodeChanges is what that adapter was called until 0.5.0.
	// A configuration written then still names it, so it is accepted and
	// read as the same adapter rather than skipped as an unknown one.
	AdapterClaudeCodeChanges = "claude-code-changes"
	// AdapterClaudeCodeProvider reads the request and response bodies
	// Claude Code writes for its model provider when
	// OTEL_LOG_RAW_API_BODIES names a directory. Pull posture, like the
	// local adapter: Claude Code writes the files, and this adapter finds
	// each body's session and lands it.
	AdapterClaudeCodeProvider = "claude-code-provider"
	// AdapterKiroLocal reads the sessions Kiro's V3 agent engine writes
	// under ~/.kiro: kiro-cli on the V3 engine, and Kiro Crew on its kas
	// backend. Pull posture, like the Claude Code local adapter.
	AdapterKiroLocal = "kiro-local"
	// AdapterLangSmithIngest receives what the LangSmith tracing client
	// sends. Push posture: an application built on LangChain or LangGraph
	// already carries that client, so four environment variables are the
	// whole integration and nothing in the application changes.
	AdapterLangSmithIngest = "langsmith-ingest"
)

// Default returns the configuration used when none is supplied.
func Default() *Config {
	return &Config{
		Storage: Storage{Root: "./data", Retention: Retention{MetadataOnlyAfter: "-1", RetireAfter: "-1"}},
		Judge: Judge{
			Hide:        []string{},
			Timeout:     "30m",
			ReadBytes:   131072,
			Mode:        "auto",
			SingleBytes: 32768,
			ChunkBytes:  24576,
			Concurrency: 4,
			StateBytes:  16384,
			Progress:    "30s",
			Evaluators: map[string]Evaluator{
				"claude": {Kind: "cli", Program: "claude", Args: []string{}, MaxTurns: 60},
				"codex":  {Kind: "cli", Program: "codex", Args: []string{}},
				"kiro":   {Kind: "cli", Program: "kiro-cli", Args: []string{}},
			},
			Plan: []JudgeStage{
				{Stage: "targets", Evaluator: "claude"},
				{Stage: "achievement", Evaluator: "claude"},
				{Stage: "subtasks", Evaluator: "claude"},
				{Stage: "efficiency", Evaluator: "claude"},
			},
			References: References{MaxBytes: 1 << 20, Timeout: "1m", Fetch: []Fetcher{}},
		},
		Adapters: []Adapter{{
			Name:    AdapterClaudeCodeLocal,
			Enabled: true,
			Exclude: []string{"/private/tmp/**"},
			Collector: Collector{
				Mode:          ModeWatch,
				Interval:      DefaultInterval,
				MaxDeltaBytes: 2 << 20,
			},
		}, {
			// The LangSmith tracing client, received. Off until pointed
			// at: the file names the address an application would be
			// given, and the defaults are what that client documents.
			Name:           AdapterLangSmithIngest,
			Enabled:        false,
			Listen:         "127.0.0.1:1985",
			ThreadKeys:     []string{"thread_id", "session_id", "conversation_id"},
			Scope:          []string{"project", "thread"},
			ProviderBodies: true,
			Collector: Collector{Mode: ModeWatch, Interval: DefaultInterval,
				MaxDeltaBytes: 2 << 20},
		}, {
			// The plugin's output, beside the runtime's own files. On by
			// default because it costs nothing when the plugin is not
			// installed: there is nothing to discover.
			Name:    AdapterChanges,
			Enabled: true,
			Exclude: []string{"/private/tmp/**"},
			Collector: Collector{
				Mode:          ModeWatch,
				Interval:      DefaultInterval,
				MaxDeltaBytes: 2 << 20,
			},
		}, {
			// The bodies Claude Code writes when it is told to. On by default
			// because it costs nothing until OTEL_LOG_RAW_API_BODIES names
			// the directory: there is nothing to list.
			Name:    AdapterClaudeCodeProvider,
			Enabled: true,
			Exclude: []string{"/private/tmp/**"},
			Collector: Collector{
				Mode:          ModeWatch,
				Interval:      DefaultInterval,
				MaxDeltaBytes: 2 << 20,
			},
		}, {
			// Kiro's V3 sessions. On by default because it costs nothing
			// where Kiro is not installed: there is nothing to discover.
			Name:    AdapterKiroLocal,
			Enabled: true,
			Exclude: []string{"/private/tmp/**"},
			Collector: Collector{
				Mode:          ModeWatch,
				Interval:      DefaultInterval,
				MaxDeltaBytes: 2 << 20,
			},
		}},
		Parse: Parse{MaxRoundBytes: 2 << 20},
		// Written out, as every default is, so the file says what the first
		// derivation reaches back to.
		Metrics: Metrics{Enabled: boolPtr(true), Lookback: "72h"},
		Export: Export{OTLP: OTLP{
			Protocol:   "grpc",
			Layer:      "AI_AGENT",
			BatchBytes: 8 << 20,
			Logs:       boolPtr(true),
			Metrics:    boolPtr(true),
		}},
		View:              View{Hide: []string{}},
		RemoteSnapshotTTL: DefaultRemoteSnapshotTTL,
		Remotes:           []Remote{},
		RemoteServer: RemoteServer{
			MaxConcurrentPerToken: DefaultMaxConcurrentPerToken,
			SnapshotTTL:           DefaultServerSnapshotTTL,
		},
	}
}

// sameAdapter reports whether two names are the same adapter.
//
// An adapter that was renamed answers to both names, so the defaults have to
// be found under either. Matching the literal name meant a configuration
// written before the rename found no defaults at all and silently lost them,
// including the exclusion that keeps the tool's own sessions out.
func sameAdapter(a, b string) bool {
	return a == b || (isChanges(a) && isChanges(b))
}

// isChanges reports whether a name is the change-record adapter, under either
// the name it has now or the one it had until 0.5.0.
func isChanges(name string) bool {
	return name == AdapterChanges || name == AdapterClaudeCodeChanges
}

// Load reads a YAML config, applying defaults for anything unset. An empty
// path returns Default.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	// The file is a supplied path like any other: it is refused inside a
	// tree pull owns, before anything is read, and read through its checked
	// parent so a link changed after the check leads nowhere else.
	f, err := storage.OpenSuppliedRead(path, "")
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	data, err := io.ReadAll(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	// Adapters decode over their named defaults. The other sections fill
	// their gaps below; pointers preserve explicit false export switches.
	var loaded Config
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		// The library names a Go type a person editing the file has never
		// seen. storage.retention is a section, and a single value there is
		// the one place a reader is likely to meet it.
		return nil, fmt.Errorf("config: parse %s: %s", path, strings.ReplaceAll(err.Error(),
			"into config.Retention", "into storage.retention, which is a section of settings such as metadata_only_after"))
	}
	if err := checkKeys(data); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	// Which of the judge's numbers the file sets: zero is a value some of
	// them take, as read_bytes 0 is no bound, so a missing key and a zero
	// are told apart.
	var set struct {
		Judge struct {
			ReadBytes   *int `yaml:"read_bytes"`
			SingleBytes *int `yaml:"single_bytes"`
			ChunkBytes  *int `yaml:"chunk_bytes"`
			Concurrency *int `yaml:"concurrency"`
			StateBytes  *int `yaml:"state_bytes"`
			References  struct {
				MaxBytes *int `yaml:"max_bytes"`
			} `yaml:"references"`
		} `yaml:"judge"`
		RemoteSnapshotTTL *string `yaml:"remote_snapshot_ttl"`
		RemoteServer      struct {
			// A node, not an int: the decoder makes 64.9 into 64.
			MaxConcurrentPerToken yaml.Node `yaml:"max_concurrent_per_token"`
			SnapshotTTL           *string   `yaml:"snapshot_ttl"`
		} `yaml:"remote_server"`
	}
	if err := yaml.Unmarshal(data, &set); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if loaded.Storage.Root != "" {
		cfg.Storage.Root = loaded.Storage.Root
	}
	if loaded.Storage.Retention.MetadataOnlyAfter != "" {
		cfg.Storage.Retention.MetadataOnlyAfter = loaded.Storage.Retention.MetadataOnlyAfter
	}
	if loaded.Storage.Retention.RetireAfter != "" {
		cfg.Storage.Retention.RetireAfter = loaded.Storage.Retention.RetireAfter
	}
	if len(loaded.Adapters) > 0 {
		cfg.Adapters = loaded.Adapters
		for i := range cfg.Adapters {
			cfg.Adapters[i].Collector.applyDefaults()
		}
	}
	if loaded.Parse.MaxRoundBytes > 0 {
		cfg.Parse.MaxRoundBytes = loaded.Parse.MaxRoundBytes
	}
	if loaded.Metrics.Enabled != nil {
		cfg.Metrics.Enabled = loaded.Metrics.Enabled
	}
	if loaded.Metrics.Lookback != "" {
		cfg.Metrics.Lookback = loaded.Metrics.Lookback
	}
	o := &loaded.Export.OTLP
	if o.Protocol != "" {
		cfg.Export.OTLP.Protocol = o.Protocol
	}
	if o.Endpoint != "" {
		cfg.Export.OTLP.Endpoint = o.Endpoint
	}
	if o.TLS {
		cfg.Export.OTLP.TLS = true
	}
	if o.ServiceName != "" {
		cfg.Export.OTLP.ServiceName = o.ServiceName
	}
	if o.InstanceID != "" {
		cfg.Export.OTLP.InstanceID = o.InstanceID
	}
	if o.Layer != "" {
		cfg.Export.OTLP.Layer = o.Layer
	}
	if len(o.Headers) > 0 {
		cfg.Export.OTLP.Headers = o.Headers
	}
	if o.BatchBytes > 0 {
		cfg.Export.OTLP.BatchBytes = o.BatchBytes
	}
	if o.MaxBytesPerMinute > 0 {
		cfg.Export.OTLP.MaxBytesPerMinute = o.MaxBytesPerMinute
	}
	if o.Logs != nil {
		cfg.Export.OTLP.Logs = o.Logs
	}
	if o.Metrics != nil {
		cfg.Export.OTLP.Metrics = o.Metrics
	}
	if len(loaded.View.Hide) > 0 {
		// hide is a set, and a name written twice is withheld once.
		cfg.View.Hide = slices.Compact(slices.Sorted(slices.Values(loaded.View.Hide)))
	}
	cfg.View.HideOverride = loaded.View.HideOverride
	j := &loaded.Judge
	if j.Pricing != "" {
		cfg.Judge.Pricing = j.Pricing
	}
	if len(j.Hide) > 0 {
		cfg.Judge.Hide = slices.Compact(slices.Sorted(slices.Values(j.Hide)))
	}
	if j.Timeout != "" {
		cfg.Judge.Timeout = j.Timeout
	}
	if j.Mode != "" {
		cfg.Judge.Mode = j.Mode
	}
	if j.Progress != "" {
		cfg.Judge.Progress = j.Progress
	}
	for _, n := range []struct {
		from *int
		to   *int
	}{{set.Judge.ReadBytes, &cfg.Judge.ReadBytes}, {set.Judge.SingleBytes, &cfg.Judge.SingleBytes},
		{set.Judge.ChunkBytes, &cfg.Judge.ChunkBytes}, {set.Judge.Concurrency, &cfg.Judge.Concurrency},
		{set.Judge.StateBytes, &cfg.Judge.StateBytes}} {
		if n.from != nil {
			*n.to = *n.from
		}
	}
	// An evaluator named as a default replaces it; any other name is added,
	// so a file that adds an API keeps the agent CLIs.
	for name, e := range j.Evaluators {
		cfg.Judge.Evaluators[name] = e
	}
	if len(j.Plan) > 0 {
		cfg.Judge.Plan = j.Plan
	}
	if set.Judge.References.MaxBytes != nil {
		cfg.Judge.References.MaxBytes = *set.Judge.References.MaxBytes
	}
	if j.References.Timeout != "" {
		cfg.Judge.References.Timeout = j.References.Timeout
	}
	if len(j.References.Fetch) > 0 {
		cfg.Judge.References.Fetch = j.References.Fetch
	}
	// A time to live written as an empty value is a value, refused below
	// with the rest; only one left out takes its default.
	if set.RemoteSnapshotTTL != nil {
		cfg.RemoteSnapshotTTL = *set.RemoteSnapshotTTL
	}
	if len(loaded.Remotes) > 0 {
		cfg.Remotes = loaded.Remotes
	}
	rs := loaded.RemoteServer
	cfg.RemoteServer.TokensFile = rs.TokensFile
	cfg.RemoteServer.TLS = rs.TLS
	if n := &set.RemoteServer.MaxConcurrentPerToken; n.Kind != 0 {
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" {
			return nil, fmt.Errorf("config: %s: remote_server.max_concurrent_per_token is %q; it is a whole number from 1 to %d", path, n.Value, MaxConcurrentPerTokenLimit)
		}
		if err := n.Decode(&cfg.RemoteServer.MaxConcurrentPerToken); err != nil {
			return nil, fmt.Errorf("config: %s: remote_server.max_concurrent_per_token: %w", path, err)
		}
	}
	if set.RemoteServer.SnapshotTTL != nil {
		cfg.RemoteServer.SnapshotTTL = *set.RemoteServer.SnapshotTTL
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Credential paths are resolved now, from the working directory the
	// configuration was loaded in, and read only when they are needed.
	if err := cfg.resolveCredentialPaths(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// checkKeys refuses a configuration that would show a reader what it meant
// to withhold, with no word said.
//
// Every section but view is read loosely, as it always was. view is not,
// because it says what a reader is kept from, and a misspelled key there
// would show everything to everyone. The YAML library resolves aliases and
// merge keys here as it does for the whole configuration, so view reads the
// same in both.
//
// Only the first YAML document of a file is read, so a later one that holds
// anything is refused rather than ignored.
func checkKeys(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var top struct {
		View View `yaml:"view"`
		// Rest holds every other top-level key as a node, not decoded, so an
		// alias in a key nothing reads is not expanded here, as the
		// configuration itself does not expand it.
		Rest map[string]yaml.Node `yaml:",inline"`
	}
	if err := dec.Decode(&top); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		// The library names a Go type a person editing the file has never
		// seen.
		return errors.New(strings.NewReplacer(": field ", ": ", " not found in type config.View",
			" is not a key of the view section").Replace(err.Error()))
	}
	for {
		var next yaml.Node
		err := dec.Decode(&next)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || holdsAnything(&next) {
			return errors.New("the file holds a second YAML document, and only the first is read. " +
				"Write the configuration as one document")
		}
	}
	return nil
}

// holdsAnything reports whether a YAML document holds a value. A document
// with nothing after its marker holds a null.
func holdsAnything(doc *yaml.Node) bool {
	for _, n := range doc.Content {
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!null" {
			return true
		}
	}
	return false
}

func (c *Collector) applyDefaults() {
	if c.Mode == "" {
		c.Mode = ModeWatch
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.MaxDeltaBytes <= 0 {
		c.MaxDeltaBytes = 2 << 20
	}
}

// Validate reports configuration that cannot be acted on.
func (c *Config) Validate() error {
	if c.Storage.Root == "" {
		return fmt.Errorf("config: storage.root must not be empty")
	}
	if err := c.Judge.check(); err != nil {
		return err
	}
	seen := map[string]bool{}
	// A machine can run more than one recorder - Claude Code's plugin and
	// a LangChain shim, each writing its own directory - so the changes
	// adapter may be named more than once, one entry per directory. Every
	// other adapter is one source, and an old name for an adapter is that
	// adapter: two entries that read one directory would land it twice.
	changesRoots := map[string]string{}
	for i, a := range c.Adapters {
		if a.Name == "" {
			return fmt.Errorf("config: adapters[%d] has no name", i)
		}
		if isChanges(a.Name) {
			// Spellings of one directory are one directory; the resolved
			// paths are compared again where they are resolved, since the
			// default is a directory only the collector can name.
			key, root := "", "Claude Code's plugin directory"
			if a.SourceRoot != "" {
				key, root = filepath.Clean(a.SourceRoot), a.SourceRoot
			}
			if prev, dup := changesRoots[key]; dup {
				return fmt.Errorf("config: adapters %q and %q both read %s; name each directory once", prev, a.Name, root)
			}
			changesRoots[key] = a.Name
		} else {
			if seen[a.Name] {
				return fmt.Errorf("config: duplicate adapter %q", a.Name)
			}
			seen[a.Name] = true
		}
		if a.Name != AdapterLangSmithIngest && a.ProviderBodies {
			return fmt.Errorf("config: adapter %q does not take provider_bodies; only %q does, and %q lands Claude Code's bodies on its own", a.Name, AdapterLangSmithIngest, AdapterClaudeCodeProvider)
		}
		if a.Name == AdapterLangSmithIngest {
			if a.Enabled && a.Listen == "" {
				return fmt.Errorf("config: adapter %q needs listen, the address LANGSMITH_ENDPOINT is pointed at, such as 127.0.0.1:1985", a.Name)
			}
			if a.SourceRoot != "" || len(a.Include) > 0 || len(a.Exclude) > 0 {
				return fmt.Errorf("config: adapter %q is a receiver: it takes listen, token, thread_keys, scope, provider_bodies and collector, not source_root, include or exclude", a.Name)
			}
			// It takes a period and a byte budget: what it accepts waits in
			// an inbox until a pass converts it, and one request can be
			// larger than a landed file should be. A single turn with a
			// 20 KB command and a 32 KB result measured 3 MB.
			if len(a.Scope) == 0 {
				return fmt.Errorf("config: adapter %q needs scope, the dimensions that own a conversation; the default is [project, thread]", a.Name)
			}
			if len(a.ThreadKeys) == 0 {
				return fmt.Errorf("config: adapter %q needs thread_keys, the metadata keys that may carry the thread", a.Name)
			}
			continue
		}
		if isChanges(a.Name) && a.Listen != "" {
			return fmt.Errorf("config: adapter %q reads the plugin's records only: it takes source_root, include, exclude and collector, not listen", a.Name)
		}
		if a.Name == AdapterClaudeCodeProvider && a.Listen != "" {
			return fmt.Errorf("config: adapter %q reads provider bodies only: it takes source_root, include, exclude and collector, not listen", a.Name)
		}
		if a.Name == AdapterKiroLocal && a.Listen != "" {
			return fmt.Errorf("config: adapter %q reads Kiro's files: it takes source_root, include, exclude and collector, not listen", a.Name)
		}
		if a.Collector.Mode != ModeWatch && a.Collector.Mode != ModeOnce {
			return fmt.Errorf("config: adapter %q: unknown collector mode %q", a.Name, a.Collector.Mode)
		}
	}
	if p := c.Export.OTLP.Protocol; p != "grpc" && p != "http" {
		return fmt.Errorf("config: export.otlp.protocol is %q, want grpc or http", p)
	}
	if !c.Export.OTLP.SendLogs() && !c.Export.OTLP.SendMetrics() {
		return errors.New("config: export.otlp has both logs and metrics off; a push would send nothing")
	}
	if _, err := c.Metrics.LookbackDuration(); err != nil {
		return err
	}
	if _, _, err := c.Storage.Retention.Durations(); err != nil {
		return err
	}
	if err := sessiondata.CheckWithholdable(c.View.Hide); err != nil {
		return fmt.Errorf("config: view.hide: %w", err)
	}
	return c.checkRemotes()
}

// ResolvedRoot returns storage.Root as an absolute path.
func (c *Config) ResolvedRoot() (string, error) {
	return filepath.Abs(c.Storage.Root)
}
