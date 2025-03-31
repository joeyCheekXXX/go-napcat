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

type Client struct {
	logger    *zap.Logger
	setupFunc func() (*websocket.Conn, error)
	conn      *websocket.Conn
	interrupt chan struct{}
	send      chan []byte
	stopped   atomic.Bool

	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration

	onRecvMsg func([]byte)
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
		writeWait:  time.Duration(cfg.Ws.Timeout) * time.Millisecond,
		pongWait:   time.Duration(cfg.Ws.PongTimeout) * time.Millisecond,
		pingPeriod: time.Duration(cfg.Ws.PingPeriod) * time.Millisecond,
		onRecvMsg:  onRecvMsg,
	}
	return wsConn, nil
}

func (c *Client) Start() error {
	return c.setupConn()
}

func (c *Client) retry() error {
	if c.stopped.Load() {
		return nil
	}
	var err error
	c.logger.Warn("connection closed unexpectedly, reconnecting...")
	c.conn, err = c.setupFunc()
	if err != nil {
		c.logger.Error("failed to reconnect", zap.Error(err))
		return err
	}
	return c.setupConn()
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
			go c.onRecvMsg(message)
		}
	}
	err := c.retry()
	if err != nil {
		return
	}
}

func (c *Client) writePump() {
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
	c.interrupt <- struct{}{}
}

func (c *Client) Send(msg []byte) {
	c.send <- msg
}
