.PHONY: build run test migrate lint seed clean frontend-install frontend-dev frontend-build frontend-lint

FRONTEND_DIR := frontend

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

frontend-install:
	cd $(FRONTEND_DIR) && npm install

frontend-dev: frontend-install
	cd $(FRONTEND_DIR) && npm run dev

frontend-build: frontend-install
	cd $(FRONTEND_DIR) && npm run build

frontend-lint:
	cd $(FRONTEND_DIR) && npm run lint
