<p align="center">
  <img src="assets/glimpse-logo.png" alt="glimpse" width="200">
</p>

<h1 align="center">glimpse</h1>

<p align="center">
  <strong>A git explorer harness for AI agents.</strong><br>
  Super-fast git file explorer with trigram indexing and lazy loading,<br>
  and a FUSE-mount-backed lazy materializer for writes,<br>
  backed by the remote git object store.
</p>

---

## Why

Most agentic coding tasks need only **read** access — exploring, searching,
understanding a repo before deciding what (if anything) to change. The default
path is `git clone`, which writes the whole repo to disk before the agent has
decided what to look at. That's slow for short-lived sessions, wasteful for
read-only ones, and an outright non-starter for repos too big to fit on the
disk you have.

glimpse keeps the read path **in memory and index-based**. The repository tree
comes back in one API call; files stream from the GitHub CDN lazily, on first
touch, and are deduped in RAM by blob SHA. The resident working set tracks
what the agent actually looked at — not the full repo — so on average memory
and disk stay small even on huge codebases.

When modification *is* needed, glimpse provisions a partial bare clone
(`--filter=blob:none`) and a FUSE-mount-backed **sparse** worktree on demand.
Only the files the agent actually edits get materialized on disk. The
worktree is a real git worktree, so every git API — `status`, `diff`,
`commit`, `push`, `rebase`, anything else — keeps working without any special
integration.

The visual version:

```
git clone <url>            # minutes, full repo on disk
cd <repo>                  # navigate
cat / grep / read files    # fast, but everything is already on disk
edit + commit              # fast
```

`glimpse` collapses it to:

```
glimpse <url>              # ~300 ms, zero bytes on disk
ls / cat / grep            # files stream on demand from a CDN
edit + commit              # first edit lazily provisions a tiny worktree
```

Same mental model, none of the upfront cost.

## Workflows

### 1. Browse a repo you've never seen

```bash
glimpse ls   https://github.com/torvalds/linux
glimpse cat  https://github.com/torvalds/linux MAINTAINERS
glimpse grep https://github.com/torvalds/linux 'EXPORT_SYMBOL_GPL'
```

No clone. No checkout. The tree shows up in ~300 ms; each file you read takes ~100 ms once.

### 2. Browse a monorepo too big for the GitHub Trees API

GitHub's Trees API caps a recursive tree response at ~7 MB / ~100k entries. For a small repo that's fine; for something the size of `snowflake-eng/snowflake` (51k+ entries truncated at the top level) it isn't.

Pin to the subdirectory you actually care about by appending `/tree/<branch>/<path>` to the URL:

```bash
glimpse ls   'https://github.com/snowflake-eng/snowflake/tree/main/AIOperations'
glimpse find 'https://github.com/snowflake-eng/snowflake/tree/main/AIOperations' 'SKILL.md'
glimpse grep 'https://github.com/snowflake-eng/snowflake/tree/main/AIOperations' 'cloudprober'
glimpse cat  'https://github.com/snowflake-eng/snowflake/tree/main/AIOperations' \
             'teams/spcs/skills/spcs-ops/spcs-prober-debug/SKILL.md'
```

Paths in command output are relative to the pinned subtree, so the user model is the same as if `AIOperations/` were a tiny repo of its own. If even the pinned subtree exceeds the cap, glimpse warns on stderr and proceeds with the partial tree it did receive.

### 3. Edit and push without touching disk (API writes)

```bash
# Write a single file and push in one shot:
echo '{"version": 2}' | glimpse write https://github.com/you/repo config.json -

# Multi-file commit:
glimpse push https://github.com/you/repo \
  --message "update configs" --branch my-branch \
  config.json:./local-config.json \
  docs/setup.md:./setup.md
```

Zero disk usage, zero clone, zero FUSE. glimpse creates blobs, assembles a tree,
creates a commit, and fast-forwards the branch ref entirely through the GitHub
Git Data API. If the branch doesn't exist yet, it's created automatically.

The MCP tools `write_file_api` and `git_push_api` expose the same capability to
AI agents: stage files in RAM, then commit+push with one call.

### 4. Mount it as a real filesystem

```bash
glimpse-fuse https://github.com/torvalds/linux --mount ./linux
ls   ./linux                            # tree from memory
cat  ./linux/Documentation/README       # CDN fetch, cached
echo "..." >> ./linux/MAINTAINERS       # first write -> lazy worktree
```

Any tool that opens files (`grep`, `rg`, `vim`, your IDE) just works against the mount. Reads are lazy; writes flip the affected file to disk-backed.

### 5. Drop it into Cursor / Claude as an MCP server

The agent gets `open_repo`, `read_file`, `grep`, `write_file`, `write_file_api`, `git_push_api`, `git_status`, `git_diff`, `git_commit`, plus `find_files`, `repo_status`, `glimpse_help`. Each tool result carries a cost hint and a next-step suggestion. Two write paths: diskless (`write_file_api` + `git_push_api`) for quick edits via the GitHub API, or disk-backed (`write_file` + `git_commit`) for full git workflows.

```json
{
  "mcpServers": {
    "glimpse": {
      "command": "glimpse-mcp",
      "env": { "GITHUB_TOKEN": "ghp_..." }
    }
  }
}
```

`glimpse-mcp --print-mcp-config` prints the snippet for you to paste.

`open_repo` accepts the same `/tree/<branch>/<path>` form as the CLI, so an agent can pin to a subtree without any extra arguments.

## Benchmarks

Measured on a wired residential connection from the US west coast, against
`api.github.com` and `raw.githubusercontent.com`. All glimpse runs use a fresh
cache directory so reads are honest cold fetches. All `git` runs are
`git clone --depth=1` (the most generous baseline for read-only browsing) plus
a follow-up `git grep`. Reproducer: [`scripts/bench.sh`](scripts/bench.sh).

Three columns of wall-clock seconds:
- **t_tree** — time until the directory listing is printable
- **t_cat** — time until the first file's contents are printable (for `git`,
  the file is on disk after `clone`, so this equals `t_clone`)
- **t_grep** — time until the first grep result is printable, starting from
  no local state (so for `git` this includes the clone)

| repo | tool | t_tree | t_cat | t_grep | disk after |
|---|---|---:|---:|---:|---:|
| `cli/cli` | **glimpse** | **1.74 s** | **1.38 s** | **1.75 s** | **0** |
| `cli/cli` | git --depth=1 | 3.70 s | 3.70 s | 3.91 s | 39 MB |
| `hashicorp/terraform` | **glimpse** | **1.63 s** | **1.74 s** | **2.50 s** | **0** |
| `hashicorp/terraform` | git --depth=1 | 6.08 s | 6.08 s | 6.36 s | 48 MB |
| `torvalds/linux` | **glimpse** | **3.06 s** | **2.93 s** | **3.46 s** | **0** |
| `torvalds/linux` | git --depth=1 | 59.7 s | 59.7 s | 64.7 s | 2.0 GB |
| `snowflake-eng/snowflake/tree/main/AIOperations` | **glimpse** | **2.03 s** | **2.15 s** | **3.35 s** | **0** |
| (same, full repo)     | git --depth=1 | (would need ≈ 22 GB on disk; not attempted) |

Speedup factors of glimpse vs `git clone --depth=1`:

| repo | t_tree | t_cat | t_grep | disk |
|---|---:|---:|---:|---:|
| `cli/cli`            | 2.1× | 2.7× | 2.2× | ∞ (39 MB → 0) |
| `hashicorp/terraform` | 3.7× | 3.5× | 2.5× | ∞ (48 MB → 0) |
| `torvalds/linux`     | **19.5×** | **20.4×** | **18.7×** | **∞ (2.0 GB → 0)** |
| monorepo subtree     | n/a (git can't fit on disk) | | | |

Takeaways:

- **Time to browse scales sub-linearly** with repo size: ~2 s for a small CLI,
  ~3 s for the Linux kernel. `git clone --depth=1` scales linearly with repo
  size: about a minute for the kernel.
- **Tree + first-read together** in glimpse beats the TCP handshake budget of
  `git clone` on every repo we measured, and is ~20× faster on the kernel.
- **Grep is now competitive on every size class.** Code Search + parallel CDN
  fetch beats `git clone --depth=1 && git grep` even on small repos because
  the bottleneck for the baseline is always the clone, not the grep.
- **Subtree pinning makes monorepos work at all.** glimpse navigates inside
  `snowflake-eng/snowflake/AIOperations` in ~2 s without ever touching the
  ~22 GB the full repo would require to clone.
- **Disk used for read-only browsing is always 0.** Nothing materializes until
  the first `write_file` triggers the lazy partial clone.

These numbers reflect a recent optimization: at session open, glimpse fires
the tree fetch, the repo metadata fetch, and the languages fetch in parallel
(they were sequential before), and reuses the metadata fetched during ref
resolution. That alone shaved 2–4 seconds off the kernel cold open and made
grep ~2× faster across the board.

## Install

```bash
git clone https://github.com/surao/gitfs-accelerator
cd gitfs-accelerator
go build -o glimpse        .                       # CLI
go build -o glimpse-mcp    ./cmd/glimpse-mcp       # MCP server
go build -o glimpse-fuse   ./cmd/glimpse-fuse      # optional FUSE mount
```

Drop the binaries somewhere on `PATH`.

## Notes

- Only `github.com` URLs are supported. Other hosts return an error.
- Set `GITHUB_TOKEN` (env or `--github-token`) for higher rate limits and private repos. Without one: 60 REST req/hr, 10 Code Search req/min.
- Subtree pinning (`/tree/<branch>/<path>`) is the recommended way to use glimpse on monorepos. The session is pinned to that subtree; relative paths in `ls` / `cat` / `grep` are relative to it.
- `repo_status` surfaces cache state, working-set index size, rate-limit headroom, and whether the tree was truncated.

## License

Apache 2.0 — see [LICENSE](LICENSE).
