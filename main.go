// http-relay 是一个 HTTP 中继服务端：
// 将收到的请求原样透传到 X-Upstream-Target（或查询参数 upstream_target）
// 指定的上游地址，并把上游响应流式写回客户端。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	listenAddr  = flag.String("listen-addr", "127.0.0.1:8080", "监听地址（禁止 0.0.0.0，需指定具体 IP）")
	enableHTTPS = flag.Bool("enable-https", false, "显式指定是否启用入站 HTTPS；启用时必须提供 --tls-cert 与 --tls-key")
	tlsCert     = flag.String("tls-cert", "", "入站 HTTPS 证书文件（PEM）；仅在 --enable-https 时生效")
	tlsKey      = flag.String("tls-key", "", "入站 HTTPS 私钥文件（PEM）；仅在 --enable-https 时生效")
)

func main() {
	flag.Parse()
	if err := ValidateListenAddr(*listenAddr); err != nil {
		log.Fatalf("listen-addr: %v", err)
	}
	if *enableHTTPS {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatalf("enable-https requires both tls-cert and tls-key")
		}
	} else if *tlsCert != "" || *tlsKey != "" {
		log.Printf("warning: tls-cert/tls-key provided but --enable-https is not set, listening on plain HTTP")
	}
	useTLS := *enableHTTPS

	// 全部使用默认行为：http.DefaultClient 出站、http.Server 不设超时，
	// 以支持 SSE 长流式响应。
	srv := &http.Server{
		Addr:    *listenAddr,
		Handler: &Relay{},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		log.Printf("http-relay listening on %s://%s (upstream via %s header)",
			scheme, *listenAddr, UpstreamTargetHeader)
		var err error
		if useTLS {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Println("shutting down...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
