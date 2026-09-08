## 1. Data model and migration

- [x] 1.1 Add `migrations/018_user_llm_credentials.sql` creating `user_llm_credentials` (`user_id` PK, plaintext `api_key VARCHAR(512)`, timestamps).
- [x] 1.2 Add the `internal/model` credential struct with `db`/`json` tags (token field never JSON-serialized).

## 2. MySQL store

- [x] 2.1 Implement MySQL get/upsert/delete methods for the credential row (upsert is idempotent overwrite; delete of a missing row succeeds).
- [x] 2.2 Add store unit tests: round-trip, overwrite, delete-then-get-miss, cross-user isolation.

## 3. Client resolver and agent wiring

- [x] 3.1 Implement the per-user client resolver: user token → `OpenAIClient(global base_url + user token, rawSink)`; no token → existing global client; per-userID cache with invalidation on token write/delete; store errors surface as call failures.
- [x] 3.2 Route `orchestratorFactory.Build` through the resolver so the whole turn's spawn tree shares one per-user client.
- [x] 3.3 Thread userID through title generation, compaction summarization, and webfetch digest so all user-attributable LLM calls resolve per-user clients.
- [x] 3.4 Add agent/service tests: configured-token client selection, fallback when unconfigured, cache invalidation on update/delete, raw-capture sink preserved.

## 4. HTTP API and OpenAPI

- [x] 4.1 Implement authenticated `GET/PUT/DELETE /api/v1/me/llm-token` handlers: GET returns `{configured, masked_key?}` and never the token; PUT trims and validates non-empty ≤512 chars; DELETE falls back to the global key. Register on api/all roles only.
- [x] 4.2 Add handler tests: auth required, masked response shape, validation errors, cross-user isolation, agent role does not register the routes.
- [x] 4.3 Update `api/openapi.yaml` with paths, schemas, examples, and 400/401 contracts.

## 5. Integration and verification

- [x] 5.1 Add integration coverage: configure token → chat/title/compaction calls carry the user token; delete token → calls fall back to the global key; invalid configured token surfaces an agent error without silent fallback.
- [x] 5.2 Verify unconfigured-user behavior is byte-for-byte unchanged (existing suite) and logs/raw capture contain no token material.
- [x] 5.3 Run focused store/agent/handler tests, `make test`, `make lint`; gofmt all changes.
