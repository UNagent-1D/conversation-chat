package channel

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Fixed vector copied verbatim from scripts/channel-vector.json at the
// umbrella root. Every service's test asserts against the same constants so
// Rust / Go / TS / Python / Java are guaranteed to agree on the AES-256-GCM
// wire format before any service-to-service test runs.
const (
	vecKeyB64        = "ASNFZ4mrze8BI0VniavN7wEjRWeJq83vASNFZ4mrze8="
	vecIVB64         = "CgsMDQ4PEBESExQV"
	vecPlaintextB64  = "eyJoZWxsbyI6IndvcmxkIiwidGVuYW50IjoiZGVtbyIsIm4iOjQyfQ=="
	vecCiphertextB64 = "HA/jiyD9Ct203QHgmTq3vFe0VYYm5ynGin9Xn7B3QVGAZkDXaJLrEQ=="
	vecTagB64        = "XjADTXgk4M//4jqs0Cu05g=="
)

func resetForTest(t *testing.T, keyB64 string, on bool) {
	t.Helper()
	if err := Init(keyB64, on); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

func TestVectorDecryptsToKnownPlaintext(t *testing.T) {
	resetForTest(t, vecKeyB64, true)
	env := Envelope{V: 1, IV: vecIVB64, CT: vecCiphertextB64, Tag: vecTagB64}
	plain, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	expected, _ := base64.StdEncoding.DecodeString(vecPlaintextB64)
	if string(plain) != string(expected) {
		t.Fatalf("plaintext mismatch:\n got  %q\n want %q", plain, expected)
	}
}

func TestRoundtripPreservesPayload(t *testing.T) {
	resetForTest(t, vecKeyB64, true)
	payload := []byte(`{"session":"abc","n":7,"msg":"hola"}`)
	env, err := Seal(payload)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if env.V != 1 {
		t.Fatalf("envelope version: got %d, want 1", env.V)
	}
	plain, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(plain) != string(payload) {
		t.Fatalf("payload mismatch:\n got  %q\n want %q", plain, payload)
	}
}

func TestTamperedCiphertextFailsAEAD(t *testing.T) {
	resetForTest(t, vecKeyB64, true)
	// Flip a single byte of ciphertext after decoding so the result is still
	// valid base64 but fails AEAD verification.
	raw, err := base64.StdEncoding.DecodeString(vecCiphertextB64)
	if err != nil {
		t.Fatalf("decode vec ct: %v", err)
	}
	raw[0] ^= 0x01
	env := Envelope{
		V:   1,
		IV:  vecIVB64,
		CT:  base64.StdEncoding.EncodeToString(raw),
		Tag: vecTagB64,
	}
	if _, err := Open(env); err == nil {
		t.Fatal("expected AEAD failure on tampered ciphertext")
	} else if !strings.Contains(err.Error(), "AEAD") {
		t.Fatalf("expected AEAD-flavoured error, got: %v", err)
	}
}

func TestSealJSONInactiveReturnsPlaintext(t *testing.T) {
	resetForTest(t, "", false)
	body, ct, encrypted, err := SealJSON(map[string]any{"a": 1})
	if err != nil {
		t.Fatalf("SealJSON: %v", err)
	}
	if encrypted {
		t.Fatal("expected encrypted=false when channel disabled")
	}
	if ct != "application/json" {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}
	if string(body) != `{"a":1}` {
		t.Fatalf("body: got %q, want %q", body, `{"a":1}`)
	}
}

func TestInitRejectsBadKey(t *testing.T) {
	if err := Init("not-base64!!!", true); err == nil {
		t.Fatal("expected error on garbage base64")
	}
	if err := Init(base64.StdEncoding.EncodeToString(make([]byte, 16)), true); err == nil {
		t.Fatal("expected error on 16-byte key")
	}
	if err := Init("", true); err == nil {
		t.Fatal("expected error when enabled but no key")
	}
}
