:=== cmd.bat ===

@echo off

go mod tidy
go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g cmd/server/main.go -o docs --parseInternal
go run ./cmd/server

:===
