package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Li-giegie/http-relay/auth"
)

// newTestAdmin 启动一个带空 token store 的管理服务实例（账号 admin）。
func newTestAdmin(t *testing.T, password string) (*httptest.Server, *auth.TokenStore) {
	t.Helper()
	tokens := auth.NewTokenStore()
	ts := httptest.NewServer(NewAdminServer("admin", password, tokens))
	t.Cleanup(ts.Close)
	return ts, tokens
}

// doAdmin 发 JSON 请求并返回状态码与解析后的 JSON。
// 非 JSON 响应体（如 405/404 纯文本）返回空 map。
func doAdmin(t *testing.T, method, url string, body any, cookie *http.Cookie) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		out = map[string]any{}
	}
	return resp.StatusCode, out
}

// loginAdmin 用账号 admin 登录管理服务，返回 session cookie。
func loginAdmin(t *testing.T, base, password string) *http.Cookie {
	t.Helper()
	resp, err := http.Post(base+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("login did not set session cookie")
	return nil
}

// TestAdminLoginPage 验证前端页面可加载。
func TestAdminLoginPage(t *testing.T) {
	ts, _ := newTestAdmin(t, "pw")
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get page: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("page content-type = %q", ct)
	}
}

// TestAdminLoginFlow 验证账号/密码错误 401、正确 200 并发 cookie。
func TestAdminLoginFlow(t *testing.T) {
	ts, _ := newTestAdmin(t, "pw")

	// 错误密码
	status, out := doAdmin(t, http.MethodPost, ts.URL+"/api/login",
		map[string]string{"username": "admin", "password": "wrong"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", status)
	}
	if out["error"] != "wrong username or password" {
		t.Errorf("error = %v, want wrong username or password", out["error"])
	}

	// 错误账号
	status, _ = doAdmin(t, http.MethodPost, ts.URL+"/api/login",
		map[string]string{"username": "root", "password": "pw"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong username status = %d, want 401", status)
	}

	// 正确账号密码：直接走 http.DefaultClient 拿 Set-Cookie
	resp, err := http.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"pw"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("login cookies = %v, want one %s", cookies, sessionCookieName)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie attributes insecure: %+v", cookies[0])
	}
}

// TestAdminRequiresSession 验证未登录访问管理 API 返回 401。
func TestAdminRequiresSession(t *testing.T) {
	ts, _ := newTestAdmin(t, "pw")
	status, _ := doAdmin(t, http.MethodGet, ts.URL+"/api/tokens", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d, want 401", status)
	}
	status, _ = doAdmin(t, http.MethodPost, ts.URL+"/api/tokens",
		map[string]string{"name": "x"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create status = %d, want 401", status)
	}
}

// TestAdminRouting 验证 ServeMux 路由行为：方法不匹配 405、未知路径 404。
func TestAdminRouting(t *testing.T) {
	ts, _ := newTestAdmin(t, "pw")
	// GET /api/login 方法不匹配 → 405
	status, _ := doAdmin(t, http.MethodGet, ts.URL+"/api/login", nil, nil)
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/login status = %d, want 405", status)
	}
	// 未知路径 → 404
	status, _ = doAdmin(t, http.MethodGet, ts.URL+"/api/nope", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET /api/nope status = %d, want 404", status)
	}
	// 根路径 → 200 页面
	status, _ = doAdmin(t, http.MethodGet, ts.URL+"/", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", status)
	}
}

// TestAdminCreateCustomToken 验证管理 API 手动录入 token：
// 指定 token 字段时按录入值签发，重复/非法格式返回 400。
func TestAdminCreateCustomToken(t *testing.T) {
	ts, tokens := newTestAdmin(t, "pw")
	cookie := loginAdmin(t, ts.URL, "pw")

	// 手动录入
	status, out := doAdmin(t, http.MethodPost, ts.URL+"/api/tokens",
		map[string]any{"token": "my-custom-token", "name": "manual", "ttl_hours": 1}, cookie)
	if status != http.StatusOK {
		t.Fatalf("create custom status = %d, want 200 (out=%v)", status, out)
	}
	if out["token"] != "my-custom-token" {
		t.Errorf("created token = %v, want my-custom-token", out["token"])
	}
	if !tokens.Verify("my-custom-token") {
		t.Error("custom token not verifiable after creation")
	}

	// 重复录入 → 400
	status, _ = doAdmin(t, http.MethodPost, ts.URL+"/api/tokens",
		map[string]any{"token": "my-custom-token"}, cookie)
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate status = %d, want 400", status)
	}

	// 非法格式（含空格）→ 400
	status, _ = doAdmin(t, http.MethodPost, ts.URL+"/api/tokens",
		map[string]any{"token": "has space"}, cookie)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid token status = %d, want 400", status)
	}
}

// TestGeneratePassword 验证随机管理员密码的格式与唯一性。
func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(p) != 16 {
			t.Errorf("password length = %d, want 16", len(p))
		}
		if seen[p] {
			t.Fatalf("duplicate password generated: %q", p)
		}
		seen[p] = true
	}
}
