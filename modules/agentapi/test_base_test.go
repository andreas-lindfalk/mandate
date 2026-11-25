package agentapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/stretchr/testify/suite"

	"github.com/andreas-lindfalk/pgmandate/internal/testfix"
)

// IntegrationSuite starts a real Postgres with the schema and a kernel
// without a job queue, and serves the agent contract both ways: as JSON
// over HTTP (Http) and over MCP (Mcp). Each test file embeds it in its own
// suite.
type IntegrationSuite struct {
	suite.Suite
	W    *testfix.World
	Http *httptest.Server
	Mcp  *httptest.Server
}

func (s *IntegrationSuite) SetupSuite() {
	w, err := testfix.Start(context.Background(), false)
	s.Require().NoError(err)
	s.W = w
	mux := http.NewServeMux()
	path, handler := w.Kernel.AgentHandler()
	mux.Handle(path, handler)
	s.Http = httptest.NewServer(mux)
	s.Mcp = httptest.NewServer(w.Kernel.Handler())
}

func (s *IntegrationSuite) TearDownSuite() {
	s.Http.Close()
	s.Mcp.Close()
	s.W.Close()
}
