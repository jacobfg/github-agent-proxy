---
name: github-agent-proxy
description: Discover repository write eligibility and branch naming rules from a running GitHub agent proxy. Use when working with GitHub repositories through this proxy, before pushing branches or creating or editing issues and pull requests, or when explaining a proxy denial.
---

# GitHub agent proxy

Use the running proxy's loaded policy to plan GitHub work. The proxy owns the
real GitHub credentials; the agent uses its existing configured `gh` and `git`
commands. Policy eligibility does not grant permission to perform work outside
the user's request.

## Discover the active policy

Use the proxy listener URL supplied by the launcher. Normally it is in
`https_proxy` or `HTTPS_PROXY`; use the endpoint actually configured for this
session, including its port. If neither is set, inspect the repository's Git
proxy setting or the launcher-provided context. Do not assume a host port from
the status response: `listen_addr` may be an internal container address.

For an environment-configured listener:

```sh
proxy_url="${https_proxy:-${HTTPS_PROXY:-}}"
if [ -n "$proxy_url" ]; then
  curl --noproxy '*' --fail --silent --show-error \
    --connect-timeout 5 --max-time 15 "${proxy_url%/}/status"
fi
```

Contact this local diagnostic endpoint directly, without an Authorization
header. Keep TLS verification enabled and use the launcher's public CA bundle
(`--cacert /path/to/ca-bundle.pem` if needed). `--noproxy '*'` here only avoids
sending the diagnostic request back through the proxy; GitHub requests must
continue using their existing proxy configuration.

Read `allowed_orgs`, `allowed_users`, `branch_prefix`, `token_owner`, and
`policy.github.write_allow_repos` / `policy.github.write_deny_repos` from the
response. Status is a loaded snapshot, not a policy file to write back. Refresh
it when planning writes in a new session or after a proxy restart or unexpected
denial. `/healthz` reports local liveness only, not GitHub permissions.

If the endpoint is unknown, unreachable, or returns incomplete policy data,
report that write eligibility is unknown. Continue useful reads or local work;
do not probe permissions with a write. Do not seek the proxy's PAT, read its
private policy/key files, or reconfigure the proxy to make a denied write work.

## Classify the target repositories

Identify the actual target owner/repository from the user's request and the
relevant Git remote. For a push, check its push URL, which can differ from its
fetch URL. Normalize repository names to `owner/repo`, without `.git`, and
compare owner and repository names case-insensitively.

Get the repository owner's type through the configured proxy when needed:

```sh
gh api repos/OWNER/REPO --jq '{repository: .full_name, owner_type: .owner.type}'
```

For each target, apply these rules in order:

1. An `Organization` owner must appear in `allowed_orgs`; a `User` owner must
   appear in `allowed_users`. A login in the wrong list does not qualify. Empty
   owner lists authorize no writes of that type. If metadata cannot establish
   the owner type, report the uncertainty rather than guessing.
2. An exact `owner/repo` match in `write_deny_repos` blocks every write, even if
   the repository also appears in the allowlist. Repository rules are exact
   names, not wildcards or regexes.
3. `write_allow_repos: []` blocks every repository write. A nonempty array
   permits only listed repositories, subject to the owner rule. In a complete
   `policy.github` object, a missing or `null` allowlist in status means no
   additional repository allowlist restriction. The entire policy being absent
   does not mean unrestricted access.

Describe blocked targets as **read-only under the proxy policy** and permitted
targets as **write-eligible under the proxy policy**, explaining the matching
rule. Reads have no owner, repository, author, or branch restrictions, but still
depend on the PAT's GitHub access and destination blocks. Write eligibility is
also subject to GitHub's PAT permissions, repository rules, and the operation
restrictions below. Neither status nor a successful read proves a write will
succeed. Evaluate the repositories relevant to the task; an omitted repository
allowlist is not an enumerable list of every accessible private repository.

## Choose branches and operations

Use `branch_prefix` exactly as returned, preserving case and the trailing `/`.
Append a descriptive suffix to create a valid Git branch, for example prefix
`agp/` plus `fix-timeout` gives `agp/fix-timeout`. Check an existing destination
branch against the prefix before pushing. In an authorized push, specify the
destination explicitly, such as `git push origin HEAD:refs/heads/agp/fix-timeout`
when that is the reported prefix. Force pushes and deletes are supported only
inside that namespace; their policy eligibility is not user authorization.

For PRs, the head branch must use the prefix, and both head and base
repositories must be write-eligible. The base branch can be `main`; opening a
PR does not write it. Cross-repository PR creation requires GraphQL with
`headRepositoryId`; REST PR creation is limited to the same repository.

Issues have no branch-prefix condition. Editing/deleting issues or editing PRs
requires their author to match `token_owner`, not the agent's Git author or
local login. Comments require an issue/PR owned by that identity; editing or
deleting a comment also requires that identity to own the comment. Issue
deletion uses GraphQL. Do not assume an eligible repository permits every
GitHub mutation: merges, auto-merge, reviews, tags, repository administration,
Actions dispatches, releases, and LFS uploads are not supported writes.

When reporting a denial, distinguish the proxy's rule from a GitHub permission
failure using the response and active status. Offer a compatible branch or
read-only/local result within the user's task. Do not bypass the proxy, obtain
alternate credentials, or retry the same denied mutation unchanged.
