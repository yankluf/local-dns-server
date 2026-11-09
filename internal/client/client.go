// Package client is the HTTP client the CLI uses to talk to `localdns serve`.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yankluf/local-dns-server/internal/store"
)

// Client talks to the localdns API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for baseURL.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 15 * time.Second}}
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
		return fmt.Errorf("can't reach localdns at %s (is `localdns serve` running? set --server or LOCALDNS_URL): %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
