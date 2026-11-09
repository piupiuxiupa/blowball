# MCP HTTP Discovery Fix Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this small, sequential fix with TDD.

**Goal:** Restore Wind MCP discovery without changing credentials, HTTP contracts, or other servers.

**Architecture:** Preserve the configured HTTP endpoint verbatim. Reject initialize results without a protocol version and tools/list results without a tools array, instead of accepting an upstream error as an empty discovery result.

**Tech Stack:** Go 1.26, net/http, httptest, Testify.

**Spec:** The approved diagnosis and design below.

## Approved Design

Wind's `/vserver_stock_data/mcp/` endpoint returns ten tools. Removing its trailing slash reaches a different route, which returns HTTP 200 with an error-shaped result. The current transport silently accepts that result and installation caches zero tools. Preserve the original URL and validate the two discovery result fields before returning success. A valid empty `tools: []` remains supported.

## Global Constraints

- No new dependencies, API changes, or credential changes.
- Restrict production edits to `internal/tool/mcpclient/http.go`.
- Preserve unrelated working-tree changes and never commit the executable.

## Review Focus

- Endpoint path trailing slashes survive initialization and discovery.
- HTTP 200 vendor error results cannot become successful discovery.
- Missing or empty protocol versions fail initialization.
- Missing or null tools arrays fail discovery; an empty array succeeds.
- Existing JSON, SSE, authentication, and session retry behavior stays intact.

## Task 1: Correct HTTP Discovery

**Files:** `internal/tool/mcpclient/http.go`, `internal/tool/mcpclient/http_test.go`.

- [x] Add endpoint-path, invalid-discovery-result, and valid-empty-tools regression tests.
- [x] Run the new tests and confirm the path and invalid-result cases fail before implementation.
- [x] Preserve the endpoint URL; validate protocolVersion and the tools array.
- [x] Run the MCP client and per-user MCP tests.
- [x] Run `make test` and `make lint`.

## Local Runtime Verification

- [x] Deploy the verified executable using the existing local API/agent configuration.
- [x] Refresh only wind-stock's cached tools, preserving its other configuration.
- [x] Verify real discovery and the authenticated MCP tools API both expose ten Wind tools.

Both `/healthz` endpoints returned 200. The authenticated tools endpoint returned ten Wind tools with input schemas; GilData, comein-ic, and doris-mcp retained their prior tool counts. Cache refresh used the existing production `mcp_list_tools` implementation, without creating a chat or changing non-tool configuration fields.

Create one scoped Conventional Commit after the source tests and local runtime verification, excluding the executable and unrelated changes.
