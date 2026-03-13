---
name: mycelium
description: Search indexed documentation and code via the Mycelium MCP server and CLI.
argument-hint: "[search <query>|status|help]"
allowed-tools: mcp__mycelium__search, mcp__mycelium__search_code, mcp__mycelium__list_sources, Bash(/Users/jonathanlanda/dev/mycelium/mctl *)
---

# Mycelium — Dependency Context for AI Agents

Mycelium indexes documentation and source code into a local vector store.
Use the MCP tools for semantic search and the CLI for lifecycle management.

## MCP Tools

| Tool | Purpose |
|------|---------|
| search | Semantic search across all indexed documentation |
| search_code | Semantic search scoped to indexed source code |
| list_sources | List all indexed sources with their versions and status |

**Always call list_sources first** to see what is available, then use search or search_code with specific queries.

## CLI Commands

| Command | Description |
|---------|-------------|
| /Users/jonathanlanda/dev/mycelium/mctl init | Initialize mycelium.toml in the current directory |
| /Users/jonathanlanda/dev/mycelium/mctl add <source> | Add a dependency to the manifest |
| /Users/jonathanlanda/dev/mycelium/mctl up | Fetch, chunk, embed, and store all dependencies |
| /Users/jonathanlanda/dev/mycelium/mctl status | Show sync status of all dependencies |
| /Users/jonathanlanda/dev/mycelium/mctl upgrade [dep] | Upgrade dependencies to latest compatible versions |
| /Users/jonathanlanda/dev/mycelium/mctl serve | Start the MCP server (stdio) |
| /Users/jonathanlanda/dev/mycelium/mctl setup | Configure Claude Code integration |

## Manifest Format (mycelium.toml)

```toml
[config]
embedding_model = "voyage-code-2"

[[sources]]
name = "my-lib"
source = "github:org/repo"
version = "v1.0.0"
paths = ["docs/**/*.md", "src/**/*.go"]
```

## Common Workflows

1. **Before making changes** — search for relevant docs:
   - Call list_sources to see indexed dependencies
   - Use search with a description of what you want to change
   - Use search_code for implementation patterns

2. **Add a new dependency**:
   ```bash
   /Users/jonathanlanda/dev/mycelium/mctl add github:org/repo --version v1.0.0 --paths "docs/**/*.md"
   /Users/jonathanlanda/dev/mycelium/mctl up
   ```

3. **Check sync status**:
   ```bash
   /Users/jonathanlanda/dev/mycelium/mctl status
   ```
