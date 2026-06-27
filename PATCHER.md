# Patcher

## Repositories

```
upstream: github/github-mcp-server
fork:     auto-patcher/github-mcp-server
```

## Upstream baseline

```
last_patched: v1.5.0
```

The upstream version tag last incorporated into this fork. The next dispatcher
cycle will analyze everything released after this.

## Purpose

This fork extends the GitHub MCP server with tools that support autonomous agent workflows — specifically the ability to drive GitHub Projects, issue graphs, and project field management without requiring callers to know internal node IDs or numeric field IDs. Upstream prioritizes breadth and stability; this fork prioritizes autonomous-workflow ergonomics: human-readable names over raw identifiers, combined operations where an agent would otherwise need multiple round-trips, and graph-traversal tools for navigating issue hierarchies.

## Character

Prefers consolidated operations over granular ones where it reduces the number of tool calls an agent needs. Values graceful error messages that tell the caller what names and options are actually available, rather than returning opaque failures. Uses GraphQL where the REST API falls short (e.g. project mutations) and falls back to raw HTTP where go-github typed structs break against API evolution. Treats `FlexibleString` as the canonical pattern for fields GitHub may return as either a plain string or a structured object.

## Architecture

Structurally identical to upstream — same package layout (`cmd/`, `internal/`, `pkg/`, `e2e/`, `script/`), same dependencies, no replaced modules. Fork divergences are additive: new tool implementations live alongside existing ones in the `pkg/github/` handler files, following the existing `ProjectsWrite` consolidated handler pattern. GraphQL node IDs are resolved at call time from numeric IDs or owner/name — never stored.

What this fork adds on top of `v1.5.0` (tagged `v1.5.0-patch`):
- `FlexibleString` — fixes JSON unmarshal crash in `list_project_fields` / `get_project_field` when GitHub returns option `name`/`description` as objects (backport of upstream #1490); tests in `projects_flexible_string_test.go`
- `create_project_field`, `add_project_field_option`, `delete_project_field` — GraphQL mutations for project field write ops
- `issue_graph` — BFS traversal from an issue across sub-issues, body refs, and timeline cross-references; classifies nodes as `epic`/`batch`/`task`/`pr`
- `set_project_item_status` — sets a project item's status by human-readable option name
- `add_issue_to_project` — adds an issue to a project via `addProjectV2ItemById` GraphQL mutation, optionally sets status in one call
- `add_issue_reaction`, `add_issue_comment_reaction` — emoji reactions on issues and issue comments (backport of upstream v1.5.0 PR #2732)
- `add_pull_request_review_comment_reaction` — emoji reaction on PR review comments (backport of upstream v1.5.0 PR #2732)

## Style

Exported MCP tool wrappers (`SetProjectItemStatus`) call unexported implementation functions (`setProjectItemStatus`). Error messages are user-facing strings via `TranslationHelperFunc`. Table-driven subtests with HTTP mocking via `github_mock` (REST) and `githubv4mock` (GraphQL). New tools follow the existing `ProjectsWrite` consolidated handler pattern. Tool registration follows the existing `register*Tools` function pattern in each handler file.

## Testing

### Unit tests

```
script/test
```

(Wraps `go test ./...`. CI runs this on ubuntu, windows, and macos via `.github/workflows/go.yml`.)

### Integration tests

None separate from unit tests. CI also runs `go mod tidy -diff` to verify the module graph is clean.

### Build

```
go build -v ./cmd/github-mcp-server
```

### Smoke tests

Build the binary, set `GITHUB_PERSONAL_ACCESS_TOKEN`, then pipe a JSON-RPC `tools/call` request to stdin and verify stdout contains a non-error result. `cmd/mcpcurl/` is an MCP test client included in the repo.

### Subagent testing

Spawn `claude --mcp-server "GITHUB_PERSONAL_ACCESS_TOKEN=<token> ./github-mcp-server stdio"` and invoke the fork-specific tools (`issue_graph`, `set_project_item_status`, `add_issue_to_project`) against a real GitHub project to verify end-to-end behavior.
