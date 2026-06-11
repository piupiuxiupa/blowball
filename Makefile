.PHONY: build run test migrate lint seed clean

build:
	go build -o bin/blowball ./cmd/server/
	go build -o bin/seed ./cmd/seed/

run: build
	./bin/blowball

test:
	go test -race ./...

migrate:
	@echo "Apply migrations manually: mysql -u root -p blowball < migrations/001_users.sql"
	@echo "Or use a migration tool like goose/golang-migrate"

seed:
	@echo "Usage: bin/seed -username <name>"
	@echo "  bin/seed -username alice                      # prompt for password"
	@echo "  bin/seed -username alice -password 'pw'        # non-interactive"
	@echo "  bin/seed -username alice -dry-run              # preview hash only"

lint:
	go vet ./...

clean:
	rm -rf bin/
