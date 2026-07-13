.PHONY: build run test migrate lint seed clean frontend-install frontend-dev frontend-build frontend-lint

FRONTEND_DIR := frontend

build:
	go build -o bin/blowball ./cmd/blowball/

run: build
	./bin/blowball serve

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

frontend-install:
	cd $(FRONTEND_DIR) && npm install

frontend-dev: frontend-install
	cd $(FRONTEND_DIR) && npm run dev

frontend-build: frontend-install
	cd $(FRONTEND_DIR) && npm run build

frontend-lint:
	cd $(FRONTEND_DIR) && npm run lint
