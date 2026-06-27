package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/github/github-mcp-server/internal/toolsnaps"
	"github.com/github/github-mcp-server/pkg/translations"
	gogithub "github.com/google/go-github/v87/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssueGraph_ToolDefinition(t *testing.T) {
	serverTool := IssueGraph(translations.NullTranslationHelper)
	tool := serverTool.Tool

	// Toolsnap test (creates snapshot on first run).
	require.NoError(t, toolsnaps.Test(tool.Name, tool))

	assert.Equal(t, "issue_graph", tool.Name)
	assert.NotEmpty(t, tool.Description)
	require.NotNil(t, tool.Annotations)
	assert.True(t, tool.Annotations.ReadOnlyHint)

	schema, ok := tool.InputSchema.(*jsonschema.Schema)
	require.True(t, ok, "InputSchema should be *jsonschema.Schema")
	assert.Contains(t, schema.Properties, "owner")
	assert.Contains(t, schema.Properties, "repo")
	assert.Contains(t, schema.Properties, "issue_number")
	assert.Contains(t, schema.Properties, "focus")
	assert.Contains(t, schema.Properties, "cross_repo")
	assert.Contains(t, schema.Properties, "max_nodes")
	assert.ElementsMatch(t, schema.Required, []string{"owner", "repo", "issue_number"})
}

// testIssueGraphDeps creates BaseDeps for issue_graph tests.
func testIssueGraphDeps(t *testing.T, httpClient *http.Client) BaseDeps {
	t.Helper()
	client := mustNewGHClient(t, httpClient)
	return BaseDeps{
		Client:    client,
		GQLClient: githubv4.NewClient(nil),
	}
}

// mockSubIssuesEmpty returns a handler that responds with an empty sub-issues list.
func mockSubIssuesEmpty() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}
}

// mockIssueJSON marshals an issue to JSON.
func mockIssueJSON(t *testing.T, issue *gogithub.Issue) []byte {
	t.Helper()
	data, err := json.Marshal(issue)
	require.NoError(t, err)
	return data
}

func TestIssueGraph_SingleIssue(t *testing.T) {
	mockIssue := &gogithub.Issue{
		Number: gogithub.Ptr(42),
		Title:  gogithub.Ptr("Fix login bug"),
		Body:   gogithub.Ptr("Users cannot log in after the recent update."),
		State:  gogithub.Ptr("open"),
		User:   &gogithub.User{Login: gogithub.Ptr("alice")},
	}

	httpClient := NewMockedHTTPClient(
		WithRequestMatch(
			GetReposIssuesByOwnerByRepoByIssueNumber,
			mockIssueJSON(t, mockIssue),
		),
		WithRequestMatchHandler(
			GetReposIssuesSubIssuesByOwnerByRepoByIssueNumber,
			mockSubIssuesEmpty(),
		),
	)

	serverTool := IssueGraph(translations.NullTranslationHelper)
	deps := testIssueGraphDeps(t, httpClient)

	args := map[string]any{
		"owner":        "owner",
		"repo":         "repo",
		"issue_number": float64(42),
	}
	request := createMCPRequest(args)
	result, err := serverTool.Handler(deps)(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "expected no error result, got: %v", result)

	text := getTextResult(t, result)
	assert.Contains(t, text.Text, "GRAPH SUMMARY")
	assert.Contains(t, text.Text, "#42")
	assert.Contains(t, text.Text, "Fix login bug")
	assert.Contains(t, text.Text, "task") // no sub-issues → task type
	assert.Contains(t, text.Text, "open")
	assert.Contains(t, text.Text, "[FOCUS]")
}

func TestIssueGraph_MissingRequiredParam(t *testing.T) {
	serverTool := IssueGraph(translations.NullTranslationHelper)
	deps := testIssueGraphDeps(t, nil)

	args := map[string]any{
		// missing owner and issue_number
		"repo": "repo",
	}
	request := createMCPRequest(args)
	result, err := serverTool.Handler(deps)(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "expected error result for missing required params")
}

func TestIssueGraph_NotFound(t *testing.T) {
	httpClient := NewMockedHTTPClient(
		WithRequestMatchHandler(
			GetReposIssuesByOwnerByRepoByIssueNumber,
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			},
		),
	)

	serverTool := IssueGraph(translations.NullTranslationHelper)
	deps := testIssueGraphDeps(t, httpClient)

	args := map[string]any{
		"owner":        "owner",
		"repo":         "repo",
		"issue_number": float64(999),
	}
	request := createMCPRequest(args)
	result, err := serverTool.Handler(deps)(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.NotNil(t, result)

	// When the root issue cannot be fetched, the graph still returns but with no nodes.
	// The summary will indicate the issue couldn't be fetched.
	text := getTextResult(t, result)
	assert.Contains(t, text.Text, "GRAPH SUMMARY")
}

func TestIssueGraph_ClosedIssue(t *testing.T) {
	stateReason := "completed"
	mockIssue := &gogithub.Issue{
		Number:      gogithub.Ptr(10),
		Title:       gogithub.Ptr("Implement feature X"),
		Body:        gogithub.Ptr("Feature X has been implemented."),
		State:       gogithub.Ptr("closed"),
		StateReason: &stateReason,
		User:        &gogithub.User{Login: gogithub.Ptr("bob")},
	}

	httpClient := NewMockedHTTPClient(
		WithRequestMatch(
			GetReposIssuesByOwnerByRepoByIssueNumber,
			mockIssueJSON(t, mockIssue),
		),
		WithRequestMatchHandler(
			GetReposIssuesSubIssuesByOwnerByRepoByIssueNumber,
			mockSubIssuesEmpty(),
		),
	)

	serverTool := IssueGraph(translations.NullTranslationHelper)
	deps := testIssueGraphDeps(t, httpClient)

	args := map[string]any{
		"owner":        "owner",
		"repo":         "repo",
		"issue_number": float64(10),
	}
	request := createMCPRequest(args)
	result, err := serverTool.Handler(deps)(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)

	text := getTextResult(t, result)
	assert.Contains(t, text.Text, "closed")
	assert.Contains(t, text.Text, "completed")
}

func TestExtractIssueRefs(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		defaultOwner string
		defaultRepo  string
		wantRefs     []issueRef
	}{
		{
			name:         "same-repo closes reference",
			text:         "This fixes #123",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs: []issueRef{
				{Owner: "owner", Repo: "repo", Number: 123, IsParent: true},
			},
		},
		{
			name:         "cross-repo reference",
			text:         "Related to other/myrepo#456",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs: []issueRef{
				{Owner: "other", Repo: "myrepo", Number: 456},
			},
		},
		{
			name:         "full github URL issue",
			text:         "See https://github.com/other/project/issues/789",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs: []issueRef{
				{Owner: "other", Repo: "project", Number: 789},
			},
		},
		{
			name:         "full github URL pull request",
			text:         "See https://github.com/other/project/pull/100",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs: []issueRef{
				{Owner: "other", Repo: "project", Number: 100},
			},
		},
		{
			name:         "no references",
			text:         "Nothing to see here",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs:     []issueRef{},
		},
		{
			name:         "reference in code block is ignored",
			text:         "```\nfixes #999\n```",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs:     []issueRef{},
		},
		{
			name:         "same-repo plain ref",
			text:         "Part of the work in #50",
			defaultOwner: "owner",
			defaultRepo:  "repo",
			wantRefs: []issueRef{
				{Owner: "owner", Repo: "repo", Number: 50},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractIssueRefs(tc.text, tc.defaultOwner, tc.defaultRepo)
			if len(tc.wantRefs) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tc.wantRefs, got)
			}
		})
	}
}

func TestClassifyIssueNode(t *testing.T) {
	tests := []struct {
		name          string
		isPR          bool
		labels        []string
		title         string
		issueTypeName string
		hasSubIssues  bool
		want          igNodeType
	}{
		{name: "PR", isPR: true, want: igNodeTypePR},
		{name: "epic by type", isPR: false, issueTypeName: "Epic", want: igNodeTypeEpic},
		{name: "epic by label", isPR: false, labels: []string{"epic"}, want: igNodeTypeEpic},
		{name: "epic by title", isPR: false, title: "Epic: Q3 roadmap", want: igNodeTypeEpic},
		{name: "batch via sub-issues", isPR: false, hasSubIssues: true, want: igNodeTypeBatch},
		{name: "plain task", isPR: false, want: igNodeTypeTask},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyIssueNode(tc.isPR, tc.labels, tc.title, tc.issueTypeName, tc.hasSubIssues)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSanitizeBodyPreview(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		maxLines   int
		maxLineLen int
		wantEmpty  bool
		wantHas    []string
	}{
		{name: "empty body", body: "", maxLines: 5, maxLineLen: 80, wantEmpty: true},
		{
			name:       "strips URLs",
			body:       "See https://example.com for details",
			maxLines:   5,
			maxLineLen: 80,
			wantHas:    []string{"[link]"},
		},
		{
			name:       "strips images",
			body:       "![alt](https://example.com/img.png)",
			maxLines:   5,
			maxLineLen: 80,
			wantHas:    []string{"[image]"},
		},
		{
			name:       "truncates long lines",
			body:       "This is a very long line that exceeds the maximum line length limit set for testing purposes here",
			maxLines:   5,
			maxLineLen: 50,
			wantHas:    []string{"..."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeBodyPreview(tc.body, tc.maxLines, tc.maxLineLen)
			if tc.wantEmpty {
				assert.Empty(t, got)
			}
			for _, s := range tc.wantHas {
				assert.Contains(t, got, s)
			}
		})
	}
}
