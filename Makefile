DATABASE_URL ?= postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable
export DATABASE_URL

.PHONY: up down test bench run lint

up:
	docker compose up --build -d

down:
	docker compose down

test:
	go test -race -count=1 ./...

bench:
	go test ./internal/ledger -run '^$$' -bench Transfer -benchtime 5s -cpu 1,8,16

run:
	go run ./cmd/server

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"
