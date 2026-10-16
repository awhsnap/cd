.PHONY: dev test build seed staff fmt

# Run locally: customer app on :8080, staff admin on 127.0.0.1:9090
dev:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/server serve

test:
	go test ./...

build:
	CGO_ENABLED=1 go build -o bin/server ./cmd/server

# Create a demo business with sample data (prints the login)
seed:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/server seed-demo

# Create a staff login: make staff EMAIL=you@company.com NAME="Your Name" ROLE=owner
staff:
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/server create-staff -email "$(EMAIL)" -name "$(NAME)" -role "$(or $(ROLE),owner)"

fmt:
	gofmt -w .
