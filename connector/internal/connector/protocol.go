package connector

const statusProtocol = "kukie.connector.v2"
const protocol = "kukie.connector.v1"
const maxRequest = 1 << 20
const chunkSize = 32 << 10
const maxResponse = 64 << 20

type message struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Path        string `json:"path,omitempty"`
	Body        []byte `json:"body,omitempty"`
	TimeoutMS   int64  `json:"timeoutMs,omitempty"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}
