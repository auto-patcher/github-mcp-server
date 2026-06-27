package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	ghErrors "github.com/github/github-mcp-server/pkg/errors"
	"github.com/github/github-mcp-server/pkg/ifc"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/scopes"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/google/go-github/v87/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shurcooL/githubv4"
)

const (
	ProjectUpdateFailedError             = "failed to update a project item"
	ProjectAddFailedError                = "failed to add a project item"
	ProjectDeleteFailedError             = "failed to delete a project item"
	ProjectListFailedError               = "failed to list project items"
	ProjectStatusUpdateListFailedError   = "failed to list project status updates"
	ProjectStatusUpdateGetFailedError    = "failed to get project status update"
	ProjectStatusUpdateCreateFailedError = "failed to create project status update"
	ProjectResolveIDFailedError          = "failed to resolve project ID"
	MaxProjectsPerPage                   = 50
)

// Method constants for consolidated project tools
const (
	projectsMethodListProjects              = "list_projects"
	projectsMethodListProjectFields         = "list_project_fields"
	projectsMethodListProjectItems          = "list_project_items"
	projectsMethodGetProject                = "get_project"
	projectsMethodGetProjectField           = "get_project_field"
	projectsMethodGetProjectItem            = "get_project_item"
	projectsMethodAddProjectItem            = "add_project_item"
	projectsMethodUpdateProjectItem         = "update_project_item"
	projectsMethodDeleteProjectItem         = "delete_project_item"
	projectsMethodListProjectStatusUpdates  = "list_project_status_updates"
	projectsMethodGetProjectStatusUpdate    = "get_project_status_update"
	projectsMethodCreateProjectStatusUpdate = "create_project_status_update"
	projectsMethodCreateProject             = "create_project"
	projectsMethodCreateIterationField      = "create_iteration_field"
	projectsMethodCreateProjectField        = "create_project_field"
	projectsMethodAddProjectFieldOption     = "add_project_field_option"
	projectsMethodDeleteProjectField        = "delete_project_field"
)

// FlexibleString handles JSON fields that the GitHub API may return as either a
// plain string or as an object with a "name" key (e.g. ProjectV2FieldOption.name
// changed from string to *ProjectV2TextContent in go-github v79+).
type FlexibleString string

func (f *FlexibleString) UnmarshalJSON(data []byte) error {
	// Try plain string first.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = FlexibleString(s)
		return nil
	}
	// Fall back to object with a "name" key.
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		*f = FlexibleString(obj.Name)
		return nil
	}
	return fmt.Errorf("FlexibleString: cannot unmarshal %s", string(data))
}

// projectFieldOption is a local type for a single-select option, using FlexibleString
// to handle the go-github v79+ schema change where "name" and "description" became objects.
type projectFieldOption struct {
	ID          string         `json:"id"`
	Name        FlexibleString `json:"name"`
	Color       string         `json:"color"`
	Description FlexibleString `json:"description"`
}

// projectFieldIteration is a local type for a single iteration entry.
type projectFieldIteration struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	StartDate string `json:"startDate"`
	Duration  int    `json:"duration"`
}

// projectFieldConfiguration holds the configuration block of an iteration field.
type projectFieldConfiguration struct {
	Iterations          []projectFieldIteration `json:"iterations"`
	CompletedIterations []projectFieldIteration `json:"completedIterations"`
}

// ProjectField is a local representation of a project field that avoids the
// go-github v79+ unmarshal bug for ProjectV2FieldOption.name.
type ProjectField struct {
	ID            int64                      `json:"id"`
	NodeID        string                     `json:"node_id,omitempty"`
	Name          string                     `json:"name,omitempty"`
	DataType      string                     `json:"data_type,omitempty"`
	Options       []projectFieldOption       `json:"options,omitempty"`
	Configuration *projectFieldConfiguration `json:"configuration,omitempty"`
}

// listProjectFieldsRaw fetches a page of project fields using a raw HTTP request,
// bypassing the go-github typed structs that fail to unmarshal the GitHub API
// response for option names when using go-github v79+.
func listProjectFieldsRaw(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, pagination github.ListProjectsPaginationOptions) ([]ProjectField, *github.Response, error) {
	var urlPath string
	if ownerType == "org" {
		urlPath = fmt.Sprintf("orgs/%s/projectsV2/%d/fields", owner, projectNumber)
	} else {
		urlPath = fmt.Sprintf("users/%s/projectsV2/%d/fields", owner, projectNumber)
	}

	// Append pagination query params.
	sep := "?"
	if pagination.PerPage > 0 {
		urlPath += fmt.Sprintf("%sper_page=%d", sep, pagination.PerPage)
		sep = "&"
	}
	if pagination.After != "" {
		urlPath += fmt.Sprintf("%safter=%s", sep, pagination.After)
		sep = "&"
	}
	if pagination.Before != "" {
		urlPath += fmt.Sprintf("%sbefore=%s", sep, pagination.Before)
	}

	req, err := client.NewRequest(ctx, http.MethodGet, urlPath, nil)
	if err != nil {
		return nil, nil, err
	}

	var fields []ProjectField
	resp, err := client.Do(req, &fields)
	if err != nil {
		return nil, resp, err
	}
	return fields, resp, nil
}

// getProjectFieldRaw fetches a single project field using a raw HTTP request.
func getProjectFieldRaw(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, fieldID int64) (*ProjectField, *github.Response, error) {
	var urlPath string
	if ownerType == "org" {
		urlPath = fmt.Sprintf("orgs/%s/projectsV2/%d/fields/%d", owner, projectNumber, fieldID)
	} else {
		urlPath = fmt.Sprintf("users/%s/projectsV2/%d/fields/%d", owner, projectNumber, fieldID)
	}

	req, err := client.NewRequest(ctx, http.MethodGet, urlPath, nil)
	if err != nil {
		return nil, nil, err
	}

	var field ProjectField
	resp, err := client.Do(req, &field)
	if err != nil {
		return nil, resp, err
	}
	return &field, resp, nil
}

// GraphQL types for ProjectV2 status updates

type statusUpdateNode struct {
	ID         githubv4.ID
	Body       *githubv4.String
	Status     *githubv4.String
	CreatedAt  githubv4.DateTime
	StartDate  *githubv4.String
	TargetDate *githubv4.String
	Creator    struct {
		Login githubv4.String
	}
}

type projectVisibility struct {
	Public githubv4.Boolean
}

type statusUpdateNodeWithProject struct {
	statusUpdateNode
	Project projectVisibility
}

type statusUpdateConnection struct {
	Nodes    []statusUpdateNode
	PageInfo PageInfoFragment
}

type statusUpdatesProject struct {
	Public        githubv4.Boolean
	StatusUpdates statusUpdateConnection `graphql:"statusUpdates(first: $first, after: $after, orderBy: {field: CREATED_AT, direction: DESC})"`
}

// statusUpdatesUserQuery is the GraphQL query for listing status updates on a user-owned project.
type statusUpdatesUserQuery struct {
	User struct {
		ProjectV2 statusUpdatesProject `graphql:"projectV2(number: $projectNumber)"`
	} `graphql:"user(login: $owner)"`
}

// statusUpdatesOrgQuery is the GraphQL query for listing status updates on an org-owned project.
type statusUpdatesOrgQuery struct {
	Organization struct {
		ProjectV2 statusUpdatesProject `graphql:"projectV2(number: $projectNumber)"`
	} `graphql:"organization(login: $owner)"`
}

// statusUpdateNodeQuery is the GraphQL query for fetching a single status update by node ID.
type statusUpdateNodeQuery struct {
	Node struct {
		StatusUpdate statusUpdateNodeWithProject `graphql:"... on ProjectV2StatusUpdate"`
	} `graphql:"node(id: $id)"`
}

// CreateProjectV2StatusUpdateInput is the input for the createProjectV2StatusUpdate mutation.
// Defined locally because the shurcooL/githubv4 library does not include this type.
type CreateProjectV2StatusUpdateInput struct {
	ProjectID        githubv4.ID      `json:"projectId"`
	Body             *githubv4.String `json:"body,omitempty"`
	Status           *githubv4.String `json:"status,omitempty"`
	StartDate        *githubv4.String `json:"startDate,omitempty"`
	TargetDate       *githubv4.String `json:"targetDate,omitempty"`
	ClientMutationID *githubv4.String `json:"clientMutationId,omitempty"`
}

// validProjectV2StatusUpdateStatuses is the set of valid status values for the createProjectV2StatusUpdate mutation.
var validProjectV2StatusUpdateStatuses = map[string]bool{
	"INACTIVE":  true,
	"ON_TRACK":  true,
	"AT_RISK":   true,
	"OFF_TRACK": true,
	"COMPLETE":  true,
}

func convertToMinimalStatusUpdate(node statusUpdateNode) MinimalProjectStatusUpdate {
	var creator *MinimalUser
	if login := string(node.Creator.Login); login != "" {
		creator = &MinimalUser{Login: login}
	}

	return MinimalProjectStatusUpdate{
		ID:         fmt.Sprintf("%v", node.ID),
		Body:       derefString(node.Body),
		Status:     derefString(node.Status),
		CreatedAt:  node.CreatedAt.Time.Format(time.RFC3339),
		StartDate:  derefString(node.StartDate),
		TargetDate: derefString(node.TargetDate),
		Creator:    creator,
	}
}

func derefString(s *githubv4.String) string {
	if s == nil {
		return ""
	}
	return string(*s)
}

// ProjectsList returns the tool and handler for listing GitHub Projects resources.
func ProjectsList(t translations.TranslationHelperFunc) inventory.ServerTool {
	tool := NewTool(
		ToolsetMetadataProjects,
		mcp.Tool{
			Name: "projects_list",
			Description: t("TOOL_PROJECTS_LIST_DESCRIPTION",
				`Tools for listing GitHub Projects resources.
Use this tool to list projects for a user or organization, or list project fields and items for a specific project.
`),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_PROJECTS_LIST_USER_TITLE", "List GitHub Projects resources"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"method": {
						Type:        "string",
						Description: "The action to perform",
						Enum: []any{
							projectsMethodListProjects,
							projectsMethodListProjectFields,
							projectsMethodListProjectItems,
							projectsMethodListProjectStatusUpdates,
						},
					},
					"owner_type": {
						Type:        "string",
						Description: "Owner type (user or org). If not provided, will automatically try both.",
						Enum:        []any{"user", "org"},
					},
					"owner": {
						Type:        "string",
						Description: "The owner (user or organization login). The name is not case sensitive.",
					},
					"project_number": {
						Type:        "number",
						Description: "The project's number. Required for 'list_project_fields', 'list_project_items', and 'list_project_status_updates' methods.",
					},
					"query": {
						Type:        "string",
						Description: `Filter/query string. For list_projects: filter by title text and state (e.g. "roadmap is:open"). For list_project_items: advanced filtering using GitHub's project filtering syntax.`,
					},
					"fields": {
						Type:        "array",
						Description: "Field IDs to include when listing project items (e.g. [\"102589\", \"985201\"]). CRITICAL: Always provide to get field values. Without this, only titles returned. Only used for 'list_project_items' method.",
						Items: &jsonschema.Schema{
							Type: "string",
						},
					},
					"per_page": {
						Type:        "number",
						Description: fmt.Sprintf("Results per page (max %d)", MaxProjectsPerPage),
					},
					"after": {
						Type:        "string",
						Description: "Forward pagination cursor from previous pageInfo.nextCursor.",
					},
					"before": {
						Type:        "string",
						Description: "Backward pagination cursor from previous pageInfo.prevCursor (rare).",
					},
				},
				Required: []string{"method", "owner"},
			},
		},
		[]scopes.Scope{scopes.ReadProject},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			method, err := RequiredParam[string](args, "method")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			ownerType, err := OptionalParam[string](args, "owner_type")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			switch method {
			case projectsMethodListProjects:
				result, visibilities, payload, err := listProjects(ctx, client, args, owner, ownerType)
				result = attachJoinedIFCLabel(ctx, deps, result, visibilities, ifc.LabelProjectList)
				return result, payload, err
			case projectsMethodListProjectFields, projectsMethodListProjectItems, projectsMethodListProjectStatusUpdates:
				// All other methods require project_number and ownerType detection
				projectNumber, err := RequiredInt(args, "project_number")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				if ownerType == "" {
					ownerType, err = detectOwnerType(ctx, client, owner, projectNumber)
					if err != nil {
						return utils.NewToolResultError(err.Error()), nil, nil
					}
				}

				switch method {
				case projectsMethodListProjectFields:
					result, payload, err := listProjectFields(ctx, client, args, owner, ownerType)
					if shouldAttachIFCLabel(ctx, deps, result) {
						isPrivate, visibilityErr := FetchProjectIsPrivate(ctx, client, owner, ownerType, projectNumber)
						if visibilityErr == nil {
							result = attachProjectVisibilityIFCLabel(ctx, deps, result, isPrivate, ifc.LabelProject)
						}
					}
					return result, payload, err
				case projectsMethodListProjectItems:
					result, payload, err := listProjectItems(ctx, client, args, owner, ownerType)
					if shouldAttachIFCLabel(ctx, deps, result) {
						isPrivate, visibilityErr := FetchProjectIsPrivate(ctx, client, owner, ownerType, projectNumber)
						if visibilityErr == nil {
							result = attachProjectVisibilityIFCLabel(ctx, deps, result, isPrivate, ifc.LabelProjectContent)
						}
					}
					return result, payload, err
				case projectsMethodListProjectStatusUpdates:
					gqlClient, err := deps.GetGQLClient(ctx)
					if err != nil {
						return utils.NewToolResultError(err.Error()), nil, nil
					}
					result, isPrivate, payload, err := listProjectStatusUpdates(ctx, gqlClient, args, owner, ownerType)
					result = attachStaticIFCLabel(ctx, deps, result, ifc.LabelProjectContent(isPrivate))
					return result, payload, err
				default:
					return utils.NewToolResultError(fmt.Sprintf("unknown method: %s", method)), nil, nil
				}
			default:
				return utils.NewToolResultError(fmt.Sprintf("unknown method: %s", method)), nil, nil
			}
		},
	)
	return tool
}

// ProjectsGet returns the tool and handler for getting GitHub Projects resources.
func ProjectsGet(t translations.TranslationHelperFunc) inventory.ServerTool {
	tool := NewTool(
		ToolsetMetadataProjects,
		mcp.Tool{
			Name: "projects_get",
			Description: t("TOOL_PROJECTS_GET_DESCRIPTION", `Get details about specific GitHub Projects resources.
Use this tool to get details about individual projects, project fields, and project items by their unique IDs.
`),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_PROJECTS_GET_USER_TITLE", "Get details of GitHub Projects resources"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"method": {
						Type:        "string",
						Description: "The method to execute",
						Enum: []any{
							projectsMethodGetProject,
							projectsMethodGetProjectField,
							projectsMethodGetProjectItem,
							projectsMethodGetProjectStatusUpdate,
						},
					},
					"owner_type": {
						Type:        "string",
						Description: "Owner type (user or org). If not provided, will be automatically detected.",
						Enum:        []any{"user", "org"},
					},
					"owner": {
						Type:        "string",
						Description: "The owner (user or organization login). The name is not case sensitive.",
					},
					"project_number": {
						Type:        "number",
						Description: "The project's number.",
					},
					"field_id": {
						Type:        "number",
						Description: "The field's ID. Required for 'get_project_field' method.",
					},
					"item_id": {
						Type:        "number",
						Description: "The item's ID. Required for 'get_project_item' method.",
					},
					"fields": {
						Type:        "array",
						Description: "Specific list of field IDs to include in the response when getting a project item (e.g. [\"102589\", \"985201\", \"169875\"]). If not provided, only the title field is included. Only used for 'get_project_item' method.",
						Items: &jsonschema.Schema{
							Type: "string",
						},
					},
					"status_update_id": {
						Type:        "string",
						Description: "The node ID of the project status update. Required for 'get_project_status_update' method.",
					},
				},
				Required: []string{"method"},
			},
		},
		[]scopes.Scope{scopes.ReadProject},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			method, err := RequiredParam[string](args, "method")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			// Handle get_project_status_update early — it only needs status_update_id
			if method == projectsMethodGetProjectStatusUpdate {
				statusUpdateID, err := RequiredParam[string](args, "status_update_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				gqlClient, err := deps.GetGQLClient(ctx)
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				result, isPrivate, payload, err := getProjectStatusUpdate(ctx, gqlClient, statusUpdateID)
				result = attachStaticIFCLabel(ctx, deps, result, ifc.LabelProjectContent(isPrivate))
				return result, payload, err
			}

			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			ownerType, err := OptionalParam[string](args, "owner_type")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			projectNumber, err := RequiredInt(args, "project_number")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			// Detect owner type if not provided
			if ownerType == "" {
				ownerType, err = detectOwnerType(ctx, client, owner, projectNumber)
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
			}

			switch method {
			case projectsMethodGetProject:
				result, isPrivate, payload, err := getProject(ctx, client, owner, ownerType, projectNumber)
				result = attachStaticIFCLabel(ctx, deps, result, ifc.LabelProject(isPrivate))
				return result, payload, err
			case projectsMethodGetProjectField:
				fieldID, err := RequiredBigInt(args, "field_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				result, payload, err := getProjectField(ctx, client, owner, ownerType, projectNumber, fieldID)
				if shouldAttachIFCLabel(ctx, deps, result) {
					isPrivate, visibilityErr := FetchProjectIsPrivate(ctx, client, owner, ownerType, projectNumber)
					if visibilityErr == nil {
						result = attachProjectVisibilityIFCLabel(ctx, deps, result, isPrivate, ifc.LabelProject)
					}
				}
				return result, payload, err
			case projectsMethodGetProjectItem:
				itemID, err := RequiredBigInt(args, "item_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				fields, err := OptionalBigIntArrayParam(args, "fields")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				result, payload, err := getProjectItem(ctx, client, owner, ownerType, projectNumber, itemID, fields)
				if shouldAttachIFCLabel(ctx, deps, result) {
					isPrivate, visibilityErr := FetchProjectIsPrivate(ctx, client, owner, ownerType, projectNumber)
					if visibilityErr == nil {
						result = attachProjectVisibilityIFCLabel(ctx, deps, result, isPrivate, ifc.LabelProjectContent)
					}
				}
				return result, payload, err
			default:
				return utils.NewToolResultError(fmt.Sprintf("unknown method: %s", method)), nil, nil
			}
		},
	)
	return tool
}

// ProjectsWrite returns the tool and handler for modifying GitHub Projects resources.
func ProjectsWrite(t translations.TranslationHelperFunc) inventory.ServerTool {
	tool := NewTool(
		ToolsetMetadataProjects,
		mcp.Tool{
			Name:        "projects_write",
			Description: t("TOOL_PROJECTS_WRITE_DESCRIPTION", "Create and manage GitHub Projects: create projects, add/update/delete items, create status updates, and add iteration fields."),
			Annotations: &mcp.ToolAnnotations{
				Title:           t("TOOL_PROJECTS_WRITE_USER_TITLE", "Manage GitHub Projects"),
				ReadOnlyHint:    false,
				DestructiveHint: jsonschema.Ptr(true),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"method": {
						Type:        "string",
						Description: "The method to execute",
						Enum: []any{
							projectsMethodAddProjectItem,
							projectsMethodUpdateProjectItem,
							projectsMethodDeleteProjectItem,
							projectsMethodCreateProjectStatusUpdate,
							projectsMethodCreateProject,
							projectsMethodCreateIterationField,
							projectsMethodCreateProjectField,
							projectsMethodAddProjectFieldOption,
							projectsMethodDeleteProjectField,
						},
					},
					"owner_type": {
						Type:        "string",
						Description: "Owner type (user or org). Required for 'create_project' method. If not provided for other methods, will be automatically detected.",
						Enum:        []any{"user", "org"},
					},
					"owner": {
						Type:        "string",
						Description: "The project owner (user or organization login). The name is not case sensitive.",
					},
					"project_number": {
						Type:        "number",
						Description: "The project's number. Required for all methods except 'create_project'.",
					},
					"title": {
						Type:        "string",
						Description: "The project title. Required for 'create_project' method.",
					},
					"item_id": {
						Type:        "number",
						Description: "The project item ID. Required for 'update_project_item' and 'delete_project_item' methods.",
					},
					"item_type": {
						Type:        "string",
						Description: "The item's type, either issue or pull_request. Required for 'add_project_item' method.",
						Enum:        []any{"issue", "pull_request"},
					},
					"item_owner": {
						Type:        "string",
						Description: "The owner (user or organization) of the repository containing the issue or pull request. Required for 'add_project_item' method.",
					},
					"item_repo": {
						Type:        "string",
						Description: "The name of the repository containing the issue or pull request. Required for 'add_project_item' method.",
					},
					"issue_number": {
						Type:        "number",
						Description: "The issue number (use when item_type is 'issue' for 'add_project_item' method). Provide either issue_number or pull_request_number.",
					},
					"pull_request_number": {
						Type:        "number",
						Description: "The pull request number (use when item_type is 'pull_request' for 'add_project_item' method). Provide either issue_number or pull_request_number.",
					},
					"updated_field": {
						Type:        "object",
						Description: "Object consisting of the ID of the project field to update and the new value for the field. To clear the field, set value to null. Example: {\"id\": 123456, \"value\": \"New Value\"}. Required for 'update_project_item' method.",
					},
					"body": {
						Type:        "string",
						Description: "The body of the status update (markdown). Used for 'create_project_status_update' method.",
					},
					"status": {
						Type:        "string",
						Description: "The status of the project. Used for 'create_project_status_update' method.",
						Enum:        []any{"INACTIVE", "ON_TRACK", "AT_RISK", "OFF_TRACK", "COMPLETE"},
					},
					"start_date": {
						Type:        "string",
						Description: "Start date in YYYY-MM-DD format. Used for 'create_project_status_update' and 'create_iteration_field' methods.",
					},
					"target_date": {
						Type:        "string",
						Description: "The target date of the status update in YYYY-MM-DD format. Used for 'create_project_status_update' method.",
					},
					"field_name": {
						Type:        "string",
						Description: "The name of the iteration field (e.g. 'Sprint'). Required for 'create_iteration_field' method.",
					},
					"iteration_duration": {
						Type:        "number",
						Description: "Duration in days for iterations of the field (e.g. 7 for weekly, 14 for bi-weekly). Required for 'create_iteration_field' method.",
					},
					"iterations": {
						Type:        "array",
						Description: "Custom iterations for 'create_iteration_field' method. Only set this when you need iterations with varying durations, breaks between them, or specific titles. Otherwise omit it: GitHub auto-creates three iterations of 'iteration_duration' days starting on 'start_date', which is the right choice for most cases.",
						Items: &jsonschema.Schema{
							Type:                 "object",
							AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
							Properties: map[string]*jsonschema.Schema{
								"title": {
									Type:        "string",
									Description: "Iteration title (e.g. 'Sprint 1')",
								},
								"start_date": {
									Type:        "string",
									Description: "Start date in YYYY-MM-DD format",
								},
								"duration": {
									Type:        "number",
									Description: "Duration in days",
								},
							},
							Required: []string{"title", "start_date", "duration"},
						},
					},
					"field_id": {
						Type:        "number",
						Description: "The numeric ID of the project field. Required for 'add_project_field_option' and 'delete_project_field' methods.",
					},
					"name": {
						Type:        "string",
						Description: "The name of the new field. Required for 'create_project_field' method.",
					},
					"data_type": {
						Type:        "string",
						Description: "The data type of the new field. Required for 'create_project_field' method.",
						Enum:        []any{"TEXT", "NUMBER", "DATE", "SINGLE_SELECT", "ITERATION"},
					},
					"options": {
						Type:        "array",
						Description: "Initial option names for a SINGLE_SELECT field. Optional for 'create_project_field' method.",
						Items: &jsonschema.Schema{
							Type: "string",
						},
					},
					"option_name": {
						Type:        "string",
						Description: "The name of the new option to add. Required for 'add_project_field_option' method.",
					},
					"color": {
						Type:        "string",
						Description: "The color for the new single-select option. Optional for 'add_project_field_option' method (default: GRAY).",
						Enum:        []any{"BLUE", "GREEN", "RED", "YELLOW", "ORANGE", "PINK", "PURPLE", "GRAY"},
					},
				},
				Required: []string{"method", "owner"},
			},
		},
		[]scopes.Scope{scopes.Project},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			method, err := RequiredParam[string](args, "method")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			ownerType, err := OptionalParam[string](args, "owner_type")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			gqlClient, err := deps.GetGQLClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			// create_project does not require project_number or a REST client
			if method == projectsMethodCreateProject {
				return createProject(ctx, gqlClient, owner, ownerType, args)
			}

			projectNumber, err := RequiredInt(args, "project_number")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			// Detect owner type if not provided
			if ownerType == "" {
				ownerType, err = detectOwnerType(ctx, client, owner, projectNumber)
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
			}

			switch method {
			case projectsMethodAddProjectItem:
				itemType, err := RequiredParam[string](args, "item_type")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				itemOwner, err := RequiredParam[string](args, "item_owner")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				itemRepo, err := RequiredParam[string](args, "item_repo")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}

				var itemNumber int
				switch itemType {
				case "issue":
					itemNumber, err = RequiredInt(args, "issue_number")
					if err != nil {
						return utils.NewToolResultError("issue_number is required when item_type is 'issue'"), nil, nil
					}
				case "pull_request":
					itemNumber, err = RequiredInt(args, "pull_request_number")
					if err != nil {
						return utils.NewToolResultError("pull_request_number is required when item_type is 'pull_request'"), nil, nil
					}
				default:
					return utils.NewToolResultError("item_type must be either 'issue' or 'pull_request'"), nil, nil
				}

				return addProjectItem(ctx, gqlClient, owner, ownerType, projectNumber, itemOwner, itemRepo, itemNumber, itemType)
			case projectsMethodUpdateProjectItem:
				itemID, err := RequiredBigInt(args, "item_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				rawUpdatedField, exists := args["updated_field"]
				if !exists {
					return utils.NewToolResultError("missing required parameter: updated_field"), nil, nil
				}
				fieldValue, ok := rawUpdatedField.(map[string]any)
				if !ok || fieldValue == nil {
					return utils.NewToolResultError("updated_field must be an object"), nil, nil
				}
				return updateProjectItem(ctx, client, owner, ownerType, projectNumber, itemID, fieldValue)
			case projectsMethodDeleteProjectItem:
				itemID, err := RequiredBigInt(args, "item_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				return deleteProjectItem(ctx, client, owner, ownerType, projectNumber, itemID)
			case projectsMethodCreateProjectStatusUpdate:
				body, err := OptionalParam[string](args, "body")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				status, err := OptionalParam[string](args, "status")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				startDate, err := OptionalParam[string](args, "start_date")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				targetDate, err := OptionalParam[string](args, "target_date")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				return createProjectStatusUpdate(ctx, gqlClient, owner, ownerType, projectNumber, body, status, startDate, targetDate)
			case projectsMethodCreateIterationField:
				return createIterationField(ctx, gqlClient, owner, ownerType, projectNumber, args)
			case projectsMethodCreateProjectField:
				return createProjectField(ctx, gqlClient, client, owner, ownerType, projectNumber, args)
			case projectsMethodAddProjectFieldOption:
				fieldID, err := RequiredBigInt(args, "field_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				optionName, err := RequiredParam[string](args, "option_name")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				color, err := OptionalParam[string](args, "color")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				if color == "" {
					color = "GRAY"
				}
				return addProjectFieldOption(ctx, gqlClient, client, owner, ownerType, projectNumber, fieldID, optionName, color)
			case projectsMethodDeleteProjectField:
				fieldID, err := RequiredBigInt(args, "field_id")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				return deleteProjectField(ctx, gqlClient, client, owner, ownerType, projectNumber, fieldID)
			default:
				return utils.NewToolResultError(fmt.Sprintf("unknown method: %s", method)), nil, nil
			}
		},
	)
	return tool
}

// Helper functions for consolidated projects tools

func listProjects(ctx context.Context, client *github.Client, args map[string]any, owner, ownerType string) (*mcp.CallToolResult, []bool, any, error) {
	queryStr, err := OptionalParam[string](args, "query")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil, nil
	}

	pagination, err := extractPaginationOptionsFromArgs(args)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil, nil
	}

	var resp *github.Response
	var projects []*github.ProjectV2

	minimalProjects := []MinimalProject{}
	opts := &github.ListProjectsOptions{
		ListProjectsPaginationOptions: pagination,
		Query:                         queryStr,
	}

	// If owner_type not provided, fetch from both user and org
	switch ownerType {
	case "":
		return listProjectsFromBothOwnerTypes(ctx, client, owner, opts)
	case "org":
		projects, resp, err = client.Projects.ListOrganizationProjects(ctx, owner, opts)
		if err != nil {
			return ghErrors.NewGitHubAPIErrorResponse(ctx,
				"failed to list projects",
				resp,
				err,
			), nil, nil, nil
		}
	default:
		projects, resp, err = client.Projects.ListUserProjects(ctx, owner, opts)
		if err != nil {
			return ghErrors.NewGitHubAPIErrorResponse(ctx,
				"failed to list projects",
				resp,
				err,
			), nil, nil, nil
		}
	}

	// For specified owner_type, process normally
	if ownerType != "" {
		defer func() { _ = resp.Body.Close() }()

		for _, project := range projects {
			mp := convertToMinimalProject(project)
			mp.OwnerType = ownerType
			minimalProjects = append(minimalProjects, *mp)
		}

		response := map[string]any{
			"projects": minimalProjects,
			"pageInfo": buildPageInfo(resp),
		}

		r, err := json.Marshal(response)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to marshal response: %w", err)
		}

		return utils.NewToolResultText(string(r)), projectVisibilities(minimalProjects), nil, nil
	}

	return nil, nil, nil, fmt.Errorf("unexpected state in listProjects")
}

// listProjectsFromBothOwnerTypes fetches projects from both user and org endpoints
// when owner_type is not specified, combining the results with owner_type labels.
func listProjectsFromBothOwnerTypes(ctx context.Context, client *github.Client, owner string, opts *github.ListProjectsOptions) (*mcp.CallToolResult, []bool, any, error) {
	var minimalProjects []MinimalProject
	var resp *github.Response

	// Fetch user projects
	userProjects, userResp, userErr := client.Projects.ListUserProjects(ctx, owner, opts)
	if userErr == nil && userResp.StatusCode == http.StatusOK {
		for _, project := range userProjects {
			mp := convertToMinimalProject(project)
			mp.OwnerType = "user"
			minimalProjects = append(minimalProjects, *mp)
		}
		_ = userResp.Body.Close()
	}

	// Fetch org projects
	orgProjects, orgResp, orgErr := client.Projects.ListOrganizationProjects(ctx, owner, opts)
	if orgErr == nil && orgResp.StatusCode == http.StatusOK {
		for _, project := range orgProjects {
			mp := convertToMinimalProject(project)
			mp.OwnerType = "org"
			minimalProjects = append(minimalProjects, *mp)
		}
		resp = orgResp // Use org response for pagination info
	} else if userResp != nil {
		resp = userResp // Fallback to user response
	}

	// If both failed, return error
	if (userErr != nil || userResp == nil || userResp.StatusCode != http.StatusOK) &&
		(orgErr != nil || orgResp == nil || orgResp.StatusCode != http.StatusOK) {
		return utils.NewToolResultError(fmt.Sprintf("failed to list projects for owner '%s': not found as user or organization", owner)), nil, nil, nil
	}

	response := map[string]any{
		"projects": minimalProjects,
		"note":     "Results include both user and org projects. Each project includes 'owner_type' field. Pagination is limited when owner_type is not specified - specify 'owner_type' for full pagination support.",
	}
	if resp != nil {
		response["pageInfo"] = buildPageInfo(resp)
		defer func() { _ = resp.Body.Close() }()
	}

	r, err := json.Marshal(response)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}
	return utils.NewToolResultText(string(r)), projectVisibilities(minimalProjects), nil, nil
}

func projectVisibilities(projects []MinimalProject) []bool {
	visibilities := make([]bool, 0, len(projects))
	for _, project := range projects {
		isPrivate := true
		if project.Public != nil {
			isPrivate = !*project.Public
		}
		visibilities = append(visibilities, isPrivate)
	}
	return visibilities
}

func listProjectFields(ctx context.Context, client *github.Client, args map[string]any, owner, ownerType string) (*mcp.CallToolResult, any, error) {
	projectNumber, err := RequiredInt(args, "project_number")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	pagination, err := extractPaginationOptionsFromArgs(args)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	projectFields, resp, err := listProjectFieldsRaw(ctx, client, owner, ownerType, projectNumber, pagination)
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to list project fields",
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	response := map[string]any{
		"fields":   projectFields,
		"pageInfo": buildPageInfo(resp),
	}

	r, err := json.Marshal(response)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

func listProjectItems(ctx context.Context, client *github.Client, args map[string]any, owner, ownerType string) (*mcp.CallToolResult, any, error) {
	projectNumber, err := RequiredInt(args, "project_number")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	queryStr, err := OptionalParam[string](args, "query")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	fields, err := OptionalBigIntArrayParam(args, "fields")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	pagination, err := extractPaginationOptionsFromArgs(args)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	var resp *github.Response
	var projectItems []*github.ProjectV2Item

	opts := &github.ListProjectItemsOptions{
		Fields: fields,
		ListProjectsOptions: github.ListProjectsOptions{
			ListProjectsPaginationOptions: pagination,
			Query:                         queryStr,
		},
	}

	if ownerType == "org" {
		projectItems, resp, err = client.Projects.ListOrganizationProjectItems(ctx, owner, projectNumber, opts)
	} else {
		projectItems, resp, err = client.Projects.ListUserProjectItems(ctx, owner, projectNumber, opts)
	}

	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			ProjectListFailedError,
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	minimalItems := make([]MinimalProjectItem, 0, len(projectItems))
	for _, item := range projectItems {
		minimalItems = append(minimalItems, convertToMinimalProjectItem(item))
	}

	response := map[string]any{
		"items":    minimalItems,
		"pageInfo": buildPageInfo(resp),
	}

	r, err := json.Marshal(response)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

func fetchProjectV2(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int) (*github.ProjectV2, *github.Response, error) {
	if ownerType == "org" {
		return client.Projects.GetOrganizationProject(ctx, owner, projectNumber)
	}
	return client.Projects.GetUserProject(ctx, owner, projectNumber)
}

// FetchProjectIsPrivate returns whether a GitHub Project is private.
func FetchProjectIsPrivate(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int) (bool, error) {
	project, resp, err := fetchProjectV2(ctx, client, owner, ownerType, projectNumber)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return false, err
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("failed to fetch project visibility")
	}
	return !project.GetPublic(), nil
}

func getProject(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int) (*mcp.CallToolResult, bool, any, error) {
	project, resp, err := fetchProjectV2(ctx, client, owner, ownerType, projectNumber)
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to get project",
			resp,
			err,
		), false, nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, nil, fmt.Errorf("failed to read response body: %w", err)
		}
		return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get project", resp, body), false, nil, nil
	}

	minimalProject := convertToMinimalProject(project)
	r, err := json.Marshal(minimalProject)
	if err != nil {
		return nil, false, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), !project.GetPublic(), nil, nil
}

func getProjectField(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, fieldID int64) (*mcp.CallToolResult, any, error) {
	projectField, resp, err := getProjectFieldRaw(ctx, client, owner, ownerType, projectNumber, fieldID)
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to get project field",
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read response body: %w", err)
		}
		return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get project field", resp, body), nil, nil
	}
	r, err := json.Marshal(projectField)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

func getProjectItem(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, itemID int64, fields []int64) (*mcp.CallToolResult, any, error) {
	var resp *github.Response
	var projectItem *github.ProjectV2Item
	var opts *github.GetProjectItemOptions
	var err error

	if len(fields) > 0 {
		opts = &github.GetProjectItemOptions{
			Fields: fields,
		}
	}

	if ownerType == "org" {
		projectItem, resp, err = client.Projects.GetOrganizationProjectItem(ctx, owner, projectNumber, itemID, opts)
	} else {
		projectItem, resp, err = client.Projects.GetUserProjectItem(ctx, owner, projectNumber, itemID, opts)
	}

	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to get project item",
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read response body: %w", err)
		}
		return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get project item", resp, body), nil, nil
	}

	r, err := json.Marshal(convertToMinimalProjectItem(projectItem))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

func updateProjectItem(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, itemID int64, fieldValue map[string]any) (*mcp.CallToolResult, any, error) {
	updatePayload, err := buildUpdateProjectItem(fieldValue)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	var resp *github.Response
	var updatedItem *github.ProjectV2Item

	if ownerType == "org" {
		updatedItem, resp, err = client.Projects.UpdateOrganizationProjectItem(ctx, owner, projectNumber, itemID, updatePayload)
	} else {
		updatedItem, resp, err = client.Projects.UpdateUserProjectItem(ctx, owner, projectNumber, itemID, updatePayload)
	}

	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			ProjectUpdateFailedError,
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read response body: %w", err)
		}
		return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, ProjectUpdateFailedError, resp, body), nil, nil
	}
	r, err := json.Marshal(convertToMinimalProjectItem(updatedItem))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

func deleteProjectItem(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, itemID int64) (*mcp.CallToolResult, any, error) {
	var resp *github.Response
	var err error

	if ownerType == "org" {
		resp, err = client.Projects.DeleteOrganizationProjectItem(ctx, owner, projectNumber, itemID)
	} else {
		resp, err = client.Projects.DeleteUserProjectItem(ctx, owner, projectNumber, itemID)
	}

	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			ProjectDeleteFailedError,
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read response body: %w", err)
		}
		return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, ProjectDeleteFailedError, resp, body), nil, nil
	}
	return utils.NewToolResultText("project item successfully deleted"), nil, nil
}

// resolveProjectNodeID resolves (owner, ownerType, projectNumber) to a project node ID via GraphQL.
func resolveProjectNodeID(ctx context.Context, gqlClient *githubv4.Client, owner, ownerType string, projectNumber int) (githubv4.ID, error) {
	var projectIDQueryUser struct {
		User struct {
			ProjectV2 struct {
				ID githubv4.ID
			} `graphql:"projectV2(number: $projectNumber)"`
		} `graphql:"user(login: $owner)"`
	}
	var projectIDQueryOrg struct {
		Organization struct {
			ProjectV2 struct {
				ID githubv4.ID
			} `graphql:"projectV2(number: $projectNumber)"`
		} `graphql:"organization(login: $owner)"`
	}

	queryVars := map[string]any{
		"owner":         githubv4.String(owner),
		"projectNumber": githubv4.Int(int32(projectNumber)), //nolint:gosec // Project numbers are small integers
	}

	if ownerType == "org" {
		err := gqlClient.Query(ctx, &projectIDQueryOrg, queryVars)
		if err != nil {
			return "", fmt.Errorf("%s: %w", ProjectResolveIDFailedError, err)
		}
		return projectIDQueryOrg.Organization.ProjectV2.ID, nil
	}

	err := gqlClient.Query(ctx, &projectIDQueryUser, queryVars)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ProjectResolveIDFailedError, err)
	}
	return projectIDQueryUser.User.ProjectV2.ID, nil
}

// addProjectItem adds an item to a project by resolving the issue/PR number to a node ID
func addProjectItem(ctx context.Context, gqlClient *githubv4.Client, owner, ownerType string, projectNumber int, itemOwner, itemRepo string, itemNumber int, itemType string) (*mcp.CallToolResult, any, error) {
	if itemType != "issue" && itemType != "pull_request" {
		return utils.NewToolResultError("item_type must be either 'issue' or 'pull_request'"), nil, nil
	}

	// Resolve the item number to a node ID
	var nodeID githubv4.ID
	var err error
	if itemType == "issue" {
		nodeID, err = resolveIssueNodeID(ctx, gqlClient, itemOwner, itemRepo, itemNumber)
	} else {
		nodeID, err = resolvePullRequestNodeID(ctx, gqlClient, itemOwner, itemRepo, itemNumber)
	}
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to resolve %s: %v", itemType, err)), nil, nil
	}

	// Use GraphQL to add the item to the project
	var mutation struct {
		AddProjectV2ItemByID struct {
			Item struct {
				ID             githubv4.ID
				FullDatabaseID string `graphql:"fullDatabaseId"`
			}
		} `graphql:"addProjectV2ItemById(input: $input)"`
	}

	// Resolve the project number to a node ID
	projectID, err := resolveProjectNodeID(ctx, gqlClient, owner, ownerType, projectNumber)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	// Add the item to the project
	input := githubv4.AddProjectV2ItemByIdInput{
		ProjectID: projectID,
		ContentID: nodeID,
	}

	err = gqlClient.Mutate(ctx, &mutation, input, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf(ProjectAddFailedError+": %v", err)), nil, nil
	}

	result := map[string]any{
		"id":      mutation.AddProjectV2ItemByID.Item.ID,
		"message": fmt.Sprintf("Successfully added %s %s/%s#%d to project %s/%d", itemType, itemOwner, itemRepo, itemNumber, owner, projectNumber),
	}
	if fullDatabaseID := mutation.AddProjectV2ItemByID.Item.FullDatabaseID; fullDatabaseID != "" {
		result["full_database_id"] = fullDatabaseID
		if itemID, err := strconv.ParseInt(fullDatabaseID, 10, 64); err == nil {
			result["item_id"] = itemID
		}
	}

	r, err := json.Marshal(result)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

// validateDateFormat checks that a date string is in YYYY-MM-DD format.
func validateDateFormat(value, fieldName string) error {
	if _, err := time.Parse("2006-01-02", value); err != nil {
		return fmt.Errorf("invalid %s %q: must be YYYY-MM-DD format", fieldName, value)
	}
	return nil
}

// createProjectStatusUpdate creates a new status update for a project via GraphQL.
func createProjectStatusUpdate(ctx context.Context, gqlClient *githubv4.Client, owner, ownerType string, projectNumber int, body, status, startDate, targetDate string) (*mcp.CallToolResult, any, error) {
	// Validate inputs
	if ownerType != "user" && ownerType != "org" {
		return utils.NewToolResultError(fmt.Sprintf("invalid owner_type %q: must be \"user\" or \"org\"", ownerType)), nil, nil
	}
	if status != "" && !validProjectV2StatusUpdateStatuses[status] {
		return utils.NewToolResultError(fmt.Sprintf("invalid status %q: must be one of INACTIVE, ON_TRACK, AT_RISK, OFF_TRACK, COMPLETE", status)), nil, nil
	}
	if startDate != "" {
		if err := validateDateFormat(startDate, "start_date"); err != nil {
			return utils.NewToolResultError(err.Error()), nil, nil
		}
	}
	if targetDate != "" {
		if err := validateDateFormat(targetDate, "target_date"); err != nil {
			return utils.NewToolResultError(err.Error()), nil, nil
		}
	}

	// Resolve project number to project node ID
	projectID, err := resolveProjectNodeID(ctx, gqlClient, owner, ownerType, projectNumber)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	// Build mutation input
	input := CreateProjectV2StatusUpdateInput{
		ProjectID: projectID,
	}

	if body != "" {
		s := githubv4.String(body)
		input.Body = &s
	}
	if status != "" {
		s := githubv4.String(status)
		input.Status = &s
	}
	if startDate != "" {
		s := githubv4.String(startDate)
		input.StartDate = &s
	}
	if targetDate != "" {
		s := githubv4.String(targetDate)
		input.TargetDate = &s
	}

	// Execute mutation
	var mutation struct {
		CreateProjectV2StatusUpdate struct {
			StatusUpdate statusUpdateNode
		} `graphql:"createProjectV2StatusUpdate(input: $input)"`
	}

	err = gqlClient.Mutate(ctx, &mutation, input, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("%s: %v", ProjectStatusUpdateCreateFailedError, err)), nil, nil
	}

	// Convert and return
	result := convertToMinimalStatusUpdate(mutation.CreateProjectV2StatusUpdate.StatusUpdate)

	r, err := json.Marshal(result)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return utils.NewToolResultText(string(r)), nil, nil
}

// listProjectStatusUpdates lists status updates for a project via GraphQL.
func listProjectStatusUpdates(ctx context.Context, gqlClient *githubv4.Client, args map[string]any, owner, ownerType string) (*mcp.CallToolResult, bool, any, error) {
	if ownerType != "user" && ownerType != "org" {
		return utils.NewToolResultError(fmt.Sprintf("invalid owner_type %q: must be \"user\" or \"org\"", ownerType)), false, nil, nil
	}

	projectNumber, err := RequiredInt(args, "project_number")
	if err != nil {
		return utils.NewToolResultError(err.Error()), false, nil, nil
	}

	perPage, err := OptionalIntParamWithDefault(args, "per_page", MaxProjectsPerPage)
	if err != nil {
		return utils.NewToolResultError(err.Error()), false, nil, nil
	}
	if perPage > MaxProjectsPerPage {
		perPage = MaxProjectsPerPage
	}
	if perPage < 1 {
		perPage = MaxProjectsPerPage
	}

	afterCursor, err := OptionalParam[string](args, "after")
	if err != nil {
		return utils.NewToolResultError(err.Error()), false, nil, nil
	}

	vars := map[string]any{
		"owner":         githubv4.String(owner),
		"projectNumber": githubv4.Int(int32(projectNumber)), //nolint:gosec // Project numbers are small integers
		"first":         githubv4.Int(int32(perPage)),       //nolint:gosec // perPage is bounded by MaxProjectsPerPage
	}
	if afterCursor != "" {
		vars["after"] = githubv4.String(afterCursor)
	} else {
		vars["after"] = (*githubv4.String)(nil)
	}

	var nodes []statusUpdateNode
	var pi PageInfoFragment
	var isPrivate bool

	if ownerType == "org" {
		var q statusUpdatesOrgQuery
		if err := gqlClient.Query(ctx, &q, vars); err != nil {
			return utils.NewToolResultError(fmt.Sprintf("%s: %v", ProjectStatusUpdateListFailedError, err)), false, nil, nil
		}
		project := q.Organization.ProjectV2
		nodes = project.StatusUpdates.Nodes
		pi = project.StatusUpdates.PageInfo
		isPrivate = !bool(project.Public)
	} else {
		var q statusUpdatesUserQuery
		if err := gqlClient.Query(ctx, &q, vars); err != nil {
			return utils.NewToolResultError(fmt.Sprintf("%s: %v", ProjectStatusUpdateListFailedError, err)), false, nil, nil
		}
		project := q.User.ProjectV2
		nodes = project.StatusUpdates.Nodes
		pi = project.StatusUpdates.PageInfo
		isPrivate = !bool(project.Public)
	}

	updates := make([]MinimalProjectStatusUpdate, 0, len(nodes))
	for _, n := range nodes {
		updates = append(updates, convertToMinimalStatusUpdate(n))
	}

	response := map[string]any{
		"statusUpdates": updates,
		"pageInfo": map[string]any{
			"hasNextPage":     pi.HasNextPage,
			"hasPreviousPage": pi.HasPreviousPage,
			"nextCursor":      string(pi.EndCursor),
			"prevCursor":      string(pi.StartCursor),
		},
	}

	r, err := json.Marshal(response)
	if err != nil {
		return nil, false, nil, fmt.Errorf("failed to marshal response: %w", err)
	}
	return utils.NewToolResultText(string(r)), isPrivate, nil, nil
}

// getProjectStatusUpdate fetches a single status update by its node ID via GraphQL.
func getProjectStatusUpdate(ctx context.Context, gqlClient *githubv4.Client, statusUpdateID string) (*mcp.CallToolResult, bool, any, error) {
	var q statusUpdateNodeQuery
	vars := map[string]any{
		"id": githubv4.ID(statusUpdateID),
	}

	if err := gqlClient.Query(ctx, &q, vars); err != nil {
		return utils.NewToolResultError(fmt.Sprintf("%s: %v", ProjectStatusUpdateGetFailedError, err)), false, nil, nil
	}

	if q.Node.StatusUpdate.ID == nil || q.Node.StatusUpdate.ID == "" {
		return utils.NewToolResultError(fmt.Sprintf("%s: node is not a ProjectV2StatusUpdate or was not found", ProjectStatusUpdateGetFailedError)), false, nil, nil
	}

	update := convertToMinimalStatusUpdate(q.Node.StatusUpdate.statusUpdateNode)
	isPrivate := !bool(q.Node.StatusUpdate.Project.Public)

	r, err := json.Marshal(update)
	if err != nil {
		return nil, false, nil, fmt.Errorf("failed to marshal response: %w", err)
	}
	return utils.NewToolResultText(string(r)), isPrivate, nil, nil
}

// validateAndConvertToInt64 ensures the value is a number and converts it to int64.
func validateAndConvertToInt64(value any) (int64, error) {
	switch v := value.(type) {
	case float64:
		// Validate that the float64 can be safely converted to int64
		intVal := int64(v)
		if float64(intVal) != v {
			return 0, fmt.Errorf("value must be a valid integer (got %v)", v)
		}
		return intVal, nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("value must be a number (got %T)", v)
	}
}

// buildUpdateProjectItem constructs UpdateProjectItemOptions from the input map.
func buildUpdateProjectItem(input map[string]any) (*github.UpdateProjectItemOptions, error) {
	if input == nil {
		return nil, fmt.Errorf("updated_field must be an object")
	}

	idField, ok := input["id"]
	if !ok {
		return nil, fmt.Errorf("updated_field.id is required")
	}

	fieldID, err := validateAndConvertToInt64(idField)
	if err != nil {
		return nil, fmt.Errorf("updated_field.id: %w", err)
	}

	valueField, ok := input["value"]
	if !ok {
		return nil, fmt.Errorf("updated_field.value is required")
	}

	payload := &github.UpdateProjectItemOptions{
		Fields: []*github.UpdateProjectV2Field{{
			ID:    fieldID,
			Value: valueField,
		}},
	}

	return payload, nil
}

func extractPaginationOptionsFromArgs(args map[string]any) (github.ListProjectsPaginationOptions, error) {
	perPage, err := OptionalIntParamWithDefault(args, "per_page", MaxProjectsPerPage)
	if err != nil {
		return github.ListProjectsPaginationOptions{}, err
	}
	if perPage > MaxProjectsPerPage {
		perPage = MaxProjectsPerPage
	}

	after, err := OptionalParam[string](args, "after")
	if err != nil {
		return github.ListProjectsPaginationOptions{}, err
	}

	before, err := OptionalParam[string](args, "before")
	if err != nil {
		return github.ListProjectsPaginationOptions{}, err
	}

	opts := github.ListProjectsPaginationOptions{
		PerPage: perPage,
		After:   after,
		Before:  before,
	}

	return opts, nil
}

// resolveIssueNodeID resolves an issue number to its GraphQL node ID
func resolveIssueNodeID(ctx context.Context, gqlClient *githubv4.Client, owner, repo string, issueNumber int) (githubv4.ID, error) {
	var query struct {
		Repository struct {
			Issue struct {
				ID githubv4.ID
			} `graphql:"issue(number: $issueNumber)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	variables := map[string]any{
		"owner":       githubv4.String(owner),
		"repo":        githubv4.String(repo),
		"issueNumber": githubv4.Int(int32(issueNumber)), //nolint:gosec // Issue numbers are small integers
	}

	err := gqlClient.Query(ctx, &query, variables)
	if err != nil {
		return "", fmt.Errorf("failed to resolve issue %s/%s#%d: %w", owner, repo, issueNumber, err)
	}

	return query.Repository.Issue.ID, nil
}

// resolvePullRequestNodeID resolves a pull request number to its GraphQL node ID
func resolvePullRequestNodeID(ctx context.Context, gqlClient *githubv4.Client, owner, repo string, prNumber int) (githubv4.ID, error) {
	var query struct {
		Repository struct {
			PullRequest struct {
				ID githubv4.ID
			} `graphql:"pullRequest(number: $prNumber)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	variables := map[string]any{
		"owner":    githubv4.String(owner),
		"repo":     githubv4.String(repo),
		"prNumber": githubv4.Int(int32(prNumber)), //nolint:gosec // PR numbers are small integers
	}

	err := gqlClient.Query(ctx, &query, variables)
	if err != nil {
		return "", fmt.Errorf("failed to resolve pull request %s/%s#%d: %w", owner, repo, prNumber, err)
	}

	return query.Repository.PullRequest.ID, nil
}

// createProject handles the create_project method for ProjectsWrite.
func createProject(ctx context.Context, gqlClient *githubv4.Client, owner, ownerType string, args map[string]any) (*mcp.CallToolResult, any, error) {
	if ownerType == "" {
		return utils.NewToolResultError("owner_type is required for create_project"), nil, nil
	}
	if ownerType != "user" && ownerType != "org" {
		return utils.NewToolResultError(fmt.Sprintf("invalid owner_type %q: must be \"user\" or \"org\"", ownerType)), nil, nil
	}

	title, err := RequiredParam[string](args, "title")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	ownerID, err := getOwnerNodeID(ctx, gqlClient, owner, ownerType)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to get owner ID: %v", err)), nil, nil
	}

	var mutation struct {
		CreateProjectV2 struct {
			ProjectV2 struct {
				ID     string
				Number int
				Title  string
				URL    string
			}
		} `graphql:"createProjectV2(input: $input)"`
	}

	input := githubv4.CreateProjectV2Input{
		OwnerID: githubv4.ID(ownerID),
		Title:   githubv4.String(title),
	}

	err = gqlClient.Mutate(ctx, &mutation, input, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to create project: %v", err)), nil, nil
	}

	result := struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
	}{
		ID:     mutation.CreateProjectV2.ProjectV2.ID,
		Number: mutation.CreateProjectV2.ProjectV2.Number,
		Title:  mutation.CreateProjectV2.ProjectV2.Title,
		URL:    mutation.CreateProjectV2.ProjectV2.URL,
	}

	return MarshalledTextResult(result), nil, nil
}

// createIterationField handles the create_iteration_field method for ProjectsWrite.
//
// GitHub's GraphQL API requires two mutations to fully configure an iteration field:
//  1. createProjectV2Field creates the field with DataType=ITERATION (no schedule yet).
//  2. updateProjectV2Field sets the start date, duration, and optional named iterations.
//
// If step 2 fails, the field already exists with default settings and can be reconfigured
// by calling this method again (the create will fail with a duplicate-name error, which
// surfaces clearly) or by deleting the field via the GitHub UI.
func createIterationField(ctx context.Context, gqlClient *githubv4.Client, owner, ownerType string, projectNumber int, args map[string]any) (*mcp.CallToolResult, any, error) {
	fieldName, err := RequiredParam[string](args, "field_name")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}
	duration, err := RequiredInt(args, "iteration_duration")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}
	startDateStr, err := RequiredParam[string](args, "start_date")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	projectID, err := resolveProjectNodeID(ctx, gqlClient, owner, ownerType, projectNumber)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to get project ID: %v", err)), nil, nil
	}

	// Step 1: Create the iteration field.
	var createMutation struct {
		CreateProjectV2Field struct {
			ProjectV2Field struct {
				ProjectV2IterationField struct {
					ID   string
					Name string
				} `graphql:"... on ProjectV2IterationField"`
			} `graphql:"projectV2Field"`
		} `graphql:"createProjectV2Field(input: $input)"`
	}

	createInput := githubv4.CreateProjectV2FieldInput{
		ProjectID: githubv4.ID(projectID),
		DataType:  githubv4.ProjectV2CustomFieldType("ITERATION"),
		Name:      githubv4.String(fieldName),
	}

	err = gqlClient.Mutate(ctx, &createMutation, createInput, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to create iteration field: %v", err)), nil, nil
	}

	fieldID := createMutation.CreateProjectV2Field.ProjectV2Field.ProjectV2IterationField.ID

	// Step 2: Configure the iteration field with start date and duration.
	var updateMutation struct {
		UpdateProjectV2Field struct {
			ProjectV2Field struct {
				ProjectV2IterationField struct {
					ID            string
					Name          string
					Configuration struct {
						Iterations []struct {
							ID        string
							Title     string
							StartDate string
							Duration  int
						}
					}
				} `graphql:"... on ProjectV2IterationField"`
			} `graphql:"projectV2Field"`
		} `graphql:"updateProjectV2Field(input: $input)"`
	}

	parsedStartDate, err := time.Parse("2006-01-02", startDateStr)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to parse start_date %s: %v", startDateStr, err)), nil, nil
	}

	// GitHub's ProjectV2IterationFieldConfigurationInput requires `iterations` as a
	// non-null array, so we always send at least an empty slice. When omitted, GitHub
	// generates a default set of iterations from start_date and duration.
	iterationsInput := []ProjectV2IterationFieldIterationInput{}

	if rawIterations, ok := args["iterations"].([]any); ok && len(rawIterations) > 0 {
		for i, item := range rawIterations {
			iterMap, ok := item.(map[string]any)
			if !ok {
				return utils.NewToolResultError(fmt.Sprintf("iterations[%d] must be an object", i)), nil, nil
			}
			iterTitle, ok := iterMap["title"].(string)
			if !ok || iterTitle == "" {
				return utils.NewToolResultError(fmt.Sprintf("iterations[%d]: title is required and must be a non-empty string", i)), nil, nil
			}
			iterStartDate, ok := iterMap["start_date"].(string)
			if !ok || iterStartDate == "" {
				return utils.NewToolResultError(fmt.Sprintf("iterations[%d]: start_date is required and must be a non-empty string", i)), nil, nil
			}
			iterDuration, ok := iterMap["duration"].(float64)
			if !ok || iterDuration <= 0 {
				return utils.NewToolResultError(fmt.Sprintf("iterations[%d]: duration is required and must be a positive number", i)), nil, nil
			}

			parsedIterStartDate, err := time.Parse("2006-01-02", iterStartDate)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("iterations[%d]: failed to parse start_date %q: %v", i, iterStartDate, err)), nil, nil
			}

			iterationsInput = append(iterationsInput, ProjectV2IterationFieldIterationInput{
				Title:     githubv4.String(iterTitle),
				StartDate: githubv4.Date{Time: parsedIterStartDate},
				Duration:  githubv4.Int(int32(iterDuration)), //nolint:gosec // Iteration durations are small day counts
			})
		}
	}

	configInput := ProjectV2IterationFieldConfigurationInput{
		Duration:   githubv4.Int(int32(duration)), //nolint:gosec // Iteration durations are small day counts
		StartDate:  githubv4.Date{Time: parsedStartDate},
		Iterations: iterationsInput,
	}

	updateInput := UpdateProjectV2FieldInput{
		FieldID:                githubv4.ID(fieldID),
		IterationConfiguration: &configInput,
	}

	err = gqlClient.Mutate(ctx, &updateMutation, updateInput, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to update iteration configuration: %v", err)), nil, nil
	}

	field := updateMutation.UpdateProjectV2Field.ProjectV2Field.ProjectV2IterationField
	iterResults := make([]map[string]any, 0, len(field.Configuration.Iterations))
	for _, iter := range field.Configuration.Iterations {
		iterResults = append(iterResults, map[string]any{
			"id":         iter.ID,
			"title":      iter.Title,
			"start_date": iter.StartDate,
			"duration":   iter.Duration,
		})
	}

	result := map[string]any{
		"id":   field.ID,
		"name": field.Name,
		"configuration": map[string]any{
			"iterations": iterResults,
		},
	}

	return MarshalledTextResult(result), nil, nil
}

// getOwnerNodeID resolves a GitHub user or organization login to its GraphQL node ID.
func getOwnerNodeID(ctx context.Context, gqlClient *githubv4.Client, owner, ownerType string) (string, error) {
	if ownerType == "org" {
		var query struct {
			Organization struct {
				ID string
			} `graphql:"organization(login: $login)"`
		}
		variables := map[string]any{
			"login": githubv4.String(owner),
		}
		err := gqlClient.Query(ctx, &query, variables)
		return query.Organization.ID, err
	}

	var query struct {
		User struct {
			ID string
		} `graphql:"user(login: $login)"`
	}
	variables := map[string]any{
		"login": githubv4.String(owner),
	}
	err := gqlClient.Query(ctx, &query, variables)
	return query.User.ID, err
}

// UpdateProjectV2FieldInput is the GraphQL input for the updateProjectV2Field mutation.
// These types are defined locally because the pinned shurcooL/githubv4 release
// (v0.0.0-20240727222349) does not yet expose them. Upstream master now generates
// equivalent types, so this block can be removed when the dependency is next bumped.
type UpdateProjectV2FieldInput struct {
	FieldID                githubv4.ID                                `json:"fieldId"`
	IterationConfiguration *ProjectV2IterationFieldConfigurationInput `json:"iterationConfiguration,omitempty"`
}

// ProjectV2IterationFieldConfigurationInput is the GraphQL input for configuring an iteration field.
// GitHub's schema marks iterations as a required non-null list, so the field is not omitempty.
type ProjectV2IterationFieldConfigurationInput struct {
	Duration   githubv4.Int                            `json:"duration"`
	StartDate  githubv4.Date                           `json:"startDate"`
	Iterations []ProjectV2IterationFieldIterationInput `json:"iterations"`
}

// ProjectV2IterationFieldIterationInput is the GraphQL input for a single iteration definition.
type ProjectV2IterationFieldIterationInput struct {
	StartDate githubv4.Date   `json:"startDate"`
	Duration  githubv4.Int    `json:"duration"`
	Title     githubv4.String `json:"title"`
}

// detectOwnerType attempts to detect whether the project owner is a user or org.
// It first asks GitHub for the account type, then falls back to project probes
// for older or mocked clients where the account type is unavailable.
func detectOwnerType(ctx context.Context, client *github.Client, owner string, projectNumber int) (string, error) {
	user, resp, err := client.Users.Get(ctx, owner)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
		switch user.GetType() {
		case "User":
			return "user", nil
		case "Organization":
			return "org", nil
		}
	}

	// Try user first (more common for personal projects)
	_, resp, err = client.Projects.GetUserProject(ctx, owner, projectNumber)
	if err == nil && resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		return "user", nil
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	// If not found (404) or other error, try org
	_, resp, err = client.Projects.GetOrganizationProject(ctx, owner, projectNumber)
	if err == nil && resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		return "org", nil
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	return "", fmt.Errorf("could not determine owner type for %s with project %d: owner is neither a user nor an org with this project", owner, projectNumber)
}

// validProjectFieldDataTypes is the set of valid data types for createProjectV2Field.
var validProjectFieldDataTypes = map[string]bool{
	"TEXT":          true,
	"NUMBER":        true,
	"DATE":          true,
	"SINGLE_SELECT": true,
	"ITERATION":     true,
}

// validSingleSelectColors is the set of valid colors for single-select options.
var validSingleSelectColors = map[string]bool{
	"BLUE":   true,
	"GREEN":  true,
	"RED":    true,
	"YELLOW": true,
	"ORANGE": true,
	"PINK":   true,
	"PURPLE": true,
	"GRAY":   true,
}

// createProjectField creates a new custom field on a project via GraphQL.
// For SINGLE_SELECT fields with initial options, it creates the field and then
// calls addProjectFieldOption for each option.
func createProjectField(ctx context.Context, gqlClient *githubv4.Client, client *github.Client, owner, ownerType string, projectNumber int, args map[string]any) (*mcp.CallToolResult, any, error) {
	fieldName, err := RequiredParam[string](args, "name")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}
	dataType, err := RequiredParam[string](args, "data_type")
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}
	if !validProjectFieldDataTypes[dataType] {
		return utils.NewToolResultError(fmt.Sprintf("invalid data_type %q: must be one of TEXT, NUMBER, DATE, SINGLE_SELECT, ITERATION", dataType)), nil, nil
	}

	projectID, err := resolveProjectNodeID(ctx, gqlClient, owner, ownerType, projectNumber)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to get project ID: %v", err)), nil, nil
	}

	var createMutation struct {
		CreateProjectV2Field struct {
			ProjectV2Field struct {
				ProjectV2Field struct {
					ID       string
					Name     string
					DataType string
				} `graphql:"... on ProjectV2Field"`
				ProjectV2SingleSelectField struct {
					ID      string
					Name    string
					Options []struct {
						ID   string
						Name string
					}
				} `graphql:"... on ProjectV2SingleSelectField"`
			} `graphql:"projectV2Field"`
		} `graphql:"createProjectV2Field(input: $input)"`
	}

	createInput := githubv4.CreateProjectV2FieldInput{
		ProjectID: githubv4.ID(projectID),
		DataType:  githubv4.ProjectV2CustomFieldType(dataType),
		Name:      githubv4.String(fieldName),
	}

	err = gqlClient.Mutate(ctx, &createMutation, createInput, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to create project field: %v", err)), nil, nil
	}

	// Determine the created field ID for subsequent option additions.
	createdFieldNodeID := createMutation.CreateProjectV2Field.ProjectV2Field.ProjectV2Field.ID
	if createdFieldNodeID == "" {
		createdFieldNodeID = createMutation.CreateProjectV2Field.ProjectV2Field.ProjectV2SingleSelectField.ID
	}

	// For SINGLE_SELECT fields, add any initial options.
	if dataType == "SINGLE_SELECT" && createdFieldNodeID != "" {
		var optionNames []string
		if raw, ok := args["options"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					optionNames = append(optionNames, s)
				}
			}
		}
		for _, optName := range optionNames {
			_, _, err := addProjectFieldOptionByNodeID(ctx, gqlClient, createdFieldNodeID, optName, "GRAY")
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("field created but failed to add option %q: %v", optName, err)), nil, nil
			}
		}
	}

	// Re-fetch the field to return up-to-date data including any new options.
	// Use the REST API for consistent output format.
	fields, _, fetchErr := listProjectFieldsRaw(ctx, client, owner, ownerType, projectNumber, github.ListProjectsPaginationOptions{PerPage: MaxProjectsPerPage})
	if fetchErr == nil {
		for _, f := range fields {
			if f.Name == fieldName {
				r, err := json.Marshal(f)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
				}
				return utils.NewToolResultText(string(r)), nil, nil
			}
		}
	}

	// Fallback: return what we got from the mutation.
	result := map[string]any{
		"id":        createdFieldNodeID,
		"name":      fieldName,
		"data_type": dataType,
	}
	if ssf := createMutation.CreateProjectV2Field.ProjectV2Field.ProjectV2SingleSelectField; ssf.ID != "" {
		opts := make([]map[string]any, 0, len(ssf.Options))
		for _, o := range ssf.Options {
			opts = append(opts, map[string]any{"id": o.ID, "name": o.Name})
		}
		result["options"] = opts
	}
	return MarshalledTextResult(result), nil, nil
}

// addProjectFieldOptionByNodeID adds a new option to a single-select field given its GraphQL node ID.
func addProjectFieldOptionByNodeID(ctx context.Context, gqlClient *githubv4.Client, fieldNodeID, optionName, color string) (string, []map[string]any, error) {
	var mutation struct {
		AddProjectV2SingleSelectFieldOption struct {
			ProjectV2SingleSelectField struct {
				ID      string
				Name    string
				Options []struct {
					ID    string
					Name  string
					Color string
				}
			}
		} `graphql:"addProjectV2SingleSelectFieldOption(input: $input)"`
	}

	input := AddProjectV2SingleSelectFieldOptionInput{
		FieldID: githubv4.ID(fieldNodeID),
		Name:    githubv4.String(optionName),
		Color:   githubv4.String(color),
	}

	err := gqlClient.Mutate(ctx, &mutation, input, nil)
	if err != nil {
		return "", nil, err
	}

	opts := make([]map[string]any, 0)
	for _, o := range mutation.AddProjectV2SingleSelectFieldOption.ProjectV2SingleSelectField.Options {
		opts = append(opts, map[string]any{"id": o.ID, "name": o.Name, "color": o.Color})
	}
	return mutation.AddProjectV2SingleSelectFieldOption.ProjectV2SingleSelectField.ID, opts, nil
}

// addProjectFieldOption resolves a field's node_id from its numeric id and adds a new option.
func addProjectFieldOption(ctx context.Context, gqlClient *githubv4.Client, client *github.Client, owner, ownerType string, projectNumber int, fieldID int64, optionName, color string) (*mcp.CallToolResult, any, error) {
	if !validSingleSelectColors[color] {
		return utils.NewToolResultError(fmt.Sprintf("invalid color %q: must be one of BLUE, GREEN, RED, YELLOW, ORANGE, PINK, PURPLE, GRAY", color)), nil, nil
	}

	// Fetch fields to find the node_id for the given numeric field_id.
	fields, resp, err := listProjectFieldsRaw(ctx, client, owner, ownerType, projectNumber, github.ListProjectsPaginationOptions{PerPage: MaxProjectsPerPage})
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to list project fields: %v", err)), nil, nil
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	var fieldNodeID string
	for _, f := range fields {
		if f.ID == fieldID {
			fieldNodeID = f.NodeID
			break
		}
	}
	if fieldNodeID == "" {
		return utils.NewToolResultError(fmt.Sprintf("field with id %d not found in project", fieldID)), nil, nil
	}

	_, opts, err := addProjectFieldOptionByNodeID(ctx, gqlClient, fieldNodeID, optionName, color)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to add project field option: %v", err)), nil, nil
	}

	result := map[string]any{
		"field_id": fieldID,
		"options":  opts,
	}
	return MarshalledTextResult(result), nil, nil
}

// deleteProjectField deletes a project field by its numeric id.
func deleteProjectField(ctx context.Context, gqlClient *githubv4.Client, client *github.Client, owner, ownerType string, projectNumber int, fieldID int64) (*mcp.CallToolResult, any, error) {
	// Fetch fields to find the node_id for the given numeric field_id.
	fields, resp, err := listProjectFieldsRaw(ctx, client, owner, ownerType, projectNumber, github.ListProjectsPaginationOptions{PerPage: MaxProjectsPerPage})
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to list project fields: %v", err)), nil, nil
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	var fieldNodeID string
	for _, f := range fields {
		if f.ID == fieldID {
			fieldNodeID = f.NodeID
			break
		}
	}
	if fieldNodeID == "" {
		return utils.NewToolResultError(fmt.Sprintf("field with id %d not found in project", fieldID)), nil, nil
	}

	var mutation struct {
		DeleteProjectV2Field struct {
			ClientMutationID *string
		} `graphql:"deleteProjectV2Field(input: $input)"`
	}

	input := DeleteProjectV2FieldInput{
		FieldID: githubv4.ID(fieldNodeID),
	}

	err = gqlClient.Mutate(ctx, &mutation, input, nil)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to delete project field: %v", err)), nil, nil
	}

	return utils.NewToolResultText("project field successfully deleted"), nil, nil
}

// AddProjectV2SingleSelectFieldOptionInput is the GraphQL input for the
// addProjectV2SingleSelectFieldOption mutation.
type AddProjectV2SingleSelectFieldOptionInput struct {
	FieldID githubv4.ID     `json:"fieldId"`
	Name    githubv4.String `json:"name"`
	Color   githubv4.String `json:"color"`
}

// DeleteProjectV2FieldInput is the GraphQL input for the deleteProjectV2Field mutation.
type DeleteProjectV2FieldInput struct {
	FieldID githubv4.ID `json:"fieldId"`
}


// setProjectItemStatus sets a single-select field on a project item by human-readable name.
// It fetches all project fields, finds the field and option by case-insensitive name, then
// calls the REST update endpoint with the resolved IDs.
func setProjectItemStatus(ctx context.Context, client *github.Client, owner, ownerType string, projectNumber int, itemID int64, fieldName, optionName string) (*mcp.CallToolResult, any, error) {
	// Fetch all fields to find the target field.
	fields, resp, err := listProjectFieldsRaw(ctx, client, owner, ownerType, projectNumber, github.ListProjectsPaginationOptions{PerPage: MaxProjectsPerPage})
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to list project fields", resp, err), nil, nil
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	// Find the field by case-insensitive name.
	var targetField *ProjectField
	for i := range fields {
		if strings.EqualFold(fields[i].Name, fieldName) {
			targetField = &fields[i]
			break
		}
	}
	if targetField == nil {
		available := make([]string, 0, len(fields))
		for _, f := range fields {
			available = append(available, f.Name)
		}
		return utils.NewToolResultError(fmt.Sprintf("field %q not found; available fields: %v", fieldName, available)), nil, nil
	}

	// Find the option by case-insensitive name.
	var optionID string
	for _, opt := range targetField.Options {
		if strings.EqualFold(string(opt.Name), optionName) {
			optionID = opt.ID
			break
		}
	}
	if optionID == "" {
		available := make([]string, 0, len(targetField.Options))
		for _, opt := range targetField.Options {
			available = append(available, string(opt.Name))
		}
		return utils.NewToolResultError(fmt.Sprintf("option %q not found in field %q; available options: %v", optionName, fieldName, available)), nil, nil
	}

	// Update the item using the existing update endpoint.
	fieldValue := map[string]any{
		"id":    float64(targetField.ID),
		"value": map[string]any{"single_select_option_id": optionID},
	}
	return updateProjectItem(ctx, client, owner, ownerType, projectNumber, itemID, fieldValue)
}

// SetProjectItemStatus returns the tool and handler for setting a single-select field
// (default: "Status") on a project item by human-readable option name.
func SetProjectItemStatus(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataProjects,
		mcp.Tool{
			Name:        "set_project_item_status",
			Description: t("TOOL_SET_PROJECT_ITEM_STATUS_DESCRIPTION", "Set a single-select field (e.g. Status) on a GitHub Project item by human-readable option name, without requiring field or option IDs."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_SET_PROJECT_ITEM_STATUS_TITLE", "Set project item status"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner_type": {
						Type:        "string",
						Description: "Owner type (user or org). If not provided, will be automatically detected.",
						Enum:        []any{"user", "org"},
					},
					"owner": {
						Type:        "string",
						Description: "The project owner (user or organization login).",
					},
					"project_number": {
						Type:        "number",
						Description: "The project's number.",
					},
					"item_id": {
						Type:        "number",
						Description: "The numeric ID of the project item to update.",
					},
					"field_name": {
						Type:        "string",
						Description: "The name of the single-select field to update (default: \"Status\").",
					},
					"option_name": {
						Type:        "string",
						Description: "The human-readable name of the option to set (case-insensitive).",
					},
				},
				Required: []string{"owner", "project_number", "item_id", "option_name"},
			},
		},
		[]scopes.Scope{scopes.Project},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			ownerType, err := OptionalParam[string](args, "owner_type")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			projectNumber, err := RequiredInt(args, "project_number")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			itemID, err := RequiredBigInt(args, "item_id")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			fieldName, err := OptionalParam[string](args, "field_name")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if fieldName == "" {
				fieldName = "Status"
			}

			optionName, err := RequiredParam[string](args, "option_name")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			if ownerType == "" {
				ownerType, err = detectOwnerType(ctx, client, owner, projectNumber)
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
			}

			return setProjectItemStatus(ctx, client, owner, ownerType, projectNumber, itemID, fieldName, optionName)
		},
	)
}

// addIssueToProject adds an issue to a GitHub Project via GraphQL and optionally sets its status.
func addIssueToProject(ctx context.Context, client *github.Client, gqlClient *githubv4.Client, issueOwner, issueRepo string, issueNumber int, projectOwner, ownerType string, projectNumber int, status string) (*mcp.CallToolResult, any, error) {
	// Step 1: Resolve the issue to a GraphQL node ID.
	issueNodeID, err := resolveIssueNodeID(ctx, gqlClient, issueOwner, issueRepo, issueNumber)
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("failed to resolve issue: %v", err)), nil, nil
	}

	// Step 2: Resolve the project to a GraphQL node ID.
	projectNodeID, err := resolveProjectNodeID(ctx, gqlClient, projectOwner, ownerType, projectNumber)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	// Step 3: Add the issue to the project via GraphQL mutation.
	var mutation struct {
		AddProjectV2ItemByID struct {
			Item struct {
				ID             githubv4.ID
				FullDatabaseID string `graphql:"fullDatabaseId"`
			}
		} `graphql:"addProjectV2ItemById(input: $input)"`
	}

	input := githubv4.AddProjectV2ItemByIdInput{
		ProjectID: projectNodeID,
		ContentID: issueNodeID,
	}

	if err := gqlClient.Mutate(ctx, &mutation, input, nil); err != nil {
		return utils.NewToolResultError(fmt.Sprintf(ProjectAddFailedError+": %v", err)), nil, nil
	}

	addedItemNodeID := mutation.AddProjectV2ItemByID.Item.ID
	fullDatabaseID := mutation.AddProjectV2ItemByID.Item.FullDatabaseID

	// Step 4: Optionally set the status.
	if status != "" {
		if fullDatabaseID == "" {
			return utils.NewToolResultError("item was added but could not determine item ID to set status"), nil, nil
		}
		addedItemID, err := strconv.ParseInt(fullDatabaseID, 10, 64)
		if err != nil {
			return utils.NewToolResultError(fmt.Sprintf("item was added but could not parse item ID %q: %v", fullDatabaseID, err)), nil, nil
		}
		statusResult, payload, statusErr := setProjectItemStatus(ctx, client, projectOwner, ownerType, projectNumber, addedItemID, "Status", status)
		if statusErr != nil || (statusResult != nil && statusResult.IsError) {
			return statusResult, payload, statusErr
		}
	}

	result := map[string]any{
		"id": addedItemNodeID,
	}
	if fullDatabaseID != "" {
		result["full_database_id"] = fullDatabaseID
		if itemID, parseErr := strconv.ParseInt(fullDatabaseID, 10, 64); parseErr == nil {
			result["item_id"] = itemID
		}
	}

	r, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return nil, nil, fmt.Errorf("failed to marshal response: %w", marshalErr)
	}
	return utils.NewToolResultText(string(r)), nil, nil
}

// AddIssueToProject returns the tool and handler for adding an issue to a GitHub Project
// and optionally setting its status in a single call.
func AddIssueToProject(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataProjects,
		mcp.Tool{
			Name:        "add_issue_to_project",
			Description: t("TOOL_ADD_ISSUE_TO_PROJECT_DESCRIPTION", "Add an issue to a GitHub Project and optionally set its status field in a single call."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_ADD_ISSUE_TO_PROJECT_TITLE", "Add issue to project"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "The owner of the repository containing the issue.",
					},
					"repo": {
						Type:        "string",
						Description: "The repository name containing the issue.",
					},
					"issue_number": {
						Type:        "number",
						Description: "The issue number to add to the project.",
					},
					"owner_type": {
						Type:        "string",
						Description: "Project owner type (user or org). If not provided, will be automatically detected.",
						Enum:        []any{"user", "org"},
					},
					"project_owner": {
						Type:        "string",
						Description: "The owner (user or organization) of the project.",
					},
					"project_number": {
						Type:        "number",
						Description: "The project's number.",
					},
					"status": {
						Type:        "string",
						Description: "Optional: human-readable status option name to set on the item after adding (e.g. \"In Progress\").",
					},
				},
				Required: []string{"owner", "repo", "issue_number", "project_owner", "project_number"},
			},
		},
		[]scopes.Scope{scopes.Project},
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

			projectOwner, err := RequiredParam[string](args, "project_owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			ownerType, err := OptionalParam[string](args, "owner_type")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			projectNumber, err := RequiredInt(args, "project_number")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			status, err := OptionalParam[string](args, "status")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			gqlClient, err := deps.GetGQLClient(ctx)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			if ownerType == "" {
				ownerType, err = detectOwnerType(ctx, client, projectOwner, projectNumber)
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
			}

			return addIssueToProject(ctx, client, gqlClient, owner, repo, issueNumber, projectOwner, ownerType, projectNumber, status)
		},
	)
}
