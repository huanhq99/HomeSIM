package publicedge

// GatewayWebSocketPath and GatewayWebSocketSubprotocol are the exact public
// edge transport contract shared by the edge server and the home-Mac
// connector. They are intentionally separate from GatewayControlPath, which
// is the signed logical path inside protocol frames.
const (
	GatewayWebSocketPath        = "/internal/v1/gateways/connect"
	GatewayWebSocketSubprotocol = "djonehub.gateway.v1"
)
