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
	Export   Export    `yaml:"export"`
}

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

// Lookback is the metrics look-back as a duration: 24h when unset, zero
// for 0 or none, and a d suffix counts days, since a look-back is spoken of
// in days.
func (a Adapter) Lookback() (time.Duration, error) {
	s := strings.TrimSpace(a.MetricsLookback)
	switch s {
	case "":
		return 24 * time.Hour, nil
	case "0", "none":
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("config: metrics_lookback %q is not a number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("config: metrics_lookback %q is not a duration such as 24h or 7d", s)
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

	// Metrics, on claude-code-local, turns on the runtime's own metric
	// family derived from the landed files: the same names and attributes
	// the runtime's exporter sends, so a receiver sees one family whichever
	// produced it. Phase one is token usage. On claude-code-otlp it says
	// the received metrics are landed and pushed. It cannot be on for both
	// at once, since the two would count the same tokens twice.
	Metrics bool `yaml:"metrics"`
	// Listen, on claude-code-otlp, is the address the receiver listens on
	// for the runtime's exporter, such as 127.0.0.1:4317, over gRPC and
	// HTTP with protobuf on the one port.
	Listen string `yaml:"listen"`
	// MetricsLookback bounds the first derivation over a root that has
	// history: a minute older than this is not derived. A duration such as
	// 24h or 7d; empty means 24h; 0 or none means everything.
	MetricsLookback string `yaml:"metrics_lookback"`

	Collector Collector `yaml:"collector"`
}

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
	AdapterClaudeCodeChanges = "claude-code-changes"
)

// Default returns the configuration used when none is supplied.
func Default() *Config {
	return &Config{
		Storage: Storage{Root: "./data"},
		Adapters: []Adapter{{
			Name:    AdapterClaudeCodeLocal,
			Enabled: true,
			Exclude: []string{"/private/tmp/**"},
			// The look-back is written out, as every default is, so the
			// file says what the first derivation reaches back to.
			MetricsLookback: "24h",
			Collector: Collector{
				Mode:          ModeWatch,
				Interval:      5 * time.Second,
				MaxDeltaBytes: 2 << 20,
			},
		}, {
			// The runtime's own exporter, received. Off until pointed at:
			// the file names the address the runtime would be given, and
			// metrics on here means the local adapter leaves them to it.
			Name:    AdapterClaudeCodeOTLP,
			Enabled: false,
			Listen:  "127.0.0.1:4317",
			Metrics: true,
		}, {
			// The plugin's output, beside the runtime's own files. On by
			// default because it costs nothing when the plugin is not
			// installed: there is nothing to discover.
			Name:    AdapterClaudeCodeChanges,
			Enabled: true,
			Exclude: []string{"/private/tmp/**"},
			Collector: Collector{
				Mode:          ModeWatch,
				Interval:      5 * time.Second,
				MaxDeltaBytes: 2 << 20,
			},
		}},
		Parse: Parse{MaxRoundBytes: 2 << 20},
		Export: Export{OTLP: OTLP{
			Protocol:   "grpc",
			Layer:      "AI_AGENT",
			BatchBytes: 8 << 20,
			Logs:       boolPtr(true),
			Metrics:    boolPtr(true),
		}},
	}
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
	// Decode over a zero value so unset fields stay distinguishable from
	// zero-valued ones, then fill gaps from the defaults.
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
			if cfg.Adapters[i].Name != AdapterClaudeCodeOTLP {
				cfg.Adapters[i].Collector.applyDefaults()
			}
		}
	}
	if loaded.Parse.MaxRoundBytes > 0 {
		cfg.Parse.MaxRoundBytes = loaded.Parse.MaxRoundBytes
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
		c.Interval = 5 * time.Second
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
	localMetrics, receiverMetrics := false, false
	for i, a := range c.Adapters {
		if a.Name == "" {
			return fmt.Errorf("config: adapters[%d] has no name", i)
		}
		if seen[a.Name] {
			return fmt.Errorf("config: duplicate adapter %q", a.Name)
		}
		seen[a.Name] = true
		if a.Name == AdapterClaudeCodeOTLP && a.Enabled {
			if a.Listen == "" {
				return fmt.Errorf("config: adapter %q needs listen, the address the runtime's exporter is pointed at, such as 127.0.0.1:4317", a.Name)
			}
			receiverMetrics = receiverMetrics || a.Metrics
		}
		if a.Name == AdapterClaudeCodeLocal && a.Enabled && a.Metrics {
			localMetrics = true
		}
		if a.Name == AdapterClaudeCodeOTLP {
			if a.Collector != (Collector{}) || a.SourceRoot != "" || len(a.Include) > 0 || len(a.Exclude) > 0 || a.MetricsLookback != "" {
				return fmt.Errorf("config: adapter %q is a receiver: it takes listen and metrics, not collector, source_root, include, exclude or metrics_lookback", a.Name)
			}
			continue
		}
		if a.Name == AdapterClaudeCodeChanges && (a.Metrics || a.MetricsLookback != "" || a.Listen != "") {
			return fmt.Errorf("config: adapter %q reads change records only: it takes source_root, include, exclude and collector, not metrics, metrics_lookback or listen", a.Name)
		}
		if a.Collector.Mode != ModeWatch && a.Collector.Mode != ModeOnce {
			return fmt.Errorf("config: adapter %q: unknown collector mode %q", a.Name, a.Collector.Mode)
		}
		if _, err := a.Lookback(); err != nil {
			return fmt.Errorf("adapter %q: %w", a.Name, err)
		}
	}
	if p := c.Export.OTLP.Protocol; p != "grpc" && p != "http" {
		return fmt.Errorf("config: export.otlp.protocol is %q, want grpc or http", p)
	}
	if !c.Export.OTLP.SendLogs() && !c.Export.OTLP.SendMetrics() {
		return errors.New("config: export.otlp has both logs and metrics off; a push would send nothing")
	}
	// The same tokens must not be counted twice: derived from the
	// transcripts and received from the runtime's exporter are two sources
	// of one metric, and a root sends one of them.
	if localMetrics && receiverMetrics {
		return fmt.Errorf("config: %s has metrics on while %s is enabled with metrics; the two would count the same tokens twice, so turn one off", AdapterClaudeCodeLocal, AdapterClaudeCodeOTLP)
	}
	return nil
}

// ResolvedRoot returns storage.Root as an absolute path.
func (c *Config) ResolvedRoot() (string, error) {
	return filepath.Abs(c.Storage.Root)
}
