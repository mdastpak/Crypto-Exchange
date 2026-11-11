// Command server is the Crypto Exchange server: the REST API, the WebSocket
// gateway, and their background work. internal/app wires and runs it (#48).
package main

import "github.com/mdastpak/crypto-exchange/internal/app"

// The general information of the OpenAPI document, which swag generates
// into docs/ from these and the handlers' annotations (#54). Its paths are
// absolute, so that it holds the API under /api/v1 and the health probes.
//
// @title Crypto Exchange API
// @version 1
// @description The HTTP API of the Crypto Exchange server. docs/api.md describes the conventions: the envelope, the error codes, sign-in, and the WebSocket messages. Every route is under /api/v1, besides the health probes /system/live and /system/ready.
// @BasePath /
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @description A user or an admin access token, as "Bearer <token>".
func main() {
	app.Main()
}
