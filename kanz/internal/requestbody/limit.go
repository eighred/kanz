// Package requestbody defines the proxied API request-body contract shared by
// the web BFF and API gateway.
package requestbody

// MaxBytes is the largest request body accepted on the control-plane API proxy.
// It is derived from the edge's 1 MiB proxy-body-size and shared so the BFF
// cannot buffer a payload that the gateway will later refuse.
const MaxBytes = 1 << 20 // 1 MiB
