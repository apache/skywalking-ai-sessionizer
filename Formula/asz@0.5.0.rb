# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

# Written by the homebrew skill in apache/skywalking-ai-sessionizer from the
# voted binary packages of 0.5.0.
class AszAT050 < Formula
  desc "Conversation-level observability for long-lived AI agents"
  homepage "https://github.com/apache/skywalking-ai-sessionizer"
  license "Apache-2.0"

  keg_only :versioned_formula

  # Each package comes from the GitHub release of v0.5.0, which carries
  # the voted files. Its URL keeps working after a newer version replaces
  # this one on the download site. The mirror, archive.apache.org, keeps
  # every version too, and Homebrew tries it only when GitHub fails.
  # Homebrew reads the version from the release tag in each URL.
  on_macos do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-darwin-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-darwin-arm64.tgz"
      sha256 "6bbcebdbe73186c1dd09171cd311d9c3d66634ddbfc33f545979cb0b0fb6150d"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-darwin-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-darwin-amd64.tgz"
      sha256 "1cabfb8ca4321999583e956d0d3a233ed00f591d8a77ea8fd03f75e243ac3033"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-linux-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-linux-arm64.tgz"
      sha256 "a673adff0819e00be280122ee4c5a8520cd860e1fbfcf409d127241b3615130a"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-linux-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.5.0/apache-skywalking-ai-sessionizer-0.5.0-bin-linux-amd64.tgz"
      sha256 "2c906aed003cd3733e4afc92240df2dfbfb928f233d09759f5558809dc5d9187"
    end
  end

  def install
    # Both binaries, from the one archive. Until 0.5.0 there were two
    # formulae fetching the identical archive and checksum and installing one
    # binary each, which only invited a machine to end up with asz of one
    # version and the recorder of another.
    bin.install "asz", "asz-changes"
    # Homebrew keeps a formula's license files at the root of its prefix. It
    # moves LICENSE and NOTICE there by itself, but not a directory, so
    # licenses/ goes with them here. It holds the license of every module
    # and font built into the binaries.
    prefix.install "LICENSE", "NOTICE", "licenses"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/asz version")
    # glossary needs no configuration and no input, and its table names
    # agent.call, so the program does real work, not only print its version.
    assert_match "agent.call", shell_output("#{bin}/asz glossary")
    # The recorder answers without a data directory, which is what a machine
    # with no agent runtime installed can check.
    assert_match version.to_s, shell_output("#{bin}/asz-changes version")
  end
end
