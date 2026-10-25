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
# voted binary packages of 0.6.0.
class Asz < Formula
  desc "Conversation-level observability for long-lived AI agents"
  homepage "https://github.com/apache/skywalking-ai-sessionizer"
  license "Apache-2.0"

  # Each package comes from the GitHub release of v0.6.0, which carries
  # the voted files. Its URL keeps working after a newer version replaces
  # this one on the download site. The mirror, archive.apache.org, keeps
  # every version too, and Homebrew tries it only when GitHub fails.
  # Homebrew reads the version from the release tag in each URL.
  on_macos do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-darwin-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-darwin-arm64.tgz"
      sha256 "118d0a506ee11a2e1ca277aed92c72bc95c1fb39a93f830ef49e1a50aab7d9d3"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-darwin-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-darwin-amd64.tgz"
      sha256 "cd4ce7e71511b1b45352972849b3ef1ecff3e0f41ec5e98fa83dbf5ebf495abd"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-linux-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-linux-arm64.tgz"
      sha256 "c05130360c5dc2b492561dbf161f849d65930a2ba4c56aa0c47b7b8451cded14"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-linux-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.6.0/apache-skywalking-ai-sessionizer-0.6.0-bin-linux-amd64.tgz"
      sha256 "6a382ca35704224e3b194559d1605411c0759c7cf8a2a7d3a84af96f6b1c8d3f"
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
