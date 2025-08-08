package llm

import (
    "bytes"
    "encoding/json"
    "net/http"
)

type Client struct {
    url, key, model string
    enabled bool
}
func New(url, key, model string, enabled bool) *Client {
    return &Client{url:url, key:key, model:model, enabled:enabled}
}
func (c *Client) Enabled() bool { return c.enabled && c.url!="" && c.key!="" }

func (c *Client) Diagnose(title, context string) (string, error) {
    if !c.Enabled() { return "", nil }
    payload := map[string]any{
        "model": c.model,
        "messages": []map[string]string{
            {"role":"system","content":"You are an SRE assistant. Be concise and practical."},
            {"role":"user","content": title + "\n" + context},
        },
        "max_tokens": 200,
    }
    b, _ := json.Marshal(payload)
    req, _ := http.NewRequest("POST", c.url, bytes.NewReader(b))
    req.Header.Set("Authorization", "Bearer "+c.key)
    req.Header.Set("Content-Type", "application/json")
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return "", err }
    defer resp.Body.Close()
    var out struct{ Choices []struct{ Message struct{ Content string `json:"content"` } `json:"message"` } `json:"choices"` }
    json.NewDecoder(resp.Body).Decode(&out)
    if len(out.Choices)>0 { return out.Choices[0].Message.Content, nil }
    return "", nil
}
