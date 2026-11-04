package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

func (p *Proxy) authorizeGraphQL(r *http.Request) error {
	var body map[string]any
	if r.Method == "GET" || r.Method == "HEAD" {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			return errDenied
		}
		q, err := parseQuery(r.URL.RawQuery)
		if err != nil {
			return err
		}
		body = map[string]any{"query": q.Get("query"), "operationName": q.Get("operationName")}
		if raw := q.Get("variables"); raw != "" {
			if uniqueJSON([]byte(raw)) != nil {
				return errDenied
			}
			var vars map[string]any
			if json.Unmarshal([]byte(raw), &vars) != nil {
				return errDenied
			}
			body["variables"] = vars
		}
	} else if r.Method == "POST" && r.URL.RawQuery == "" {
		var err error
		body, err = readJSONBody(r)
		if err != nil {
			return err
		}
	} else {
		return errDenied
	}
	query, _ := body["query"].(string)
	doc, err := parser.ParseQueryWithTokenLimit(&ast.Source{Input: query}, 100000)
	if err != nil {
		return fmt.Errorf("invalid GraphQL document")
	}
	name, _ := body["operationName"].(string)
	var op *ast.OperationDefinition
	names := map[string]bool{}
	for _, candidate := range doc.Operations {
		if names[candidate.Name] {
			return errDenied
		}
		names[candidate.Name] = true
		if candidate.Name == name || name == "" && len(doc.Operations) == 1 {
			if op != nil {
				return errDenied
			}
			op = candidate
		}
	}
	if op == nil {
		return fmt.Errorf("ambiguous GraphQL operation")
	}
	if op.Operation == ast.Query {
		return nil
	}
	if op.Operation != ast.Mutation || r.Method != "POST" {
		return errDenied
	}
	if !only(body, "query", "variables", "operationName") {
		return errDenied
	}
	vars, _ := body["variables"].(map[string]any)
	if vars == nil {
		vars = map[string]any{}
	}
	seen := map[string]bool{}
	for _, v := range op.VariableDefinitions {
		if seen[v.Variable] {
			return errDenied
		}
		seen[v.Variable] = true
		if _, exists := vars[v.Variable]; !exists && v.DefaultValue != nil {
			if !uniqueValue(v.DefaultValue) {
				return errDenied
			}
			val, e := v.DefaultValue.Value(nil)
			if e != nil {
				return errDenied
			}
			vars[v.Variable] = val
		}
	}
	fragments := map[string]*ast.FragmentDefinition{}
	for _, f := range doc.Fragments {
		if fragments[f.Name] != nil {
			return errDenied
		}
		fragments[f.Name] = f
	}
	budget := 100
	var walk func(ast.SelectionSet, map[string]bool) error
	walk = func(set ast.SelectionSet, visiting map[string]bool) error {
		for _, selection := range set {
			budget--
			if budget < 0 {
				return fmt.Errorf("too many mutation selections")
			}
			switch s := selection.(type) {
			case *ast.Field:
				if len(s.Arguments) != 1 || s.Arguments[0].Name != "input" || !uniqueValue(s.Arguments[0].Value) {
					return errDenied
				}
				value, e := s.Arguments[0].Value.Value(vars)
				if e != nil {
					return errDenied
				}
				input, ok := value.(map[string]any)
				if !ok {
					return errDenied
				}
				if e = p.mutation(r, s.Name, input); e != nil {
					return e
				}
			case *ast.InlineFragment:
				if e := walk(s.SelectionSet, visiting); e != nil {
					return e
				}
			case *ast.FragmentSpread:
				f := fragments[s.Name]
				if f == nil || visiting[s.Name] {
					return errDenied
				}
				visiting[s.Name] = true
				if e := walk(f.SelectionSet, visiting); e != nil {
					return e
				}
				delete(visiting, s.Name)
			default:
				return errDenied
			}
		}
		return nil
	}
	return walk(op.SelectionSet, map[string]bool{})
}

func parseQuery(raw string) (url.Values, error) {
	if len(raw) > maxJSON {
		return nil, errDenied
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errDenied
	}
	for _, v := range q {
		if len(v) != 1 {
			return nil, errDenied
		}
	}
	return q, nil
}

func uniqueValue(v *ast.Value) bool {
	seen := map[string]bool{}
	for _, c := range v.Children {
		if v.Kind == ast.ObjectValue && seen[c.Name] {
			return false
		}
		seen[c.Name] = true
		if !uniqueValue(c.Value) {
			return false
		}
	}
	return true
}

type gqlRepo struct {
	Name  string `json:"name"`
	Owner struct {
		Type  string `json:"__typename"`
		Login string `json:"login"`
	} `json:"owner"`
}
type gqlNode struct {
	Type   string `json:"__typename"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Owner  struct {
		Type  string `json:"__typename"`
		Login string `json:"login"`
	} `json:"owner"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository     gqlRepo  `json:"repository"`
	HeadRefName    string   `json:"headRefName"`
	HeadRepository *gqlRepo `json:"headRepository"`
}

const nodeQuery = `query ProxyPolicyNode($id: ID!) {
 node(id: $id) {
  __typename
  ... on Repository { name owner { __typename login } }
  ... on Issue { author { login } repository { name owner { __typename login } } }
  ... on PullRequest { author { login } headRefName headRepository { name owner { __typename login } } repository { name owner { __typename login } } }
  ... on Ref { name prefix repository { name owner { __typename login } } }
 }
}`

func (p *Proxy) node(r *http.Request, id string) (*gqlNode, error) {
	if id == "" {
		return nil, errDenied
	}
	var response struct {
		Data struct {
			Node *gqlNode `json:"node"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if p.api(r.Context(), "POST", "/graphql", map[string]any{"query": nodeQuery, "variables": map[string]any{"id": id}}, &response) != nil || len(response.Errors) > 0 || response.Data.Node == nil {
		return nil, errDenied
	}
	return response.Data.Node, nil
}

func (p *Proxy) gqlRepoAllowed(repo gqlRepo) bool {
	return p.repositoryAllowed(repo.Owner.Type, repo.Owner.Login, repo.Name)
}
func (p *Proxy) ownNode(n *gqlNode) bool {
	if (n.Type != "Issue" && n.Type != "PullRequest") || !p.gqlRepoAllowed(n.Repository) || !strings.EqualFold(n.Author.Login, p.login) {
		return false
	}
	return n.Type != "PullRequest" || p.branch(n.HeadRefName) && n.HeadRepository != nil && p.gqlRepoAllowed(*n.HeadRepository)
}

func (p *Proxy) mutation(r *http.Request, name string, in map[string]any) error {
	str := func(key string) string { s, _ := in[key].(string); return s }
	allowed := func(keys ...string) bool { return only(in, append(keys, "clientMutationId")...) }
	var id, kind string
	switch name {
	case "createIssue":
		if !allowed("repositoryId", "title", "body", "assigneeIds", "labelIds", "milestoneId", "issueTemplate") {
			return errDenied
		}
		id = str("repositoryId")
		kind = "Repository"
	case "createPullRequest":
		if !allowed("repositoryId", "title", "body", "baseRefName", "headRefName", "headRepositoryId", "draft", "maintainerCanModify") {
			return errDenied
		}
		id = str("repositoryId")
		kind = "Repository"
	case "updateIssue":
		if !allowed("id", "title", "body", "state", "stateReason", "assigneeIds", "labelIds", "milestoneId") {
			return errDenied
		}
		id = str("id")
		kind = "Issue"
	case "closeIssue", "reopenIssue", "deleteIssue":
		if !allowed("issueId", "stateReason") {
			return errDenied
		}
		id = str("issueId")
		kind = "Issue"
	case "updatePullRequest":
		if !allowed("pullRequestId", "title", "body", "baseRefName", "state", "assigneeIds", "labelIds", "milestoneId", "maintainerCanModify") {
			return errDenied
		}
		id = str("pullRequestId")
		kind = "PullRequest"
	case "closePullRequest", "reopenPullRequest", "convertPullRequestToDraft", "markPullRequestReadyForReview":
		if !allowed("pullRequestId") {
			return errDenied
		}
		id = str("pullRequestId")
		kind = "PullRequest"
	case "addComment":
		if !allowed("subjectId", "body") {
			return errDenied
		}
		id = str("subjectId")
	case "addLabelsToLabelable", "removeLabelsFromLabelable":
		if !allowed("labelableId", "labelIds") {
			return errDenied
		}
		id = str("labelableId")
	case "addAssigneesToAssignable", "removeAssigneesFromAssignable":
		if !allowed("assignableId", "assigneeIds") {
			return errDenied
		}
		id = str("assignableId")
	case "createRef":
		if !allowed("repositoryId", "name", "oid") || !strings.HasPrefix(str("name"), "refs/heads/") || !p.branch(strings.TrimPrefix(str("name"), "refs/heads/")) {
			return errDenied
		}
		id = str("repositoryId")
		kind = "Repository"
	case "updateRef", "deleteRef":
		if name == "updateRef" && !allowed("refId", "oid", "force") || name == "deleteRef" && !allowed("refId") {
			return errDenied
		}
		id = str("refId")
		kind = "Ref"
	default:
		return errDenied
	}
	n, err := p.node(r, id)
	if err != nil || kind != "" && n.Type != kind {
		return errDenied
	}
	switch kind {
	case "Repository":
		repo := gqlRepo{Name: n.Name, Owner: n.Owner}
		if !p.gqlRepoAllowed(repo) {
			return errDenied
		}
		if name == "createPullRequest" {
			headRepo := repo
			if headID := str("headRepositoryId"); headID != "" {
				h, e := p.node(r, headID)
				if e != nil || h.Type != "Repository" {
					return errDenied
				}
				headRepo = gqlRepo{Name: h.Name, Owner: h.Owner}
				if !p.gqlRepoAllowed(headRepo) {
					return errDenied
				}
			}
			head := str("headRefName")
			if owner, branch, ok := strings.Cut(head, ":"); ok {
				if !strings.EqualFold(owner, headRepo.Owner.Login) {
					return errDenied
				}
				head = branch
			}
			if !p.branch(head) {
				return errDenied
			}
		}
	case "Ref":
		if !p.gqlRepoAllowed(n.Repository) || n.Prefix != "refs/heads/" || !p.branch(n.Name) {
			return errDenied
		}
	default:
		if !p.ownNode(n) {
			return errDenied
		}
	}
	return nil
}
