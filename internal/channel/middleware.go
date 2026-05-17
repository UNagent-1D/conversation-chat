package channel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ContextKey marks a request that arrived in envelope form; the sealing
// writer reads this to decide whether to encrypt the response.
const ContextKey = "secure_channel.inbound"

// Middleware decrypts inbound bodies whose Content-Type matches the secure
// channel and re-seals the response after the handler chain runs. Plaintext
// requests (health probes, browser-fronted endpoints) pass through.
//
// Trigger is dual-signal: either the request Content-Type is the envelope
// MIME (request HAS a body to decrypt) OR X-Secure-Channel: aes256gcm/1 is
// set on a bodyless verb (GET / DELETE want their response sealed).
//
// Order matters: mount this AFTER request_id / logger so they still see the
// real Content-Type and Content-Length, but BEFORE auth / RBAC middleware
// so they read plaintext claims out of the request body.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ct := c.GetHeader("Content-Type")
		hasHeader := c.GetHeader(HeaderName) != ""
		bodyIsEnvelope := strings.HasPrefix(ct, ContentType)
		if !bodyIsEnvelope && !hasHeader {
			c.Next()
			return
		}

		if bodyIsEnvelope {
			raw, err := io.ReadAll(c.Request.Body)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error":  "secure_channel_read_failed",
					"detail": err.Error(),
				})
				return
			}
			_ = c.Request.Body.Close()

			var env Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error":  "secure_channel_envelope_invalid",
					"detail": err.Error(),
				})
				return
			}
			plain, err := Open(env)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error":  "secure_channel_decrypt_failed",
					"detail": err.Error(),
				})
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(plain))
			c.Request.ContentLength = int64(len(plain))
			c.Request.Header.Set("Content-Type", "application/json")
		}
		c.Set(ContextKey, true)

		// Outbound: buffer the response so we can seal it on flush.
		sealed := &sealingWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = sealed

		c.Next()

		if err := sealed.flush(); err != nil {
			// Best effort. The underlying connection may already be in a
			// broken state at this point so we just log.
			c.Error(err) //nolint:errcheck
		}
	}
}

// sealingWriter buffers handler output and seals it into an envelope on
// flush. It satisfies gin.ResponseWriter via embedding.
type sealingWriter struct {
	gin.ResponseWriter
	buf       *bytes.Buffer
	status    int
	hasStatus bool
	written   bool
}

func (w *sealingWriter) Write(b []byte) (int, error) {
	return w.buf.Write(b)
}

func (w *sealingWriter) WriteString(s string) (int, error) {
	return w.buf.WriteString(s)
}

func (w *sealingWriter) WriteHeader(code int) {
	w.status = code
	w.hasStatus = true
}

// flush seals whatever the handler buffered and emits the envelope. A 204
// or otherwise empty body is passed through unchanged (still 2xx).
func (w *sealingWriter) flush() error {
	if w.written {
		return nil
	}
	w.written = true
	body := w.buf.Bytes()
	status := w.status
	if !w.hasStatus {
		status = http.StatusOK
	}
	h := w.ResponseWriter.Header()
	if len(body) == 0 {
		w.ResponseWriter.WriteHeader(status)
		return nil
	}
	env, err := Seal(body)
	if err != nil {
		// Encryption failure — fall back to plaintext so the caller at least
		// sees the status code. This is purely defensive.
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.ResponseWriter.WriteHeader(status)
		_, writeErr := w.ResponseWriter.Write(body)
		if writeErr != nil {
			return writeErr
		}
		return err
	}
	envBytes, err := json.Marshal(env)
	if err != nil {
		return err
	}
	h.Set("Content-Type", ContentType)
	h.Set(HeaderName, HeaderValue)
	h.Set("Content-Length", strconv.Itoa(len(envBytes)))
	w.ResponseWriter.WriteHeader(status)
	_, err = w.ResponseWriter.Write(envBytes)
	return err
}
