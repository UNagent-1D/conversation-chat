// Package channel implements the AES-256-GCM secure channel used between
// conversation-chat and the rest of the backend services. The wire format
// matches the Rust (chat-orch) / TypeScript (agent-runtime) / Python
// (Hospital-MP, Compliance) / Java (UN_email_send_ms) implementations:
//
//	{ "v": 1, "iv": <b64 12B>, "ct": <b64>, "tag": <b64 16B> }
//
// Detected on HTTP via Content-Type: application/vnd.unagent.secure+json +
// X-Secure-Channel: aes256gcm/1. Detected on AMQP via the same Content-Type
// on amqp.Publishing. Use Init() once at startup, then call Seal/Open or
// the higher-level helpers from anywhere in the service.
package channel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	HeaderName  = "X-Secure-Channel"
	HeaderValue = "aes256gcm/1"
	ContentType = "application/vnd.unagent.secure+json"
	ivLen       = 12
	tagLen      = 16
)

// Envelope is the JSON wire format. Field order matches the cross-language
// vector in scripts/channel-vector.json.
type Envelope struct {
	V   int    `json:"v"`
	IV  string `json:"iv"`
	CT  string `json:"ct"`
	Tag string `json:"tag"`
}

var (
	gcmAEAD cipher.AEAD
	enabled bool
)

// Init configures the package from env material. Pass keyB64 as the base64
// of 32 random bytes (`openssl rand -base64 32`). Returns an error when the
// key is malformed, or when enabled is true but no key is provided.
func Init(keyB64 string, enabledFlag bool) error {
	if keyB64 == "" {
		if enabledFlag {
			return errors.New("BACKEND_CHANNEL_ENABLED=true but BACKEND_CHANNEL_KEY is empty")
		}
		gcmAEAD = nil
		enabled = false
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return fmt.Errorf("BACKEND_CHANNEL_KEY base64 decode: %w", err)
	}
	if len(decoded) != 32 {
		return fmt.Errorf("BACKEND_CHANNEL_KEY must decode to 32 bytes, got %d", len(decoded))
	}
	block, err := aes.NewCipher(decoded)
	if err != nil {
		return fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("new gcm: %w", err)
	}
	gcmAEAD = gcm
	enabled = enabledFlag
	return nil
}

// Active reports whether outbound traffic should be encrypted.
func Active() bool {
	return enabled && gcmAEAD != nil
}

// Seal encrypts plaintext under the configured key and returns the envelope.
func Seal(plaintext []byte) (Envelope, error) {
	if gcmAEAD == nil {
		return Envelope{}, errors.New("channel not initialised")
	}
	iv := make([]byte, ivLen)
	if _, err := rand.Read(iv); err != nil {
		return Envelope{}, fmt.Errorf("rand iv: %w", err)
	}
	// Go's GCM.Seal appends the tag to the ciphertext (last 16 bytes).
	sealed := gcmAEAD.Seal(nil, iv, plaintext, nil)
	ct := sealed[:len(sealed)-tagLen]
	tag := sealed[len(sealed)-tagLen:]
	return Envelope{
		V:   1,
		IV:  base64.StdEncoding.EncodeToString(iv),
		CT:  base64.StdEncoding.EncodeToString(ct),
		Tag: base64.StdEncoding.EncodeToString(tag),
	}, nil
}

// Open verifies and decrypts the envelope. Returns BadRequest-shaped errors
// the caller can surface as a 400.
func Open(env Envelope) ([]byte, error) {
	if gcmAEAD == nil {
		return nil, errors.New("channel not initialised")
	}
	if env.V != 1 {
		return nil, fmt.Errorf("unsupported envelope version %d", env.V)
	}
	iv, err := base64.StdEncoding.DecodeString(env.IV)
	if err != nil {
		return nil, fmt.Errorf("decode iv: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(env.CT)
	if err != nil {
		return nil, fmt.Errorf("decode ct: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(env.Tag)
	if err != nil {
		return nil, fmt.Errorf("decode tag: %w", err)
	}
	if len(iv) != ivLen {
		return nil, fmt.Errorf("iv must be %d bytes, got %d", ivLen, len(iv))
	}
	if len(tag) != tagLen {
		return nil, fmt.Errorf("tag must be %d bytes, got %d", tagLen, len(tag))
	}
	// Reassemble ct||tag for Go's GCM.Open
	sealed := make([]byte, 0, len(ct)+len(tag))
	sealed = append(sealed, ct...)
	sealed = append(sealed, tag...)
	plain, err := gcmAEAD.Open(nil, iv, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("AEAD verification failed: %w", err)
	}
	return plain, nil
}

// SealJSON marshals v to JSON and seals it when active. Returns the
// wire-ready bytes, the content type to set on the request, and a flag
// indicating whether the body was encrypted (caller uses it to attach the
// X-Secure-Channel header).
func SealJSON(v any) (body []byte, contentType string, encrypted bool, err error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, "", false, fmt.Errorf("marshal: %w", err)
	}
	if !Active() {
		return plain, "application/json", false, nil
	}
	env, err := Seal(plain)
	if err != nil {
		return nil, "", false, err
	}
	body, err = json.Marshal(env)
	if err != nil {
		return nil, "", false, fmt.Errorf("marshal envelope: %w", err)
	}
	return body, ContentType, true, nil
}

// OpenBytes decrypts the wire bytes when contentType marks them as an
// envelope, otherwise returns them unchanged. Used by HTTP clients reading
// responses and by the RabbitMQ worker reading queue messages.
func OpenBytes(contentType string, bytes []byte) ([]byte, error) {
	if !strings.HasPrefix(contentType, ContentType) {
		return bytes, nil
	}
	var env Envelope
	if err := json.Unmarshal(bytes, &env); err != nil {
		return nil, fmt.Errorf("parse envelope: %w", err)
	}
	return Open(env)
}
