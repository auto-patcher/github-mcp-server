package github

import (
	"context"
	"container/heap"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/scopes"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	gogithub "github.com/google/go-github/v87/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shurcooL/githubv4"
)

const (
	// maxGraphDepth is the maximum BFS depth to crawl.
	maxGraphDepth = 4
	// maxGraphNodes is the default maximum number of nodes in the graph.
	maxGraphNodes = 50
	// maxConcurrentFetches is the maximum number of concurrent API calls.
	maxConcurrentFetches = 5
	// rateLimitBackoff is the base backoff duration when rate limited.
	rateLimitBackoff = 100 * time.Millisecond
)

// crawl priority levels (lower = higher priority)
const (
	priorityParent   = 0
	priorityChild    = 1
	priorityCrossRef = 2
)

// crawlItem represents an item to crawl in the BFS queue.
type crawlItem struct {
	owner      string
	repo       string
	number     int
	depth      int
	priority   int
	isAncestor bool // true if this is an ancestor of the focus node
	isCrossRef bool // true if reached via cross-reference (don't crawl further)
}

// crawlQueue implements heap.Interface for a priority queue.
type crawlQueue []*crawlItem

func (q crawlQueue) Len() int { return len(q) }
func (q crawlQueue) Less(i, j int) bool {
	if q[i].priority != q[j].priority {
		return q[i].priority < q[j].priority
	}
	return q[i].depth < q[j].depth
}
func (q crawlQueue) Swap(i, j int)       { q[i], q[j] = q[j], q[i] }
func (q *crawlQueue) Push(x any)         { *q = append(*q, x.(*crawlItem)) }
func (q *crawlQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return item
}

// igNodeType represents the type of a graph node.
type igNodeType string

const (
	igNodeTypeEpic  igNodeType = "epic"
	igNodeTypeBatch igNodeType = "batch"
	igNodeTypeTask  igNodeType = "task"
	igNodeTypePR    igNodeType = "pr"
)

// igRelationType represents the relationship between nodes.
type igRelationType string

const (
	igRelationParent  igRelationType = "parent"
	igRelationChild   igRelationType = "child"
	igRelationRelated igRelationType = "related"
)

// igNode represents a node in the issue graph.
type igNode struct {
	Owner       string     `json:"owner"`
	Repo        string     `json:"repo"`
	Number      int        `json:"number"`
	NodeType    igNodeType `json:"nodeType"`
	State       string     `json:"state"`
	StateReason string     `json:"stateReason,omitempty"`
	Title       string     `json:"title"`
	BodyPreview string     `json:"bodyPreview,omitempty"`
	Depth       int        `json:"depth"`
	IsFocus     bool       `json:"isFocus"`
}

// igEdge represents an edge in the issue graph.
type igEdge struct {
	FromOwner  string         `json:"fromOwner"`
	FromRepo   string         `json:"fromRepo"`
	FromNumber int            `json:"fromNumber"`
	ToOwner    string         `json:"toOwner"`
	ToRepo     string         `json:"toRepo"`
	ToNumber   int            `json:"toNumber"`
	Relation   igRelationType `json:"relation"`
}

// igGraph represents the complete graph structure.
type igGraph struct {
	FocusOwner  string    `json:"focusOwner"`
	FocusRepo   string    `json:"focusRepo"`
	FocusNumber int       `json:"focusNumber"`
	Nodes       []igNode  `json:"nodes"`
	Edges       []igEdge  `json:"edges"`
	Summary     string    `json:"summary"`
}

// igNodeKey creates a unique key for a node.
func igNodeKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", strings.ToLower(owner), strings.ToLower(repo), number)
}

// igRepoKey creates a unique key for a repository.
func igRepoKey(owner, repo string) string {
	return fmt.Sprintf("%s/%s", strings.ToLower(owner), strings.ToLower(repo))
}

// igEdgeKey creates a unique key for an edge for deduplication.
func igEdgeKey(e igEdge) string {
	return fmt.Sprintf("%s/%s#%d->%s/%s#%d:%s",
		strings.ToLower(e.FromOwner), strings.ToLower(e.FromRepo), e.FromNumber,
		strings.ToLower(e.ToOwner), strings.ToLower(e.ToRepo), e.ToNumber,
		e.Relation)
}

// issueRef is a reference to an issue or PR extracted from text.
type issueRef struct {
	Owner    string
	Repo     string
	Number   int
	IsParent bool
}

// Regular expressions for extracting issue references from text.
var (
	sameRepoRefRegex    = regexp.MustCompile(`(?:^|[^\w])#(\d+)`)
	crossRepoRefRegex   = regexp.MustCompile(`([a-zA-Z0-9](?:[a-zA-Z0-9._-]*[a-zA-Z0-9])?)/([a-zA-Z0-9._-]+)#(\d+)`)
	githubURLRefRegex   = regexp.MustCompile(`https?://(?:www\.)?github\.com/([a-zA-Z0-9](?:[a-zA-Z0-9._-]*[a-zA-Z0-9])?)/([a-zA-Z0-9._-]+)/(?:issues|pull)/(\d+)`)
	closesRefRegex      = regexp.MustCompile(`(?i)(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+(?:(?:([a-zA-Z0-9](?:[a-zA-Z0-9._-]*[a-zA-Z0-9])?)/([a-zA-Z0-9._-]+))?#(\d+))`)
	urlCleanRegex       = regexp.MustCompile(`https?://[^\s<>\[\]]+`)
	imageCleanRegex     = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	whitespaceCleanRegex = regexp.MustCompile(`\s+`)
	htmlTagCleanRegex   = regexp.MustCompile(`<[^>]*>`)
	fencedCodeRegex     = regexp.MustCompile("(?s)```[^`]*```")
	inlineCodeRegex     = regexp.MustCompile("`[^`]+`")
)

// stripCodeBlocks removes fenced and inline code blocks from text.
func stripCodeBlocks(text string) string {
	text = fencedCodeRegex.ReplaceAllString(text, "")
	text = inlineCodeRegex.ReplaceAllString(text, "")
	return text
}

// extractIssueRefs extracts all issue/PR references from the given text.
func extractIssueRefs(text, defaultOwner, defaultRepo string) []issueRef {
	text = stripCodeBlocks(text)
	refs := make([]issueRef, 0)
	seen := make(map[string]bool)

	// Extract "closes/fixes/resolves" references (these indicate parent relationship).
	for _, match := range closesRefRegex.FindAllStringSubmatch(text, -1) {
		owner := defaultOwner
		repo := defaultRepo
		if match[1] != "" && match[2] != "" {
			owner = match[1]
			repo = match[2]
		}
		var number int
		if _, err := fmt.Sscanf(match[3], "%d", &number); err == nil && number > 0 {
			key := igNodeKey(owner, repo, number)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, issueRef{Owner: owner, Repo: repo, Number: number, IsParent: true})
			}
		}
	}

	// Extract cross-repo references (owner/repo#number).
	for _, match := range crossRepoRefRegex.FindAllStringSubmatch(text, -1) {
		owner, repo := match[1], match[2]
		var number int
		if _, err := fmt.Sscanf(match[3], "%d", &number); err == nil && number > 0 {
			key := igNodeKey(owner, repo, number)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, issueRef{Owner: owner, Repo: repo, Number: number})
			}
		}
	}

	// Extract full GitHub URL references.
	for _, match := range githubURLRefRegex.FindAllStringSubmatch(text, -1) {
		owner, repo := match[1], match[2]
		var number int
		if _, err := fmt.Sscanf(match[3], "%d", &number); err == nil && number > 0 {
			key := igNodeKey(owner, repo, number)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, issueRef{Owner: owner, Repo: repo, Number: number})
			}
		}
	}

	// Extract same-repo references (#number).
	for _, match := range sameRepoRefRegex.FindAllStringSubmatch(text, -1) {
		var number int
		if _, err := fmt.Sscanf(match[1], "%d", &number); err == nil && number > 0 {
			key := igNodeKey(defaultOwner, defaultRepo, number)
			if !seen[key] {
				seen[key] = true
				refs = append(refs, issueRef{Owner: defaultOwner, Repo: defaultRepo, Number: number})
			}
		}
	}

	return refs
}

// sanitizeBodyPreview produces a short text preview of an issue body.
func sanitizeBodyPreview(body string, maxLines, maxLineLen int) string {
	if body == "" {
		return ""
	}
	body = imageCleanRegex.ReplaceAllString(body, "[image]")
	body = urlCleanRegex.ReplaceAllString(body, "[link]")
	body = htmlTagCleanRegex.ReplaceAllString(body, "")

	lines := strings.Split(body, "\n")
	result := make([]string, 0, maxLines)
	for _, line := range lines {
		line = whitespaceCleanRegex.ReplaceAllString(line, " ")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > maxLineLen {
			line = line[:maxLineLen-3] + "..."
		}
		result = append(result, line)
		if len(result) >= maxLines {
			break
		}
	}
	return strings.Join(result, " | ")
}

// classifyIssueNode determines the node type based on issue properties.
func classifyIssueNode(isPR bool, labels []string, title string, issueTypeName string, hasSubIssues bool) igNodeType {
	if isPR {
		return igNodeTypePR
	}
	typeLower := strings.ToLower(issueTypeName)
	if strings.Contains(typeLower, "epic") {
		return igNodeTypeEpic
	}
	titleLower := strings.ToLower(title)
	for _, label := range labels {
		if strings.Contains(strings.ToLower(label), "epic") {
			return igNodeTypeEpic
		}
	}
	if strings.Contains(titleLower, "epic") {
		return igNodeTypeEpic
	}
	if hasSubIssues {
		return igNodeTypeBatch
	}
	return igNodeTypeTask
}

// graphCrawlerState manages the state of the BFS crawl.
type graphCrawlerState struct {
	client           *gogithub.Client
	gqlClient        *githubv4.Client
	focusOwner       string
	focusRepo        string
	focusNumber      int
	originalOwner    string
	originalRepo     string
	originalNumber   int
	maxNodes         int
	crossRepo        bool
	nodes            map[string]*igNode
	edges            []igEdge
	parentMap        map[string]string
	inaccessibleRepo map[string]bool
	mu               sync.RWMutex
	sem              chan struct{}
}

func newGraphCrawlerState(client *gogithub.Client, gqlClient *githubv4.Client, owner, repo string, number, maxNodes int, crossRepo bool) *graphCrawlerState {
	return &graphCrawlerState{
		client:           client,
		gqlClient:        gqlClient,
		focusOwner:       owner,
		focusRepo:        repo,
		focusNumber:      number,
		originalOwner:    owner,
		originalRepo:     repo,
		originalNumber:   number,
		maxNodes:         maxNodes,
		crossRepo:        crossRepo,
		nodes:            make(map[string]*igNode),
		edges:            make([]igEdge, 0),
		parentMap:        make(map[string]string),
		inaccessibleRepo: make(map[string]bool),
		sem:              make(chan struct{}, maxConcurrentFetches),
	}
}

func (gc *graphCrawlerState) isRepoInaccessible(owner, repo string) bool {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return gc.inaccessibleRepo[igRepoKey(owner, repo)]
}

func (gc *graphCrawlerState) markRepoInaccessible(owner, repo string) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.inaccessibleRepo[igRepoKey(owner, repo)] = true
}

func (gc *graphCrawlerState) nodeCount() int {
	gc.mu.RLock()
	defer gc.mu.RUnlock()
	return len(gc.nodes)
}

// fetchNode fetches a single issue/PR and adds it to the graph.
func (gc *graphCrawlerState) fetchNode(ctx context.Context, owner, repo string, number, depth int) (*igNode, *gogithub.Issue, error) {
	key := igNodeKey(owner, repo, number)

	gc.mu.RLock()
	if node, exists := gc.nodes[key]; exists {
		gc.mu.RUnlock()
		return node, nil, nil
	}
	gc.mu.RUnlock()

	if gc.isRepoInaccessible(owner, repo) {
		return nil, nil, nil
	}

	select {
	case gc.sem <- struct{}{}:
		defer func() { <-gc.sem }()
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	var issue *gogithub.Issue
	var resp *gogithub.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		issue, resp, err = gc.client.Issues.Get(ctx, owner, repo, number)
		if err == nil {
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 429 || (resp.StatusCode == 403 && resp.Rate.Remaining == 0) {
				backoff := rateLimitBackoff * time.Duration(1<<attempt)
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
			if resp.StatusCode == 403 && resp.Rate.Remaining > 0 {
				gc.markRepoInaccessible(owner, repo)
			}
			if resp.StatusCode == 403 || resp.StatusCode == 404 {
				return nil, nil, nil
			}
		}
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	isPR := issue.IsPullRequest()

	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		if label.Name != nil {
			labels = append(labels, *label.Name)
		}
	}

	// Check for sub-issues (needed to classify as batch).
	hasSubIssues := false
	if !isPR {
		subIssues, subResp, subErr := gc.client.SubIssue.ListByIssue(ctx, owner, repo, int64(number), &gogithub.ListOptions{PerPage: 1})
		if subErr == nil && len(subIssues) > 0 {
			hasSubIssues = true
		}
		if subResp != nil {
			_ = subResp.Body.Close()
		}
	}

	issueTypeName := ""
	if issue.Type != nil {
		issueTypeName = issue.Type.GetName()
	}

	nodeType := classifyIssueNode(isPR, labels, issue.GetTitle(), issueTypeName, hasSubIssues)

	state := issue.GetState()
	stateReason := ""
	if isPR {
		if prLinks := issue.GetPullRequestLinks(); prLinks != nil && !prLinks.GetMergedAt().IsZero() {
			state = "merged"
			stateReason = "merged"
		}
	} else if issue.StateReason != nil {
		stateReason = *issue.StateReason
	}

	bodyPreviewLines := 5
	bodyPreviewLineLen := 100
	if depth == 0 {
		bodyPreviewLines = 8
		bodyPreviewLineLen = 120
	}

	node := &igNode{
		Owner:       owner,
		Repo:        repo,
		Number:      number,
		NodeType:    nodeType,
		State:       state,
		StateReason: stateReason,
		Title:       issue.GetTitle(),
		BodyPreview: sanitizeBodyPreview(issue.GetBody(), bodyPreviewLines, bodyPreviewLineLen),
		Depth:       depth,
		IsFocus: strings.EqualFold(owner, gc.focusOwner) &&
			strings.EqualFold(repo, gc.focusRepo) &&
			number == gc.focusNumber,
	}

	gc.mu.Lock()
	gc.nodes[key] = node
	gc.mu.Unlock()

	return node, issue, nil
}

// crawlResult is the result of processing a single node.
type crawlResult struct {
	key      string
	node     *igNode
	newItems []*crawlItem
	err      error
}

// processNode fetches a node and discovers related items to crawl next.
func (gc *graphCrawlerState) processNode(ctx context.Context, item *crawlItem) *crawlResult {
	result := &crawlResult{
		key:      igNodeKey(item.owner, item.repo, item.number),
		newItems: []*crawlItem{},
	}

	node, issue, err := gc.fetchNode(ctx, item.owner, item.repo, item.number, item.depth)
	if err != nil {
		result.err = err
		return result
	}
	result.node = node

	if node == nil || issue == nil {
		return result
	}

	if item.depth >= maxGraphDepth || item.isCrossRef {
		return result
	}

	key := result.key

	// For issues (not PRs), fetch parent via GraphQL and sub-issues via REST.
	if !issue.IsPullRequest() {
		// Fetch parent via GraphQL (lightweight query).
		if gc.gqlClient != nil {
			if parentRef := gc.fetchParentRef(ctx, item.owner, item.repo, item.number); parentRef != nil {
				parentKey := igNodeKey(parentRef.Owner, parentRef.Repo, parentRef.Number)

				gc.mu.Lock()
				gc.parentMap[key] = parentKey
				gc.edges = append(gc.edges, igEdge{
					FromOwner:  item.owner,
					FromRepo:   item.repo,
					FromNumber: item.number,
					ToOwner:    parentRef.Owner,
					ToRepo:     parentRef.Repo,
					ToNumber:   parentRef.Number,
					Relation:   igRelationParent,
				})
				gc.mu.Unlock()

				if gc.crossRepo || strings.EqualFold(parentRef.Owner, gc.focusOwner) && strings.EqualFold(parentRef.Repo, gc.focusRepo) {
					result.newItems = append(result.newItems, &crawlItem{
						owner:      parentRef.Owner,
						repo:       parentRef.Repo,
						number:     parentRef.Number,
						depth:      item.depth,
						priority:   priorityParent,
						isAncestor: true,
					})
				}
			}
		}

		// Fetch sub-issues via REST API. Skip for ancestor nodes.
		if !item.isAncestor {
			subIssues, subResp, subErr := gc.client.SubIssue.ListByIssue(ctx, item.owner, item.repo, int64(item.number), &gogithub.ListOptions{PerPage: 50})
			if subErr == nil {
				for _, sub := range subIssues {
					subOwner := item.owner
					subRepo := item.repo
					if sub.Repository != nil {
						if sub.Repository.Owner != nil && sub.Repository.Owner.Login != nil {
							subOwner = *sub.Repository.Owner.Login
						}
						if sub.Repository.Name != nil {
							subRepo = *sub.Repository.Name
						}
					}
					if sub.Number == nil {
						continue
					}
					subNumber := int(*sub.Number)
					subKey := igNodeKey(subOwner, subRepo, subNumber)

					// Skip cross-repo if not requested.
					if !gc.crossRepo && (!strings.EqualFold(subOwner, gc.focusOwner) || !strings.EqualFold(subRepo, gc.focusRepo)) {
						continue
					}

					gc.mu.Lock()
					gc.parentMap[subKey] = key
					gc.edges = append(gc.edges, igEdge{
						FromOwner:  item.owner,
						FromRepo:   item.repo,
						FromNumber: item.number,
						ToOwner:    subOwner,
						ToRepo:     subRepo,
						ToNumber:   subNumber,
						Relation:   igRelationChild,
					})
					gc.mu.Unlock()

					result.newItems = append(result.newItems, &crawlItem{
						owner:    subOwner,
						repo:     subRepo,
						number:   subNumber,
						depth:    item.depth + 1,
						priority: priorityChild,
					})
				}
			}
			if subResp != nil {
				_ = subResp.Body.Close()
			}
		}
	}

	// Process body references (closes/fixes + cross-refs).
	bodyRefs := extractIssueRefs(issue.GetBody(), item.owner, item.repo)
	for _, ref := range bodyRefs {
		if gc.isRepoInaccessible(ref.Owner, ref.Repo) {
			continue
		}

		// Skip cross-repo if not requested.
		if !gc.crossRepo && (!strings.EqualFold(ref.Owner, gc.focusOwner) || !strings.EqualFold(ref.Repo, gc.focusRepo)) {
			continue
		}

		refKey := igNodeKey(ref.Owner, ref.Repo, ref.Number)
		if refKey == key {
			continue
		}

		relation := igRelationRelated
		priority := priorityCrossRef
		if ref.IsParent {
			relation = igRelationParent
			priority = priorityParent
			gc.mu.Lock()
			gc.parentMap[key] = refKey
			gc.mu.Unlock()
		}

		gc.mu.Lock()
		gc.edges = append(gc.edges, igEdge{
			FromOwner:  item.owner,
			FromRepo:   item.repo,
			FromNumber: item.number,
			ToOwner:    ref.Owner,
			ToRepo:     ref.Repo,
			ToNumber:   ref.Number,
			Relation:   relation,
		})
		gc.mu.Unlock()

		result.newItems = append(result.newItems, &crawlItem{
			owner:      ref.Owner,
			repo:       ref.Repo,
			number:     ref.Number,
			depth:      item.depth + 1,
			priority:   priority,
			isCrossRef: !ref.IsParent,
		})
	}

	// For the focus node, also fetch cross-referenced issues from timeline.
	if node.IsFocus {
		gc.processTimeline(ctx, item, key, result)
	}

	return result
}

// processTimeline fetches cross-references from the issue timeline (focus node only).
func (gc *graphCrawlerState) processTimeline(ctx context.Context, item *crawlItem, key string, result *crawlResult) {
	timelineEvents, timelineResp, err := gc.client.Issues.ListIssueTimeline(ctx, item.owner, item.repo, item.number, &gogithub.ListOptions{
		PerPage: 100,
	})
	if err == nil {
		for _, event := range timelineEvents {
			if event.GetEvent() != "cross-referenced" {
				continue
			}
			source := event.GetSource()
			if source == nil {
				continue
			}
			sourceIssue := source.GetIssue()
			if sourceIssue == nil || sourceIssue.Number == nil {
				continue
			}

			refOwner, refRepo := item.owner, item.repo
			if sourceIssue.RepositoryURL != nil {
				parts := strings.Split(*sourceIssue.RepositoryURL, "/")
				if len(parts) >= 2 {
					refOwner = parts[len(parts)-2]
					refRepo = parts[len(parts)-1]
				}
			}

			if gc.isRepoInaccessible(refOwner, refRepo) {
				continue
			}
			if !gc.crossRepo && (!strings.EqualFold(refOwner, gc.focusOwner) || !strings.EqualFold(refRepo, gc.focusRepo)) {
				continue
			}

			refNumber := int(*sourceIssue.Number)
			refKey := igNodeKey(refOwner, refRepo, refNumber)
			if refKey == key {
				continue
			}

			gc.mu.Lock()
			gc.edges = append(gc.edges, igEdge{
				FromOwner:  refOwner,
				FromRepo:   refRepo,
				FromNumber: refNumber,
				ToOwner:    item.owner,
				ToRepo:     item.repo,
				ToNumber:   item.number,
				Relation:   igRelationRelated,
			})
			gc.mu.Unlock()

			result.newItems = append(result.newItems, &crawlItem{
				owner:      refOwner,
				repo:       refRepo,
				number:     refNumber,
				depth:      item.depth + 1,
				priority:   priorityCrossRef,
				isCrossRef: true,
			})
		}
	}
	if timelineResp != nil {
		_ = timelineResp.Body.Close()
	}
}

// fetchParentRef fetches the parent issue ref via GraphQL.
func (gc *graphCrawlerState) fetchParentRef(ctx context.Context, owner, repo string, number int) *issueRef {
	if gc.gqlClient == nil {
		return nil
	}

	var query struct {
		Repository struct {
			Issue struct {
				Parent *struct {
					Number     githubv4.Int
					Repository struct {
						Owner struct{ Login githubv4.String }
						Name  githubv4.String
					}
				}
			} `graphql:"issue(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	vars := map[string]interface{}{
		"owner":  githubv4.String(owner),
		"repo":   githubv4.String(repo),
		"number": githubv4.Int(int32(number)), //nolint:gosec
	}

	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if err := gc.gqlClient.Query(queryCtx, &query, vars); err != nil {
		return nil
	}
	if query.Repository.Issue.Parent == nil {
		return nil
	}

	p := query.Repository.Issue.Parent
	return &issueRef{
		Owner:  string(p.Repository.Owner.Login),
		Repo:   string(p.Repository.Name),
		Number: int(p.Number),
	}
}

// crawl performs the BFS traversal.
func (gc *graphCrawlerState) crawl(ctx context.Context) error {
	queue := &crawlQueue{}
	heap.Init(queue)
	heap.Push(queue, &crawlItem{
		owner:    gc.focusOwner,
		repo:     gc.focusRepo,
		number:   gc.focusNumber,
		depth:    0,
		priority: priorityChild,
	})

	queued := make(map[string]bool)
	queued[igNodeKey(gc.focusOwner, gc.focusRepo, gc.focusNumber)] = true

	const numWorkers = maxConcurrentFetches
	jobs := make(chan *crawlItem, numWorkers*2)
	results := make(chan *crawlResult, numWorkers*2)

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				r := gc.processNode(ctx, item)
				select {
				case results <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	inFlight := 0

	for {
		select {
		case <-ctx.Done():
			close(jobs)
			for range results { //nolint:revive
			}
			return ctx.Err()
		default:
		}

		for queue.Len() > 0 && inFlight < numWorkers {
			if gc.nodeCount() >= gc.maxNodes {
				break
			}

			item := heap.Pop(queue).(*crawlItem)
			itemKey := igNodeKey(item.owner, item.repo, item.number)

			gc.mu.RLock()
			_, visited := gc.nodes[itemKey]
			gc.mu.RUnlock()
			if visited {
				continue
			}

			if gc.isRepoInaccessible(item.owner, item.repo) {
				continue
			}
			if item.depth > maxGraphDepth {
				continue
			}

			select {
			case jobs <- item:
				inFlight++
			case <-ctx.Done():
				close(jobs)
				for range results { //nolint:revive
				}
				return ctx.Err()
			}
		}

		if queue.Len() == 0 && inFlight == 0 {
			close(jobs)
			for range results { //nolint:revive
			}
			return nil
		}

		select {
		case result, ok := <-results:
			if !ok {
				return nil
			}
			inFlight--
			if result.err != nil || result.node == nil {
				continue
			}

			for _, newItem := range result.newItems {
				newKey := igNodeKey(newItem.owner, newItem.repo, newItem.number)
				if !queued[newKey] && gc.nodeCount() < gc.maxNodes {
					queued[newKey] = true
					heap.Push(queue, newItem)
				}
			}

		case <-ctx.Done():
			close(jobs)
			for range results { //nolint:revive
			}
			return ctx.Err()
		}
	}
}

// findAncestors walks up the parentMap chain.
func (gc *graphCrawlerState) findAncestors(key string) []string {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	ancestors := make([]string, 0)
	seen := make(map[string]bool)
	current := key
	for {
		parentKey, exists := gc.parentMap[current]
		if !exists || seen[parentKey] {
			break
		}
		seen[parentKey] = true
		ancestors = append(ancestors, parentKey)
		current = parentKey
	}
	return ancestors
}

// refocusTo updates the focus node, used when focus=epic|batch.
func (gc *graphCrawlerState) refocusTo(owner, repo string, number int) {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.focusOwner = owner
	gc.focusRepo = repo
	gc.focusNumber = number
	focusKey := igNodeKey(owner, repo, number)
	for k, node := range gc.nodes {
		node.IsFocus = k == focusKey
	}
}

// findBestFocus searches for the best focus node of the given type.
func (gc *graphCrawlerState) findBestFocus(focusType string) (string, string, int, bool) {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	var targetType igNodeType
	switch focusType {
	case "epic":
		targetType = igNodeTypeEpic
	case "batch":
		targetType = igNodeTypeBatch
	default:
		return gc.focusOwner, gc.focusRepo, gc.focusNumber, false
	}

	originalKey := igNodeKey(gc.focusOwner, gc.focusRepo, gc.focusNumber)
	if node, exists := gc.nodes[originalKey]; exists && node.NodeType == targetType {
		return gc.focusOwner, gc.focusRepo, gc.focusNumber, false
	}

	// Walk up ancestor chain.
	seen := make(map[string]bool)
	current := originalKey
	var batchFallback *igNode
	for {
		parentKey, exists := gc.parentMap[current]
		if !exists || seen[parentKey] {
			break
		}
		seen[parentKey] = true
		if node, exists := gc.nodes[parentKey]; exists {
			if node.NodeType == targetType {
				return node.Owner, node.Repo, node.Number, true
			}
			if targetType == igNodeTypeEpic && node.NodeType == igNodeTypeBatch && batchFallback == nil {
				batchFallback = node
			}
		}
		current = parentKey
	}

	if batchFallback != nil {
		return batchFallback.Owner, batchFallback.Repo, batchFallback.Number, true
	}

	return gc.focusOwner, gc.focusRepo, gc.focusNumber, false
}

// buildGraph assembles the final igGraph.
func (gc *graphCrawlerState) buildGraph() *igGraph {
	gc.mu.RLock()
	defer gc.mu.RUnlock()

	nodes := make([]igNode, 0, len(gc.nodes))
	for _, node := range gc.nodes {
		nodes = append(nodes, *node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].Number < nodes[j].Number
	})

	seenEdges := make(map[string]bool)
	uniqueEdges := make([]igEdge, 0, len(gc.edges))
	for _, e := range gc.edges {
		k := igEdgeKey(e)
		if !seenEdges[k] {
			seenEdges[k] = true
			uniqueEdges = append(uniqueEdges, e)
		}
	}

	return &igGraph{
		FocusOwner:  gc.focusOwner,
		FocusRepo:   gc.focusRepo,
		FocusNumber: gc.focusNumber,
		Nodes:       nodes,
		Edges:       uniqueEdges,
		Summary:     gc.generateSummary(nodes, uniqueEdges),
	}
}

// igFormatRef formats a node reference using short form (#N) for same-repo.
func igFormatRef(owner, repo string, number int, focusOwner, focusRepo string) string {
	if strings.EqualFold(owner, focusOwner) && strings.EqualFold(repo, focusRepo) {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// generateSummary builds the natural-language summary block.
func (gc *graphCrawlerState) generateSummary(nodes []igNode, edges []igEdge) string {
	focusKey := igNodeKey(gc.focusOwner, gc.focusRepo, gc.focusNumber)
	var focusNode *igNode
	for i := range nodes {
		if igNodeKey(nodes[i].Owner, nodes[i].Repo, nodes[i].Number) == focusKey {
			focusNode = &nodes[i]
			break
		}
	}
	if focusNode == nil {
		return "Unable to fetch the requested issue or pull request."
	}

	var sb strings.Builder
	focusRef := igFormatRef(gc.focusOwner, gc.focusRepo, gc.focusNumber, gc.originalOwner, gc.originalRepo)
	fmt.Fprintf(&sb, "Focus: %s (%s) %q\n", focusRef, focusNode.NodeType, focusNode.Title)

	stateStr := focusNode.State
	if focusNode.StateReason != "" && focusNode.StateReason != focusNode.State {
		stateStr = fmt.Sprintf("%s (%s)", focusNode.State, focusNode.StateReason)
	}
	fmt.Fprintf(&sb, "State: %s\n", stateStr)

	// Ancestry path.
	ancestors := gc.findAncestors(focusKey)
	if len(ancestors) > 0 {
		sb.WriteString("Hierarchy: ")
		for i := len(ancestors) - 1; i >= 0; i-- {
			if an, exists := gc.nodes[ancestors[i]]; exists {
				ref := igFormatRef(an.Owner, an.Repo, an.Number, gc.focusOwner, gc.focusRepo)
				fmt.Fprintf(&sb, "%s (%s) → ", ref, an.NodeType)
			}
		}
		fmt.Fprintf(&sb, "#%d (%s)\n", gc.focusNumber, focusNode.NodeType)
	}

	// Child count.
	childCount := 0
	for _, e := range edges {
		if strings.EqualFold(e.FromOwner, gc.focusOwner) && strings.EqualFold(e.FromRepo, gc.focusRepo) &&
			e.FromNumber == gc.focusNumber && e.Relation == igRelationChild {
			childCount++
		}
	}
	if childCount > 0 {
		fmt.Fprintf(&sb, "Direct children: %d\n", childCount)
	}

	sb.WriteString("\n")

	var epicC, batchC, taskC, prC int
	for _, n := range nodes {
		switch n.NodeType {
		case igNodeTypeEpic:
			epicC++
		case igNodeTypeBatch:
			batchC++
		case igNodeTypeTask:
			taskC++
		case igNodeTypePR:
			prC++
		}
	}

	fmt.Fprintf(&sb, "Graph contains %d nodes: ", len(nodes))
	parts := make([]string, 0, 4)
	if epicC > 0 {
		parts = append(parts, fmt.Sprintf("%d epic(s)", epicC))
	}
	if batchC > 0 {
		parts = append(parts, fmt.Sprintf("%d batch issue(s)", batchC))
	}
	if taskC > 0 {
		parts = append(parts, fmt.Sprintf("%d task(s)", taskC))
	}
	if prC > 0 {
		parts = append(parts, fmt.Sprintf("%d PR(s)", prC))
	}
	sb.WriteString(strings.Join(parts, ", "))
	sb.WriteString("\n")
	return sb.String()
}

// formatIssueGraph formats the graph for LLM consumption.
func formatIssueGraph(graph *igGraph) string {
	var sb strings.Builder

	sb.WriteString("GRAPH SUMMARY\n")
	sb.WriteString("=============\n")
	sb.WriteString(graph.Summary)
	sb.WriteString("\n")
	sb.WriteString("Node types: epic (large initiative), batch (has sub-issues), task (regular issue), pr (pull request)\n\n")

	fmt.Fprintf(&sb, "NODES (%d total)\n", len(graph.Nodes))
	sb.WriteString("===============\n")
	for _, node := range graph.Nodes {
		focusMarker := ""
		if node.IsFocus {
			focusMarker = " [FOCUS]"
		}
		ref := igFormatRef(node.Owner, node.Repo, node.Number, graph.FocusOwner, graph.FocusRepo)
		stateStr := node.State
		if node.StateReason != "" && node.StateReason != node.State {
			stateStr = fmt.Sprintf("%s (%s)", node.State, node.StateReason)
		}
		fmt.Fprintf(&sb, "%s|%s|%s|%s%s\n", ref, node.NodeType, stateStr, node.Title, focusMarker)
		if node.BodyPreview != "" {
			fmt.Fprintf(&sb, "  Preview: %s\n", node.BodyPreview)
		}
	}

	// Parent→child edges.
	sb.WriteString("\nSUB-ISSUES (parent → child)\n")
	sb.WriteString("===========================\n")
	var parentChildEdges, relatedEdges []igEdge
	for _, e := range graph.Edges {
		switch e.Relation {
		case igRelationChild:
			parentChildEdges = append(parentChildEdges, e)
		case igRelationParent:
			parentChildEdges = append(parentChildEdges, igEdge{
				FromOwner:  e.ToOwner,
				FromRepo:   e.ToRepo,
				FromNumber: e.ToNumber,
				ToOwner:    e.FromOwner,
				ToRepo:     e.FromRepo,
				ToNumber:   e.FromNumber,
				Relation:   igRelationChild,
			})
		case igRelationRelated:
			relatedEdges = append(relatedEdges, e)
		}
	}

	if len(parentChildEdges) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, e := range parentChildEdges {
			fromRef := igFormatRef(e.FromOwner, e.FromRepo, e.FromNumber, graph.FocusOwner, graph.FocusRepo)
			toRef := igFormatRef(e.ToOwner, e.ToRepo, e.ToNumber, graph.FocusOwner, graph.FocusRepo)
			fmt.Fprintf(&sb, "%s → %s\n", fromRef, toRef)
		}
	}

	sb.WriteString("\nCROSS-REFERENCES (mentioned/referenced)\n")
	sb.WriteString("=======================================\n")
	if len(relatedEdges) == 0 {
		sb.WriteString("(none)\n")
	} else {
		for _, e := range relatedEdges {
			fromRef := igFormatRef(e.FromOwner, e.FromRepo, e.FromNumber, graph.FocusOwner, graph.FocusRepo)
			toRef := igFormatRef(e.ToOwner, e.ToRepo, e.ToNumber, graph.FocusOwner, graph.FocusRepo)
			fmt.Fprintf(&sb, "%s ↔ %s\n", fromRef, toRef)
		}
	}

	return sb.String()
}

// IssueGraph creates a tool to build a graph of issue/PR relationships via BFS.
func IssueGraph(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataIssues,
		mcp.Tool{
			Name: "issue_graph",
			Description: t("TOOL_ISSUE_GRAPH_DESCRIPTION", `Get a graph representation of issue and pull request relationships, showing the full work hierarchy in one call.

Returns a comprehensive view including:
- Node types: epic (large initiatives), batch (parent issues), task (regular issues), pr (pull requests)
- Full hierarchy: epic → batch → task → PR relationships
- Sub-issues and "closes/fixes" references
- Cross-references and related work
- Open/closed/merged state of all related items

Use focus="epic" to automatically find and focus on the parent epic of any issue.
Use focus="batch" to find the nearest batch/parent issue in the hierarchy.`),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_ISSUE_GRAPH_USER_TITLE", "Get issue relationship graph"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"issue_number": {
						Type:        "number",
						Description: "Issue or pull request number to build the graph from",
					},
					"focus": {
						Type:        "string",
						Description: "Which node type to focus on: 'provided' (default) uses the specified issue/PR, 'epic' shifts focus to the nearest epic in the hierarchy, 'batch' shifts focus to the nearest batch/parent issue",
						Enum:        []any{"provided", "epic", "batch"},
					},
					"cross_repo": {
						Type:        "boolean",
						Description: "Follow references into other repositories (default: false)",
					},
					"max_nodes": {
						Type:        "number",
						Description: "Maximum number of nodes to include in the graph (default: 50)",
					},
				},
				Required: []string{"owner", "repo", "issue_number"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			issueNumber, err := RequiredInt(args, "issue_number")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			focusType, err := OptionalParam[string](args, "focus")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if focusType == "" {
				focusType = "provided"
			}
			crossRepo, err := OptionalParam[bool](args, "cross_repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			maxNodes, err := OptionalIntParam(args, "max_nodes")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if maxNodes <= 0 {
				maxNodes = maxGraphNodes
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultErrorFromErr("failed to get GitHub client", err), nil, nil
			}

			gqlClient, _ := deps.GetGQLClient(ctx) // nil is OK

			crawlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			gc := newGraphCrawlerState(client, gqlClient, owner, repo, issueNumber, maxNodes, crossRepo)
			if err := gc.crawl(crawlCtx); err != nil && crawlCtx.Err() != context.DeadlineExceeded {
				return nil, nil, fmt.Errorf("failed to crawl issue graph: %w", err)
			}

			if focusType != "provided" {
				newOwner, newRepo, newNumber, changed := gc.findBestFocus(focusType)
				if changed {
					gc.refocusTo(newOwner, newRepo, newNumber)
				}
			}

			graph := gc.buildGraph()
			return utils.NewToolResultText(formatIssueGraph(graph)), nil, nil
		},
	)
}
