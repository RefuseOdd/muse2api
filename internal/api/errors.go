package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPError is a status/message pair translated into the OpenAI error envelope
// on /v1 paths and into {"detail": ...} on admin paths.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

func httpError(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Message: fmt.Sprintf(format, args...)}
}

func errType(status int) string {
	switch {
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := 500
	message := "internal server error"
	var he *HTTPError
	if errors.As(err, &he) {
		status = he.Status
		message = he.Message
	} else if err != nil {
		message = err.Error()
	}
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeJSON(w, status, map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    errType(status),
				"param":   nil,
				"code":    status,
			},
		})
		return
	}
	writeJSON(w, status, map[string]any{"detail": message})
}

func jsonMarshalNoEscape(value any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return httpError(400, "request body is empty")
		}
		return httpError(422, "request validation failed: %v", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return httpError(422, "request body must contain one JSON value")
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

func bearerToken(header string) string {
	if header == "" || !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
