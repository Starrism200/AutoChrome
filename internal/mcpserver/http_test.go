package mcpserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"autochrome/internal/jobs"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

func TestStreamableHTTPHandler(t *testing.T) {
	t.Setenv("AUTOCHROME_HOME", t.TempDir())
	u, err := users.Add("mcp")
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(u, "/bin/true")
	jm := jobs.New(u, mgr, true, nil)
	defer jm.Close()

	s := New(u, "/bin/true")
	s.Manager = mgr
	s.Jobs = jm
	s.AllowEval = true
	s.AllowScripts = true
	s.Build()

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"script_job_start", "script_job_status", "script_job_logs", "script_job_cancel", "script_job_list", "browser_open"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "script_job_list", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("script_job_list error: %+v", res.Content)
	}
}
