package main

// Browser automation for Microsoft Edge on Android.
//
// Edge exposes the normal Chromium DevTools protocol (CDP) over a root-only
// abstract Unix socket. phoneserver connects to that socket directly with an
// http.Transport DialContext hook, so there is no TCP listener or raw CDP port
// exposed to the LAN. The REST and MCP surfaces stay behind phoneserver's normal
// API token and read-only gate.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	androidEdgePackage = "com.microsoft.emmx"
	edgeCDPLimit       = 96 << 20
	edgeDefaultTimeout = 20 * time.Second
)

var errEdgeUnavailable = errors.New("Microsoft Edge DevTools socket is unavailable")

const browserDisabledMessage = "Edge browser tools are disabled. Set browser.enabled=true in config.json and restart phoneserver."

type edgeUnixSocket struct {
	inode string
	path  string
}

type edgeTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	Description          string `json:"description"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type edgeVersion struct {
	Browser         string `json:"Browser"`
	ProtocolVersion string `json:"Protocol-Version"`
	UserAgent       string `json:"User-Agent"`
	V8Version       string `json:"V8-Version"`
}

type edgeCDPPage struct {
	target edgeTarget
	socket string
	ws     *websocket.Conn
	mu     sync.Mutex
	nextID int64
}

type edgeSnapshotElement struct {
	Ref      string  `json:"ref"`
	Tag      string  `json:"tag"`
	Role     string  `json:"role,omitempty"`
	Name     string  `json:"name,omitempty"`
	Text     string  `json:"text,omitempty"`
	Value    string  `json:"value,omitempty"`
	Type     string  `json:"type,omitempty"`
	Href     string  `json:"href,omitempty"`
	Disabled bool    `json:"disabled,omitempty"`
	Checked  any     `json:"checked,omitempty"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Width    float64 `json:"width"`
	Height   float64 `json:"height"`
	CenterX  float64 `json:"center_x"`
	CenterY  float64 `json:"center_y"`
}

type edgeSnapshot struct {
	Title      string                `json:"title"`
	URL        string                `json:"url"`
	ReadyState string                `json:"ready_state"`
	Text       string                `json:"text"`
	Elements   []edgeSnapshotElement `json:"elements"`
	Truncated  bool                  `json:"truncated"`
	Viewport   map[string]float64    `json:"viewport,omitempty"`
}

type edgeElementTarget struct {
	Found  bool    `json:"found"`
	Ref    string  `json:"ref,omitempty"`
	Tag    string  `json:"tag,omitempty"`
	Label  string  `json:"label,omitempty"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type edgePageInfo struct {
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	ReadyState string  `json:"ready_state"`
	Width      float64 `json:"width"`
	Height     float64 `json:"height"`
}

type cdpCallError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

type cdpReply struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *cdpCallError   `json:"error"`
}

type cdpEvalResult struct {
	Result struct {
		Type                string          `json:"type"`
		Value               json.RawMessage `json:"value"`
		Description         string          `json:"description"`
		UnserializableValue string          `json:"unserializableValue"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text      string `json:"text"`
		Exception *struct {
			Description string `json:"description"`
		} `json:"exception"`
	} `json:"exceptionDetails"`
}

func normalizeUnixSocket(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, `\x00`):
		return "\x00" + strings.TrimPrefix(s, `\x00`)
	case strings.HasPrefix(s, "@"), strings.HasPrefix(s, "\x00"), strings.HasPrefix(s, "/"):
		return s
	case s != "":
		return "@" + s
	default:
		return s
	}
}

func parseEdgeUnixSockets(r io.Reader) ([]edgeUnixSocket, error) {
	data, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		return nil, err
	}
	var out []edgeUnixSocket
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[0] == "Num" {
			continue
		}
		path := normalizeUnixSocket(fields[7])
		if path == "" {
			continue
		}
		out = append(out, edgeUnixSocket{inode: fields[6], path: path})
	}
	return out, nil
}

func readEdgeUnixSockets() ([]edgeUnixSocket, error) {
	f, err := os.Open("/proc/net/unix")
	if err != nil {
		return nil, fmt.Errorf("read /proc/net/unix: %w", err)
	}
	defer f.Close()
	return parseEdgeUnixSockets(f)
}

func edgeProcessIDs(ctx context.Context) []int {
	var seen = map[int]bool{}
	var out []int
	add := func(raw string) {
		for _, f := range strings.Fields(raw) {
			pid, err := strconv.Atoi(f)
			if err == nil && pid > 0 && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
	}
	if p, err := exec.LookPath("pidof"); err == nil {
		if b, err := exec.CommandContext(ctx, p, androidEdgePackage).Output(); err == nil {
			add(string(b))
		}
	}
	if len(out) > 0 {
		return out
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(cmdline, androidEdgePackage) {
			add(e.Name())
		}
	}
	return out
}

func edgeSocketInodes(pid int) map[string]bool {
	out := map[string]bool{}
	fdDir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		link, err := os.Readlink(fdDir + "/" + e.Name())
		if err != nil {
			continue
		}
		if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
			continue
		}
		out[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
	}
	return out
}

func chooseEdgeDevToolsSocket(sockets []edgeUnixSocket, pidInodes map[string]bool) string {
	rank := func(path string) (int, bool) {
		lower := strings.ToLower(path)
		if !strings.Contains(lower, "devtools_remote") && !strings.Contains(lower, "devtools") {
			return 0, false
		}
		switch {
		case strings.Contains(lower, "emmx"), strings.Contains(lower, "edge"):
			return 0, true
		case lower == "@chrome_devtools_remote", lower == "chrome_devtools_remote":
			return 1, true
		case strings.Contains(lower, "webview_devtools_remote"):
			return 3, true
		default:
			return 2, true
		}
	}

	var best edgeUnixSocket
	bestRank := 100
	found := false
	sort.SliceStable(sockets, func(i, j int) bool { return sockets[i].path < sockets[j].path })
	for _, s := range sockets {
		r, ok := rank(s.path)
		if !ok {
			continue
		}
		if len(pidInodes) > 0 && !pidInodes[s.inode] {
			continue
		}
		if !found || r < bestRank {
			best, bestRank, found = s, r, true
		}
	}
	if found {
		return best.path
	}

	// Process ownership can disappear between pidof and fd inspection. Fall
	// back to the conventional names instead of making the browser unavailable.
	for _, s := range sockets {
		if r, ok := rank(s.path); ok && (r <= 1 || strings.Contains(strings.ToLower(s.path), "devtools_remote")) {
			return s.path
		}
	}
	return ""
}

func discoverEdgeDevToolsSocket(ctx context.Context, override string) (string, error) {
	if s := normalizeUnixSocket(override); s != "" {
		return s, nil
	}
	sockets, err := readEdgeUnixSockets()
	if err != nil {
		return "", err
	}
	pidInodes := map[string]bool{}
	for _, pid := range edgeProcessIDs(ctx) {
		for inode := range edgeSocketInodes(pid) {
			pidInodes[inode] = true
		}
	}
	if s := chooseEdgeDevToolsSocket(sockets, pidInodes); s != "" {
		return s, nil
	}
	if s := chooseEdgeDevToolsSocket(sockets, nil); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("%w: no devtools_remote socket for %s in /proc/net/unix; open Edge once and confirm phoneserver runs as root", errEdgeUnavailable, androidEdgePackage)
}

func edgeHTTPClient(socket string) *http.Client {
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	addr := normalizeUnixSocket(socket)
	tr := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", addr)
		},
	}
	return &http.Client{
		Transport: tr,
	}
}

func edgeGetJSON(ctx context.Context, client *http.Client, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:9222"+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("CDP HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	return dec.Decode(dst)
}

func edgeTargets(ctx context.Context, client *http.Client) ([]edgeTarget, error) {
	var targets []edgeTarget
	if err := edgeGetJSON(ctx, client, "/json/list", &targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func edgeVersionInfo(ctx context.Context, client *http.Client) (edgeVersion, error) {
	var v edgeVersion
	err := edgeGetJSON(ctx, client, "/json/version", &v)
	return v, err
}

func chooseEdgePageTarget(targets []edgeTarget) (edgeTarget, bool) {
	var fallback *edgeTarget
	for i := range targets {
		t := targets[i]
		if t.Type != "page" || strings.TrimSpace(t.WebSocketDebuggerURL) == "" {
			continue
		}
		lower := strings.ToLower(t.URL)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return t, true
		}
		if fallback == nil {
			cp := t
			fallback = &cp
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return edgeTarget{}, false
}

func dialEdgePage(ctx context.Context, client *http.Client, socket string, target edgeTarget) (*edgeCDPPage, error) {
	wsURL, err := url.Parse(target.WebSocketDebuggerURL)
	if err != nil || wsURL.Path == "" {
		return nil, fmt.Errorf("invalid CDP WebSocket URL for target %s", target.ID)
	}
	// /json/list normally returns ws://127.0.0.1:9222/...; the host is
	// meaningless because the transport always dials the Unix socket.
	wsURL.Scheme = "ws"
	wsURL.Host = "localhost:9222"
	conn, resp, err := websocket.Dial(ctx, wsURL.String(), &websocket.DialOptions{HTTPClient: client})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect CDP target %s: %w", target.ID, err)
	}
	conn.SetReadLimit(edgeCDPLimit)
	return &edgeCDPPage{target: target, socket: socket, ws: conn}, nil
}

func (a *App) openEdgePage(ctx context.Context) (*edgeCDPPage, error) {
	socket, err := discoverEdgeDevToolsSocket(ctx, a.config().Browser.Socket)
	if err != nil {
		return nil, err
	}
	client := edgeHTTPClient(socket)
	targets, err := edgeTargets(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("%w: connect %s: %v", errEdgeUnavailable, socket, err)
	}
	target, ok := chooseEdgePageTarget(targets)
	if !ok {
		return nil, fmt.Errorf("%w: Edge is running but has no page target", errEdgeUnavailable)
	}
	return dialEdgePage(ctx, client, socket, target)
}

func (p *edgeCDPPage) close() {
	if p == nil || p.ws == nil {
		return
	}
	_ = p.ws.Close(websocket.StatusNormalClosure, "done")
}

func (p *edgeCDPPage) call(ctx context.Context, method string, params any, dst any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nextID == 0 {
		p.nextID = 1
	}
	id := p.nextID
	p.nextID++
	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if err := p.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("CDP write %s: %w", method, err)
	}
	for {
		typ, data, err := p.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("CDP read %s: %w", method, err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var reply cdpReply
		if json.Unmarshal(data, &reply) != nil || reply.ID == 0 {
			continue // event or unrelated message
		}
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			msg := strings.TrimSpace(reply.Error.Message)
			if msg == "" {
				msg = "unknown CDP error"
			}
			return fmt.Errorf("CDP %s: %s", method, msg)
		}
		if dst != nil && len(reply.Result) > 0 {
			if err := json.Unmarshal(reply.Result, dst); err != nil {
				return fmt.Errorf("decode CDP %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (p *edgeCDPPage) evaluate(ctx context.Context, expression string, awaitPromise bool) (json.RawMessage, string, error) {
	var out cdpEvalResult
	err := p.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	}, &out)
	if err != nil {
		return nil, "", err
	}
	if out.ExceptionDetails != nil {
		msg := strings.TrimSpace(out.ExceptionDetails.Text)
		if out.ExceptionDetails.Exception != nil && strings.TrimSpace(out.ExceptionDetails.Exception.Description) != "" {
			msg = strings.TrimSpace(out.ExceptionDetails.Exception.Description)
		}
		if msg == "" {
			msg = "JavaScript exception"
		}
		return nil, "", errors.New(msg)
	}
	value := out.Result.Value
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	return value, out.Result.Type, nil
}

func (p *edgeCDPPage) evaluateInto(ctx context.Context, expression string, dst any) error {
	raw, _, err := p.evaluate(ctx, expression, false)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

func (p *edgeCDPPage) pageInfo(ctx context.Context) (edgePageInfo, error) {
	var info edgePageInfo
	err := p.evaluateInto(ctx, `({title:document.title,url:location.href,ready_state:document.readyState,width:innerWidth,height:innerHeight})`, &info)
	return info, err
}

func edgeSnapshotJS(maxNodes int) string {
	if maxNodes <= 0 {
		maxNodes = 300
	}
	if maxNodes > 2000 {
		maxNodes = 2000
	}
	return fmt.Sprintf(`(() => {
  const selector = 'a[href],button,input,textarea,select,summary,[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="radio"],[role="combobox"],[role="menuitem"],[role="tab"],[contenteditable="true"],[tabindex]:not([tabindex="-1"])';
  const clean = (s, n) => (s || '').replace(/\s+/g, ' ').trim().slice(0, n);
  const visible = (el) => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden' || Number(st.opacity) === 0) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && r.bottom >= 0 && r.right >= 0 && r.top <= innerHeight * 4 && r.left <= innerWidth * 4;
  };
  document.querySelectorAll('[data-phoneserver-ref]').forEach((el) => el.removeAttribute('data-phoneserver-ref'));
  const all = [...document.querySelectorAll(selector)].filter(visible);
  const limited = all.slice(0, %d);
  const elements = limited.map((el, i) => {
    const ref = 'e' + i;
    el.setAttribute('data-phoneserver-ref', ref);
    const r = el.getBoundingClientRect();
    const label = clean(el.getAttribute('aria-label') || el.innerText || el.value || el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('name'), 240);
    return {
      ref,
      tag: (el.tagName || '').toLowerCase(),
      role: clean(el.getAttribute('role'), 80),
      name: clean(el.getAttribute('aria-label') || el.getAttribute('name') || el.getAttribute('placeholder') || el.getAttribute('title'), 240),
      text: clean(el.innerText || '', 400),
      value: clean(el.value || '', 240),
      type: clean(el.getAttribute('type'), 80),
      href: clean(el.href || '', 800),
      disabled: !!el.disabled || el.getAttribute('aria-disabled') === 'true',
      checked: (el.type === 'checkbox' || el.type === 'radio') ? !!el.checked : undefined,
      x: r.x, y: r.y, width: r.width, height: r.height,
      center_x: r.x + r.width / 2, center_y: r.y + r.height / 2
    };
  });
  return {
    title: document.title,
    url: location.href,
    ready_state: document.readyState,
    text: clean(document.body ? document.body.innerText : '', 16000),
    elements,
    truncated: all.length > elements.length,
    viewport: {width: innerWidth, height: innerHeight, scroll_x: scrollX, scroll_y: scrollY}
  };
})()`, maxNodes)
}

func (p *edgeCDPPage) snapshot(ctx context.Context, maxNodes int) (edgeSnapshot, error) {
	var snap edgeSnapshot
	err := p.evaluateInto(ctx, edgeSnapshotJS(maxNodes), &snap)
	return snap, err
}

func edgeTargetJS(target string) string {
	raw, _ := json.Marshal(target)
	return fmt.Sprintf(`(() => {
  const wanted = %s;
  const clean = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const selector = 'a[href],button,input,textarea,select,summary,[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="radio"],[role="combobox"],[role="menuitem"],[role="tab"],[contenteditable="true"],[tabindex]:not([tabindex="-1"])';
  let el = null;
  if (/^e\d+$/.test(wanted)) {
    el = document.querySelector('[data-phoneserver-ref="' + wanted + '"]');
  } else {
    const nodes = [...document.querySelectorAll(selector)];
    el = nodes.find((candidate) => {
      const st = getComputedStyle(candidate);
      if (st.display === 'none' || st.visibility === 'hidden') return false;
      const r = candidate.getBoundingClientRect();
      if (r.width <= 0 || r.height <= 0) return false;
      const label = clean(candidate.getAttribute('aria-label') || candidate.innerText || candidate.value || candidate.getAttribute('placeholder') || candidate.getAttribute('title') || candidate.getAttribute('name'));
      return label.includes(wanted);
    }) || null;
  }
  if (!el) return {found:false};
  el.scrollIntoView({block:'center', inline:'center', behavior:'instant'});
  const r = el.getBoundingClientRect();
  return {
    found: r.width > 0 && r.height > 0,
    ref: el.getAttribute('data-phoneserver-ref') || '',
    tag: (el.tagName || '').toLowerCase(),
    label: clean(el.getAttribute('aria-label') || el.innerText || el.value || el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('name')).slice(0, 240),
    x: r.x, y: r.y, width: r.width, height: r.height
  };
})()`, string(raw))
}

func parseBrowserCoordinate(target string) (float64, float64, bool) {
	parts := strings.Split(target, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	x, errX := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	y, errY := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	return x, y, errX == nil && errY == nil
}

func (p *edgeCDPPage) resolveTarget(ctx context.Context, target string) (edgeElementTarget, error) {
	if x, y, ok := parseBrowserCoordinate(target); ok {
		return edgeElementTarget{Found: true, Label: target, X: x, Y: y}, nil
	}
	if strings.TrimSpace(target) == "" {
		return edgeElementTarget{}, errors.New("target is required")
	}
	var resolved edgeElementTarget
	if err := p.evaluateInto(ctx, edgeTargetJS(strings.TrimSpace(target)), &resolved); err != nil {
		return resolved, err
	}
	if !resolved.Found {
		return resolved, fmt.Errorf("page element %q was not found; call phone_browser_snapshot first", target)
	}
	return resolved, nil
}

func (p *edgeCDPPage) clickAt(ctx context.Context, x, y float64, double bool) error {
	press := func(count int) error {
		if err := p.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": count, "pointerType": "mouse",
		}, nil); err != nil {
			return err
		}
		return p.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseReleased", "x": x, "y": y, "button": "left", "buttons": 0, "clickCount": count, "pointerType": "mouse",
		}, nil)
	}
	if double {
		if err := press(1); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
		return press(2)
	}
	return press(1)
}

func (p *edgeCDPPage) focusTarget(ctx context.Context, target string) error {
	raw, _ := json.Marshal(strings.TrimSpace(target))
	expr := fmt.Sprintf(`(() => {
  const wanted = %s;
  const clean = (s) => (s || '').replace(/\s+/g, ' ').trim();
  const selector = 'a[href],button,input,textarea,select,summary,[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="radio"],[role="combobox"],[contenteditable="true"],[tabindex]:not([tabindex="-1"])';
  let el = null;
  if (/^e\d+$/.test(wanted)) el = document.querySelector('[data-phoneserver-ref="' + wanted + '"]');
  else el = [...document.querySelectorAll(selector)].find((candidate) => clean(candidate.getAttribute('aria-label') || candidate.innerText || candidate.value || candidate.getAttribute('placeholder') || candidate.getAttribute('title') || candidate.getAttribute('name')).includes(wanted)) || null;
  if (!el) return false;
  el.focus({preventScroll:false});
  return document.activeElement === el || el.contains(document.activeElement);
})()`, string(raw))
	value, _, err := p.evaluate(ctx, expr, false)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(value)) != "true" {
		return fmt.Errorf("page element %q could not be focused", target)
	}
	return nil
}

type edgeKeyDef struct {
	key  string
	code string
	vk   int
	text string
}

func edgeKey(key string) (edgeKeyDef, bool) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "enter", "return":
		return edgeKeyDef{"Enter", "Enter", 13, "\r"}, true
	case "tab":
		return edgeKeyDef{"Tab", "Tab", 9, ""}, true
	case "escape", "esc":
		return edgeKeyDef{"Escape", "Escape", 27, ""}, true
	case "backspace":
		return edgeKeyDef{"Backspace", "Backspace", 8, ""}, true
	case "delete", "del":
		return edgeKeyDef{"Delete", "Delete", 46, ""}, true
	case "arrowleft", "left":
		return edgeKeyDef{"ArrowLeft", "ArrowLeft", 37, ""}, true
	case "arrowright", "right":
		return edgeKeyDef{"ArrowRight", "ArrowRight", 39, ""}, true
	case "arrowup", "up":
		return edgeKeyDef{"ArrowUp", "ArrowUp", 38, ""}, true
	case "arrowdown", "down":
		return edgeKeyDef{"ArrowDown", "ArrowDown", 40, ""}, true
	case "pageup":
		return edgeKeyDef{"PageUp", "PageUp", 33, ""}, true
	case "pagedown":
		return edgeKeyDef{"PageDown", "PageDown", 34, ""}, true
	case "home":
		return edgeKeyDef{"Home", "Home", 36, ""}, true
	case "end":
		return edgeKeyDef{"End", "End", 35, ""}, true
	case "space", "spacebar":
		return edgeKeyDef{" ", "Space", 32, " "}, true
	default:
		return edgeKeyDef{}, false
	}
}

func (p *edgeCDPPage) pressKey(ctx context.Context, key string) error {
	if utf8.RuneCountInString(key) == 1 {
		return p.call(ctx, "Input.insertText", map[string]any{"text": key}, nil)
	}
	def, ok := edgeKey(key)
	if !ok {
		return fmt.Errorf("unsupported browser key %q", key)
	}
	typ := "rawKeyDown"
	if def.text != "" {
		typ = "keyDown"
	}
	if err := p.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type":                  typ,
		"key":                   def.key,
		"code":                  def.code,
		"windowsVirtualKeyCode": def.vk,
		"nativeVirtualKeyCode":  def.vk,
		"text":                  def.text,
	}, nil); err != nil {
		return err
	}
	return p.call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type":                  "keyUp",
		"key":                   def.key,
		"code":                  def.code,
		"windowsVirtualKeyCode": def.vk,
		"nativeVirtualKeyCode":  def.vk,
	}, nil)
}

func (p *edgeCDPPage) waitReady(ctx context.Context, timeout time.Duration) (edgePageInfo, error) {
	deadline := time.Now().Add(timeout)
	var last edgePageInfo
	var lastErr error
	for {
		info, err := p.pageInfo(ctx)
		if err == nil {
			last = info
			if info.ReadyState == "complete" || info.ReadyState == "interactive" {
				return info, nil
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return last, lastErr
			}
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (a *App) launchEdgeURL(ctx context.Context, rawURL string) error {
	out, err := a.runCmd(ctx, 30*time.Second, "/system/bin/am", "start", "--user", "0",
		"-a", "android.intent.action.VIEW", "-d", rawURL, "-p", androidEdgePackage)
	if err != nil {
		return fmt.Errorf("launch Edge: %v: %s", err, firstLine(out))
	}
	return nil
}

func (a *App) openEdgePageAfterLaunch(ctx context.Context) (*edgeCDPPage, error) {
	deadline := time.Now().Add(12 * time.Second)
	var lastErr error
	for {
		page, err := a.openEdgePage(ctx)
		if err == nil {
			return page, nil
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(350 * time.Millisecond):
		}
	}
}

func normalizeBrowserURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New(`"url" is required`)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		raw = "https://" + raw
		u, err = url.Parse(raw)
		if err != nil {
			return "", err
		}
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "file", "about", "data", "edge":
		return u.String(), nil
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
}

func writeBrowserError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	if errors.Is(err, errEdgeUnavailable) {
		code = http.StatusServiceUnavailable
	}
	writeErr(w, code, err.Error())
}

func browserTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), edgeDefaultTimeout)
}

func (a *App) browserGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.config().Browser.Enabled {
			writeErr(w, http.StatusForbidden, browserDisabledMessage)
			return
		}
		h(w, r)
	}
}

func (a *App) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := browserTimeout(r)
	defer cancel()
	cfg := a.config()
	socket, err := discoverEdgeDevToolsSocket(ctx, cfg.Browser.Socket)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	client := edgeHTTPClient(socket)
	version, _ := edgeVersionInfo(ctx, client)
	targets, targetErr := edgeTargets(ctx, client)
	out := map[string]any{
		"ok":            true,
		"socket":        socket,
		"package":       androidEdgePackage,
		"transport":     "abstract unix socket (no TCP/LAN exposure)",
		"root_required": true,
		"version":       version,
		"page_count":    countEdgePages(targets),
		"targets":       redactEdgeTargets(targets),
		"cdp_ready":     targetErr == nil,
	}
	if targetErr != nil {
		out["target_error"] = targetErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

func countEdgePages(targets []edgeTarget) int {
	n := 0
	for _, t := range targets {
		if t.Type == "page" {
			n++
		}
	}
	return n
}

func redactEdgeTargets(targets []edgeTarget) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		out = append(out, map[string]any{
			"id": t.ID, "type": t.Type, "title": t.Title, "url": t.URL,
		})
	}
	return out
}

func (a *App) handleBrowserNavigate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := readJSONBody(r, &body, 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	targetURL, err := normalizeBrowserURL(body.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	page, err := a.openEdgePage(ctx)
	launched := false
	if err != nil {
		if launchErr := a.launchEdgeURL(ctx, targetURL); launchErr != nil {
			writeBrowserError(w, fmt.Errorf("%v; %w", err, launchErr))
			return
		}
		launched = true
		page, err = a.openEdgePageAfterLaunch(ctx)
		if err != nil {
			writeBrowserError(w, err)
			return
		}
	}
	defer page.close()

	if err := page.call(ctx, "Page.enable", nil, nil); err != nil {
		writeBrowserError(w, err)
		return
	}
	var nav struct {
		FrameID   string `json:"frameId"`
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := page.call(ctx, "Page.navigate", map[string]any{"url": targetURL}, &nav); err != nil {
		writeBrowserError(w, err)
		return
	}
	if nav.ErrorText != "" {
		writeErr(w, http.StatusBadGateway, "CDP Page.navigate: "+nav.ErrorText)
		return
	}
	time.Sleep(150 * time.Millisecond)
	info, _ := page.waitReady(ctx, 12*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "requested_url": targetURL, "url": info.URL, "title": info.Title,
		"ready_state": info.ReadyState, "target_id": page.target.ID, "socket": page.socket,
		"launched": launched,
	})
}

func (a *App) handleBrowserSnapshot(w http.ResponseWriter, r *http.Request) {
	maxNodes, _ := strconv.Atoi(r.URL.Query().Get("max_nodes"))
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	snap, err := page.snapshot(ctx, maxNodes)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "target_id": page.target.ID, "snapshot": snap})
}

func (a *App) handleBrowserFind(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	if text == "" {
		writeErr(w, http.StatusBadRequest, `"text" is required`)
		return
	}
	maxNodes, _ := strconv.Atoi(r.URL.Query().Get("max_nodes"))
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	snap, err := page.snapshot(ctx, maxNodes)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	matches := edgeFindSnapshot(snap, text)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "text": text, "match_count": len(matches), "matches": matches,
		"text_match": strings.Contains(strings.ToLower(snap.Text), strings.ToLower(text)),
	})
}

func edgeFindSnapshot(snap edgeSnapshot, text string) []edgeSnapshotElement {
	needle := strings.ToLower(strings.TrimSpace(text))
	var out []edgeSnapshotElement
	for _, el := range snap.Elements {
		haystack := strings.ToLower(strings.Join([]string{el.Name, el.Text, el.Value, el.Href, el.Role, el.Tag}, " "))
		if strings.Contains(haystack, needle) {
			out = append(out, el)
		}
	}
	return out
}

func (a *App) handleBrowserClick(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target      string  `json:"target"`
		X           float64 `json:"x"`
		Y           float64 `json:"y"`
		DoubleClick bool    `json:"doubleClick"`
	}
	if err := readJSONBody(r, &body, 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Target == "" && body.X == 0 && body.Y == 0 {
		writeErr(w, http.StatusBadRequest, `give "target" or x/y`)
		return
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	resolved := edgeElementTarget{Found: true, X: body.X, Y: body.Y}
	if body.Target != "" {
		resolved, err = page.resolveTarget(ctx, body.Target)
		if err != nil {
			writeBrowserError(w, err)
			return
		}
	}
	x := resolved.X + resolved.Width/2
	y := resolved.Y + resolved.Height/2
	if resolved.Width == 0 && resolved.Height == 0 {
		x, y = resolved.X, resolved.Y
	}
	if err := page.clickAt(ctx, x, y, body.DoubleClick); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "clicked": map[string]any{"x": x, "y": y, "target": resolved}})
}

func (a *App) handleBrowserType(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Target string `json:"target"`
		Submit bool   `json:"submit"`
	}
	if err := readJSONBody(r, &body, 2<<20); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, `"text" is required`)
		return
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	if body.Target != "" {
		if err := page.focusTarget(ctx, body.Target); err != nil {
			writeBrowserError(w, err)
			return
		}
	}
	if err := page.call(ctx, "Input.insertText", map[string]any{"text": body.Text}, nil); err != nil {
		writeBrowserError(w, err)
		return
	}
	if body.Submit {
		if err := page.pressKey(ctx, "Enter"); err != nil {
			writeBrowserError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "typed_chars": utf8.RuneCountInString(body.Text), "submitted": body.Submit})
}

func (a *App) handleBrowserPressKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Key == "" {
		writeErr(w, http.StatusBadRequest, `"key" is required`)
		return
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	if err := page.pressKey(ctx, body.Key); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": body.Key})
}

func (a *App) handleBrowserScroll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Direction string  `json:"direction"`
		Distance  float64 `json:"distance"`
		X         float64 `json:"x"`
		Y         float64 `json:"y"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := strings.ToLower(strings.TrimSpace(body.Direction))
	dx, dy := float64(0), float64(0)
	distance := body.Distance
	if distance <= 0 {
		distance = 600
	}
	switch dir {
	case "down":
		dy = distance
	case "up":
		dy = -distance
	case "right":
		dx = distance
	case "left":
		dx = -distance
	default:
		writeErr(w, http.StatusBadRequest, `"direction" must be up/down/left/right`)
		return
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	info, err := page.pageInfo(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	x, y := body.X, body.Y
	if x <= 0 {
		x = info.Width / 2
	}
	if y <= 0 {
		y = info.Height / 2
	}
	if err := page.call(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy,
	}, nil); err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "direction": dir, "distance": distance, "x": x, "y": y})
}

func (a *App) handleBrowserBack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Steps int `json:"steps"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Steps <= 0 {
		body.Steps = 1
	}
	if body.Steps > 20 {
		body.Steps = 20
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	var hist struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID    int    `json:"id"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"entries"`
	}
	if err := page.call(ctx, "Page.getNavigationHistory", nil, &hist); err != nil {
		writeBrowserError(w, err)
		return
	}
	targetIndex := hist.CurrentIndex - body.Steps
	if targetIndex < 0 || targetIndex >= len(hist.Entries) {
		writeErr(w, http.StatusConflict, "not enough browser history entries")
		return
	}
	if err := page.call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": hist.Entries[targetIndex].ID}, nil); err != nil {
		writeBrowserError(w, err)
		return
	}
	time.Sleep(150 * time.Millisecond)
	info, _ := page.waitReady(ctx, 8*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "steps": body.Steps, "url": info.URL, "title": info.Title})
}

func (a *App) handleBrowserWaitFor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text      string  `json:"text"`
		TextGone  string  `json:"textGone"`
		Time      float64 `json:"time"`
		TimeoutMS int     `json:"timeout_ms"`
	}
	if err := readJSONBody(r, &body, 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	timeout := 10 * time.Second
	if body.TimeoutMS > 0 {
		timeout = time.Duration(body.TimeoutMS) * time.Millisecond
	} else if body.Time > 0 {
		timeout = time.Duration(body.Time * float64(time.Second))
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	if body.Text == "" && body.TextGone == "" {
		select {
		case <-r.Context().Done():
			writeErr(w, http.StatusRequestTimeout, r.Context().Err().Error())
		case <-time.After(timeout):
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "waited_seconds": timeout.Seconds()})
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	started := time.Now()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	textRaw, _ := json.Marshal(body.Text)
	goneRaw, _ := json.Marshal(body.TextGone)
	expr := fmt.Sprintf(`(() => { const t=document.body?document.body.innerText:''; return {text: t.includes(%s), gone: !t.includes(%s)}; })()`, string(textRaw), string(goneRaw))
	for {
		var state struct {
			Text bool `json:"text"`
			Gone bool `json:"gone"`
		}
		if err := page.evaluateInto(ctx, expr, &state); err != nil && ctx.Err() == nil {
			writeBrowserError(w, err)
			return
		}
		if (body.Text == "" || state.Text) && (body.TextGone == "" || state.Gone) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": body.Text, "textGone": body.TextGone, "elapsed_seconds": time.Since(started).Seconds()})
			return
		}
		select {
		case <-ctx.Done():
			writeErr(w, http.StatusGatewayTimeout, "browser wait timed out")
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (a *App) handleBrowserScreenshot(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "png"
	}
	if format == "jpg" {
		format = "jpeg"
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		writeErr(w, http.StatusBadRequest, `"format" must be png, jpeg/jpg, or webp`)
		return
	}
	quality, _ := strconv.Atoi(r.URL.Query().Get("quality"))
	if quality <= 0 || quality > 100 {
		quality = 80
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	params := map[string]any{"format": format, "fromSurface": true, "captureBeyondViewport": false}
	if format != "png" {
		params["quality"] = quality
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := page.call(ctx, "Page.captureScreenshot", params, &shot); err != nil {
		writeBrowserError(w, err)
		return
	}
	img, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "decode CDP screenshot: "+err.Error())
		return
	}
	if boolParam(r.URL.Query().Get("meta")) || boolParam(r.URL.Query().Get("base64")) {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": base64.StdEncoding.EncodeToString(img), "format": format, "mime": "image/" + format,
			"bytes": len(img), "encoding": "base64", "stored": false, "target_id": page.target.ID,
		})
		return
	}
	w.Header().Set("Content-Type", "image/"+format)
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	_, _ = w.Write(img)
}

func (a *App) handleBrowserEvaluate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Expression   string `json:"expression"`
		AwaitPromise bool   `json:"awaitPromise"`
	}
	if err := readJSONBody(r, &body, 4<<20); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Expression) == "" {
		writeErr(w, http.StatusBadRequest, `"expression" is required`)
		return
	}
	ctx, cancel := browserTimeout(r)
	defer cancel()
	page, err := a.openEdgePage(ctx)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	defer page.close()
	value, typ, err := page.evaluate(ctx, body.Expression, body.AwaitPromise)
	if err != nil {
		writeBrowserError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "type": typ, "value": value})
}

func (a *App) handleBrowserClose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kill bool `json:"kill"`
	}
	if err := readJSONBody(r, &body, 1<<16); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	closedByCDP := false
	page, err := a.openEdgePage(ctx)
	if err == nil {
		defer page.close()
		if page.call(ctx, "Page.close", nil, nil) == nil {
			closedByCDP = true
		}
	}
	if body.Kill || !closedByCDP {
		out, stopErr := a.runCmd(ctx, 20*time.Second, "/system/bin/am", "force-stop", androidEdgePackage)
		if stopErr != nil {
			writeBrowserError(w, fmt.Errorf("close Edge: %v: %s", stopErr, firstLine(out)))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "package": androidEdgePackage, "cdp_page_close": closedByCDP, "force_stopped": body.Kill || !closedByCDP})
}
