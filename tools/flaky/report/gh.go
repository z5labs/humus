// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// GH is the GitHub of the repository at Dir, reached with the gh command,
// which runs with the environment Env. gh picks the repository as it does
// for any command run in Dir.
type GH struct {
	Dir string
	Env []string
}

var _ GitHub = GH{}

// bugs is the name of the discussion category a report is posted in.
const bugs = "Bugs"

const searchQuery = `query($discussions: String!, $stories: String!) {
  discussions: search(type: DISCUSSION, query: $discussions, first: 100) {
    nodes { ... on Discussion { title body url closed createdAt category { name } } }
  }
  stories: search(type: ISSUE, query: $stories, first: 100) {
    nodes { ... on Issue { title body url state createdAt } }
  }
}`

const categoriesQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
    discussionCategories(first: 100) { nodes { id name } }
  }
}`

const createMutation = `mutation($repository: ID!, $category: ID!, $title: String!, $body: String!) {
  createDiscussion(input: {repositoryId: $repository, categoryId: $category, title: $title, body: $body}) {
    discussion { url }
  }
}`

type repository struct {
	NameWithOwner string `json:"nameWithOwner"`
	URL           string `json:"url"`
}

func (g GH) repository(ctx context.Context) (repository, error) {
	var repo repository
	if err := g.json(ctx, &repo, "repo", "view", "--json", "nameWithOwner,url"); err != nil {
		return repo, err
	}
	if repo.NameWithOwner == "" || repo.URL == "" {
		return repo, errors.New("gh repo view named no repository")
	}
	return repo, nil
}

// Search searches the repository's discussions in the Bugs category, and its
// issues labelled story, for test.
func (g GH) Search(ctx context.Context, test string) (Found, error) {
	repo, err := g.repository(ctx)
	if err != nil {
		return Found{}, err
	}
	// A quote would end the phrase, so it is searched for as a space.
	phrase := `"` + strings.NewReplacer(`"`, " ", `\`, " ").Replace(test) + `"`
	var out struct {
		Data struct {
			Discussions struct {
				Nodes []struct {
					Title     string    `json:"title"`
					Body      string    `json:"body"`
					URL       string    `json:"url"`
					Closed    bool      `json:"closed"`
					CreatedAt time.Time `json:"createdAt"`
					Category  struct {
						Name string `json:"name"`
					} `json:"category"`
				} `json:"nodes"`
			} `json:"discussions"`
			Stories struct {
				Nodes []struct {
					Title     string    `json:"title"`
					Body      string    `json:"body"`
					URL       string    `json:"url"`
					State     string    `json:"state"`
					CreatedAt time.Time `json:"createdAt"`
				} `json:"nodes"`
			} `json:"stories"`
		} `json:"data"`
	}
	err = g.json(ctx, &out, "api", "graphql",
		"-f", "query="+searchQuery,
		"-f", "discussions=repo:"+repo.NameWithOwner+" "+phrase,
		"-f", "stories=repo:"+repo.NameWithOwner+" is:issue label:story "+phrase,
	)
	if err != nil {
		return Found{}, err
	}
	found := Found{Repository: repo.URL}
	for _, d := range out.Data.Discussions.Nodes {
		if d.URL == "" || d.Category.Name != bugs {
			continue
		}
		found.Reports = append(found.Reports, Earlier{Title: d.Title, Body: d.Body, URL: d.URL, Open: !d.Closed, Created: d.CreatedAt})
	}
	for _, s := range out.Data.Stories.Nodes {
		if s.URL == "" {
			continue
		}
		found.Reports = append(found.Reports, Earlier{Title: s.Title, Body: s.Body, URL: s.URL, Open: s.State == "OPEN", Created: s.CreatedAt})
	}
	return found, nil
}

// Create finds the repository's discussion category named Bugs, and creates
// the discussion in it.
func (g GH) Create(ctx context.Context, title, body string) (string, error) {
	repo, err := g.repository(ctx)
	if err != nil {
		return "", err
	}
	owner, name, _ := strings.Cut(repo.NameWithOwner, "/")
	var categories struct {
		Data struct {
			Repository struct {
				ID                   string `json:"id"`
				DiscussionCategories struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"discussionCategories"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := g.json(ctx, &categories, "api", "graphql", "-f", "query="+categoriesQuery, "-f", "owner="+owner, "-f", "name="+name); err != nil {
		return "", err
	}
	category := ""
	for _, c := range categories.Data.Repository.DiscussionCategories.Nodes {
		if c.Name == bugs {
			category = c.ID
		}
	}
	if category == "" {
		return "", fmt.Errorf("%s has no discussion category named %s", repo.NameWithOwner, bugs)
	}
	var created struct {
		Data struct {
			CreateDiscussion struct {
				Discussion struct {
					URL string `json:"url"`
				} `json:"discussion"`
			} `json:"createDiscussion"`
		} `json:"data"`
	}
	err = g.json(ctx, &created, "api", "graphql",
		"-f", "query="+createMutation,
		"-f", "repository="+categories.Data.Repository.ID,
		"-f", "category="+category,
		"-f", "title="+title,
		"-f", "body="+body,
	)
	if err != nil {
		return "", err
	}
	if created.Data.CreateDiscussion.Discussion.URL == "" {
		return "", errors.New("gh api graphql created no discussion")
	}
	return created.Data.CreateDiscussion.Discussion.URL, nil
}

// json runs gh with args and decodes what it prints into v.
func (g GH) json(ctx context.Context, v any, args ...string) error {
	out, err := command(ctx, g.Dir, g.Env, "gh", args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("gh %s printed what isn't JSON: %w", args[0], err)
	}
	return nil
}

func command(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
