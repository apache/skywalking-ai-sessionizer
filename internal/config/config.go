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
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration document.
type Config struct {
	Storage  Storage   `yaml:"storage"`
	Adapters []Adapter `yaml:"adapters"`
	Export   Export    `yaml:"export"`
}

// Export configures where collected data is sent.
type Export struct {
	OTLP OTLP `yaml:"otlp"`
}

// OTLP configures the OpenTelemetry logs push: every landed file and every
// round, one log record per line, over OTLP/HTTP.
type OTLP struct {
	// Endpoint is the receiver's base URL; the logs path is appended. Empty
	// means asz push has nowhere to send and refuses to run.
	Endpoint string `yaml:"endpoint"`
	// ServiceName is the service every record is attributed to. Empty means
	// the project directory each session was recorded under.
	ServiceName string `yaml:"service_name"`
	// InstanceID is sent as service.instance.id and identifies this sender.
	// Empty means a new UUID each time asz push starts.
	InstanceID string `yaml:"instance_id"`
	// Layer is sent as service.layer, which the SkyWalking OAP uses to place
	// the service.
	Layer string `yaml:"layer"`
	// Headers are added to every request, for example an authorization token.
	Headers map[string]string `yaml:"headers"`
	// BatchBytes is how much body text one request carries at most.
	BatchBytes int64 `yaml:"batch_bytes"`
	// Interval is how long asz push sleeps between passes in watch mode.
	Interval time.Duration `yaml:"interval"`
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

	Collector Collector `yaml:"collector"`
}

// Collector controls collection cadence for one adapter.
type Collector struct {
	// Mode is "watch" (poll continuously) or "once" (single pass, then exit).
	// "once" is the backfill path over history that already exists.
	Mode string `yaml:"mode"`

	Interval time.Duration `yaml:"interval"`

	// MaxDeltaBytes caps how much of a source is landed in a single file, so a
	// large catch-up is split rather than producing one enormous delta. It is
	// also the largest unit a receiver has to accept in one message when
	// landed files travel, which is why the default fits the 4 MiB limit an
	// OpenTelemetry Collector applies out of the box.
	MaxDeltaBytes int64 `yaml:"max_delta_bytes"`
}

const (
	ModeWatch = "watch"
	ModeOnce  = "once"

	// AdapterClaudeCodeLocal reads Claude Code's local files. Pull posture.
	AdapterClaudeCodeLocal = "claude-code-local"
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
				Interval:      5 * time.Second,
				MaxDeltaBytes: 4 << 20,
			},
		}},
		Export: Export{OTLP: OTLP{
			Layer:      "AI-AGENT",
			BatchBytes: 1 << 20,
			Interval:   5 * time.Second,
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
			cfg.Adapters[i].Collector.applyDefaults()
		}
	}
	o := &loaded.Export.OTLP
	if o.Endpoint != "" {
		cfg.Export.OTLP.Endpoint = o.Endpoint
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
	if o.Interval > 0 {
		cfg.Export.OTLP.Interval = o.Interval
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
		c.MaxDeltaBytes = 4 << 20
	}
}

// Validate reports configuration that cannot be acted on.
func (c *Config) Validate() error {
	if c.Storage.Root == "" {
		return fmt.Errorf("config: storage.root must not be empty")
	}
	seen := map[string]bool{}
	for i, a := range c.Adapters {
		if a.Name == "" {
			return fmt.Errorf("config: adapters[%d] has no name", i)
		}
		if seen[a.Name] {
			return fmt.Errorf("config: duplicate adapter %q", a.Name)
		}
		seen[a.Name] = true
		if a.Collector.Mode != ModeWatch && a.Collector.Mode != ModeOnce {
			return fmt.Errorf("config: adapter %q: unknown collector mode %q", a.Name, a.Collector.Mode)
		}
	}
	return nil
}

// ResolvedRoot returns storage.Root as an absolute path.
func (c *Config) ResolvedRoot() (string, error) {
	return filepath.Abs(c.Storage.Root)
}
