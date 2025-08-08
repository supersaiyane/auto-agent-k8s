package slack

import (
    "bytes"
    "encoding/json"
    "net/http"
)

type Client struct{ hook string }
func New(hook string) *Client { return &Client{hook: hook} }
func (c *Client) Post(text string) error {
    if c.hook == "" { return nil }
    body, _ := json.Marshal(map[string]string{"text": text})
    _, err := http.Post(c.hook, "application/json", bytes.NewReader(body))
    return err
}
