package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const prFragment = `
fragment pr on PullRequest {
  number title url isDraft state merged createdAt updatedAt closedAt mergedAt
  additions deletions mergeable reviewDecision body
  headRefName baseRefName
  author { login }
  repository { nameWithOwner }
  reviewRequests(first: 20) { nodes { requestedReviewer {
    __typename
    ... on User { login }
    ... on Team { slug }
    ... on Bot { login }
    ... on Mannequin { login }
  } } }
  latestReviews(first: 20) { nodes { state author { __typename login } } }
  reviewThreads(first: 100) { nodes {
    isResolved
    comments(first: 1) { nodes { author { __typename login } } }
  } }
  commits(last: 1) { nodes { commit { statusCheckRollup {
    state
    contexts(first: 100) { nodes {
      __typename
      ... on CheckRun { name status conclusion detailsUrl }
      ... on StatusContext { context state targetUrl }
    } }
  } } } }
  timelineItems(first: 50, itemTypes: [CROSS_REFERENCED_EVENT]) { nodes {
    ... on CrossReferencedEvent { source { __typename ... on PullRequest { url } } }
  } }
}`

// Closed PRs only need enough to list them and link them into bundles.
const prLiteFragment = `
fragment pr on PullRequest {
  number title url isDraft state merged createdAt updatedAt closedAt mergedAt
  additions deletions body headRefName baseRefName
  author { login }
  repository { nameWithOwner }
  latestReviews(first: 20) { nodes { state author { __typename login } } }
  timelineItems(first: 50, itemTypes: [CROSS_REFERENCED_EVENT]) { nodes {
    ... on CrossReferencedEvent { source { __typename ... on PullRequest { url } } }
  } }
}`

// Each section is its own request: one combined query takes longer than
// GitHub's ~10s GraphQL timeout and comes back as a 502.
const sectionQuery = `
query($q: String!) {
  viewer { login }
  rateLimit { remaining }
  search(query: $q, type: ISSUE, first: 50) { nodes { ...pr } }
}`

type actor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

type rawPR struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	URL            string  `json:"url"`
	IsDraft        bool    `json:"isDraft"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	ClosedAt       *string `json:"closedAt"`
	MergedAt       *string `json:"mergedAt"`
	Additions      int     `json:"additions"`
	Deletions      int     `json:"deletions"`
	Mergeable      string  `json:"mergeable"`
	ReviewDecision *string `json:"reviewDecision"`
	Body           string  `json:"body"`
	HeadRefName    string  `json:"headRefName"`
	BaseRefName    string  `json:"baseRefName"`
	Author         *actor  `json:"author"`
	Repository     struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
				Slug     string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	LatestReviews struct {
		Nodes []struct {
			State  string `json:"state"`
			Author *actor `json:"author"`
		} `json:"nodes"`
	} `json:"latestReviews"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
			Comments   struct {
				Nodes []struct {
					Author *actor `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Typename   string `json:"__typename"`
							Name       string `json:"name"`
							Status     string `json:"status"`
							Conclusion string `json:"conclusion"`
							DetailsURL string `json:"detailsUrl"`
							Context    string `json:"context"`
							State      string `json:"state"`
							TargetURL  string `json:"targetUrl"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	TimelineItems struct {
		Nodes []struct {
			Source *struct {
				Typename string `json:"__typename"`
				URL      string `json:"url"`
			} `json:"source"`
		} `json:"nodes"`
	} `json:"timelineItems"`
}

type sectionResponse struct {
	Data *struct {
		Viewer    struct{ Login string } `json:"viewer"`
		RateLimit struct {
			Remaining int `json:"remaining"`
		} `json:"rateLimit"`
		Search struct{ Nodes []rawPR } `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type queryResult struct {
	Viewer        string
	RateRemaining int
	Mine          []rawPR
	Review        []rawPR
	Closed        []rawPR
	Errors        []string // partial errors from GitHub alongside data
}

var errUnauthorized = errors.New("github: unauthorized")

type client struct {
	mu    sync.Mutex
	token string
	http  *http.Client
}

func newClient() *client {
	return &client{http: &http.Client{Timeout: 30 * time.Second}}
}

func ghToken() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w (run `gh auth login`)", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *client) getToken(refresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || refresh {
		t, err := ghToken()
		if err != nil {
			return "", err
		}
		c.token = t
	}
	return c.token, nil
}

func (c *client) fetch(closedDays int) (*queryResult, error) {
	since := time.Now().AddDate(0, 0, -closedDays).Format("2006-01-02")
	sections := []struct {
		q, fragment string
		dst         *[]rawPR
	}{
		{"is:pr is:open author:@me archived:false sort:updated-desc", prFragment, nil},
		{"is:pr is:open review-requested:@me archived:false sort:updated-desc", prFragment, nil},
		{"is:pr is:closed author:@me closed:>=" + since + " sort:updated-desc", prLiteFragment, nil},
	}
	out := &queryResult{}
	sections[0].dst, sections[1].dst, sections[2].dst = &out.Mine, &out.Review, &out.Closed

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, s := range sections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.query(sectionQuery+s.fragment, s.q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			*s.dst = res.Data.Search.Nodes
			out.Viewer = res.Data.Viewer.Login
			if out.RateRemaining == 0 || res.Data.RateLimit.Remaining < out.RateRemaining {
				out.RateRemaining = res.Data.RateLimit.Remaining
			}
			for _, e := range res.Errors {
				out.Errors = append(out.Errors, e.Message)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return out, nil
}

func (c *client) query(q, search string) (*sectionResponse, error) {
	res, err := c.do(q, search, false)
	if errors.Is(err, errUnauthorized) {
		res, err = c.do(q, search, true)
	}
	return res, err
}

func (c *client) do(q, search string, refreshToken bool) (*sectionResponse, error) {
	token, err := c.getToken(refreshToken)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{
		"query":     q,
		"variables": map[string]string{"q": search},
	})
	req, _ := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewReader(body))
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: HTTP %d", resp.StatusCode)
	}
	var out sectionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		if len(out.Errors) > 0 {
			return nil, fmt.Errorf("github: %s", out.Errors[0].Message)
		}
		return nil, errors.New("github: empty response")
	}
	return &out, nil
}
