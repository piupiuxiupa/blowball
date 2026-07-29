package handler

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"
)

// noopHandler is a stand-in gin.HandlerFunc used to populate RouteDeps for
// route-set inspection without depending on any real handler.
func noopHandler(*gin.Context) {}

// stubRouteDeps returns a RouteDeps with every handler field set to a no-op so
// any partition can be registered purely for route-set inspection.
func stubRouteDeps() RouteDeps {
	noop := noopHandler
	return RouteDeps{
		AuthMW:                      noop,
		QueryTokenAuthMW:            noop,
		WorkspaceTokenDownload:      noop,
		Login:                       noop,
		SessionList:                 noop,
		SessionCreate:               noop,
		SessionMessages:             noop,
		SendMessage:                 noop,
		SessionDelete:               noop,
		SessionUpdateTitle:          noop,
		WorkspaceList:               noop,
		WorkspaceUpload:             noop,
		WorkspaceDownload:           noop,
		WorkspaceContent:            noop,
		WorkspaceDelete:             noop,
		WorkspaceRename:             noop,
		WorkspaceOnlyOfficeConfig:   noop,
		WorkspaceOnlyOfficeCallback: noop,
		MCPTools:                    noop,
		SkillsList:                  noop,
	}
}

// routeSet returns the sorted "METHOD path" strings registered on r. It is the
// shape used by the per-role partition assertions.
func routeSet(r *gin.Engine) []string {
	out := make([]string, 0, len(r.Routes()))
	for _, ri := range r.Routes() {
		out = append(out, ri.Method+" "+ri.Path)
	}
	sort.Strings(out)
	return out
}

// expectedAPIRoutes is the exact route set the api role registers
// (RegisterHealthz + RegisterAPIRoutes): health, auth, session CRUD, message
// history read, workspace file CRUD, the OnlyOffice save callback, and skills.
// It must NOT contain the streaming message endpoint or the MCP tool list.
var expectedAPIRoutes = []string{
	"DELETE /api/v1/sessions/:session_id",
	"GET /api/v1/sessions",
	"GET /api/v1/sessions/:session_id/messages",
	"GET /api/v1/skills",
	"GET /api/v1/workspace/files",
	"GET /api/v1/workspace/files/*path",
	"PATCH /api/v1/sessions/:session_id",
	"POST /api/v1/auth/login",
	"POST /api/v1/sessions",
	"POST /api/v1/workspace/files/*path",
	"POST /api/v1/workspace/onlyoffice-callback",
	"POST /api/v1/workspace/upload",
	"PUT /api/v1/workspace/files/*path",
	"DELETE /api/v1/workspace/files/*path",
	"GET /healthz",
}

// expectedAgentRoutes is the exact route set the agent role registers
// (RegisterHealthz + RegisterAgentRoutes): health, the streaming message
// endpoint, and the MCP tool list. It must NOT contain any CRUD route.
var expectedAgentRoutes = []string{
	"GET /healthz",
	"GET /api/v1/mcp/tools",
	"POST /api/v1/sessions/:session_id/messages",
}

// TestRegisterAPIRoutes_ExactRouteSet asserts the api partition registers
// exactly the CRUD route set and nothing from the agent partition.
func TestRegisterAPIRoutes_ExactRouteSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterHealthz(r)
	RegisterAPIRoutes(r, stubRouteDeps())

	got := routeSet(r)
	sort.Strings(expectedAPIRoutes)
	if !equalStringSlices(got, expectedAPIRoutes) {
		t.Errorf("api route set mismatch\ngot:  %v\nwant: %v", got, expectedAPIRoutes)
	}
}

// TestRegisterAgentRoutes_ExactRouteSet asserts the agent partition registers
// exactly the streaming + MCP route set and nothing from the CRUD partition.
func TestRegisterAgentRoutes_ExactRouteSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterHealthz(r)
	RegisterAgentRoutes(r, stubRouteDeps())

	got := routeSet(r)
	sort.Strings(expectedAgentRoutes)
	if !equalStringSlices(got, expectedAgentRoutes) {
		t.Errorf("agent route set mismatch\ngot:  %v\nwant: %v", got, expectedAgentRoutes)
	}
}

// TestRegisterRoutes_AllRoleIsUnion asserts the all-role registration is the
// exact union of the two partitions plus a single health check — i.e. the
// pre-split monolith route set, preserving back-compat.
func TestRegisterRoutes_AllRoleIsUnion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterRoutes(r, stubRouteDeps())

	want := append([]string{}, expectedAPIRoutes...)
	// Agent partition contributes two non-health routes (healthz already in the
	// API set via RegisterRoutes' single RegisterHealthz call).
	want = append(want, "GET /api/v1/mcp/tools", "POST /api/v1/sessions/:session_id/messages")
	sort.Strings(want)

	got := routeSet(r)
	if !equalStringSlices(got, want) {
		t.Errorf("all-role route set mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

// TestAPIPartition_StreamingAndMCPRoutesReturn404 drives the api-role engine
// with a real (no-op) handler set and asserts the agent-owned routes — the
// streaming endpoint (POST /sessions/:id/messages) and the MCP tool list
// (GET /mcp/tools) — are not served (404). Note GET /sessions/:id/messages
// (history read) IS an api route and is excluded from this assertion. This is
// the behavioral half of the fault-isolation requirement (the structural half
// is wireAPI never constructing the agent layer).
func TestAPIPartition_StreamingAndMCPRoutesReturn404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterHealthz(r)
	RegisterAPIRoutes(r, stubRouteDeps())

	agentOnly := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/sessions/sess-1/messages"},
		{http.MethodGet, "/api/v1/mcp/tools"},
	}
	for _, tc := range agentOnly {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("api engine: %s %s = %d, want 404 (agent route must not be registered)", tc.method, tc.path, w.Code)
		}
	}
}

// TestAgentPartition_CRUDRoutesReturn404 drives the agent-role engine and
// asserts the session/workspace/skills CRUD routes are not served (404),
// proving the agent role carries no CRUD entry point.
func TestAgentPartition_CRUDRoutesReturn404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterHealthz(r)
	RegisterAgentRoutes(r, stubRouteDeps())

	targets := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/sessions"},
		{http.MethodPost, "/api/v1/sessions"},
		{http.MethodGet, "/api/v1/sessions/sess-1/messages"},
		{http.MethodDelete, "/api/v1/sessions/sess-1"},
		{http.MethodPatch, "/api/v1/sessions/sess-1"},
		{http.MethodGet, "/api/v1/workspace/files"},
		{http.MethodGet, "/api/v1/skills"},
	}
	for _, tc := range targets {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("agent engine: %s %s = %d, want 404 (route must not be registered)", tc.method, tc.path, w.Code)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
