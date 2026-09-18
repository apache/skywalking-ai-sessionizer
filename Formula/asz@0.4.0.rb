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
# voted binary packages of 0.4.0.
class AszAT040 < Formula
  desc "Conversation-level observability for long-lived AI agents"
  homepage "https://github.com/apache/skywalking-ai-sessionizer"
  license "Apache-2.0"

  keg_only :versioned_formula

  # Each package comes from the GitHub release of v0.4.0, which carries
  # the voted files. Its URL keeps working after a newer version replaces
  # this one on the download site. The mirror, archive.apache.org, keeps
  # every version too, and Homebrew tries it only when GitHub fails.
  # Homebrew reads the version from the release tag in each URL.
  on_macos do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-darwin-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-darwin-arm64.tgz"
      sha256 "44b15c9896c0d30aabd16c7194473c5369cc4f123fc852bc7c41eb7d1b9fe01d"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-darwin-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-darwin-amd64.tgz"
      sha256 "70ac46dbaf9756c79cf8fef682a8fb1225522a2b0c0c2608c97023278d014620"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-linux-arm64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-linux-arm64.tgz"
      sha256 "a0a9f2a0b9abd4e81f6587f395b54d5a67aa3c92039a2d943c6dd08c84955a3b"
    end
    on_intel do
      url "https://github.com/apache/skywalking-ai-sessionizer/releases/download/v0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-linux-amd64.tgz"
      mirror "https://archive.apache.org/dist/skywalking/ai-sessionizer/0.4.0/apache-skywalking-ai-sessionizer-0.4.0-bin-linux-amd64.tgz"
      sha256 "8dcce356f7fdcd3aaf862f6167fbee6bc993b29749ff3261a765bc8a10e3cef6"
    end
  end

  def install
    bin.install "asz"
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
  end
end
