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

package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
)

var claudeManagedDir = claudecode.ManagedSettingsDir()

const claudeRetentionNotDetermined = "Claude Code's effective retention was not determined. " +
	"Project settings, command-line settings, device policy and settings managed by a server can change it. " +
	"From version 2.1.248, Claude Desktop and Cowork keep sessions at any age by default. " +
	"desktopSessionCleanupPeriodDays or a managed cleanupPeriodDays can limit them. " +
	"The organization's HIPAA configuration also ends this exemption when it applies. " +
	"Settings that cannot be read or parsed pause Claude Code's cleanup unless managed settings provide cleanupPeriodDays."

func retentionWarnings(ads []config.Adapter, retire time.Duration) []string {
	if retire <= 0 {
		return nil
	}
	var lines []string
	for _, ad := range ads {
		if !ad.Enabled {
			continue
		}
		switch ad.Name {
		case config.AdapterClaudeCodeLocal:
			lines = append(lines, claudeRetentionWarnings(ad, retire)...)
		case config.AdapterKiroLocal:
			lines = append(lines, retirementWarning("Kiro", "until it is deleted", retire))
		}
	}
	return lines
}

func claudeRetentionWarnings(ad config.Adapter, retire time.Duration) []string {
	if ad.SourceRoot != "" {
		return []string{
			"No Claude Code settings were inspected. An explicit source_root does not identify the user's configuration directory.",
			claudeRetentionNotDetermined,
		}
	}
	root, err := claudecode.ResolveSourceRoot("")
	if err != nil {
		return []string{fmt.Sprintf("No Claude Code settings were inspected. The configuration directory could not be resolved: %v", err), claudeRetentionNotDetermined}
	}
	dir := filepath.Dir(root)
	inspected := claudecode.InspectRetention(dir, claudeManagedDir)
	var lines []string
	for _, problem := range inspected.Problems {
		lines = append(lines, fmt.Sprintf("Claude Code settings: %v. No value was assumed for it.", problem))
	}
	days := inspected.Days
	basis := "inspected value"
	switch {
	case days > 0:
		kind := "user"
		if inspected.Managed {
			kind = "managed"
		}
		lines = append(lines, fmt.Sprintf("The inspected %s settings %q set cleanupPeriodDays to %s.", kind, inspected.Path, retentionPeriod(time.Duration(days)*24*time.Hour)))
	case len(inspected.Problems) == 0:
		days = 30
		basis = "default"
		lines = append(lines, fmt.Sprintf("No inspected file in %q or %q sets cleanupPeriodDays. Claude Code's default is 30 days.", dir, claudeManagedDir))
	default:
		lines = append(lines, "No valid cleanupPeriodDays value was found in the inspected files. No default was assumed.")
	}
	lines = append(lines, claudeRetentionNotDetermined)
	if days > 0 && retire < time.Duration(days)*24*time.Hour {
		lines = append(lines, "For the "+basis+": "+retirementWarning("Claude Code", "for "+retentionPeriod(time.Duration(days)*24*time.Hour), retire))
	}
	return lines
}

func retirementWarning(runtime, keeps string, retire time.Duration) string {
	return fmt.Sprintf("%s keeps a session %s; storage.retention.retire_after is %s. A session resumed after it is retired is not collected.", runtime, keeps, retentionPeriod(retire))
}

func retentionPeriod(period time.Duration) string {
	if period == 24*time.Hour {
		return "1 day"
	}
	if period%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", period/(24*time.Hour))
	}
	return period.String()
}

func printSourceRetention(cfg *config.Config, ad config.Adapter) error {
	_, retire, err := cfg.Storage.Retention.Durations()
	if err != nil {
		return err
	}
	for _, line := range retentionWarnings([]config.Adapter{ad}, retire) {
		fmt.Printf("warning  : %s\n", line)
	}
	return nil
}
