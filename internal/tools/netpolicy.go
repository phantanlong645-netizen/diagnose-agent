package tools

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// netpolicy.go 承载 web_search / web_fetch 的网络安全策略（SSRF 防护 + 限流）。
//
// 分层防护（从早到晚）：
//  1. Prepare 阶段 validateWebURL：schema 白名单（仅 http/https）+ IP/域名解析校验。
//  2. Execute 阶段 webGet：固定 IP 拨号（防 DNS rebinding）+ 令牌桶限流。
//
// 相比 paicli-go 原版（仅 non-global-unicast 检查），这里额外：
//   - 拦截 RFC1918 / ULA 私网段（诊断 Agent 的内网访问必须走 profile 绑定的
//     nbi_request / netconf_rpc，不允许绕道 web_fetch 打内网）；
//   - 对域名做解析校验并固定 IP 拨号，堵住 DNS rebinding 攻击面；
//   - 进程级令牌桶限流，防止模型触发风暴式外网请求。

const (
	// webRatePerSecond 令牌桶补充速率：每秒放行 1 个请求。
	webRatePerSecond = 1.0
	// webRateBurst 令牌桶容量：允许短时突发最多 5 个请求。
	webRateBurst = 5.0
)

// webHTTPClient 是所有 web 外网请求共享的 http.Client。
// 它使用 safeWebTransport：每次拨号都重新解析域名并校验所有地址为公网，
// 再用校验后的固定 IP 建立 TCP 连接，TLS SNI 仍保持原域名（证书校验不受影响）。
var webHTTPClient = &http.Client{
	Timeout: webRequestTimeout,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           safeWebDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// webRateLimiter 是进程级令牌桶。所有 web_search / web_fetch 共享同一配额，
// 避免模型在一个 ReAct 轮次里发出大量外网请求把目标站或本机网络打满。
var webRateLimiter = newTokenBucket(webRatePerSecond, webRateBurst)

// safeWebDialContext 是 webHTTPClient 的拨号入口。
// 它把域名重新解析成公网 IP 后固定拨号，防止第一次校验通过后、
// 实际建立连接时被解析到不同（私网）地址的 DNS rebinding 攻击。
func safeWebDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split web address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ip, err = resolvePublicWebIP(ctx, host)
		if err != nil {
			return nil, err
		}
	} else if !isPublicWebIP(ip) {
		return nil, fmt.Errorf("blocked non-public ip: %s", host)
	}
	dialer := &net.Dialer{Timeout: webRequestTimeout}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

// resolvePublicWebIP 解析域名并返回第一个公网地址。
// 只要解析结果里混入任意非公网地址即拒绝（严格模式）。
func resolvePublicWebIP(ctx context.Context, host string) (net.IP, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve host %s: %w", host, err)
	}
	var chosen net.IP
	for _, address := range addresses {
		if !isPublicWebIP(address.IP) {
			return nil, fmt.Errorf("blocked non-public ip: %s", host)
		}
		if chosen == nil {
			chosen = address.IP
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("host %s resolves to no addresses", host)
	}
	return chosen, nil
}

// tokenBucket 是标准库实现的无阻塞令牌桶。
// allow() 从不等待：token 不足时立即返回 false，由调用方把错误作为工具结果返回，
// 让模型看到"限流"信息后自行降速，而不是阻塞整个诊断 run。
type tokenBucket struct {
	mu       sync.Mutex
	rate     float64 // 每秒补充的 token 数
	capacity float64 // 桶容量（突发上限）
	tokens   float64 // 当前 token 数
	last     time.Time
}

func newTokenBucket(rate, capacity float64) *tokenBucket {
	return &tokenBucket{
		rate:     rate,
		capacity: capacity,
		tokens:   capacity,
		last:     time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.rate
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
