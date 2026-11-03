package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Review struct {
	Login string `json:"login"`
	State string `json:"state"`
	Bot   bool   `json:"bot"`
}

type Check struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type CI struct {
	State   string  `json:"state"` // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED, or "" when no checks
	Failed  []Check `json:"failed"`
	Pending []Check `json:"pending"`
	Total   int     `json:"total"`
}

type PR struct {
	Key                string   `json:"key"` // owner/repo#123
	Number             int      `json:"number"`
	Title              string   `json:"title"`
	URL                string   `json:"url"`
	Repo               string   `json:"repo"`
	Author             string   `json:"author"`
	Head               string   `json:"head"`
	Base               string   `json:"base"`
	State              string   `json:"state"` // OPEN, DRAFT, MERGED, CLOSED
	CreatedAt          string   `json:"createdAt"`
	UpdatedAt          string   `json:"updatedAt"`
	ClosedAt           string   `json:"closedAt,omitempty"`
	Additions          int      `json:"additions"`
	Deletions          int      `json:"deletions"`
	Conflicting        bool     `json:"conflicting"`
	ReviewDecision     string   `json:"reviewDecision"`
	RequestedReviewers []string `json:"requestedReviewers"`
	Reviews            []Review `json:"reviews"`
	ChangesRequested   bool     `json:"changesRequested"`
	HumanThreads       int      `json:"humanThreads"` // unresolved
	BotThreads         int      `json:"botThreads"`   // unresolved
	CI                 CI       `json:"ci"`
	Links              []string `json:"links"` // keys of PRs referenced from/to this one
	Bundle             int      `json:"bundle"`
}

type Snapshot struct {
	Viewer        string              `json:"viewer"`
	FetchedAt     string              `json:"fetchedAt"`
	Error         string              `json:"error,omitempty"`
	RateRemaining int                 `json:"rateRemaining"`
	PRs           map[string]*PR      `json:"prs"`
	Sections      map[string][]string `json:"sections"`
	Bundles       map[int][]string    `json:"bundles"`
}

var (
	urlRef  = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/(?:pull|issues)/(\d+)`)
	fullRef = regexp.MustCompile(`(?:^|[\s(\[,])([\w.-]+/[\w.-]+)#(\d+)\b`)
	// "#12", or "rent-api #12" / "rent-api#12" naming a sibling repo without its owner.
	shortRef = regexp.MustCompile(`(?:^|[\s(\[,])(?:([\w.-]+) ?)?#(\d+)\b`)
)

func refKey(repo, num string) string { return strings.ToLower(repo) + "#" + num }

// bodyRefs returns keys of PRs/issues the body mentions. repos maps
// lowercased "owner/name" of every fetched repo, used to resolve a bare
// repo name in front of "#N" to a sibling repo under the same owner.
func bodyRefs(repo, body string, repos map[string]bool) []string {
	owner, _, _ := strings.Cut(repo, "/")
	var keys []string
	for _, m := range urlRef.FindAllStringSubmatch(body, -1) {
		keys = append(keys, refKey(m[1], m[2]))
	}
	for _, m := range fullRef.FindAllStringSubmatch(body, -1) {
		keys = append(keys, refKey(m[1], m[2]))
	}
	for _, m := range shortRef.FindAllStringSubmatch(body, -1) {
		target := repo
		if sibling := strings.ToLower(owner + "/" + m[1]); m[1] != "" && repos[sibling] {
			target = sibling
		}
		keys = append(keys, refKey(target, m[2]))
	}
	return keys
}

func isBot(a *actor, bots map[string]bool) bool {
	if a == nil {
		return false
	}
	login := strings.ToLower(a.Login)
	return a.Typename == "Bot" || strings.HasSuffix(login, "[bot]") || bots[login]
}

func convert(r rawPR, bots, repos map[string]bool) *PR {
	p := &PR{
		Key:       refKey(r.Repository.NameWithOwner, strconv.Itoa(r.Number)),
		Number:    r.Number,
		Title:     r.Title,
		URL:       r.URL,
		Repo:      r.Repository.NameWithOwner,
		Head:      r.HeadRefName,
		Base:      r.BaseRefName,
		State:     r.State,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		Additions: r.Additions,
		Deletions: r.Deletions,
	}
	if r.Author != nil {
		p.Author = r.Author.Login
	}
	if r.IsDraft && r.State == "OPEN" {
		p.State = "DRAFT"
	}
	if r.ClosedAt != nil {
		p.ClosedAt = *r.ClosedAt
	}
	p.Conflicting = r.Mergeable == "CONFLICTING"
	if r.ReviewDecision != nil {
		p.ReviewDecision = *r.ReviewDecision
	}

	p.RequestedReviewers = []string{}
	for _, n := range r.ReviewRequests.Nodes {
		if rr := n.RequestedReviewer; rr != nil {
			if rr.Typename == "Team" {
				p.RequestedReviewers = append(p.RequestedReviewers, "@"+rr.Slug)
			} else {
				p.RequestedReviewers = append(p.RequestedReviewers, rr.Login)
			}
		}
	}

	p.Reviews = []Review{}
	for _, n := range r.LatestReviews.Nodes {
		if n.Author == nil {
			continue
		}
		p.Reviews = append(p.Reviews, Review{Login: n.Author.Login, State: n.State, Bot: isBot(n.Author, bots)})
		if n.State == "CHANGES_REQUESTED" {
			p.ChangesRequested = true
		}
	}

	for _, t := range r.ReviewThreads.Nodes {
		if t.IsResolved {
			continue
		}
		var a *actor
		if len(t.Comments.Nodes) > 0 {
			a = t.Comments.Nodes[0].Author
		}
		if isBot(a, bots) {
			p.BotThreads++
		} else {
			p.HumanThreads++
		}
	}

	p.CI = CI{Failed: []Check{}, Pending: []Check{}}
	if len(r.Commits.Nodes) > 0 {
		if roll := r.Commits.Nodes[0].Commit.StatusCheckRollup; roll != nil {
			p.CI.State = roll.State
			for _, c := range roll.Contexts.Nodes {
				p.CI.Total++
				switch c.Typename {
				case "CheckRun":
					ch := Check{Name: c.Name, URL: c.DetailsURL}
					switch {
					case c.Status != "COMPLETED":
						p.CI.Pending = append(p.CI.Pending, ch)
					case c.Conclusion == "FAILURE", c.Conclusion == "TIMED_OUT", c.Conclusion == "CANCELLED",
						c.Conclusion == "ACTION_REQUIRED", c.Conclusion == "STARTUP_FAILURE":
						p.CI.Failed = append(p.CI.Failed, ch)
					}
				case "StatusContext":
					ch := Check{Name: c.Context, URL: c.TargetURL}
					switch c.State {
					case "PENDING", "EXPECTED":
						p.CI.Pending = append(p.CI.Pending, ch)
					case "FAILURE", "ERROR":
						p.CI.Failed = append(p.CI.Failed, ch)
					}
				}
			}
		}
	}

	seen := map[string]bool{p.Key: true}
	p.Links = []string{}
	refs := bodyRefs(p.Repo, r.Body, repos)
	for _, n := range r.TimelineItems.Nodes {
		if n.Source != nil && n.Source.Typename == "PullRequest" {
			if m := urlRef.FindStringSubmatch(n.Source.URL); m != nil {
				refs = append(refs, refKey(m[1], m[2]))
			}
		}
	}
	for _, k := range refs {
		if !seen[k] {
			seen[k] = true
			p.Links = append(p.Links, k)
		}
	}
	return p
}

func buildSnapshot(res *queryResult, bots map[string]bool) *Snapshot {
	s := &Snapshot{
		Viewer:        res.Viewer,
		RateRemaining: res.RateRemaining,
		PRs:           map[string]*PR{},
		Sections:      map[string][]string{},
		Bundles:       map[int][]string{},
	}
	sections := map[string][]rawPR{"mine": res.Mine, "review": res.Review, "closed": res.Closed}
	repos := map[string]bool{}
	for _, nodes := range sections {
		for _, r := range nodes {
			repos[strings.ToLower(r.Repository.NameWithOwner)] = true
		}
	}
	for name, nodes := range sections {
		keys := []string{}
		for _, r := range nodes {
			if r.Number == 0 { // non-PR search hit
				continue
			}
			p := convert(r, bots, repos)
			s.PRs[p.Key] = p
			keys = append(keys, p.Key)
		}
		s.Sections[name] = keys
	}

	// Union-find over references between open PRs. A link in either
	// direction connects two PRs into the same bundle. Closed PRs stay out:
	// old ones chain unrelated work together, and they still show up as
	// "Linked:" chips on the open PRs that reference them.
	open := func(p *PR) bool { return p.State == "OPEN" || p.State == "DRAFT" }
	parent := map[string]string{}
	var find func(string) string
	find = func(k string) string {
		if parent[k] == "" || parent[k] == k {
			return k
		}
		parent[k] = find(parent[k])
		return parent[k]
	}
	for k, p := range s.PRs {
		if !open(p) {
			continue
		}
		for _, l := range p.Links {
			if q, ok := s.PRs[l]; ok && open(q) {
				parent[find(k)] = find(l)
			}
		}
	}
	groups := map[string][]string{}
	for k, p := range s.PRs {
		if !open(p) {
			continue
		}
		root := find(k)
		groups[root] = append(groups[root], k)
	}
	roots := make([]string, 0, len(groups))
	for r, members := range groups {
		if len(members) > 1 {
			roots = append(roots, r)
		}
	}
	sort.Strings(roots)
	for i, r := range roots {
		id := i + 1
		members := groups[r]
		sort.Strings(members)
		s.Bundles[id] = members
		for _, k := range members {
			s.PRs[k].Bundle = id
		}
	}
	return s
}
