package ws

import (
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nekoite/go-napcat/config"
	"go.uber.org/zap"
)

// ConnectionState 连接状态
type ConnectionState int32

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

type Client struct {
	logger    *zap.Logger
	setupFunc func() (*websocket.Conn, error)
	conn      *websocket.Conn
	interrupt chan struct{}
	send      chan []byte
	stopped   atomic.Bool
	state     atomic.Int32

	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration

	onRecvMsg         func([]byte)
	onConnectionError func(error)
	onStateChange     func(ConnectionState)
}

func NewConn(logger *zap.Logger, cfg *config.BotConfig, onRecvMsg func([]byte)) (*Client, error) {
	logger = logger.Named("ws")
	setupFunc := func() (*websocket.Conn, error) {
		u := url.URL{Scheme: "ws", Host: fmt.Sprintf("%s:%d", cfg.Ws.Host, cfg.Ws.Port), Path: cfg.Ws.Endpoint}
		logger.Info("connecting to", zap.String("url", u.String()))
		header := http.Header{}
		header.Add("Authorization", fmt.Sprintf("Bearer %s", cfg.Ws.Token))
		conn, _, err := websocket.DefaultDialer.Dial(u.String(), header)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
	conn, err := setupFunc()
	if err != nil {
		logger.Error("dial:", zap.Error(err))
		return nil, err
	}

	interrupt := make(chan struct{}, 1)
	wsConn := &Client{
		logger:     logger,
		setupFunc:  setupFunc,
		conn:       conn,
		interrupt:  interrupt,
		send:       make(chan []byte, 256),
		stopped:    atomic.Bool{},
		state:      atomic.Int32{},
		writeWait:  time.Duration(cfg.Ws.Timeout) * time.Millisecond,
		pongWait:   time.Duration(cfg.Ws.PongTimeout) * time.Millisecond,
		pingPeriod: time.Duration(cfg.Ws.PingPeriod) * time.Millisecond,
		onRecvMsg:  onRecvMsg,
	}
	wsConn.setState(StateDisconnected)
	return wsConn, nil
}

func (c *Client) Start() error {
	c.setState(StateConnecting)
	err := c.setupConn()
	if err != nil {
		c.setState(StateDisconnected)
		return err
	}
	c.setState(StateConnected)
	return nil
}

func (c *Client) GetState() ConnectionState {
	return ConnectionState(c.state.Load())
}

func (c *Client) setState(state ConnectionState) {
	c.state.Store(int32(state))
	if c.onStateChange != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("panic in onStateChange callback", zap.Any("panic", r))
				}
			}()
			c.onStateChange(state)
		}()
	}
}

func (c *Client) SetOnConnectionError(handler func(error)) {
	c.onConnectionError = handler
}

func (c *Client) SetOnStateChange(handler func(ConnectionState)) {
	c.onStateChange = handler
}

// Reconnect 手动触发重连
func (c *Client) Reconnect() error {
	if c.stopped.Load() {
		return fmt.Errorf("connection is stopped, cannot reconnect")
	}
	
	c.logger.Info("manual reconnect triggered")
	c.setState(StateReconnecting)
	
	// 关闭旧连接
	if c.conn != nil {
		_ = c.conn.Close()
	}
	
	var err error
	c.conn, err = c.setupFunc()
	if err != nil {
		c.logger.Error("reconnect failed", zap.Error(err))
		c.setState(StateDisconnected)
		if c.onConnectionError != nil {
			go func(e error) {
				defer func() {
					if r := recover(); r != nil {
						c.logger.Error("panic in onConnectionError callback", zap.Any("panic", r))
					}
				}()
				c.onConnectionError(e)
			}(err)
		}
		return err
	}
	
	err = c.setupConn()
	if err != nil {
		c.logger.Error("setup connection failed", zap.Error(err))
		c.setState(StateDisconnected)
		return err
	}
	
	c.setState(StateConnected)
	c.logger.Info("reconnected successfully")
	return nil
}

func (c *Client) setupConn() error {
	err := c.conn.SetReadDeadline(time.Now().Add(c.pongWait))
	if err != nil {
		c.logger.Error("set read deadline", zap.Error(err))
		return err
	}
	c.conn.SetPongHandler(func(string) error {
		c.logger.Debug("received pong")
		if err := c.conn.SetReadDeadline(time.Now().Add(c.pongWait)); err != nil {
			c.logger.Error("set read deadline", zap.Error(err))
			return err
		}
		return nil
	})
	go c.readPump()
	go c.writePump()
	return nil
}

func (c *Client) readPump() {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("panic in readPump", zap.Any("panic", r), zap.Stack("stack"))
		}
	}()
	
	conn := c.conn
	defer func() {
		_ = conn.Close()
	}()
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Error("wsrecv", zap.Error(err))
			}
			break
		}
		c.logger.Debug("wsrecv", zap.String("message", string(message)))
		if c.onRecvMsg != nil {
			go func(msg []byte) {
				defer func() {
					if r := recover(); r != nil {
						c.logger.Error("panic in onRecvMsg callback", zap.Any("panic", r), zap.Stack("stack"))
					}
				}()
				c.onRecvMsg(msg)
			}(message)
		}
	}
	// 连接断开，设置状态为已断开，不自动重连
	c.setState(StateDisconnected)
	c.logger.Warn("connection closed, waiting for manual reconnect")
	if c.onConnectionError != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("panic in onConnectionError callback", zap.Any("panic", r))
				}
			}()
			c.onConnectionError(fmt.Errorf("connection closed"))
		}()
	}
}

func (c *Client) writePump() {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("panic in writePump", zap.Any("panic", r), zap.Stack("stack"))
		}
	}()
	
	ticker := time.NewTicker(c.pingPeriod)
	conn := c.conn
	defer func() {
		ticker.Stop()
		_ = conn.Close()
	}()
	for {
		select {
		case <-ticker.C:
			if err := c.writeMessage(websocket.PingMessage, nil); err != nil {
				c.logger.Error("wssend", zap.Error(err))
				return
			}
			c.logger.Debug("sent ping")
		case <-c.interrupt:
			c.logger.Info("ws connection close")
			c.stopped.Store(true)
			c.setState(StateClosed)
			err := c.writeMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				c.logger.Error("close", zap.Error(err))
			}
			return
		case message, ok := <-c.send:
			if !ok {
				c.logger.Error("send channel closed")
				_ = c.writeMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			c.logger.Debug("wssend", zap.String("message", string(message)))
			if err := c.writeMessage(websocket.TextMessage, message); err != nil {
				c.logger.Error("wssend", zap.Error(err))
				return
			}
		}
	}
}

func (c *Client) writeMessage(messageType int, data []byte) error {
	err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait))
	if err != nil {
		c.logger.Error("set write deadline", zap.Error(err))
		return err
	}
	return c.conn.WriteMessage(messageType, data)
}

func (c *Client) Close() {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("panic in Close", zap.Any("panic", r))
		}
	}()
	
	if c.stopped.Load() {
		return
	}
	
	select {
	case c.interrupt <- struct{}{}:
	default:
		c.logger.Warn("interrupt channel is full or closed")
	}
}

func (c *Client) Send(msg []byte) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("panic in Send", zap.Any("panic", r))
		}
	}()
	
	if c.stopped.Load() {
		c.logger.Warn("cannot send message: connection is stopped")
		return
	}
	
	select {
	case c.send <- msg:
		// 发送成功
	default:
		c.logger.Warn("send channel is full, message dropped")
	}
}
