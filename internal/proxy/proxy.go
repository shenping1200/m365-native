package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

type Kind string

const (
	KindDirect Kind = "direct"
	KindHTTP   Kind = "http"
	KindSOCKS5 Kind = "socks5"
	KindSOCKS4 Kind = "socks4"
)

type Config struct {
	Raw  string
	Type Kind
	Host string
	Port string
	User string
	Pass string
}

// Parse 识别以下格式：
//   http(s)://[user:pass@]host:port
//   socks5://[user:pass@]host:port          (标准)
//   socks5://host:port:user:pass            (非标准, 部分代理服务商常用)
//   socks5h://host:port                      (远程解析 DNS)
//   socks4://[user:pass@]host:port
//   host:port                                (无 scheme, 默认按 socks5)
func Parse(raw string) (Config, error) {
	raw = strings.TrimSpace(raw)
	c := Config{Raw: raw}
	if raw == "" {
		c.Type = KindDirect
		return c, nil
	}
	low := strings.ToLower(raw)
	if i := strings.Index(low, "://"); i > 0 {
		scheme := low[:i]
		rest := raw[i+3:]
		switch scheme {
		case "http", "https":
			c.Type = KindHTTP
			return parseAuthHostPort(c, rest)
		case "socks5", "socks5h":
			c.Type = KindSOCKS5
			return parseSocks(c, rest)
		case "socks4":
			c.Type = KindSOCKS4
			return parseSocks(c, rest)
		default:
			return c, fmt.Errorf("不支持的代理协议: %q", scheme)
		}
	}
	// 无 scheme: 默认 socks5
	c.Type = KindSOCKS5
	return parseSocks(c, raw)
}

func parseSocks(c Config, rest string) (Config, error) {
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		// 标准: user:pass@host:port
		authPart := rest[:at]
		hp := rest[at+1:]
		if co := strings.Index(authPart, ":"); co >= 0 {
			c.User = authPart[:co]
			c.Pass = authPart[co+1:]
		} else {
			c.User = authPart
		}
		return setHostPort(c, hp)
	}
	// 非标准: host:port:user:pass (恰好 4 段, 无 @)
	parts := strings.Split(rest, ":")
	if len(parts) == 4 {
		c.Host, c.Port, c.User, c.Pass = parts[0], parts[1], parts[2], parts[3]
		return c, nil
	}
	if len(parts) == 2 {
		c.Host, c.Port = parts[0], parts[1]
		return c, nil
	}
	return c, fmt.Errorf("无法解析 SOCKS 代理地址: %q", rest)
}

func parseAuthHostPort(c Config, rest string) (Config, error) {
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		authPart := rest[:at]
		hp := rest[at+1:]
		if co := strings.Index(authPart, ":"); co >= 0 {
			c.User = authPart[:co]
			c.Pass = authPart[co+1:]
		} else {
			c.User = authPart
		}
		return setHostPort(c, hp)
	}
	return setHostPort(c, rest)
}

func setHostPort(c Config, hp string) (Config, error) {
	if co := strings.LastIndex(hp, ":"); co >= 0 {
		c.Host = hp[:co]
		c.Port = hp[co+1:]
	} else {
		c.Host = hp
	}
	if c.Host == "" {
		return c, fmt.Errorf("代理地址缺少 host")
	}
	return c, nil
}

func (c Config) addr() string { return net.JoinHostPort(c.Host, c.Port) }

func (c Config) socksAuth() *proxy.Auth {
	if c.User == "" {
		return nil
	}
	return &proxy.Auth{User: c.User, Password: c.Pass}
}

// HTTPClient 返回带代理的 *http.Client (直连返回 DefaultClient)。
func (c Config) HTTPClient() (*http.Client, error) {
	if c.Type == KindDirect {
		return http.DefaultClient, nil
	}
	switch c.Type {
	case KindHTTP:
		u := url.URL{Scheme: "http", Host: c.addr()}
		if c.User != "" {
			u.User = url.UserPassword(c.User, c.Pass)
		}
		return &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(&u)},
			Timeout:   30 * time.Second,
		}, nil
	case KindSOCKS5, KindSOCKS4:
		d, err := proxy.SOCKS5("tcp", c.addr(), c.socksAuth(), proxy.Direct)
		if err != nil {
			return nil, err
		}
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return d.Dial(network, addr)
				},
			},
			Timeout: 30 * time.Second,
		}, nil
	}
	return http.DefaultClient, nil
}

// WebSocketDialer 在 base 基础上注入代理, 返回新的 gorilla Dialer (不修改 base)。
func (c Config) WebSocketDialer(base *websocket.Dialer) (*websocket.Dialer, error) {
	d := *base
	if c.Type == KindDirect {
		return &d, nil
	}
	switch c.Type {
	case KindHTTP:
		u := url.URL{Scheme: "http", Host: c.addr()}
		if c.User != "" {
			u.User = url.UserPassword(c.User, c.Pass)
		}
		d.Proxy = http.ProxyURL(&u)
	case KindSOCKS5, KindSOCKS4:
		sd, err := proxy.SOCKS5("tcp", c.addr(), c.socksAuth(), proxy.Direct)
		if err != nil {
			return nil, err
		}
		d.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return sd.Dial(network, addr)
		}
	}
	return &d, nil
}

// HTTPClientFor 便捷封装: 直接传原始代理串。
func HTTPClientFor(raw string) (*http.Client, error) {
	c, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return c.HTTPClient()
}

// WebSocketDialerFor 便捷封装: 直接传原始代理串。
func WebSocketDialerFor(base *websocket.Dialer, raw string) (*websocket.Dialer, error) {
	c, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return c.WebSocketDialer(base)
}
