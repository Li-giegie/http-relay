// Package relay 提供纯粹的 HTTP 中继转发：
// 不解析、不修改请求/响应内容，仅做透传（含 SSE 流式响应）。
// 转发目标由请求头 X-Upstream-Target 指定（必传）。
package main

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

// UpstreamTargetHeader 指定本次请求的转发目标（必传），
// 例如 X-Upstream-Target: https://api.anthropic.com。
// 该头是中继自身的控制头，不会转发给上游。
const UpstreamTargetHeader = "X-Upstream-Target"

// UpstreamTargetParam 与 UpstreamTargetHeader 同作用，通过查询参数指定
// 转发目标（如 ?upstream_target=https://api.anthropic.com）。
// 优先级低于请求头；该参数同样不会转发给上游。
const UpstreamTargetParam = "upstream_target"

// Relay 是 HTTP 中继，实现 http.Handler。
type Relay struct {
	client *http.Client
}

// ServeHTTP 处理一次中继转发：读取并校验 X-Upstream-Target，
// 把请求原样发往 base+请求路径，再把上游响应流式写回客户端。
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// 上游目标必传（请求头或查询参数二选一），
	// 且必须是合法的 http/https URL、带主机名。
	query := req.URL.Query()
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
	// 查询串默认原样透传；仅当请求用了控制参数 upstream_target 时，
	// 从转发查询串中剥除它（其余参数会随之重新编码、按 key 排序）。
	outURL.RawQuery = req.URL.RawQuery
	if query.Has(UpstreamTargetParam) {
		query.Del(UpstreamTargetParam)
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

// ValidateListenAddr 校验监听地址：禁止空 host / 0.0.0.0 / ::，
// 必须显式指定监听的具体 IP，避免误监听所有网卡。
func ValidateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %v", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("listen address %q binds to all interfaces; specify a concrete IP (e.g. 127.0.0.1:8080) instead", addr)
	}
	return nil
}
