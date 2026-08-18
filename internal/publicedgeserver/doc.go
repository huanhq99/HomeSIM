// Package publicedgeserver terminates the deliberately narrow public-edge
// transport. It accepts one authenticated home-gateway WebSocket and exposes
// only a coarse, authenticated, read-only state snapshot over HTTP.
//
// This package is not a reverse proxy. It has no mutation dispatcher, SMS
// payload endpoint, media relay, arbitrary tunnel, or device-management API.
package publicedgeserver
