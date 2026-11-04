class GithubAgentProxy < Formula
  desc "Policy proxy for coding agents' GitHub and outbound HTTPS access"
  homepage "https://github.com/jacobfg/github-agent-proxy"
  # BEGIN RELEASE SOURCE
  # Stable source metadata is added by the first release workflow.
  # END RELEASE SOURCE
  head "https://github.com/jacobfg/github-agent-proxy.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  def install
    build_version = build.head? ? "dev" : version.to_s
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{build_version}"), "./cmd/github-agent-proxy"
    pkgshare.install "examples/homebrew-policy.json" => "policy.example.json"
    doc.install "README.md", "docs/policy.md", "docs/homebrew.md", "THIRD_PARTY_NOTICES.md"
  end

  def caveats
    <<~EOS
      Example policy: #{opt_pkgshare}/policy.example.json
      Your trusted sandbox launcher supplies the policy, PAT file and TLS paths
      and manages the proxy process. Installation does not start a proxy or
      modify your policy, credentials or certificate trust.
    EOS
  end

  test do
    expected_version = build.head? ? "dev" : version.to_s
    assert_equal "github-agent-proxy #{expected_version}\n", shell_output("#{bin}/github-agent-proxy --version")
    system bin/"github-agent-proxy", "init-ca", "--cert", testpath/"ca.pem", "--key", testpath/"ca-key.pem"
    assert_match "BEGIN CERTIFICATE", (testpath/"ca.pem").read
    assert_equal 0600, (testpath/"ca-key.pem").stat.mode & 0777
    assert_match '"write_allow_repos": []', (pkgshare/"policy.example.json").read
  end
end
