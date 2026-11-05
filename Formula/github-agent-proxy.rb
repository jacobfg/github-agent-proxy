class GithubAgentProxy < Formula
  desc "Policy proxy for coding agents' GitHub and outbound HTTPS access"
  homepage "https://github.com/jacobfg/github-agent-proxy"
  url "https://github.com/jacobfg/github-agent-proxy/archive/fa51bf78b83bf1a552e11d6f44dd740041d34c85.tar.gz?version=0.0.1"
  sha256 "721c6726d0a5d5df68653283255eca095e91ba25e79ca43a973b56beb3098f05"
  head "https://github.com/jacobfg/github-agent-proxy.git", branch: "main"

  bottle do
    root_url "https://ghcr.io/v2/jacobfg/github-agent-proxy"
    sha256 cellar: :any_skip_relocation, arm64_monterey: "da929a35ee0a352e3f4f853de74b22cf6a70404f3fd07f5f3ca10f12cc883552"
  end

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    ldflags = %W[
      -X main.version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/github-agent-proxy"
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
    assert_equal "github-agent-proxy #{version}\n", shell_output("#{bin}/github-agent-proxy --version")
    assert_match "Copyright (c) 2018 Adam Scarr", shell_output("#{bin}/github-agent-proxy --licenses")
    system bin/"github-agent-proxy", "init-policy", "--output", testpath/"policy.json"
    policy = JSON.parse((testpath/"policy.json").read)
    assert_equal [], policy.fetch("github").fetch("allowed_orgs")
    assert_equal [], policy.fetch("github").fetch("allowed_users")
    assert_equal [], policy.fetch("github").fetch("write_allow_repos")
    system bin/"github-agent-proxy", "init-ca", "--cert", testpath/"ca.pem", "--key", testpath/"ca-key.pem"
    assert_match "BEGIN CERTIFICATE", (testpath/"ca.pem").read
    assert_equal 0600, (testpath/"ca-key.pem").stat.mode & 0777
  end
end
