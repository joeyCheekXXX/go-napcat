package gonapcat

import (
	"testing"
	"time"

	"github.com/nekoite/go-napcat/config"
	"github.com/nekoite/go-napcat/event"
	"github.com/nekoite/go-napcat/ws"
	"go.uber.org/zap"
)

// 实现连接事件处理器
type MyConnectionHandler struct {
	t   *testing.T
	bot *Bot
}

func (h *MyConnectionHandler) OnConnectionError(err error) {
	h.t.Logf("连接错误: %v", err)

	// 外部决定是否重连以及何时重连
	// 这里演示：等待 5 秒后尝试重连
	go func() {
		time.Sleep(5 * time.Second)
		h.t.Log("尝试重新连接...")
		err := h.bot.Reconnect()
		if err != nil {
			h.t.Logf("重连失败: %v", err)
			// 可以继续重试或采取其他措施
		} else {
			h.t.Log("重连成功")
		}
	}()
}

func (h *MyConnectionHandler) OnStateChange(state ws.ConnectionState) {
	stateStr := ""
	switch state {
	case ws.StateDisconnected:
		stateStr = "已断开"
	case ws.StateConnecting:
		stateStr = "连接中"
	case ws.StateConnected:
		stateStr = "已连接"
	case ws.StateReconnecting:
		stateStr = "重连中"
	case ws.StateClosed:
		stateStr = "已关闭"
	}
	h.t.Logf("连接状态变化: %s", stateStr)
}

func TestConnectionWithPanicProtection(t *testing.T) {
	conf := config.DefaultBotConfig(1, "[qSX*&Z-c{]|e?v3").WithWs("127.0.0.1", 3001, "")
	bot, err := NewBot(conf)
	if err != nil {
		t.Errorf("NewBot failed, error: %s", err)
		return
	}

	// 设置连接事件处理器
	handler := &MyConnectionHandler{t: t, bot: bot}
	bot.SetConnectionEventHandler(handler)

	bot.RegisterHandlerNotice(func(e event.IEvent) {
		t.Logf("Received event %+v", e)
	})

	bot.RegisterHandlerPrivateMessage(func(e event.IEvent) {
		bot.Logger().Info("Received private message", zap.Any("event", e.(*event.PrivateMessageEvent)))
	})

	err = bot.Start()
	if err != nil {
		t.Fatal("Start failed")
		return
	}

	// 监控连接状态
	go func() {
		ticker := time.NewTicker(time.Second * 10)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// 检查连接状态
				if bot.IsConnected() {
					// 现在可以安全地调用 API
					status, err := bot.Api().GetStatus()
					if err != nil {
						t.Logf("GetStatus failed, error: %s", err)
					} else {
						t.Logf("server status: %+v", status)
					}
				} else {
					state := bot.GetConnectionState()
					t.Logf("Bot is not connected, current state: %d", state)
				}
			}
		}
	}()

	// 阻塞主线程
	select {}
}

// TestManualReconnectWithRetry 演示带重试策略的手动重连
func TestManualReconnectWithRetry(t *testing.T) {
	conf := config.DefaultBotConfig(1, "[qSX*&Z-c{]|e?v3").WithWs("127.0.0.1", 3001, "")
	bot, err := NewBot(conf)
	if err != nil {
		t.Errorf("NewBot failed, error: %s", err)
		return
	}

	// 实现带重试的重连策略
	bot.SetConnectionEventHandler(&ReconnectWithRetryHandler{
		t:          t,
		bot:        bot,
		maxRetries: 5,
		baseDelay:  time.Second * 2,
	})

	err = bot.Start()
	if err != nil {
		t.Fatal("Start failed")
		return
	}

	// 阻塞主线程
	select {}
}

// ReconnectWithRetryHandler 实现了带指数退避的重连策略
type ReconnectWithRetryHandler struct {
	t          *testing.T
	bot        *Bot
	maxRetries int
	baseDelay  time.Duration
	retryCount int
}

func (h *ReconnectWithRetryHandler) OnConnectionError(err error) {
	h.t.Logf("连接错误: %v", err)

	// 重置重试计数并开始重连
	h.retryCount = 0
	h.retryReconnect()
}

func (h *ReconnectWithRetryHandler) retryReconnect() {
	if h.retryCount >= h.maxRetries {
		h.t.Logf("已达到最大重试次数 %d，停止重连", h.maxRetries)
		return
	}

	h.retryCount++

	// 计算退避时间（指数退避）
	delay := h.baseDelay * time.Duration(1<<uint(h.retryCount-1))
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}

	h.t.Logf("第 %d 次重连尝试，等待 %v...", h.retryCount, delay)

	go func() {
		time.Sleep(delay)

		err := h.bot.Reconnect()
		if err != nil {
			h.t.Logf("第 %d 次重连失败: %v", h.retryCount, err)
			// 继续重试
			h.retryReconnect()
		} else {
			h.t.Logf("重连成功")
			h.retryCount = 0 // 重置计数
		}
	}()
}

func (h *ReconnectWithRetryHandler) OnStateChange(state ws.ConnectionState) {
	stateStr := ""
	switch state {
	case ws.StateDisconnected:
		stateStr = "已断开"
	case ws.StateConnecting:
		stateStr = "连接中"
	case ws.StateConnected:
		stateStr = "已连接"
		h.retryCount = 0 // 连接成功，重置计数
	case ws.StateReconnecting:
		stateStr = "重连中"
	case ws.StateClosed:
		stateStr = "已关闭"
	}
	h.t.Logf("连接状态变化: %s", stateStr)
}
