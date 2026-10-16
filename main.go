// http-relay 是一个 HTTP 中继服务端：
// 将收到的请求原样透传到 X-Upstream-Target（或查询参数 upstream_target）
// 指定的上游地址，并把上游响应流式写回客户端。
// X-Upstream-Target 支持多个目标（逗号/空白分隔），每个中继节点消费一个，
// 剩余的随该头传给下一跳，形成链式中继。
// main 只做装配：转发面（relay）与认证实现（auth）通过 relay.Authorizer
// 接口解耦，管理面（admin）与认证实现通过 admin.TokenManager 接口解耦。
// token 认证由 --enable-auth 显式开关：不开时中继不校验 token；
// 管理界面由 --admin-addr 非空即启用。
// token 为不透明随机串或管理员手动录入的值（过期时间 + 使用次数上限），
// 签发记录仅存内存，重启即失效。
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

	"github.com/Li-giegie/http-relay/admin"
	"github.com/Li-giegie/http-relay/auth"
	"github.com/Li-giegie/http-relay/relay"
)

var (
	listenAddr    = flag.String("listen-addr", "127.0.0.1:8080", "监听地址（host:port，可用 0.0.0.0 绑定所有网卡）")
	enableHTTPS   = flag.Bool("enable-https", false, "显式指定是否启用入站 HTTPS；启用时必须提供 --tls-cert 与 --tls-key")
	tlsCert       = flag.String("tls-cert", "", "入站 HTTPS 证书文件（PEM）；仅在 --enable-https 时生效")
	tlsKey        = flag.String("tls-key", "", "入站 HTTPS 私钥文件（PEM）；仅在 --enable-https 时生效")
	enableAuth    = flag.Bool("enable-auth", false, "显式启用中继 token 认证（校验 X-Relay-Token）；不启用则不校验 token")
	adminAddr     = flag.String("admin-addr", "", "管理界面监听地址；非空即启用管理界面（如 127.0.0.1:8081，可用 0.0.0.0 绑定所有网卡）")
	adminUser     = flag.String("admin-user", "admin", "管理界面管理员账号，默认 admin")
	adminPassword = flag.String("admin-password", "", "管理界面管理员密码；为空则启动时随机生成并打印；也可用环境变量 RELAY_ADMIN_PASSWORD")
)

func main() {
	flag.Parse()
	if err := relay.ValidateListenAddr(*listenAddr); err != nil {
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

	// token 存储（auth 实现）：中继认证或管理界面任一启用时都创建（纯内存，重启即空）。
	// 注意：存在 token 不等于开启认证——中继是否校验 token 只由 --enable-auth 决定。
	var tokens *auth.TokenStore
	if *enableAuth || *adminAddr != "" {
		tokens = auth.NewTokenStore()
	}

	// 转发面（数据面）：全部使用默认行为——http.DefaultClient 出站、
	// http.Server 不设超时，以支持 SSE 长流式响应。
	// 认证经 relay.Authorizer 接口注入：不开启认证时传 nil。
	var authorizer relay.Authorizer
	if *enableAuth {
		authorizer = tokens
		log.Printf("token auth enabled (opaque token, in-memory, use %s header)", relay.RelayTokenHeader)
	} else {
		log.Printf("token auth disabled: relay does not verify tokens")
	}
	srv := &http.Server{
		Addr:    *listenAddr,
		Handler: relay.New(nil, authorizer),
	}

	// 管理面（控制面）：--admin-addr 非空即启用。
	// 管理员密码为空时随机生成并打印一次（每次启动都不同）。
	adminUser := *adminUser
	adminPassword := *adminPassword
	if env := os.Getenv("RELAY_ADMIN_PASSWORD"); env != "" {
		adminPassword = env
	}
	var adminSrv *http.Server
	if *adminAddr != "" {
		if adminPassword == "" {
			var err error
			adminPassword, err = admin.GeneratePassword()
			if err != nil {
				log.Fatalf("generate admin password: %v", err)
			}
			log.Printf("admin password generated (user %q): %s", adminUser, adminPassword)
		}
		if err := relay.ValidateListenAddr(*adminAddr); err != nil {
			log.Fatalf("admin-addr: %v", err)
		}
		adminSrv = &http.Server{
			Addr:    *adminAddr,
			Handler: admin.NewAdminServer(adminUser, adminPassword, tokens),
		}
	} else if adminPassword != "" {
		log.Printf("warning: admin-password provided but --admin-addr is not set, admin UI disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		log.Printf("http-relay listening on %s://%s (upstream via %s header)",
			scheme, *listenAddr, relay.UpstreamTargetHeader)
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
	if adminSrv != nil {
		go func() {
			log.Printf("admin UI listening on http://%s (login user %q, password required)", *adminAddr, adminUser)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Println("shutting down...")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 先关数据端口（等待在途流式响应收尾），再关管理端口。
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("admin graceful shutdown failed: %v", err)
		}
	}
}
