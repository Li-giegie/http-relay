// 集成测试：跨 relay（转发面）、admin（管理面）、auth（认证实现）的端到端验证，
// 相当于在装配根上确认三个包通过接口协作的行为。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Li-giegie/http-relay/admin"
	"github.com/Li-giegie/http-relay/auth"
	"github.com/Li-giegie/http-relay/relay"
)

// TestEndToEnd 集成链路：管理台登录 → 签发（有效期+次数）→ 列表可见 →
// relay 转发认证 → 次数用尽拒绝 → 删除即拒绝。
func TestEndToEnd(t *testing.T) {
	tokens := auth.NewTokenStore()
	adminTS := httptest.NewServer(admin.NewAdminServer("admin", "pw", tokens))
	t.Cleanup(adminTS.Close)
	relayTS := httptest.NewServer(relay.New(nil, tokens))
	t.Cleanup(relayTS.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(relay.RelayTokenHeader) != "" {
			t.Errorf("upstream received %s header", relay.RelayTokenHeader)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	// 登录管理台
	resp, err := http.Post(adminTS.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"pw"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		cookie = c
	}
	if cookie == nil {
		t.Fatal("no session cookie after login")
	}

	// 签发 token：有效期 1 小时、限 2 次
	body, _ := json.Marshal(map[string]any{"name": "e2e", "ttl_hours": 1, "max_uses": 2})
	req, _ := http.NewRequest(http.MethodPost, adminTS.URL+"/api/tokens", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	plaintext, _ := created["token"].(string)
	if !strings.HasPrefix(plaintext, "relay_") || len(plaintext) != len("relay_")+43 {
		t.Fatalf("created token = %q, want relay_ prefix + 43 chars", plaintext)
	}

	// 列表可见完整明文与过期时间
	req, _ = http.NewRequest(http.MethodGet, adminTS.URL+"/api/tokens", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed struct {
		Tokens []map[string]any `json:"tokens"`
	}
	json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed.Tokens) != 1 {
		t.Fatalf("list length = %d, want 1", len(listed.Tokens))
	}
	if listed.Tokens[0]["token"] != plaintext {
		t.Errorf("list token = %v, want full plaintext", listed.Tokens[0]["token"])
	}
	if exp, _ := listed.Tokens[0]["expires_at"].(string); exp == "" {
		t.Error("list item missing expires_at")
	}
	id, _ := listed.Tokens[0]["id"].(string)

	// relay 转发：前 2 次认证通过，第 3 次次数用尽 401
	relayGet := func() int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, relayTS.URL+"/x", nil)
		req.Header.Set(relay.UpstreamTargetHeader, upstream.URL)
		req.Header.Set(relay.RelayTokenHeader, plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("relay request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := relayGet(); code != http.StatusOK {
		t.Fatalf("relay #1 status = %d, want 200", code)
	}
	if code := relayGet(); code != http.StatusOK {
		t.Fatalf("relay #2 status = %d, want 200", code)
	}
	if code := relayGet(); code != http.StatusUnauthorized {
		t.Fatalf("relay #3 status = %d, want 401 (max uses reached)", code)
	}

	// 删除即撤销
	req, _ = http.NewRequest(http.MethodDelete, adminTS.URL+"/api/tokens/"+id, nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", resp.StatusCode)
	}
	if code := relayGet(); code != http.StatusUnauthorized {
		t.Fatalf("relay after delete = %d, want 401", code)
	}
}

// TestEndToEndExpired 签发即过期的 token 直接被 relay 拒绝。
func TestEndToEndExpired(t *testing.T) {
	tokens := auth.NewTokenStore()
	plaintext, _, err := tokens.Create("expired", -time.Hour, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	relayTS := httptest.NewServer(relay.New(nil, tokens))
	t.Cleanup(relayTS.Close)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	req, _ := http.NewRequest(http.MethodGet, relayTS.URL+"/x", nil)
	req.Header.Set(relay.UpstreamTargetHeader, upstream.URL)
	req.Header.Set(relay.RelayTokenHeader, plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("relay status = %d, want 401 (expired)", resp.StatusCode)
	}
}

// TestInterfaceDecoupling 编译期确认：auth.TokenStore 同时满足
// 转发面与管理面两边的接口（这是装配可行的前提）。
func TestInterfaceDecoupling(t *testing.T) {
	var tokens *auth.TokenStore
	var _ relay.Authorizer = tokens
	var _ admin.TokenManager = tokens
}
