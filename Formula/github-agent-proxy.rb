class GithubAgentProxy < Formula
  desc "Policy proxy for coding agents' GitHub and outbound HTTPS access"
  homepage "https://github.com/jacobfg/github-agent-proxy"
  # HEAD-only bootstrap. The first gobottle release replaces this formula.
  head "https://github.com/jacobfg/github-agent-proxy.git", branch: "main"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    build_version = build.head? ? "dev" : version.to_s
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{build_version}"), "./cmd/github-agent-proxy"
  end

  def caveats
    <<~EOS
      Your trusted sandbox launcher supplies the policy, PAT file and TLS paths
      and manages the proxy process. Installation does not start a proxy or
      modify credentials or certificate trust. See:
        https://github.com/jacobfg/github-agent-proxy/blob/main/docs/homebrew.md
    EOS
  end

  test do
    expected_version = build.head? ? "dev" : version.to_s
    assert_equal "github-agent-proxy #{expected_version}\n", shell_output("#{bin}/github-agent-proxy --version")
    assert_match "Copyright (c) 2018 Adam Scarr", shell_output("#{bin}/github-agent-proxy --licenses")
    system bin/"github-agent-proxy", "init-ca", "--cert", testpath/"ca.pem", "--key", testpath/"ca-key.pem"
    assert_match "BEGIN CERTIFICATE", (testpath/"ca.pem").read
    assert_equal 0600, (testpath/"ca-key.pem").stat.mode & 0777
  end
end
