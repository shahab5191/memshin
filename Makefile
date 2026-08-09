# Versions are pinned by the tool directives in go.mod, so they're covered by
# go.sum and bumpable by Dependabot rather than re-resolved at build time.
GOOSE ?= go tool goose
SQLC  ?= go tool sqlc

# Optional so CI, which has no .env and sets the vars directly, still works.
-include .env
export

# Derived from the discrete vars so credentials live in exactly one place and
# cannot drift from what docker-compose provisions. ?= lets CI override with a
# fully-formed DSN. Note this concatenates without URL-escaping, unlike
# buildDSN in cmd/api — fine for local credentials, but a password containing
# @ : / ? # will need escaping here.
GOOSE_DBSTRING ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@$(POSTGRES_HOST):$(POSTGRES_PORT)/$(POSTGRES_DB)?sslmode=$(POSTGRES_SSLMODE)

.PHONY: migrate-create migrate-up migrate-down migrate-status sqlc sqlc-check run

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

run:
	go run ./cmd/api

