package notifier

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hh/heliox-mon/internal/config"
)

const testBotToken = "123456:SECRET-token"

func newTestNotifier(apiBase string, timeout time.Duration) *Notifier {
	n := New(&config.Config{TelegramBotToken: testBotToken, TelegramChatID: "1"}, nil)
	n.apiBase = apiBase
	n.client.Timeout = timeout
	return n
}

// 对端建连后不响应时必须按时返回：预警在日汇总循环里同步发送，挂住就会拖停整个汇总
func TestSendTelegramTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	n := newTestNotifier(srv.URL, 100*time.Millisecond)
	start := time.Now()
	err := n.sendTelegram("hi")
	if err == nil {
		t.Fatal("对端不响应时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Errorf("错误信息泄露了 bot token: %v", err)
	}
}

// 网络层错误的文本默认带完整请求 URL（含 token），会进日志和页面回显
func TestSendTelegramNetworkErrorHidesToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	n := newTestNotifier("http://"+addr, time.Second)
	err = n.sendTelegram("hi")
	if err == nil {
		t.Fatal("连接被拒绝时应返回错误")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Errorf("错误信息泄露了 bot token: %v", err)
	}
}

func TestSendTelegramAPIErrorDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+testBotToken+"/") {
			t.Errorf("请求路径 %q 不含 token", r.URL.Path)
		}
		http.Error(w, `{"ok":false,"description":"Bad Request: chat not found"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	err := newTestNotifier(srv.URL, time.Second).sendTelegram("hi")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("err = %v，期望带上 Telegram 返回的原因", err)
	}
}
