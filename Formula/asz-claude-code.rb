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
class AszClaudeCode < Formula
  desc "Claude Code plugin that records which files each tool call changed"
  homepage "https://github.com/apache/skywalking-ai-sessionizer"
  license "Apache-2.0"

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
    # The plugin's hooks run asz-claude-plugin by name, so it goes on PATH.
    bin.install "asz-claude-plugin"
    prefix.install "LICENSE", "NOTICE", "licenses"
  end

  def caveats
    # A formula cannot change the user's Claude Code configuration, so the
    # plugin itself is installed by these commands. They add the marketplace
    # at the tag of this version, so its hooks are the ones this binary was
    # released with.
    <<~EOS
      asz-claude-plugin, the binary of the Claude Code plugin, is on your
      PATH. To install the plugin itself into Claude Code:
        claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v#{version}" --sparse .claude-plugin plugins/claude-code/plugin
        claude plugin install asz-changes@skywalking-ai-sessionizer

      To move an installed plugin to this version without losing the records
      asz has not collected, see:
        https://github.com/apache/skywalking-ai-sessionizer/blob/v#{version}/docs/en/setup/claude-code-plugin.md#upgrade
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/asz-claude-plugin version")
    # Claude Code names the plugin's data directory in CLAUDE_PLUGIN_DATA.
    # status reads the settings there and compiles the exclusion rules. With
    # no settings file it takes the defaults, the set named standard-v1.
    ENV["CLAUDE_PLUGIN_DATA"] = (testpath/"plugin-data").to_s
    assert_match "standard-v1", shell_output("#{bin}/asz-claude-plugin status")
  end
end
