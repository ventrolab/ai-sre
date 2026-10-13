// Package tunnel carries tool calls between the Tool Hub and agents.
//
// Agents dial out to the hub over WebSocket (TLS). The agent authenticates with a bearer
// agent token, which the hub maps to the agent name, or with a client certificate whose
// CommonName is the name (mutual TLS, optional). Frames are JSON:
// the agent sends "hello" once, then the hub sends "request" frames and the
// agent answers each with a "response" frame carrying the same id.
package tunnel

import "encoding/json"

const (
	FrameHello    = "hello"
	FrameRequest  = "request"
	FrameResponse = "response"

	// Path of the tunnel endpoint on the hub.
	Path = "/v1/tunnel"
)

type Hello struct {
	// Kind is "cluster" or "host".
	Kind    string            `json:"kind"`
	Labels  map[string]string `json:"labels,omitempty"`
	Tools   []string          `json:"tools"`
	Version string            `json:"version,omitempty"`
}

type Request struct {
	ID        string          `json:"id"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args,omitempty"`
	TimeoutMS int             `json:"timeout_ms,omitempty"`
}

type Response struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Frame struct {
	Type     string    `json:"type"`
	Hello    *Hello    `json:"hello,omitempty"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
}
