package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrowserToolTableUsesCDPEndpoints(t *testing.T) {
	want := map[string]string{
		"browser_status":          "/api/v1/browser/status",
		"browser_navigate":        "/api/v1/browser/navigate",
		"browser_snapshot":        "/api/v1/browser/snapshot",
		"browser_find":            "/api/v1/browser/find",
		"browser_take_screenshot": "/api/v1/browser/screenshot",
		"browser_click":           "/api/v1/browser/click",
		"browser_type":            "/api/v1/browser/type",
		"browser_press_key":       "/api/v1/browser/press_key",
		"browser_scroll":          "/api/v1/browser/scroll",
		"browser_navigate_back":   "/api/v1/browser/back",
		"browser_wait_for":        "/api/v1/browser/wait_for",
		"browser_close":           "/api/v1/browser/close",
		"browser_evaluate":        "/api/v1/browser/evaluate",
	}
	for name, path := range want {
		tool, ok := findTool(name)
		if !ok {
			t.Fatalf("missing browser tool %s", name)
		}
		if err := validateToolSchema(tool); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if tool.path != path {
			t.Errorf("%s path = %q, want %q", name, tool.path, path)
		}
	}
}

func TestBrowserNavigateBuildsCDPRequest(t *testing.T) {
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/browser/navigate" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-token" {
			t.Errorf("missing test token")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": got["url"]})
	}))
	defer ts.Close()

	tool, _ := findTool("browser_navigate")
	blocks, isErr := testMCPServer(ts.URL).invoke(context.Background(), tool, map[string]any{"url": "https://example.com"})
	if isErr {
		t.Fatalf("unexpected error: %s", browserContentText(blocks))
	}
	if got["url"] != "https://example.com" {
		t.Fatalf("navigate body = %#v", got)
	}
}

func TestParseEdgeUnixSocketsAndChooseProcessSocket(t *testing.T) {
	raw := `Num       RefCount Protocol Flags    Type St Inode Path
0000000000000000: 00000002 00000000 00010000 0001 01 1111 @chrome_devtools_remote
0000000000000000: 00000002 00000000 00010000 0001 01 2222 /dev/socket/other
`
	sockets, err := parseEdgeUnixSockets(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(sockets) != 2 {
		t.Fatalf("sockets = %#v", sockets)
	}
	if got := chooseEdgeDevToolsSocket(sockets, map[string]bool{"1111": true}); got != "@chrome_devtools_remote" {
		t.Fatalf("chosen socket = %q", got)
	}
	if got := chooseEdgeDevToolsSocket(sockets, map[string]bool{"2222": true}); got != "@chrome_devtools_remote" {
		t.Fatalf("fallback socket = %q", got)
	}
}

func TestCDPPageSnapshotOverWebSocket(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "done")
		ctx := r.Context()
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Errorf("read CDP request: %v", err)
			return
		}
		if typ != websocket.MessageText {
			t.Errorf("message type = %v", typ)
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &req); err != nil {
			t.Errorf("decode CDP request: %v", err)
			return
		}
		if req.Method != "Runtime.evaluate" {
			t.Errorf("method = %s", req.Method)
		}
		value := map[string]any{
			"title": "Example", "url": "https://example.com", "ready_state": "complete",
			"text": "Hello",
			"elements": []any{map[string]any{
				"ref": "e0", "tag": "button", "name": "Submit", "text": "Submit",
				"x": 10, "y": 20, "width": 80, "height": 30, "center_x": 50, "center_y": 35,
			}},
		}
		resp, _ := json.Marshal(map[string]any{
			"id": req.ID,
			"result": map[string]any{
				"result": map[string]any{"type": "object", "value": value},
			},
		})
		if err := c.Write(ctx, websocket.MessageText, resp); err != nil {
			t.Errorf("write CDP response: %v", err)
		}
	}))
	defer ts.Close()

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(edgeCDPLimit)
	page := &edgeCDPPage{ws: conn}
	defer page.close()

	snap, err := page.snapshot(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Title != "Example" || len(snap.Elements) != 1 || snap.Elements[0].Ref != "e0" {
		t.Fatalf("snapshot = %#v", snap)
	}
	matches := edgeFindSnapshot(snap, "submit")
	if len(matches) != 1 {
		t.Fatalf("find matches = %#v", matches)
	}
}

func TestNormalizeBrowserURL(t *testing.T) {
	got, err := normalizeBrowserURL("example.com")
	if err != nil || got != "https://example.com" {
		t.Fatalf("normalizeBrowserURL = %q, %v", got, err)
	}
	if _, err := normalizeBrowserURL("javascript:alert(1)"); err == nil {
		t.Fatal("javascript: scheme should be rejected")
	}
}

func TestBrowserSnapshotOverAbstractUnixCDP(t *testing.T) {
	socket := fmt.Sprintf("@phoneserver-cdp-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/list":
			writeJSON(w, http.StatusOK, []map[string]any{{
				"id": "page-1", "type": "page", "title": "Example", "url": "https://example.com",
				"webSocketDebuggerUrl": "ws://localhost:9222/devtools/page/page-1",
			}})
		case "/devtools/page/page-1":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept websocket: %v", err)
				return
			}
			defer c.Close(websocket.StatusNormalClosure, "done")
			_, data, err := c.Read(r.Context())
			if err != nil {
				t.Errorf("read CDP request: %v", err)
				return
			}
			var req struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(data, &req)
			resp, _ := json.Marshal(map[string]any{
				"id": req.ID,
				"result": map[string]any{
					"result": map[string]any{"type": "object", "value": map[string]any{
						"title": "Example", "url": "https://example.com", "ready_state": "complete",
						"text": "Hello", "elements": []any{},
					}},
				},
			})
			if err := c.Write(r.Context(), websocket.MessageText, resp); err != nil {
				t.Errorf("write CDP response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = server.Serve(ln) }()
	defer server.Close()

	cfg := defaultConfig()
	cfg.Browser.Socket = socket
	app := &App{cfg: cfg}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/browser/snapshot", nil)
	rec := httptest.NewRecorder()
	app.handleBrowserSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Snapshot edgeSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Snapshot.Title != "Example" {
		t.Fatalf("snapshot = %#v", body.Snapshot)
	}
}

func testMCPServer(baseURL string) *mcpServer {
	cfg := defaultConfig()
	cfg.APIToken = "test" + "-" + "token"
	return &mcpServer{baseURL: baseURL, cfgFn: func() Config { return cfg }}
}

func browserContentText(blocks []contentBlock) string {
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			return block.Text
		}
	}
	return "browser tool returned no text"
}
