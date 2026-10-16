// Package admin 提供管理面：独立端口上的 token 管理界面（控制面）。
// 登录凭管理员账号密码（账号默认 admin，密码为空则启动时随机生成），
// 签发内存 session cookie；提供 token 的列表/签发/删除 JSON API 与内嵌前端页面。
// 对 token 存储的依赖通过 TokenManager 接口解耦，本包不关心实现细节。
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Li-giegie/http-relay/auth"
)

// TokenManager 是管理面对 token 存储的抽象：
// 签发（带有效期与次数上限）、删除（撤销）、列出全部记录。
// auth.TokenStore 实现了该接口。
type TokenManager interface {
	Create(name string, ttl time.Duration, maxUses int64) (plaintext string, info auth.TokenInfo, err error)
	Delete(id string) bool
	List() []auth.TokenInfo
}

//go:embed admin.html
var adminHTML []byte

// sessionCookieName 是管理登录 session 的 cookie 名。
const sessionCookieName = "relay_admin_session"

// sessionTTL 是 session 的有效期。
const sessionTTL = 12 * time.Hour

// defaultAdminUser 是管理员账号的默认值。
const defaultAdminUser = "admin"

// AdminServer 是 token 管理界面，实现 http.Handler。
type AdminServer struct {
	mux          *http.ServeMux
	username     string
	passwordHash [32]byte // SHA-256(管理员密码)
	tokens       TokenManager

	mu       sync.Mutex
	sessions map[string]time.Time // session id -> 过期时间
}

// NewAdminServer 创建管理服务并注册路由。
// user 为管理员账号，password 为明文密码（user 为空时使用默认账号 admin）；
// tokens 为 token 存储的 TokenManager 实现。
func NewAdminServer(user, password string, tokens TokenManager) *AdminServer {
	if user == "" {
		user = defaultAdminUser
	}
	a := &AdminServer{
		username:     user,
		passwordHash: sha256.Sum256([]byte(password)),
		tokens:       tokens,
		sessions:     map[string]time.Time{},
	}
	mux := http.NewServeMux()
	// {$} 仅匹配根路径；GET 模式同时覆盖 HEAD。
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.handleLogout)
	// 需要登录的 API：套上 session 校验中间件。
	mux.Handle("GET /api/tokens", a.requireSession(http.HandlerFunc(a.handleList)))
	mux.Handle("POST /api/tokens", a.requireSession(http.HandlerFunc(a.handleCreate)))
	mux.Handle("DELETE /api/tokens/{id}", a.requireSession(http.HandlerFunc(a.handleDelete)))
	a.mux = mux
	return a
}

// ServeHTTP 交给 ServeMux 路由：未知路径 404、方法不匹配 405。
func (a *AdminServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// handleIndex 返回内嵌的前端页面。
func (a *AdminServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(adminHTML)
}

// handleLogin 校验管理员账号密码，签发 session cookie。
func (a *AdminServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// 账号与密码都走哈希 + 常数时间比较，避免时序侧信道泄露哪个字段错了。
	userSum := sha256.Sum256([]byte(body.Username))
	userHash := sha256.Sum256([]byte(a.username))
	passSum := sha256.Sum256([]byte(body.Password))
	userOK := subtle.ConstantTimeCompare(userSum[:], userHash[:]) == 1
	passOK := subtle.ConstantTimeCompare(passSum[:], a.passwordHash[:]) == 1
	if !userOK || !passOK {
		writeJSONError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}

	sid, err := newSessionID()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "generate session id failed")
		return
	}
	a.mu.Lock()
	a.pruneSessionsLocked()
	a.sessions[sid] = time.Now().Add(sessionTTL)
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogout 删除当前 session。
func (a *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	// 让浏览器一并清掉 cookie。
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleList 返回全部签发记录（含完整 JWT 明文，界面可见）。
func (a *AdminServer) handleList(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID        string `json:"id"` // jti
		Name      string `json:"name"`
		Token     string `json:"token"` // 完整 JWT，可见可复制
		CreatedAt string `json:"created_at"`
		ExpiresAt string `json:"expires_at"` // 空串 = 永不过期
		MaxUses   int64  `json:"max_uses"`   // 0 = 不限
		Uses      int64  `json:"uses"`
		LastUsed  string `json:"last_used"`
	}
	list := a.tokens.List()
	items := make([]item, 0, len(list))
	for _, t := range list {
		expires := ""
		if !t.ExpiresAt.IsZero() {
			expires = t.ExpiresAt.Format(time.RFC3339)
		}
		lastUsed := ""
		if !t.LastUsed.IsZero() {
			lastUsed = t.LastUsed.Format(time.RFC3339)
		}
		items = append(items, item{
			ID:        t.ID,
			Name:      t.Name,
			Token:     t.Token,
			CreatedAt: t.CreatedAt.Format(time.RFC3339),
			ExpiresAt: expires,
			MaxUses:   t.MaxUses,
			Uses:      t.Uses,
			LastUsed:  lastUsed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": items})
}

// handleCreate 签发 token；明文随列表可见，响应中直接返回。
// body.ttl_hours <= 0 表示永不过期；body.max_uses <= 0 表示不限次数。
func (a *AdminServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		TTLHours int64  `json:"ttl_hours"`
		MaxUses  int64  `json:"max_uses"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	plaintext, info, err := a.tokens.Create(strings.TrimSpace(body.Name),
		time.Duration(body.TTLHours)*time.Hour, body.MaxUses)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	expires := ""
	if !info.ExpiresAt.IsZero() {
		expires = info.ExpiresAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      plaintext,
		"name":       info.Name,
		"created":    info.CreatedAt.Format(time.RFC3339),
		"expires_at": expires,
		"max_uses":   info.MaxUses,
	})
}

// handleDelete 按路径中的 id（jti）删除签发记录（= 撤销）。
func (a *AdminServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing token id")
		return
	}
	if !a.tokens.Delete(id) {
		writeJSONError(w, http.StatusNotFound, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// requireSession 是登录校验中间件：未登录/过期返回 401。
func (a *AdminServer) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "login required")
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.pruneSessionsLocked()
		exp, ok := a.sessions[c.Value]
		if !ok || time.Now().After(exp) {
			writeJSONError(w, http.StatusUnauthorized, "login required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pruneSessionsLocked 清理过期 session（调用方需已持锁）。
func (a *AdminServer) pruneSessionsLocked() {
	now := time.Now()
	for sid, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, sid)
		}
	}
}

// newSessionID 生成随机的 hex session id。
func newSessionID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// GeneratePassword 生成 16 字符的随机管理员密码（未指定 --admin-password 时使用）。
func GeneratePassword() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
