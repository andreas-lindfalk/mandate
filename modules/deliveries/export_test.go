package deliveries

import "net/http"

// WrapTransport lets a test stand between the service and each call it
// makes.
func WrapTransport(s *Service, wrap func(http.RoundTripper) http.RoundTripper) {
	s.client.Transport = wrap(s.client.Transport)
}
