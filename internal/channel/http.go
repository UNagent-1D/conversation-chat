package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Request is a thin record of an outbound JSON-over-HTTP call. The helper
// here serialises body, seals it when the channel is active, attaches the
// right headers, and returns the plaintext response bytes plus the upstream
// status code.
//
// Use this from every outbound HTTP client in conversation-chat (acr_client,
// tenant_client, ChatService.executeTool) so the secure-channel handshake
// is applied uniformly without duplicating boilerplate at each call site.
type Request struct {
	Method  string
	URL     string
	Body    any            // serialised + sealed when non-nil and Active()
	Headers map[string]string
}

// Do executes the request with the supplied http.Client and returns the
// decrypted body bytes alongside the status code. Non-2xx is NOT treated
// as an error here — callers decide based on status.
func Do(ctx context.Context, client *http.Client, r Request) ([]byte, int, error) {
	var reqBody io.Reader
	contentType := ""
	encrypted := false
	if r.Body != nil {
		bodyBytes, ct, enc, err := SealJSON(r.Body)
		if err != nil {
			return nil, 0, fmt.Errorf("seal request body: %w", err)
		}
		reqBody = bytes.NewReader(bodyBytes)
		contentType = ct
		encrypted = enc
	}

	httpReq, err := http.NewRequestWithContext(ctx, r.Method, r.URL, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	// Set X-Secure-Channel either because we sealed a body OR because the
	// channel is active but we have no body to seal (GET, DELETE) — the
	// header alone tells the callee to seal its response.
	if encrypted || (r.Body == nil && Active()) {
		httpReq.Header.Set(HeaderName, HeaderValue)
	}
	for k, v := range r.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	plain, err := OpenBytes(resp.Header.Get("Content-Type"), raw)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decrypt response: %w", err)
	}
	return plain, resp.StatusCode, nil
}

// DoJSON is the same as Do but unmarshals the decrypted response into v on
// 2xx. On non-2xx it returns the raw body so the caller can build a
// service-specific error message.
func DoJSON(ctx context.Context, client *http.Client, r Request, v any) (int, []byte, error) {
	body, status, err := Do(ctx, client, r)
	if err != nil {
		return status, body, err
	}
	if status < 200 || status >= 300 {
		return status, body, nil
	}
	if v == nil || len(body) == 0 {
		return status, body, nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return status, body, fmt.Errorf("unmarshal response: %w", err)
	}
	return status, body, nil
}
