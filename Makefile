.PHONY: build build-api build-agent build-scheduler build-all run run-api run-agent run-scheduler test migrate lint seed clean proto

build:
	go build -o bin/blowball ./cmd/blowball/

# Standalone Scheduler (extract-scheduler-service). The main `blowball` binary
# never embeds scheduling; deploy this process separately and enable execution
# in services/scheduler/config.yaml.
# Standalone service binaries (monorepo-restructure): blowball-api (api role
# pinned), blowball-agent (agent role pinned), blowball-scheduler. The dev
# monolith `blowball` keeps serve --role all|api|agent.
build-api:
	go build -o bin/blowball-api ./services/apiserver/cmd/blowball-api/

build-agent:
	go build -o bin/blowball-agent ./services/agent/cmd/blowball-agent/

build-scheduler:
	go build -o bin/blowball-scheduler ./services/scheduler/cmd/blowball-scheduler/

build-all: build build-api build-agent build-scheduler

run-scheduler: build-scheduler
	./bin/blowball-scheduler -f services/scheduler/config.yaml -d .

# Regenerate the versioned internal gRPC/protobuf bindings (api/gen/go).
# protoc must be installed; the plugins are locked in go.mod tool directives.
proto:
	./scripts/gen-proto.sh

# run == --role all: the pre-split monolith (full route set on server.port).
run: build
	./bin/blowball serve

# Split process roles. Run both against the SAME -d root so they share the data
# plane (MySQL/Redis/local FS); external routing directs the streaming route and
# /mcp/tools to the agent port and everything else to the api port.
run-api: build-api
	./bin/blowball-api serve

run-agent: build-agent
	./bin/blowball-agent serve

test:
	go test -race ./...

migrate:
	@echo "Apply migrations manually: mysql -u root -p blowball < migrations/001_users.sql"
	@echo "Or use a migration tool like goose/golang-migrate"

seed:
	@echo "Usage: ./bin/blowball seed --username <name>"
	@echo "  ./bin/blowball seed --username alice                      # prompt for password"
	@echo "  ./bin/blowball seed --username alice --password 'pw'        # non-interactive"
	@echo "  ./bin/blowball seed --username alice --dry-run              # preview hash only"

lint:
	go vet ./...

clean:
	rm -rf bin/
