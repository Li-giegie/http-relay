package main

import (
	"bufio"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestRelay 启动一个挂在 httptest 上的中继实例。
func newTestRelay(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(nil))
	t.Cleanup(ts.Close)
	return ts
}

// TestForwardWithUpstreamTarget 验证正向转发：
// 路径/Header/Body 透传，X-Upstream-Target 与逐跳头被清理，Host 改写。
func TestForwardWithUpstreamTarget(t *testing.T) {
	var got struct {
		method, path, host, auth, body, ct string
		hasUpstreamHeader, hasTE           bool
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.host = r.Host
		got.auth = r.Header.Get("Authorization")
		got.ct = r.Header.Get("Content-Type")
		got.hasUpstreamHeader = r.Header.Get(UpstreamTargetHeader) != ""
		got.hasTE = r.Header.Get("Transfer-Encoding") != ""
		body, _ := io.ReadAll(r.Body)
		got.body = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Resp-Header", "kept")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"reply":"ok"}`)
	}))
	defer upstream.Close()

	proxy := newTestRelay(t)

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"messages":[]}`))
	req.Header.Set(UpstreamTargetHeader, upstream.URL)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "close")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"reply":"ok"}` {
		t.Fatalf("body = %q, want %q", body, `{"reply":"ok"}`)
	}
	if resp.Header.Get("X-Custom-Resp-Header") != "kept" {
		t.Fatalf("custom response header was not passed through")
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	}
	if got.method != http.MethodPost {
		t.Errorf("upstream method = %s, want POST", got.method)
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("upstream path = %s, want /v1/chat/completions", got.path)
	}
	if got.host != strings.TrimPrefix(upstream.URL, "http://") {
		t.Errorf("upstream Host = %s, want %s", got.host, strings.TrimPrefix(upstream.URL, "http://"))
	}
	if got.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want forwarded unchanged", got.auth)
	}
	if got.ct != "application/json" {
		t.Errorf("Content-Type = %q, want forwarded unchanged", got.ct)
	}
	if got.hasUpstreamHeader {
		t.Errorf("X-Upstream-Target was forwarded to upstream, must be stripped")
	}
	if got.hasTE {
		t.Errorf("Transfer-Encoding was forwarded to upstream, must be stripped")
	}
	if got.body != `{"messages":[]}` {
		t.Errorf("upstream body = %q, want forwarded unchanged", got.body)
	}
}

// TestForwardWithBasePath 验证上游地址带基路径时的路径拼接。
func TestForwardWithBasePath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer upstream.Close()

	proxy := newTestRelay(t)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/models?limit=1", nil)
	req.Header.Set(UpstreamTargetHeader, upstream.URL+"/openai")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	resp.Body.Close()
	if gotPath != "/openai/v1/models" {
		t.Fatalf("upstream path = %s, want /openai/v1/models", gotPath)
	}
}

// TestForwardWithQueryParam 通过查询参数 upstream_target 指定目标：
// 该参数不转发给上游，其余查询参数保留。
func TestForwardWithQueryParam(t *testing.T) {
	var gotRawQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
	}))
	defer upstream.Close()

	proxy := newTestRelay(t)

	u := proxy.URL + "/v1/models?limit=1&" + UpstreamTargetParam + "=" + url.QueryEscape(upstream.URL)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	resp.Body.Close()
	if gotRawQuery != "limit=1" {
		t.Fatalf("upstream query = %q, want %q (control param must be stripped)", gotRawQuery, "limit=1")
	}
}

// TestHeaderPrecedence 请求头与查询参数同时存在时，请求头优先。
func TestHeaderPrecedence(t *testing.T) {
	var hitBy string
	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hitBy = name
		}))
	}
	upA, upB := newUpstream("header"), newUpstream("param")
	defer upA.Close()
	defer upB.Close()

	proxy := newTestRelay(t)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/?"+UpstreamTargetParam+"="+url.QueryEscape(upB.URL), nil)
	req.Header.Set(UpstreamTargetHeader, upA.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	resp.Body.Close()
	if hitBy != "header" {
		t.Fatalf("upstream hit = %s, want header (header must take precedence)", hitBy)
	}
}

// TestMissingUpstreamTarget 不带 X-Upstream-Target 时应返回 400（目标必传）。
func TestMissingUpstreamTarget(t *testing.T) {
	proxy := newTestRelay(t)
	resp, err := http.Get(proxy.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestInvalidUpstreamTarget 非法上游地址应返回 400。
func TestInvalidUpstreamTarget(t *testing.T) {
	proxy := newTestRelay(t)

	cases := []string{
		"http://bad url with spaces", // 解析失败
		"ftp://example.com",          // 非 http/https scheme
		"http://",                    // 缺 host
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		req.Header.Set(UpstreamTargetHeader, c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("relay request with target %q: %v", c, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("target %q: status = %d, want 400", c, resp.StatusCode)
		}
	}
}

// TestUpstreamUnreachable 上游连接失败时应返回 502。
func TestUpstreamUnreachable(t *testing.T) {
	proxy := newTestRelay(t)

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
	// 127.0.0.1:1 基本不可能有服务在监听。
	req.Header.Set(UpstreamTargetHeader, "http://127.0.0.1:1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// TestSSEStreaming 验证 SSE 流式透传：上游每写出一块，客户端立刻能读到，
// 而不是等上游写完整个响应才收到（即响应未被中继缓存）。
func TestSSEStreaming(t *testing.T) {
	// gotFirst 在客户端读到第一个 chunk 后关闭；
	// proceed 由测试方关闭，允许上游写出第二个 chunk。
	gotFirst := make(chan struct{})
	proceed := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		io.WriteString(w, "data: chunk-1\n\n")
		flusher.Flush()
		// 阻塞，直到客户端确认已收到 chunk-1。
		<-proceed
		io.WriteString(w, "data: chunk-2\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	proxy := newTestRelay(t)

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages",
		strings.NewReader(`{"stream":true}`))
	req.Header.Set(UpstreamTargetHeader, upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var events []string
	readDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				events = append(events, strings.TrimPrefix(line, "data: "))
				if len(events) == 1 {
					close(gotFirst)
				}
			}
		}
		readDone <- scanner.Err()
	}()

	// 关键断言：上游还阻塞着（chunk-2 未写出），客户端必须已经收到 chunk-1。
	// 若中继缓存了整个响应，这里会超时。
	select {
	case <-gotFirst:
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE chunk was not delivered while upstream was still writing — response is being buffered")
	}
	close(proceed)

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for stream to finish")
	}
	if len(events) != 2 || events[0] != "chunk-1" || events[1] != "chunk-2" {
		t.Fatalf("events = %v, want [chunk-1 chunk-2]", events)
	}
}

// TestOutboundHTTPS 验证中继能转发到 HTTPS 上游（X-Upstream-Target 为 https://）。
func TestOutboundHTTPS(t *testing.T) {
	var hit bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		io.WriteString(w, "via-tls-upstream")
	}))
	defer upstream.Close()
	if !strings.HasPrefix(upstream.URL, "https://") {
		t.Fatalf("test upstream is not TLS")
	}

	// httptest 的自签证书不被系统信任，测试中跳过校验。
	relay := New(&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}})
	proxy := httptest.NewServer(relay)
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages",
		strings.NewReader(`{"hi":true}`))
	req.Header.Set(UpstreamTargetHeader, upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !hit {
		t.Fatalf("request did not reach the HTTPS upstream")
	}
	if string(body) != "via-tls-upstream" {
		t.Fatalf("body = %q, want %q", body, "via-tls-upstream")
	}
}

// TestInboundTLS 验证 TLS 监听入口（对应 --tls-cert/--tls-key 启用 HTTPS 的场景）。
func TestInboundTLS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	relayTLS := httptest.NewTLSServer(New(nil))
	defer relayTLS.Close()

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	req, _ := http.NewRequest(http.MethodPost, relayTLS.URL+"/v1/messages",
		strings.NewReader(`{"hi":true}`))
	req.Header.Set(UpstreamTargetHeader, upstream.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("relay request over TLS: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestStatusPreserved 验证上游非 200 状态码原样保留。
func TestStatusPreserved(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream says no", http.StatusTeapot)
	}))
	defer upstream.Close()

	proxy := newTestRelay(t)
	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/v1/x", nil)
	req.Header.Set(UpstreamTargetHeader, upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("relay request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
}

// TestValidateListenAddr 监听地址校验：拒绝 0.0.0.0 / :: / 空 host。
func TestValidateListenAddr(t *testing.T) {
	bad := []string{"0.0.0.0:8080", ":8080", ":::8080", "not-an-addr"}
	for _, addr := range bad {
		if err := ValidateListenAddr(addr); err == nil {
			t.Errorf("ValidateListenAddr(%q) = nil, want error", addr)
		}
	}
	good := []string{"127.0.0.1:8080", "10.8.0.2:8080", "192.168.1.5:9000"}
	for _, addr := range good {
		if err := ValidateListenAddr(addr); err != nil {
			t.Errorf("ValidateListenAddr(%q) = %v, want nil", addr, err)
		}
	}
}
