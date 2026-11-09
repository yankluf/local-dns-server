// Package client is the HTTP client the CLI uses to talk to `lds serve`.
package client

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yankluf/local-dns-server/internal/names"
	"github.com/yankluf/local-dns-server/internal/store"
)

// Client talks to the lds API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for baseURL. Redirects are never followed, so the
// token can't be forwarded elsewhere. Set LDS_CA_FILE to trust a private CA
// for https:// servers.
func New(baseURL, token string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if ca := os.Getenv("LDS_CA_FILE"); ca != "" {
		if pem, err := os.ReadFile(ca); err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP: &http.Client{
			Timeout:       15 * time.Second,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// List returns all records.
func (c *Client) List() ([]store.View, error) {
	var out struct {
		Records []store.View `json:"records"`
	}
	return out.Records, c.do(http.MethodGet, "/api/records", nil, &out)
}

// Add creates a record. The type is inferred from name and value.
func (c *Client) Add(name, value, comment string) (store.View, error) {
	var out store.View
	in := map[string]string{"name": name, "value": value, "comment": comment}
	return out, c.do(http.MethodPost, "/api/records", in, &out)
}

// Remove deletes a manual record by name or ID.
func (c *Client) Remove(key string) (store.Record, error) {
	var out store.Record
	return out, c.do(http.MethodDelete, "/api/records/"+url.PathEscape(key), nil, &out)
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach lds at %s (is `lds serve` running? set --server or LDS_URL): %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", names.CleanText(e.Error, 500))
		}
		return fmt.Errorf("%s: %s", resp.Status, names.CleanText(string(data), 200))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
