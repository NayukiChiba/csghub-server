.PHONY: build test lint cover mock_wire mock_gen swag migrate_local

# Match the Swagger library version in go.mod; also works on a clean checkout.
SWAG ?= go run github.com/swaggo/swag/cmd/swag@v1.16.6

build: swag
	go build -o ./bin/csghub-server ./cmd/csghub-server

test: swag
	go test ./...

lint: swag
	golangci-lint run

cover: swag
	go test -coverprofile=cover.out ./...
	go tool cover -html=cover.out -o cover.html
	open cover.html

mock_wire:
	@echo "Running wire for component mocks..."
	@go run -mod=mod github.com/google/wire/cmd/wire opencsg.com/csghub-server/component/...
	@if [ $$? -eq 0 ]; then \
		echo "Renaming component wire_gen.go to wire_gen_test.go..."; \
		mv component/wire_gen.go component/wire_gen_test.go; \
		echo "Renaming component/callback wire_gen.go to wire_gen_test.go..."; \
		mv component/callback/wire_gen.go component/callback/wire_gen_test.go; \
	else \
		echo "Wire failed, skipping renaming."; \
	fi

mock_gen:
	mockery

swag:
	$(SWAG) init --pd -d cmd/csghub-server/cmd/start,api/router,api/handler,builder/store/database,common/types,accounting/handler,user/handler,moderation/handler,component,dataviewer/handler,aigateway/handler,aigateway/types,notification/handler -g server.go

migrate_local:
	go run cmd/csghub-server/main.go migration migrate --config local.toml
	
db_migrate:
	@go run cmd/csghub-server/main.go migration migrate --config local.toml

db_rollback:
	@go run cmd/csghub-server/main.go migration rollback --config local.toml

error_doc:
	@go run cmd/csghub-server/main.go errorx doc-gen

error_scan:
	@go run cmd/csghub-server/main.go errorx scan --dir $(dir) -v

notify_gen:
	@go run cmd/csghub-server/main.go notification notify-gen -l Info
