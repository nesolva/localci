package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ResolveHeads implements Forge: each repo uses the trigger's own head when it
// is the trigger's repo, else the open PR from the same branch name (opened from
// a branch of that repo, never a fork), else its default branch. A fork names
// its own branches, so a fork trigger always uses the other repos' default
// branches: it never tests, nor posts statuses on, someone else's PR.
func (g *GitHub) ResolveHeads(ctx context.Context, repos []string, t Trigger) ([]Head, error) {
	heads := make([]Head, 0, len(repos))
	for _, repo := range repos {
		if strings.EqualFold(repo, t.Repo) {
			heads = append(heads, Head{Repo: repo, Branch: t.Branch, SHA: t.Head, PR: t.PR})
			continue
		}
		branch := t.Branch
		if t.Fork {
			branch = ""
		}
		h, err := g.resolveOne(ctx, repo, branch)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", repo, err)
		}
		heads = append(heads, h)
	}
	return heads, nil
}

// resolveOne returns the open PR of repo from branch, else its default branch
// (also when branch is empty).
func (g *GitHub) resolveOne(ctx context.Context, repo, branch string) (Head, error) {
	if branch != "" {
		owner, _, _ := strings.Cut(repo, "/")
		q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}, "per_page": {"1"}}
		r, err := g.do(ctx, http.MethodGet, repoPath(repo)+"/pulls?"+q.Encode(), nil, nil)
		if err != nil {
			return Head{}, err
		}
		if r.status != http.StatusOK {
			return Head{}, r.err("list PRs")
		}
		var prs []apiPull
		if err := json.Unmarshal(r.body, &prs); err != nil {
			return Head{}, err
		}
		if len(prs) > 0 {
			p := prs[0]
			return Head{Repo: repo, Branch: p.Head.Ref, SHA: p.Head.SHA, PR: p.Number}, nil
		}
	}

	r, err := g.do(ctx, http.MethodGet, repoPath(repo), nil, nil)
	if err != nil {
		return Head{}, err
	}
	if r.status != http.StatusOK {
		return Head{}, r.err("get repo")
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(r.body, &info); err != nil || info.DefaultBranch == "" {
		return Head{}, fmt.Errorf("no default branch")
	}
	r, err = g.do(ctx, http.MethodGet, repoPath(repo)+"/branches/"+url.PathEscape(info.DefaultBranch), nil, nil)
	if err != nil {
		return Head{}, err
	}
	if r.status != http.StatusOK {
		return Head{}, r.err("get branch")
	}
	var br struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(r.body, &br); err != nil || br.Commit.SHA == "" {
		return Head{}, fmt.Errorf("no head commit on %s", info.DefaultBranch)
	}
	return Head{Repo: repo, Branch: info.DefaultBranch, SHA: br.Commit.SHA}, nil
}
