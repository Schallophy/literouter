package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Schallophy/literouter/internal/runtime"
	"github.com/Schallophy/literouter/internal/util"
	"github.com/Schallophy/literouter/web"
)

// Version is reported by /health and /api/overview.
const Version = "0.1.0"

var staticPathPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

var uiAssets = map[string]bool{
	"/index.html":           true,
	"/favicon.svg":          true,
	"/fonts.css":            true,
	"/js/main.js":           true,
	"/styles/tokens.css":    true,
	"/styles/base.css":      true,
	"/styles/shell.css":     true,
	"/styles/components.css": true,
	"/styles/views.css":     true,
}

var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".json":  "application/json; charset=utf-8",
	".woff2": "font/woff2",
}

const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; font-src 'self' data:; base-uri 'none'; form-action 'none'"

// HttpError aborts a handler with a status code.
type HttpError struct {
	Status  int
	Message string
	Payload any
}

func (e *HttpError) Error() string { return e.Message }

// Ctx is the request context handed to handlers.
type Ctx struct {
	W      *responseRecorder
	R      *http.Request
	URL    *url.URL
	Params map[string]string
	Body   any
	IP     string
}

// Send writes a JSON response unless headers were already sent.
func (c *Ctx) Send(status int, data any, headers map[string]string) {
	if c.W.Wrote() {
		return
	}
	c.W.Header().Set("content-type", "application/json; charset=utf-8")
	for key, value := range headers {
		c.W.Header().Set(key, value)
	}
	c.W.WriteHeader(status)
	encoded, err := json.Marshal(data)
	if err != nil {
		encoded = []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"literouter.internal","code":"literouter.internal"}}`, err.Error()))
	}
	_, _ = c.W.Write(encoded)
}

// SendRaw writes an already-encoded body.
func (c *Ctx) SendRaw(status int, body []byte, headers map[string]string) {
	if c.W.Wrote() {
		return
	}
	for key, value := range headers {
		c.W.Header().Set(key, value)
	}
	c.W.WriteHeader(status)
	_, _ = c.W.Write(body)
}

// Text writes a plain text response.
func (c *Ctx) Text(status int, value string, contentType string) {
	if c.W.Wrote() {
		return
	}
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	c.W.Header().Set("content-type", contentType)
	c.W.WriteHeader(status)
	_, _ = io.WriteString(c.W, value)
}

type handler func(*Ctx) error

type route struct {
	method  string
	parts   []string
	handler handler
}

// Server owns the router, static files and shared middleware.
type Server struct {
	Runtime *runtime.Runtime
	Logger  *util.Logger
	routes  []route
}

// NewServer builds the HTTP surface for a runtime.
func NewServer(rt *runtime.Runtime, logger *util.Logger) *Server {
	server := &Server{Runtime: rt, Logger: logger}
	server.registerOpenAI()
	server.registerAdmin()
	return server
}

func (s *Server) add(method, pattern string, h handler) {
	trimmed := strings.Trim(pattern, "/")
	parts := []string{}
	if trimmed != "" {
		parts = strings.Split(trimmed, "/")
	}
	s.routes = append(s.routes, route{method: method, parts: parts, handler: h})
}

func (s *Server) match(method, pathname string) (handler, map[string]string) {
	trimmed := strings.TrimSuffix(pathname, "/")
	if trimmed == "" {
		trimmed = "/"
	}
	segments := []string{}
	if trimmed != "/" {
		segments = strings.Split(strings.TrimPrefix(trimmed, "/"), "/")
	}
	for _, candidate := range s.routes {
		if candidate.method != method || len(candidate.parts) != len(segments) {
			continue
		}
		params := map[string]string{}
		matched := true
		for index, part := range candidate.parts {
			if strings.HasPrefix(part, ":") {
				decoded, err := url.PathUnescape(segments[index])
				if err != nil {
					decoded = segments[index]
				}
				params[part[1:]] = decoded
				continue
			}
			if part != segments[index] {
				matched = false
				break
			}
		}
		if matched {
			return candidate.handler, params
		}
	}
	return nil, nil
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *responseRecorder) Wrote() bool { return r.wrote }

func (r *responseRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(data)
}

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ServeHTTP applies security headers, authentication, admin isolation and routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	recorder := &responseRecorder{ResponseWriter: w}
	requestURL := r.URL
	isAPI := strings.HasPrefix(requestURL.Path, "/v1/")
	cfg := s.Runtime.ConfigSnapshot()

	header := recorder.Header()
	header.Set("x-content-type-options", "nosniff")
	header.Set("referrer-policy", "no-referrer")
	header.Set("content-security-policy", contentSecurityPolicy)
	header.Set("x-frame-options", "SAMEORIGIN")
	if isAPI && cfg.Server.Cors {
		header.Set("access-control-allow-origin", "*")
		header.Set("access-control-allow-headers", "authorization, content-type")
		header.Set("access-control-allow-methods", "GET, POST, OPTIONS")
		header.Set("access-control-expose-headers", "x-literouter-model, x-literouter-provider, x-literouter-failovers")
	}

	ip := remoteHost(r.RemoteAddr)
	ctx := &Ctx{
		W: recorder, R: r, URL: requestURL, Params: map[string]string{}, IP: ip,
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			if !recorder.Wrote() {
				ctx.Send(500, errorBody(fmt.Sprint(recovered), "literouter.internal"), nil)
			}
			s.Logger.Error("request failed", map[string]any{"method": r.Method, "path": requestURL.Path, "error": fmt.Sprint(recovered)})
		}
		duration := time.Since(started).Milliseconds()
		if isAPI && r.Method != "OPTIONS" {
			s.Logger.Access(r.Method, requestURL.Path, recorder.status, duration)
		} else {
			s.Logger.Debug("http request", map[string]any{"method": r.Method, "path": requestURL.Path, "status": recorder.status, "ms": duration})
		}
	}()

	if r.Method == "OPTIONS" && isAPI && cfg.Server.Cors {
		recorder.WriteHeader(204)
		return
	}

	if isAPI {
		token := strings.TrimSpace(cfg.GatewayToken)
		if token != "" && !bearerMatches(r.Header.Get("authorization"), token) {
			ctx.Send(401, errorBody("网关访问令牌无效", "literouter.auth", withCode("literouter.unauthorized")), nil)
			return
		}
	}

	body, err := readBody(r, cfg.Server.MaxBodyBytes)
	if err != nil {
		s.writeError(ctx, err)
		return
	}
	ctx.Body = body

	if strings.HasPrefix(requestURL.Path, "/api/") && !isLoopback(ip) {
		ctx.Send(403, errorBody("管理接口仅允许本机访问", "literouter.forbidden", withCode("literouter.remote_admin")), nil)
		return
	}

	matched, params := s.match(r.Method, requestURL.EscapedPath())
	if matched != nil {
		ctx.Params = params
		if err := matched(ctx); err != nil {
			s.writeError(ctx, err)
		}
		return
	}

	if r.Method == "GET" || r.Method == "HEAD" {
		if cfg.UI.Enabled {
			if s.serveStatic(ctx, requestURL.Path) {
				return
			}
		} else if requestURL.Path == "/" || uiAssets[requestURL.Path] {
			ctx.Send(404, map[string]any{"error": map[string]any{
				"message": "管理界面已关闭（config.json 中 ui.enabled = false），请直接编辑配置文件。",
				"type":    "literouter.ui_disabled",
				"code":    "literouter.ui_disabled",
			}}, nil)
			return
		}
	}

	ctx.Send(404, map[string]any{"error": map[string]any{
		"message": "未找到 " + requestURL.Path,
		"type":    "literouter.not_found",
		"code":    "literouter.not_found",
	}}, nil)
}

func (s *Server) writeError(ctx *Ctx, err error) {
	status := 500
	message := err.Error()
	var httpErr *HttpError
	if errors.As(err, &httpErr) {
		status = httpErr.Status
		message = httpErr.Message
	}
	if status >= 500 {
		s.Logger.Error("request failed", map[string]any{"method": ctx.R.Method, "path": ctx.URL.Path, "error": message})
	}
	if !ctx.W.Wrote() {
		kind := "literouter.bad_request"
		if status >= 500 {
			kind = "literouter.internal"
		}
		ctx.Send(status, errorBody(message, kind), nil)
	}
}

func errorBody(message, kind string, options ...func(map[string]any)) map[string]any {
	detail := map[string]any{
		"message": message,
		"type":    kind,
		"code":    kind,
	}
	for _, option := range options {
		option(detail)
	}
	return map[string]any{"error": detail}
}

func withCode(code string) func(map[string]any) {
	return func(detail map[string]any) { detail["code"] = code }
}

func readBody(r *http.Request, limit int) (any, error) {
	if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return nil, &HttpError{Status: 400, Message: err.Error()}
	}
	if len(data) > limit {
		return nil, &HttpError{
			Status:  413,
			Message: fmt.Sprintf("请求体超过 %dKB 限制（config.json 的 server.maxBodyBytes）", int(math.Round(float64(limit)/1024))),
		}
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, nil
	}
	contentType := r.Header.Get("content-type")
	if strings.Contains(contentType, "application/json") || strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return nil, &HttpError{Status: 400, Message: "请求体不是合法 JSON"}
		}
		return parsed, nil
	}
	return text, nil
}

func (s *Server) serveStatic(ctx *Ctx, pathname string) bool {
	relative := "index.html"
	if pathname != "/" {
		relative = strings.TrimLeft(pathname, "/")
	}
	if !staticPathPattern.MatchString(relative) || strings.Contains(relative, "..") {
		return false
	}
	content, err := fs.ReadFile(web.FS, relative)
	if err != nil {
		return false
	}
	extension := strings.ToLower(path.Ext(relative))
	contentType := mimeTypes[extension]
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ctx.W.Header().Set("content-type", contentType)
	ctx.W.Header().Set("cache-control", "no-store")
	ctx.W.WriteHeader(200)
	if ctx.R.Method != "HEAD" {
		_, _ = ctx.W.Write(content)
	}
	return true
}

func bearerMatches(header, token string) bool {
	trimmed := strings.TrimSpace(header)
	if len(trimmed) < 7 || !strings.EqualFold(trimmed[:6], "Bearer") || (trimmed[6] != ' ' && trimmed[6] != '\t') {
		return false
	}
	provided := strings.TrimSpace(trimmed[6:])
	if provided == "" {
		return false
	}
	providedHash := sha256.Sum256([]byte(provided))
	expectedHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

func remoteHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

func isLoopback(ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	trimmed := strings.TrimPrefix(ip, "::ffff:")
	parsed := net.ParseIP(trimmed)
	if parsed != nil && parsed.IsLoopback() {
		return true
	}
	return strings.HasPrefix(trimmed, "127.")
}
