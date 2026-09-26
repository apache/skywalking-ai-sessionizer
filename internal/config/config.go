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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration document.
type Config struct {
	Storage  Storage   `yaml:"storage"`
	Adapters []Adapter `yaml:"adapters"`
	Parse    Parse     `yaml:"parse"`
	Metrics  Metrics   `yaml:"metrics"`
	Export   Export    `yaml:"export"`
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

	// Listen, on claude-code-otlp, is the address the receiver listens on
	// for the runtime's exporter, such as 127.0.0.1:4317, over gRPC and
	// HTTP with protobuf on the one port.
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
	// record a receiver has to accept, apart from a single source record
	// larger than the budget, which is landed whole. The default is 2 MiB.
	MaxDeltaBytes int64 `yaml:"max_delta_bytes"`
}

const (
	ModeWatch = "watch"
	ModeOnce  = "once"

	// AdapterClaudeCodeLocal reads Claude Code's local files. Pull posture.
	AdapterClaudeCodeLocal = "claude-code-local"
	// AdapterClaudeCodeOTLP receives what Claude Code's own OpenTelemetry
	// exporter sends. Push posture, the runtime's side.
	AdapterClaudeCodeOTLP = "claude-code-otlp"
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
	// AdapterLangSmithIngest receives what the LangSmith tracing client
	// sends. Push posture: an application built on LangChain or LangGraph
	// already carries that client, so four environment variables are the
	// whole integration and nothing in the application changes.
	AdapterLangSmithIngest = "langsmith-ingest"
)

// Default returns the configuration used when none is supplied.
func Default() *Config {
	return &Config{
		Storage: Storage{Root: "./data"},
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
			// The runtime's own exporter, received. Off until pointed at:
			// the file names the address the runtime would be given.
			Name:    AdapterClaudeCodeOTLP,
			Enabled: false,
			Listen:  "127.0.0.1:4317",
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

// isReceiver reports whether an adapter is a server rather than a reader. A
// receiver polls nothing and lands no transcript of its own, so the collector
// settings do not apply to it.
func isReceiver(name string) bool {
	return name == AdapterClaudeCodeOTLP
}

// Load reads a YAML config, applying defaults for anything unset. An empty
// path returns Default.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	// Adapters decode over their named defaults. The other sections fill
	// their gaps below; pointers preserve explicit false export switches.
	var loaded Config
	if err := yaml.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if loaded.Storage.Root != "" {
		cfg.Storage.Root = loaded.Storage.Root
	}
	if len(loaded.Adapters) > 0 {
		cfg.Adapters = loaded.Adapters
		for i := range cfg.Adapters {
			// A receiver is a server: it polls nothing and lands no
			// transcript, so the collector settings do not apply to it.
			if !isReceiver(cfg.Adapters[i].Name) {
				cfg.Adapters[i].Collector.applyDefaults()
			}
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
	return cfg, cfg.Validate()
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
		if a.Name == AdapterClaudeCodeOTLP && a.Enabled {
			if a.Listen == "" {
				return fmt.Errorf("config: adapter %q needs listen, the address the runtime's exporter is pointed at, such as 127.0.0.1:4317", a.Name)
			}
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
			// It takes a period and a byte budget, unlike the metrics
			// receiver: what it accepts waits in an inbox until a pass
			// converts it, and one request can be larger than a landed file
			// should be. A single turn with a 20 KB command and a 32 KB
			// result measured 3 MB.
			if len(a.Scope) == 0 {
				return fmt.Errorf("config: adapter %q needs scope, the dimensions that own a conversation; the default is [project, thread]", a.Name)
			}
			if len(a.ThreadKeys) == 0 {
				return fmt.Errorf("config: adapter %q needs thread_keys, the metadata keys that may carry the thread", a.Name)
			}
			continue
		}
		if a.Name == AdapterClaudeCodeOTLP {
			if a.Collector != (Collector{}) || a.SourceRoot != "" || len(a.Include) > 0 || len(a.Exclude) > 0 {
				return fmt.Errorf("config: adapter %q is a receiver: it takes listen, not collector, source_root, include or exclude", a.Name)
			}
			continue
		}
		if isChanges(a.Name) && a.Listen != "" {
			return fmt.Errorf("config: adapter %q reads the plugin's records only: it takes source_root, include, exclude and collector, not listen", a.Name)
		}
		if a.Name == AdapterClaudeCodeProvider && a.Listen != "" {
			return fmt.Errorf("config: adapter %q reads provider bodies only: it takes source_root, include, exclude and collector, not listen", a.Name)
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
	return nil
}

// ResolvedRoot returns storage.Root as an absolute path.
func (c *Config) ResolvedRoot() (string, error) {
	return filepath.Abs(c.Storage.Root)
}
