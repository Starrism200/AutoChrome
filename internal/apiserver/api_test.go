package apiserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"autochrome/internal/jobs"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

func newTestAPI(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AUTOCHROME_HOME", home)
	u, err := users.Add("test")
	if err != nil {
		t.Fatal(err)
	}
	s := New(u, "/bin/true")
	s.Token = "test-token"
	s.AllowScripts = true
	s.AllowEval = true
	s.Manager = session.NewManager(u, "/bin/true")
	s.Jobs = jobs.New(u, s.Manager, true, nil)
	t.Cleanup(s.Jobs.Close)
	mux := http.NewServeMux()
	s.routes(mux)
	ts := httptest.NewServer(s.cors(s.auth(mux)))
	t.Cleanup(ts.Close)
	return s, ts
}

func request(t *testing.T, ts *httptest.Server, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func TestAuthHealthAndOpenAPI(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := request(t, ts, http.MethodGet, "/api/v1/health", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	resp, _ = request(t, ts, http.MethodGet, "/api/v1/user", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized user = %d", resp.StatusCode)
	}
	resp, _ = request(t, ts, http.MethodGet, "/api/v1/user", "test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user = %d", resp.StatusCode)
	}
	resp, data := request(t, ts, http.MethodGet, "/api/v1/openapi.yaml", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "openapi:") {
		t.Fatalf("openapi = %d %q", resp.StatusCode, data)
	}
}

func TestProxyCRUD(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, data := request(t, ts, http.MethodPost, "/api/v1/proxies", "test-token", map[string]any{
		"name": "local", "url": "socks5://127.0.0.1:1080",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add proxy = %d %s", resp.StatusCode, data)
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &p); err != nil || p.ID == "" {
		t.Fatalf("proxy id: %v %s", err, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/proxies", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), p.ID) {
		t.Fatalf("list proxies = %d %s", resp.StatusCode, data)
	}
	resp, _ = request(t, ts, http.MethodPatch, "/api/v1/proxies/"+p.ID, "test-token", map[string]any{"name": "renamed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update proxy = %d", resp.StatusCode)
	}
	resp, _ = request(t, ts, http.MethodDelete, "/api/v1/proxies/"+p.ID, "test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete proxy = %d", resp.StatusCode)
	}
}

func TestAttachmentUploadDownload(t *testing.T) {
	_, ts := newTestAPI(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/attachments", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d %s", resp.StatusCode, data)
	}
	var f struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &f); err != nil || f.ID == "" {
		t.Fatalf("attachment id: %v %s", err, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/attachments/"+f.ID+"/download", "test-token", nil)
	if resp.StatusCode != http.StatusOK || string(data) != "hello" {
		t.Fatalf("download = %d %q", resp.StatusCode, data)
	}
}

func TestEnvCreateDuplicateAndStorage(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := request(t, ts, http.MethodPost, "/api/v1/envs", "test-token", map[string]any{"id": "e1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create env = %d", resp.StatusCode)
	}
	resp, _ = request(t, ts, http.MethodPost, "/api/v1/envs", "test-token", map[string]any{"id": "e1"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate env = %d", resp.StatusCode)
	}
	resp, data := request(t, ts, http.MethodGet, "/api/v1/envs/e1", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"id":"e1"`) {
		t.Fatalf("get env = %d %s", resp.StatusCode, data)
	}

	resp, data = request(t, ts, http.MethodPut, "/api/v1/storage", "test-token", map[string]any{
		"env_root": "envs2", "attachment_root": "att2",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("storage = %d %s", resp.StatusCode, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/storage", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "/envs2") {
		t.Fatalf("get storage = %d %s", resp.StatusCode, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/envs/e1", "test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("old env after storage change = %d %s", resp.StatusCode, data)
	}
	resp, _ = request(t, ts, http.MethodPost, "/api/v1/envs", "test-token", map[string]any{"id": "e2"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create env after storage change = %d", resp.StatusCode)
	}
}

func TestTokenRotate(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, data := request(t, ts, http.MethodPost, "/api/v1/token/rotate", "test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d %s", resp.StatusCode, data)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		t.Fatalf("token: %v %s", err, data)
	}
	if out.Token == "test-token" {
		t.Fatal("token did not change")
	}
	resp, _ = request(t, ts, http.MethodGet, "/api/v1/user", "test-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token = %d", resp.StatusCode)
	}
	resp, _ = request(t, ts, http.MethodGet, "/api/v1/user", out.Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new token = %d", resp.StatusCode)
	}
}

func TestSchedulerCRUDAndRun(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, data := request(t, ts, http.MethodPost, "/api/v1/schedules", "test-token", map[string]any{
		"type": "close", "interval_seconds": 5, "enabled": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add schedule = %d %s", resp.StatusCode, data)
	}
	var j struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &j); err != nil || j.ID == "" {
		t.Fatalf("schedule id: %v %s", err, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/schedules", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), j.ID) {
		t.Fatalf("list schedules = %d %s", resp.StatusCode, data)
	}
	resp, _ = request(t, ts, http.MethodPatch, "/api/v1/schedules/"+j.ID, "test-token", map[string]any{"name": "renamed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update schedule = %d", resp.StatusCode)
	}
	resp, data = request(t, ts, http.MethodPost, "/api/v1/schedules/"+j.ID+"/run", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"status":"ok"`) {
		t.Fatalf("run schedule = %d %s", resp.StatusCode, data)
	}
	resp, _ = request(t, ts, http.MethodDelete, "/api/v1/schedules/"+j.ID, "test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete schedule = %d", resp.StatusCode)
	}
}

func TestEventsStream(t *testing.T) {
	_, ts := newTestAPI(t)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("events = %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	lines := make(chan string, 16)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- line
		}
	}()
	request(t, ts, http.MethodPost, "/api/v1/proxies", "test-token", map[string]any{
		"name": "event", "url": "socks5://127.0.0.1:1080",
	})
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("event stream closed")
			}
			if strings.Contains(line, "proxy.created") {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for proxy.created event")
		}
	}
}

func TestContainerNameAndUpdate(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, data := request(t, ts, http.MethodPost, "/api/v1/containers", "test-token", map[string]any{
		"id": "c1", "name": "alpha", "description": "first",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create container = %d %s", resp.StatusCode, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/containers/alpha", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"name":"alpha"`) {
		t.Fatalf("get container = %d %s", resp.StatusCode, data)
	}
	resp, data = request(t, ts, http.MethodPatch, "/api/v1/containers/alpha", "test-token", map[string]any{
		"name": "beta", "description": "updated",
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"name":"beta"`) {
		t.Fatalf("update container = %d %s", resp.StatusCode, data)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/containers/beta", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"description":"updated"`) {
		t.Fatalf("get renamed container = %d %s", resp.StatusCode, data)
	}
	resp, _ = request(t, ts, http.MethodGet, "/api/v1/containers/alpha", "test-token", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("old name = %d", resp.StatusCode)
	}
	resp, _ = request(t, ts, http.MethodPost, "/api/v1/containers", "test-token", map[string]any{
		"id": "c2", "name": "beta",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate name = %d", resp.StatusCode)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/containers/beta", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"id":"c1"`) {
		t.Fatalf("get by name = %d %s", resp.StatusCode, data)
	}
	resp, _ = request(t, ts, http.MethodDelete, "/api/v1/containers/beta", "test-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete container = %d", resp.StatusCode)
	}
}

func TestAsyncScriptJobs(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, data := request(t, ts, http.MethodPut, "/api/v1/scripts/jobdemo", "test-token", map[string]any{
		"source":  "console.log('hello'); return {ok: true, n: input.n};",
		"timeout": "10s",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write script = %d %s", resp.StatusCode, data)
	}
	resp, data = request(t, ts, http.MethodPost, "/api/v1/scripts/jobdemo/jobs", "test-token", map[string]any{
		"input": map[string]any{"n": 7},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start job = %d %s", resp.StatusCode, data)
	}
	var job struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &job); err != nil || job.ID == "" {
		t.Fatalf("job id: %v %s", err, data)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, data = request(t, ts, http.MethodGet, "/api/v1/jobs/"+job.ID, "test-token", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get job = %d %s", resp.StatusCode, data)
		}
		var current struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(data, &current); err != nil {
			t.Fatal(err)
		}
		if current.Status == "succeeded" {
			break
		}
		if current.Status == "failed" || current.Status == "cancelled" {
			t.Fatalf("job status = %s %s", current.Status, data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job timeout: %s", data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	resp, data = request(t, ts, http.MethodGet, "/api/v1/jobs/"+job.ID+"/logs", "test-token", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "finished: succeeded") {
		t.Fatalf("logs = %d %s", resp.StatusCode, data)
	}
}

func TestContainerSize(t *testing.T) {
	_, ts := newTestAPI(t)
	resp, _ := request(t, ts, http.MethodPost, "/api/v1/containers", "test-token", map[string]any{"id": "sized"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	resp, data := request(t, ts, http.MethodPut, "/api/v1/containers/sized/size", "test-token", map[string]any{
		"width": 1920, "height": 1080, "restart": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("size = %d %s", resp.StatusCode, data)
	}
	var out struct {
		Env struct {
			Fingerprint struct {
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"fingerprint"`
		} `json:"env"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Env.Fingerprint.Width != 1920 || out.Env.Fingerprint.Height != 1080 {
		t.Fatalf("size = %+v", out)
	}
}
