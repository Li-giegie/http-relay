// Package relay 提供纯粹的 HTTP 中继转发（数据面）：
// 不解析、不修改请求/响应内容，仅做透传（含 SSE 流式响应）。
// 转发目标由请求头 X-Upstream-Target 指定（必传）。
// 认证通过 Authorizer 接口解耦：本包不关心 token 如何签发与校验。
package relay

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Authorizer 是转发面的认证抽象：只回答"这个 token 放不放行"。
// 实现方（如 auth 包的 TokenStore）自行定义签发、过期、次数等策略。
// nil 表示不认证。
type Authorizer interface {
	Verify(token string) bool
}

// UpstreamTargetHeader 指定本次请求的转发目标（必传），
// 例如 X-Upstream-Target: https://api.anthropic.com。
// 该头是中继自身的控制头，不会转发给上游。
const UpstreamTargetHeader = "X-Upstream-Target"

// UpstreamTargetParam 与 UpstreamTargetHeader 同作用，通过查询参数指定
// 转发目标（如 ?upstream_target=https://api.anthropic.com）。
// 优先级低于请求头；该参数同样不会转发给上游。
const UpstreamTargetParam = "upstream_target"

// RelayTokenHeader 是中继自身的认证头：客户端携带已签发的 token。
// 该头不会转发给上游。选用独立头而非 Authorization，是为了不与客户端
// 透传给上游的 Authorization（如 Bearer sk-xxx）冲突。
const RelayTokenHeader = "X-Relay-Token"

// RelayTokenParam 与 RelayTokenHeader 同作用，通过查询参数认证
// （?relay_token=xxx），优先级低于请求头；不会转发给上游。
const RelayTokenParam = "relay_token"

// Relay 是 HTTP 中继，实现 http.Handler。
// Authorizer 非 nil 时启用 token 认证；nil 表示不认证。
type Relay struct {
	client     *http.Client
	authorizer Authorizer
}

// New 创建一个 Relay。client 为 nil 时使用 http.DefaultClient；
// authorizer 为 nil 时不做认证。
func New(client *http.Client, authorizer Authorizer) *Relay {
	return &Relay{client: client, authorizer: authorizer}
}

// ServeHTTP 处理一次中继转发：先做 token 认证（若启用），
// 再读取并校验 X-Upstream-Target，把请求原样发往 base+请求路径，
// 最后把上游响应流式写回客户端。
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// token 认证（若启用）：请求头优先，其次查询参数。
	query := req.URL.Query()
	if r.authorizer != nil {
		token := req.Header.Get(RelayTokenHeader)
		if token == "" {
			token = query.Get(RelayTokenParam)
		}
		if !r.authorizer.Verify(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="http-relay", error="invalid_token"`)
			http.Error(w, "invalid or missing relay token ("+RelayTokenHeader+
				" header or "+RelayTokenParam+" query param required)", http.StatusUnauthorized)
			return
		}
	}

	// 上游目标必传（请求头或查询参数二选一），
	// 且必须是合法的 http/https URL、带主机名。
	target := req.Header.Get(UpstreamTargetHeader)
	if target == "" {
		target = query.Get(UpstreamTargetParam)
	}
	if target == "" {
		http.Error(w, "missing required header "+UpstreamTargetHeader+
			" or query param "+UpstreamTargetParam, http.StatusBadRequest)
		return
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, fmt.Sprintf("invalid upstream target %q: must be a valid http/https URL with host", target), http.StatusBadRequest)
		return
	}

	// 目标 URL = 上游 base（可含基路径，去尾斜杠）+ 请求路径 + 查询串。
	outURL := *u
	if base := strings.TrimSuffix(u.Path, "/"); base != "" {
		outURL.Path = base + "/" + strings.TrimPrefix(req.URL.Path, "/")
	} else {
		outURL.Path = req.URL.Path
	}
	// 查询串默认原样透传；仅当请求用了中继控制参数
	// （upstream_target / relay_token）时，从转发查询串中剥除
	// （其余参数会随之重新编码、按 key 排序）。
	outURL.RawQuery = req.URL.RawQuery
	usedParam := false
	for _, p := range []string{UpstreamTargetParam, RelayTokenParam} {
		if query.Has(p) {
			query.Del(p)
			usedParam = true
		}
	}
	if usedParam {
		outURL.RawQuery = query.Encode()
	}
	outURL.Fragment = ""

	outReq := req.Clone(req.Context())
	outReq.URL = &outURL
	// http.Client 出站请求要求 RequestURI 为空（服务端入站请求才有值）。
	outReq.RequestURI = ""
	// Host 头改为上游目标域名。
	outReq.Host = u.Host
	outReq.Body = req.Body
	outReq.ContentLength = req.ContentLength
	// 删除中继自身的控制头与逐跳头；Content-Length / Transfer-Encoding
	// 交给 net/http 按实际 Body 重新计算，避免冲突。
	outReq.Header.Del(UpstreamTargetHeader)
	outReq.Header.Del(RelayTokenHeader)
	outReq.Header.Del("Content-Length")
	removeHopByHop(outReq.Header)
	client := r.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(outReq)
	if err != nil {
		// 客户端已断开时写响应是 no-op，无需特殊处理。
		log.Printf("upstream request failed: %v", err)
		http.Error(w, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	removeHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)

	// 流式透传：读到一块立刻写出并 Flush，绝不缓存整个响应体（SSE 必备）。
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				log.Printf("client write failed: %v", werr)
				return // 客户端断开，无需再记访问日志
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("upstream response body read error: %v", rerr)
			}
			break
		}
	}

	log.Printf("%s %s upstream=%s status=%d dur=%s",
		req.Method, req.URL.RequestURI(), u, resp.StatusCode, time.Since(start).Round(time.Millisecond))
}

// removeHopByHop 删除逐跳 Header，包括 Connection 头中声明的扩展头。
func removeHopByHop(h http.Header) {
	if c := h.Get("Connection"); c != "" {
		for _, name := range strings.Split(c, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(name)
	}
}

// ValidateListenAddr 校验监听地址格式：必须是 host:port 形式且带端口。
// 允许绑定所有网卡（0.0.0.0 / [::] / 空 host），由使用者自行评估暴露面。
func ValidateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %v", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid listen address %q: missing port", addr)
	}
	return nil
}
