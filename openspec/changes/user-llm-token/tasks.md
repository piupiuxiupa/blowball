## 1. Data model and migration

- [ ] 1.1 Add `migrations/018_user_llm_credentials.sql` creating `user_llm_credentials` (`user_id` PK, plaintext `api_key VARCHAR(512)`, timestamps).
- [ ] 1.2 Add the `internal/model` credential struct with `db`/`json` tags (token field never JSON-serialized).

## 2. MySQL store

- [ ] 2.1 Implement MySQL get/upsert/delete methods for the credential row (upsert is idempotent overwrite; delete of a missing row succeeds).
- [ ] 2.2 Add store unit tests: round-trip, overwrite, delete-then-get-miss, cross-user isolation.

## 3. Client resolver and agent wiring

- [ ] 3.1 Implement the per-user client resolver: user token → `OpenAIClient(global base_url + user token, rawSink)`; no token → existing global client; per-userID cache with invalidation on token write/delete; store errors surface as call failures.
- [ ] 3.2 Route `orchestratorFactory.Build` through the resolver so the whole turn's spawn tree shares one per-user client.
- [ ] 3.3 Thread userID through title generation, compaction summarization, and webfetch digest so all user-attributable LLM calls resolve per-user clients.
- [ ] 3.4 Add agent/service tests: configured-token client selection, fallback when unconfigured, cache invalidation on update/delete, raw-capture sink preserved.

## 4. HTTP API and OpenAPI

- [ ] 4.1 Implement authenticated `GET/PUT/DELETE /api/v1/me/llm-token` handlers: GET returns `{configured, masked_key?}` and never the token; PUT trims and validates non-empty ≤512 chars; DELETE falls back to the global key. Register on api/all roles only.
- [ ] 4.2 Add handler tests: auth required, masked response shape, validation errors, cross-user isolation, agent role does not register the routes.
- [ ] 4.3 Update `api/openapi.yaml` with paths, schemas, examples, and 400/401 contracts.

## 5. Integration and verification

- [ ] 5.1 Add integration coverage: configure token → chat/title/compaction calls carry the user token; delete token → calls fall back to the global key; invalid configured token surfaces an agent error without silent fallback.
- [ ] 5.2 Verify unconfigured-user behavior is byte-for-byte unchanged (existing suite) and logs/raw capture contain no token material.
- [ ] 5.3 Run focused store/agent/handler tests, `make test`, `make lint`; gofmt all changes.
