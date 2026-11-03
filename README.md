# github-overlay

A local dashboard for your GitHub pull requests: which ones have a reviewer
requested, unresolved review threads (people and bots counted separately),
changes requested, CI status with links to failing checks, merge conflicts,
and bundles of PRs that reference each other.

It's a single Go binary with the page embedded. It authenticates with your
`gh` CLI token and polls GitHub's GraphQL API every 60 seconds.

## Install

```sh
go install .            # puts github-overlay in $(go env GOPATH)/bin
ln -s "$(go env GOPATH)/bin/github-overlay" ~/.local/bin/   # if GOPATH/bin isn't on PATH
```

Requires `gh auth login`.

## Usage

```sh
github-overlay start     # run in the background on http://127.0.0.1:7777
github-overlay open      # open in the browser (starts the server if needed)
github-overlay status
github-overlay logs -f
github-overlay restart -closed-days 14
github-overlay stop
github-overlay serve     # run in the foreground
```

Flags for `start`, `restart`, `open` and `serve`:

| Flag | Default | |
|---|---|---|
| `-addr` | `127.0.0.1:7777` | listen address |
| `-interval` | `1m` | GitHub poll interval |
| `-closed-days` | `7` | how far back "Recently closed" goes |
| `-bots` | `igloo-code-reviewer,chatgpt-codex-connector,codex,copilot-pull-request-reviewer` | logins counted as bots, in addition to GitHub App accounts |

The PID file and log are kept in `~/Library/Caches/github-overlay/`.

## Tabs

- **Mine**: your open PRs and drafts, filterable by needs action, waiting on
  review, no reviewer, ready to merge, and drafts.
- **To review**: open PRs where you or one of your teams is a requested reviewer.
- **Closed**: your PRs merged or closed in the last `-closed-days` days.

Each row shows CI (hover for failing checks), reviewer avatars ringed by
state (dotted yellow requested, green approved, red changes requested, gray
commented), unresolved threads (people in red, bots as a gray `+N`), and age.
Press `r` to refresh.

## Connected PRs

Unconnected PRs share one box at the top; below it, open PRs are grouped when one references another, either in its description
(a URL, `owner/repo#12`, `repo #12` for a repo under the same owner, or `#12`)
or through GitHub's cross-reference events. Closed PRs don't join bundles, but
they show as "Linked:" chips on the open PRs that reference them.

Each search returns at most 50 PRs.
