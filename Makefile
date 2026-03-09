.PHONY: run build test tidy docker-up docker-down

run:
	go run ./cmd/server/main.go

build:
	go build -o bin/conversation-chat ./cmd/server/main.go

test:
	go test ./... -v -race -count=1

tidy:
	go mod tidy

docker-up:
	docker compose up -d

docker-down:
	docker compose down
