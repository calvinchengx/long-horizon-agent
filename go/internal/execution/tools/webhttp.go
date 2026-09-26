package tools

import "net/http"

// Exported HTTP helpers of the web tools, shared with `lha vendor` (internal/state/vendor), which
// fetches with the same egress rules and reports errors the same way.

// HTTPStatusErrorText is httpx's raise_for_status message for resp (requested as rawURL).
func HTTPStatusErrorText(resp *http.Response, rawURL string) string {
	return statusErrorText(resp, rawURL)
}

// HTTPErrorText renders a client (transport) error without Go's `Get "url": ` prefix.
func HTTPErrorText(err error) string { return httpErrorText(err) }

// DecodeBody decodes a response body like httpx's Response.text: the Content-Type charset when
// it is a known encoding, else UTF-8 with undecodable bytes replaced.
func DecodeBody(body []byte, contentType string) string { return decodeBody(body, contentType) }
