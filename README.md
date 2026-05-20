<p align="center">
  <img src="assets/glimpse-logo.png" alt="glimpse" width="200">
</p>

<h1 align="center">glimpse</h1>

<p align="center">
  <strong>Gaze into your codebase without checking it out.</strong><br>
  Lazy in-memory file access for git repos — for humans via FUSE, for agents via MCP.
</p>

---

A drop-in replacement for `git sparse-checkout` that requires zero configuration. Instead of defining patterns upfront, `glimpse` mounts your repo with FUSE and lazily serves files from the Git object store **entirely from memory**. Files only materialize to disk when you write to them — reads never touch disk.

**The result:** clone a 10GB monorepo, mount it, and start working in seconds. Only the files you *edit* ever hit disk.

## Why?

| | `git checkout` | `git sparse-checkout` | `glimpse` |
|---|---|---|---|
| Time to first file access | O(repo size) | O(sparse set size) | **O(1)** |
| Disk usage | Full working tree | Sparse subset | **Only edited files** |
| Configuration needed | None | Cone/pattern rules | **None** |
| Adapts to your workflow | No | No (manual pattern updates) | **Yes** |
| Agent integration | None | None | **MCP server** |

Sparse checkout makes you answer *"what files do I need?"* before you start working. glimpse figures it out automatically by watching what you access.

## How It Works

```
┌─────────────────────────────────────────────┐
│              Your workflow                   │
│  ls, cat, vim, grep, go build, etc.         │
└──────────────────────┬──────────────────────┘
                       │
┌──────────────────────┴──────────────────────┐
│            FUSE Layer (glimpse)                 │
│                                              │
│  On first READ:                              │
│  1. Fetch blob from git object store         │
│  2. Cache in memory (zero disk I/O)          │
│  3. Serve all future reads from cache        │
│                                              │
│  On first WRITE:                             │
│  4. Flush cached blob to real worktree       │
│  5. Update sparse-checkout                   │
│  6. All future access goes to disk           │
└──────────────────────┬──────────────────────┘
                       │
          ┌────────────┼────────────┐
          │            │            │
  ┌───────┴──────┐ ┌───┴───┐ ┌─────┴──────┐
  │  Git Trees   │ │ Blobs │ │ Worktree   │
  │  (lazy dirs) │ │ (ODB) │ │ (on write) │
  └──────────────┘ └───────┘ └────────────┘
```

### The Flow

1. You clone with `--sparse` (or `glimpse` sets up sparse-checkout for you)
2. `glimpse` overlays FUSE on the worktree
3. **Directories** resolve instantly from git tree objects
4. **File reads** are served from an in-memory cache — zero disk I/O
5. **File writes** trigger materialization to the real worktree + sparse-checkout
6. Once materialized, the file is a **real file** — FUSE gets out of the way
7. **All git commands work natively** for edited files

## Installation

### Prerequisites

- **Go 1.21+**
- **macOS:** [macFUSE](https://osxfuse.github.io/) (`brew install macfuse`) — only for the FUSE mount
- **Linux:** FUSE3 (`sudo apt install fuse3 libfuse3-dev`) — only for the FUSE mount

The MCP server requires **only Go and git**. No FUSE needed.

### Build

```bash
git clone https://github.com/yourusername/glimpse.git
cd glimpse

# FUSE filesystem (for human developers)
go build -o glimpse .

# MCP server (for AI agents — no FUSE required)
go build -o glimpse-mcp ./cmd/glimpse-mcp
```

## Usage

### FUSE Mount (for developers)

```bash
# Clone a huge repo (sparse — only .git is fetched)
git clone --sparse https://github.com/example/monorepo.git
cd monorepo

# Mount
glimpse

# Browse the entire tree — directories resolve instantly
ls src/services/auth/

# Reads are served from memory
cat src/services/auth/handler.go

# Editing triggers materialization — git sees the file
vim src/services/auth/handler.go
git add src/services/auth/handler.go
git commit -m "fix auth bug"

# Unmount
# Press Ctrl+C, or:
umount monorepo          # macOS
fusermount -u monorepo   # Linux
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | *(auto-detected from cwd)* | Path to the git repository |
| `--mount` | *(repo worktree)* | Directory to mount the filesystem on |
| `--ref` | `HEAD` | Git ref to mount (branch, tag, or commit hash) |
| `--ephemeral` | `false` | Ephemeral mode: skip sparse-checkout setup |
| `--debug` | `false` | Enable FUSE debug logging |

### Examples

```bash
# Auto-detect repo from cwd
cd ~/repos/big-monorepo && glimpse

# Explicit repo path
glimpse --repo /path/to/repo

# Mount a specific branch
glimpse --ref feature/new-api

# Ephemeral mode (nothing persists to disk, ideal for agents)
glimpse --ephemeral
```

### Session Stats

On unmount, glimpse shows what stayed in memory vs what hit disk:

```
Session stats:
  Blobs fetched:             42
  Bytes fetched:             168240
  Served from memory:        35
  Materialized to disk:      7
  Directories listed:        12
```

42 files accessed, only 7 (the ones you edited) ever touched disk.

## Agent Integration (MCP Server)

The MCP server lets AI agents glimpse into repos directly from the git object store. **Zero external dependencies** — just the Go stdlib and the `git` CLI.

### Quick Setup

```bash
go build -o glimpse-mcp ./cmd/glimpse-mcp
```

### Two Modes

**Single repo** — lock to one repo at startup (backward compatible):

```bash
glimpse-mcp --repo /path/to/repo --index
```

**Multi repo** — one server, any repo on demand. Agents call `open_repo` with a URL or path. Repos are bare-cloned into `~/.cache/glimpse/` and reused across sessions:

```bash
glimpse-mcp
```

Then the agent calls:
```
open_repo(url: "https://github.com/org/repo.git")
```

The server bare-clones, builds a trigram index, and is ready in seconds. Subsequent opens of the same URL skip the clone entirely.

### Claude Code

```json
{
  "mcpServers": {
    "glimpse": {
      "command": "/path/to/glimpse-mcp"
    }
  }
}
```

### Cursor

Add to `.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "glimpse": {
      "command": "/path/to/glimpse-mcp"
    }
  }
}
```

### MCP Server Options

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | *(optional)* | Lock to one repo at startup |
| `--ref` | `HEAD` | Git ref to serve |
| `--index` | `false` | Build trigram index on startup (always on for `open_repo`) |
| `--cache-dir` | `~/.cache/glimpse` | Where to store bare clones |

### Tools

| Tool | Description |
|------|-------------|
| `open_repo(url, ref?)` | Open a repo by URL (bare-cloned, cached) or local path. Builds trigram index automatically. |
| `list_directory(path?)` | List entries with sizes (cached) |
| `read_file(path)` | Read file from object store (cached in memory) |
| `write_file(path, content)` | Write to worktree (local repos only, not bare clones) |
| `file_info(path)` | Size, type, mode (cached) |
| `grep(pattern, path?)` | Search contents — sub-10ms with trigram index |

### Try It

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_directory","arguments":{"path":""}}}' \
| ./glimpse-mcp --repo . 2>/dev/null | tail -1 \
| python3 -c "import sys,json; print(json.load(sys.stdin)['result']['content'][0]['text'])"
```

## How It Compares

### vs `git sparse-checkout`

- **No patterns to maintain.** Sparse checkout requires `git sparse-checkout set src/ docs/` and manual updates. glimpse adapts automatically.
- **Full tree visibility.** With sparse checkout, `ls` only shows files in your sparse set. With glimpse, you can browse and grep the entire tree.
- **Same git integration.** Both result in real files on disk that work with `git status`, `git diff`, and `git commit`.

### vs `git clone --depth`

- Shallow clones limit history, not breadth. You still check out every file.
- glimpse limits breadth (only edited files materialize) without limiting history.

### vs VFS for Git (Microsoft)

- VFS for Git (formerly GVFS) is a similar concept built for Windows + Azure DevOps.
- glimpse is cross-platform, works with any git remote, and is a single binary with no daemon.

## Running Tests

```bash
go test ./... -v
```

## Project Structure

```
glimpse/
├── main.go                  # FUSE CLI
├── cmd/
│   └── glimpse-mcp/
│       └── main.go          # MCP server (zero deps, stdlib + git CLI)
├── gitbackend/
│   ├── backend.go           # Git object store access via go-git
│   └── backend_test.go      # Backend unit tests
├── fusefs/
│   ├── fs.go                # FUSE root, directory nodes, lazy tree population
│   ├── file.go              # In-memory blob cache + materialize-on-write
│   └── sparse.go            # Sparse-checkout integration
├── assets/
│   └── glimpse-logo.png        # Logo
├── go.mod
└── README.md
```

## Technical Details

### Dependencies

**FUSE filesystem:**
- [go-fuse](https://github.com/hanwen/go-fuse) (v2) — High-performance FUSE bindings for Go
- [go-git](https://github.com/go-git/go-git) (v5) — Pure Go git implementation (no C dependencies)

**MCP server:**
- None. Just the Go standard library and `git` on your PATH.

### Design Decisions

- **Pure Go.** No CGo, no libgit2. Builds anywhere Go does.
- **Hybrid in-memory/disk.** Reads are served from an in-memory blob cache — zero disk I/O for browsing, grepping, and building. Files only materialize to the real worktree when you write to them.
- **Materialize on write, then get out of the way.** Once a file is flushed to disk (triggered by a write), FUSE delegates to the real file. No ongoing overhead.
- **Zero-dep MCP server.** The agent integration server uses only the Go stdlib and the `git` CLI — no go-git, no mcp-go, no FUSE. Builds instantly, runs anywhere.
- **Trigram index.** With `--index`, the MCP server builds an in-memory trigram index on startup (like [zoekt](https://github.com/sourcegraph/zoekt)). Grep narrows candidates via posting list intersection before doing a regexp match — sub-10ms for most queries.
- **Ephemeral mode.** With `--ephemeral`, sparse-checkout setup is skipped entirely — ideal for AI agent sessions or CI pipelines where nothing needs to persist.

## Known Limitations

### FUSE mode: `extensions.worktreeConfig`

The FUSE filesystem uses [go-git](https://github.com/go-git/go-git), which doesn't support the `extensions.worktreeConfig` git extension. Many large monorepos (especially those using `git worktree`) enable this extension, causing glimpse FUSE mode to fail on open.

**Workaround:** Use the MCP server instead — it shells out to the native `git` CLI and handles all extensions correctly.

**Status:** Upstream go-git limitation. The long-term fix is to either patch go-git or migrate the FUSE backend to use the git CLI (like the MCP server already does).

## License

Apache 2.0 — see [LICENSE](LICENSE).
