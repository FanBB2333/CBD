// Package yakpush shares a rotating YakPhone token between call and SMS push.
package yakpush

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrNoToken = errors.New("YakPhone push token is not configured")

type credentials struct {
	CallbackKey string `json:"callback_key"`
	Token       string `json:"voip_token"`
}

type Client struct {
	mu          sync.RWMutex
	credentials credentials
	path        string
	endpoint    string
	http        *http.Client
}

// Open loads a persistent callback credential and the last token reported by
// the phone. A stored token takes precedence over the legacy configuration.
func Open(path, initialToken string) (*Client, error) {
	c := &Client{path: path, endpoint: "https://push.yakteam.com/v1/notify", http: &http.Client{Timeout: 8 * time.Second}}
	c.credentials.Token = strings.TrimSpace(initialToken)
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &c.credentials); err != nil {
			return nil, fmt.Errorf("decode YakPhone credentials: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if c.credentials.CallbackKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		c.credentials.CallbackKey = hex.EncodeToString(key)
	}
	if err := c.save(c.credentials); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) save(value credentials) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".yakphone-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), c.path)
}

func (c *Client) CallbackPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return "/token/" + c.credentials.CallbackKey
}

// ServeHTTP is deliberately isolated from the gateway's control API. The
// unguessable callback URL is needed because Yak only lets users set a URL.
func (c *Client) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte(c.CallbackPath())) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Token string `json:"voip_token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		http.Error(w, "invalid JSON", 400)
		return
	}
	token := strings.TrimSpace(body.Token)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) < 32 || len(decoded) > 256 {
		http.Error(w, "invalid voip_token", 400)
		return
	}
	c.mu.Lock()
	next := c.credentials
	next.Token = token
	err = c.save(next)
	if err == nil {
		c.credentials = next
	}
	c.mu.Unlock()
	if err != nil {
		slog.Error("yakpush token persistence failed")
		http.Error(w, "cannot save token", 500)
		return
	}
	slog.Info("yakpush token updated", "token_bytes", len(decoded))
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}

func (c *Client) Send(ctx context.Context, callerURI, pushType, messageBody string) error {
	c.mu.RLock()
	token := c.credentials.Token
	c.mu.RUnlock()
	if token == "" {
		return ErrNoToken
	}
	payload := map[string]string{"token": token, "caller_uri": callerURI, "caller_name": strings.TrimPrefix(callerURI, "sip:"), "type": pushType}
	if messageBody != "" {
		payload["message_body"] = messageBody
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Never log the response body: upstream validation may echo the token.
	response, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("YakPhone relay rejected request: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Error   json.RawMessage `json:"error"`
		Success *bool           `json:"success"`
	}
	if json.Unmarshal(response, &result) == nil && ((len(result.Error) > 0 && string(result.Error) != "null" && string(result.Error) != `""`) || (result.Success != nil && !*result.Success)) {
		return errors.New("YakPhone relay returned an application error")
	}
	slog.Info("yakpush accepted", "type", pushType, "status", resp.StatusCode)
	return nil
}
