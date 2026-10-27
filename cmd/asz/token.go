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
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

const tokenUsage = `Usage:
  asz token create [-file FILE] NAME   make a token for an asz server's tokens file

The token is printed once, with the line its tokens file takes: NAME and the
token's SHA-256. With -file the line is added to FILE, under a lock beside it;
a name or a token FILE names already is refused. A deployment that mounts its
tokens file from a secret adds the line there instead.
`

// cmdToken makes a token for an asz server. It reads no storage root.
func cmdToken(args []string) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprint(os.Stderr, tokenUsage)
		return 2
	}
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, tokenUsage) }
	file := fs.String("file", "", "the tokens file to add the line to")
	_ = fs.Parse(args[1:])
	if fs.NArg() != 1 {
		fmt.Fprint(os.Stderr, tokenUsage)
		return 2
	}
	name := fs.Arg(0)
	if err := storage.CheckRemoteName(name); err != nil {
		return fail(err)
	}
	token, hash, err := server.NewToken()
	if err != nil {
		return fail(err)
	}
	if *file != "" {
		path, err := config.ResolvePath(*file)
		if err != nil {
			return fail(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		// Nothing is printed of a token whose line was not written: a
		// person would keep a token no server takes.
		if err := server.AddToken(ctx, path, name, hash, nil); err != nil {
			return fail(err)
		}
		fmt.Printf("token %s\nadded to %s: %s\n", token, path, server.TokenLine(name, hash))
		return 0
	}
	fmt.Printf("token %s\ntokens file line: %s\n", token, server.TokenLine(name, hash))
	return 0
}
