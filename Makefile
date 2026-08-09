GOOSE ?= go run github.com/pressly/goose/v3/cmd/goose@v3.27.3
SQLC  ?= go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0

.PHONY: migrate-create migrate-up migrate-down migrate-status sqlc sqlc-check

# make migrate-create name=add_summaries
migrate-create:
	$(GOOSE) -s create $(name) sql

migrate-up:
	$(GOOSE) up

migrate-down:
	$(GOOSE) down

migrate-status:
	$(GOOSE) status

sqlc:
	$(SQLC) generate

# CI gate: fails if the committed generated code is stale relative to the SQL.
sqlc-check:
	$(SQLC) diff
