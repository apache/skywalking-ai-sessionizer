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

package config

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Remote is one asz server that asz pull copies conversations from. Its name
// identifies the copies on this machine, so the URL can change without
// losing them.
type Remote struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Exactly one of the two names where the token is: a file, or an
	// environment variable. Neither is read until the remote is contacted.
	TokenFile string `yaml:"token_file"`
	TokenEnv  string `yaml:"token_env"`
	// CAFile replaces the system's trusted roots for this remote.
	CAFile         string `yaml:"ca_file"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
	// ServerName is the host the certificate is checked against, when the
	// URL's host differs, as through a tunnel.
	ServerName string `yaml:"server_name"`
}

// RemoteServer is what asz view and asz server need to answer pulls, and to
// protect the page and its API with the same tokens.
type RemoteServer struct {
	// TokensFile names the tokens that may read this instance. Empty serves
	// no pull routes and leaves the page as it was.
	TokensFile string    `yaml:"tokens_file"`
	TLS        ServerTLS `yaml:"tls"`
	// MaxConcurrentPerToken bounds the requests one token runs at once.
	MaxConcurrentPerToken int `yaml:"max_concurrent_per_token"`
	// SnapshotTTL is how long a frozen snapshot stays after its last use.
	SnapshotTTL string `yaml:"snapshot_ttl"`
}

// ServerTLS is the certificate asz serves with. Both files or neither.
type ServerTLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile makes the server require a client certificate it signs.
	ClientCAFile string `yaml:"client_ca_file"`
}

// The bounds of the pull settings.
const (
	// MinSnapshotTTL is the shortest time to live either setting takes.
	MinSnapshotTTL = 2 * time.Second
	// DefaultRemoteSnapshotTTL keeps a pulled snapshot, and the judge runs
	// in it, for 30 days from publication.
	DefaultRemoteSnapshotTTL = "720h"
	// DefaultServerSnapshotTTL keeps a frozen snapshot for an hour after it
	// was last used.
	DefaultServerSnapshotTTL = "1h"
	// DefaultMaxConcurrentPerToken and MaxConcurrentPerTokenLimit bound
	// max_concurrent_per_token.
	DefaultMaxConcurrentPerToken = 2
	MaxConcurrentPerTokenLimit   = 64
)

// RemoteSnapshotTTLDuration is remote_snapshot_ttl as a duration.
func (c *Config) RemoteSnapshotTTLDuration() (time.Duration, error) {
	return snapshotTTL("remote_snapshot_ttl", c.RemoteSnapshotTTL)
}

// SnapshotTTLDuration is remote_server.snapshot_ttl as a duration.
func (s RemoteServer) SnapshotTTLDuration() (time.Duration, error) {
	return snapshotTTL("remote_server.snapshot_ttl", s.SnapshotTTL)
}

// SweepInterval is how often a serving owner renews in-flight work and
// sweeps expired snapshots: half the time to live, between one second and
// one minute. A ticker refuses a zero interval.
func (s RemoteServer) SweepInterval() (time.Duration, error) {
	ttl, err := s.SnapshotTTLDuration()
	if err != nil {
		return 0, err
	}
	return max(time.Second, min(time.Minute, ttl/2)), nil
}

// TLSEnabled reports whether the server serves TLS itself.
func (s RemoteServer) TLSEnabled() bool { return s.TLS.CertFile != "" }

// Enabled reports whether the server takes tokens, which also enables the
// pull routes.
func (s RemoteServer) Enabled() bool { return s.TokensFile != "" }

func snapshotTTL(name, value string) (time.Duration, error) {
	// time.ParseDuration refuses a value past its range, so an overflow is an
	// error here rather than a wrapped duration.
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("config: %s is %q: %w", name, value, err)
	}
	if d < MinSnapshotTTL {
		return 0, fmt.Errorf("config: %s is %q; it is at least %s, and nothing turns expiry off", name, value, MinSnapshotTTL)
	}
	return d, nil
}

// Origin is the scheme, the host in lower case and the port, with the
// default port written out. Two remotes at one origin with one token share a
// limit on requests in flight.
func (r Remote) Origin() (string, error) {
	u, err := r.parsedURL()
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port), nil
}

// BaseURL is the URL with any trailing slash taken off, so the routes go
// under its path when an ingress serves asz below one. An escaped path keeps
// its escapes: team%2Fprod is one segment, and team/prod is two.
func (r Remote) BaseURL() (*url.URL, error) {
	u, err := r.parsedURL()
	if err != nil {
		return nil, err
	}
	// The slash is taken off the path as written, escapes and all, and the
	// decoded path follows from it, so the two always agree: team%2Fprod%2F
	// ends in an escaped slash, which is part of a segment, and stays.
	escaped := strings.TrimSuffix(u.EscapedPath(), "/")
	path, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("config: remote %q: url %q: %w", r.Name, r.URL, err)
	}
	u.Path, u.RawPath = path, escaped
	return u, nil
}

func (r Remote) parsedURL() (*url.URL, error) {
	u, err := url.Parse(r.URL)
	if err != nil {
		return nil, fmt.Errorf("config: remote %q: url %q: %w", r.Name, r.URL, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("config: remote %q: url %q is not http or https", r.Name, r.URL)
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("config: remote %q: url %q has no host", r.Name, r.URL)
	case u.User != nil:
		return nil, fmt.Errorf("config: remote %q: url %q carries a user; give the token through token_file or token_env", r.Name, r.URL)
	case u.RawQuery != "" || u.ForceQuery:
		return nil, fmt.Errorf("config: remote %q: url %q has a query", r.Name, r.URL)
	case u.Fragment != "" || strings.Contains(r.URL, "#"):
		return nil, fmt.Errorf("config: remote %q: url %q has a fragment", r.Name, r.URL)
	}
	return u, nil
}

// Remote returns the remote of that name.
func (c *Config) Remote(name string) (Remote, bool) {
	for _, r := range c.Remotes {
		if r.Name == name {
			return r, true
		}
	}
	return Remote{}, false
}

// checkRemotes checks the pull settings' structure. No credential is read.
func (c *Config) checkRemotes() error {
	if _, err := c.RemoteSnapshotTTLDuration(); err != nil {
		return err
	}
	folded := map[string]string{}
	for i, r := range c.Remotes {
		if err := storage.CheckRemoteName(r.Name); err != nil {
			return fmt.Errorf("config: remotes[%d]: %w", i, err)
		}
		key := strings.ToLower(r.Name)
		if prev, dup := folded[key]; dup {
			if prev == r.Name {
				return fmt.Errorf("config: remote %q is named twice", r.Name)
			}
			return fmt.Errorf("config: remotes %q and %q differ only in letter case, and their copies would share a directory on some machines", prev, r.Name)
		}
		folded[key] = r.Name
		if r.URL == "" {
			return fmt.Errorf("config: remote %q has no url", r.Name)
		}
		if _, err := r.parsedURL(); err != nil {
			return err
		}
		if (r.TokenFile == "") == (r.TokenEnv == "") {
			return fmt.Errorf("config: remote %q takes exactly one of token_file and token_env", r.Name)
		}
		if (r.ClientCertFile == "") != (r.ClientKeyFile == "") {
			return fmt.Errorf("config: remote %q: set client_cert_file and client_key_file both, or neither", r.Name)
		}
	}
	s := c.RemoteServer
	if _, err := s.SnapshotTTLDuration(); err != nil {
		return err
	}
	if s.MaxConcurrentPerToken < 1 || s.MaxConcurrentPerToken > MaxConcurrentPerTokenLimit {
		return fmt.Errorf("config: remote_server.max_concurrent_per_token is %d; it is 1 to %d", s.MaxConcurrentPerToken, MaxConcurrentPerTokenLimit)
	}
	if (s.TLS.CertFile == "") != (s.TLS.KeyFile == "") {
		return errors.New("config: remote_server.tls: set cert_file and key_file both, or neither")
	}
	if s.TLS.ClientCAFile != "" && s.TLS.CertFile == "" {
		return errors.New("config: remote_server.tls.client_ca_file asks for client certificates, which needs TLS: set cert_file and key_file too")
	}
	return nil
}

// ResolvePath turns a configured file path into the one to open: a leading
// ~/ is the home directory, and a relative path is taken from the working
// directory, as storage.root is.
func ResolvePath(p string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: %s: %w", p, err)
		}
		p = filepath.Join(home, rest)
	}
	return filepath.Abs(p)
}

// ReadSupplied reads a file a person named in the configuration, such as a
// token or a certificate, after the protected-path check and through its
// checked parent. It is called when the operation that needs the file opens
// it, never when the configuration loads. A file larger than limit is
// refused rather than read whole.
func ReadSupplied(p string, limit int64) ([]byte, error) {
	resolved, err := ResolvePath(p)
	if err != nil {
		return nil, err
	}
	f, err := storage.OpenSuppliedRead(resolved, "")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if limit <= 0 || limit == math.MaxInt64 {
		return nil, fmt.Errorf("config: %s: no read limit", p)
	}
	b, err := readLimited(f, limit)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", p, err)
	}
	return b, nil
}

// readLimited reads r whole, refusing more than limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}

// resolveCredentialPaths makes every credential path absolute, a leading ~/
// expanded, as loading leaves them: one read later reads the file the
// configuration named when it was loaded, whatever the working directory is
// by then.
func (c *Config) resolveCredentialPaths() error {
	paths := []*string{&c.RemoteServer.TokensFile, &c.RemoteServer.TLS.CertFile, &c.RemoteServer.TLS.KeyFile, &c.RemoteServer.TLS.ClientCAFile}
	for i := range c.Remotes {
		r := &c.Remotes[i]
		paths = append(paths, &r.TokenFile, &r.CAFile, &r.ClientCertFile, &r.ClientKeyFile)
	}
	for _, p := range paths {
		if *p == "" {
			continue
		}
		resolved, err := ResolvePath(*p)
		if err != nil {
			return err
		}
		*p = resolved
	}
	return nil
}
