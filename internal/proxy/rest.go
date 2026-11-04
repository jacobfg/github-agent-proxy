package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type repository struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}
type issue struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	PullRequest *json.RawMessage `json:"pull_request"`
}
type pull struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string      `json:"ref"`
		Repo *repository `json:"repo"`
	} `json:"head"`
}

func (p *Proxy) repoAllowed(r *http.Request, owner, name string) bool {
	if !p.config.Organizations[strings.ToLower(owner)] && !p.config.Users[strings.ToLower(owner)] {
		return false
	}
	var repo repository
	return p.api(r.Context(), "GET", "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &repo) == nil && p.repositoryAllowed(repo.Owner.Type, repo.Owner.Login, repo.Name) && strings.EqualFold(repo.Owner.Login, owner) && strings.EqualFold(repo.Name, name)
}

func (p *Proxy) ownPull(r *http.Request, base, id string) bool {
	var pr pull
	return p.api(r.Context(), "GET", base+"/pulls/"+id, nil, &pr) == nil && strings.EqualFold(pr.User.Login, p.login) && p.branch(pr.Head.Ref) && pr.Head.Repo != nil && p.repositoryAllowed(pr.Head.Repo.Owner.Type, pr.Head.Repo.Owner.Login, pr.Head.Repo.Name)
}

func (p *Proxy) ownIssue(r *http.Request, base, id string) bool {
	var item issue
	if p.api(r.Context(), "GET", base+"/issues/"+id, nil, &item) != nil || !strings.EqualFold(item.User.Login, p.login) {
		return false
	}
	return item.PullRequest == nil || p.ownPull(r, base, id)
}

func (p *Proxy) authorizeAPI(r *http.Request, path string) error {
	if path == "/graphql" {
		return p.authorizeGraphQL(r)
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		return nil
	}
	if r.Method == "POST" && (path == "/markdown" || path == "/markdown/raw") {
		return nil
	}
	if r.URL.RawQuery != "" {
		return errDenied
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" || !p.repoAllowed(r, parts[1], parts[2]) {
		return errDenied
	}
	base := "/repos/" + parts[1] + "/" + parts[2]
	tail := parts[3:]
	body, err := readJSONBody(r)
	if err != nil {
		return err
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	method := r.Method
	switch tail[0] {
	case "issues":
		if len(tail) == 1 && method == "POST" && only(body, "title", "body", "assignees", "milestone", "labels", "type") && str("title") != "" {
			return nil
		}
		if len(tail) == 2 && number(tail[1]) && method == "PATCH" && only(body, "title", "body", "assignees", "milestone", "labels", "state", "state_reason", "type") && p.ownIssue(r, base, tail[1]) {
			return nil
		}
		if len(tail) == 3 && number(tail[1]) && tail[2] == "comments" && method == "POST" && only(body, "body") && str("body") != "" && p.ownIssue(r, base, tail[1]) {
			return nil
		}
		if len(tail) == 3 && tail[1] == "comments" && number(tail[2]) && (method == "PATCH" && only(body, "body") || method == "DELETE" && len(body) == 0) {
			var comment struct {
				User struct {
					Login string `json:"login"`
				} `json:"user"`
				IssueURL string `json:"issue_url"`
			}
			if p.api(r.Context(), "GET", base+"/issues/comments/"+tail[2], nil, &comment) == nil && strings.EqualFold(comment.User.Login, p.login) && strings.HasPrefix(comment.IssueURL, "https://api.github.com"+base+"/issues/") {
				id := strings.TrimPrefix(comment.IssueURL, "https://api.github.com"+base+"/issues/")
				if number(id) && p.ownIssue(r, base, id) {
					return nil
				}
			}
		}
	case "pulls":
		if len(tail) == 1 && method == "POST" && only(body, "title", "body", "head", "base", "draft", "maintainer_can_modify", "issue") {
			head := str("head")
			if strings.Contains(head, ":") {
				owner, branch, _ := strings.Cut(head, ":")
				if !strings.EqualFold(owner, parts[1]) {
					return errDenied
				}
				head = branch
			}
			if !p.branch(head) {
				return errDenied
			}
			if id, ok := body["issue"]; ok {
				n, ok := id.(json.Number)
				if !ok || !number(string(n)) || !p.ownIssue(r, base, string(n)) {
					return errDenied
				}
			}
			return nil
		}
		if len(tail) == 2 && number(tail[1]) && method == "PATCH" && only(body, "title", "body", "state", "base", "maintainer_can_modify") && p.ownPull(r, base, tail[1]) {
			return nil
		}
	case "git":
		if len(tail) == 2 && tail[1] == "refs" && method == "POST" && only(body, "ref", "sha") && strings.HasPrefix(str("ref"), "refs/heads/") && p.branch(strings.TrimPrefix(str("ref"), "refs/heads/")) {
			return nil
		}
		if len(tail) >= 4 && tail[1] == "refs" && tail[2] == "heads" && p.branch(strings.Join(tail[3:], "/")) {
			if method == "PATCH" && only(body, "sha", "force") || method == "DELETE" && len(body) == 0 {
				return nil
			}
		}
	case "contents":
		if len(tail) >= 2 && (method == "PUT" || method == "DELETE") && only(body, "message", "content", "sha", "branch", "committer", "author") && p.branch(str("branch")) {
			return nil
		}
	case "branches":
		if len(tail) >= 3 && tail[len(tail)-1] == "rename" && method == "POST" && only(body, "new_name") && p.branch(strings.Join(tail[1:len(tail)-1], "/")) && p.branch(str("new_name")) {
			return nil
		}
	}
	return errDenied
}

func number(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == s
}
func only(body map[string]any, keys ...string) bool {
	for k := range body {
		found := false
		for _, allowed := range keys {
			if k == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func readJSONBody(r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxJSON+1))
	if err != nil || len(b) > maxJSON {
		return nil, fmt.Errorf("invalid or oversized JSON body")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.TransferEncoding = nil
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	if err = uniqueJSON(b); err != nil {
		return nil, err
	}
	var result map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&result) != nil || result == nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return result, nil
}

// Reject duplicate keys at every depth: the policy and GitHub must not choose
// different values from the same payload. Also reject concatenated documents.
func uniqueJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 100 {
			return fmt.Errorf("JSON nesting too deep")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, err = d.Token()
				if err != nil {
					return err
				}
				key, ok := t.(string)
				if !ok || seen[key] {
					return fmt.Errorf("duplicate or invalid JSON key")
				}
				seen[key] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON")
		}
		_, err = d.Token()
		return err
	}
	if err := value(0); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}
