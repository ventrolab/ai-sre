// Tool Hub: routes read-only tool calls from HolmesGPT (MCP) to agents (community edition).
package main

import "github.com/ventrolab/ai-sre/services/tool-hub/hub"

var version = "dev"

func main() {
	hub.Run(hub.Options{Version: version})
}
