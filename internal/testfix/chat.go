package testfix

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
)

// Chat stands in for Slack and Discord: every notification the kernel
// sends arrives here, whatever its address, and is recorded with it.
type Chat struct {
	server *httptest.Server
	mu     sync.Mutex
	posts  []ChatPost
	status int
}

// ChatPost is one notification as it arrived.
type ChatPost struct {
	// Url is the address it was sent to.
	Url  string
	Body map[string]any
}

func newChat() *Chat {
	c := &Chat{status: http.StatusOK}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.posts = append(c.posts, ChatPost{Url: r.Header.Get("X-Original-Url"), Body: body})
		status := c.status
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	return c
}

// Client sends every request to the fake chat server.
func (c *Chat) Client() *http.Client {
	target, _ := url.Parse(c.server.URL)
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("X-Original-Url", r.URL.String())
		r.URL.Scheme, r.URL.Host, r.Host = target.Scheme, target.Host, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Posts are the notifications received so far.
func (c *Chat) Posts() []ChatPost {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.posts)
}

// Answer sets the status the fake chat tool answers with.
func (c *Chat) Answer(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = status
}

// PublicUrl is the web app's address the fixture's kernel links to.
const PublicUrl = "https://pgmandate.test"
