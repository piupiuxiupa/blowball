.PHONY: build run run-api run-agent test migrate lint seed clean

build:
	go build -o bin/blowball ./cmd/blowball/

# run == --role all: the pre-split monolith (full route set on server.port).
run: build
	./bin/blowball serve

# Split process roles. Run both against the SAME -d root so they share the data
# plane (MySQL/Redis/local FS); external routing directs the streaming route and
# /mcp/tools to the agent port and everything else to the api port.
run-api: build
	./bin/blowball serve --role api

run-agent: build
	./bin/blowball serve --role agent

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
