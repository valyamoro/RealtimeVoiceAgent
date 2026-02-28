package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type Socket struct {
	apiKey  string
	wsURL   string
	conn    *websocket.Conn
	onMsg   func(map[string]interface{})
	stopCh  chan struct{}
	stopOnce sync.Once
	lock     sync.Mutex
}

func NewSocket(apiKey, wsURL string) *Socket {
	return &Socket{
		apiKey: apiKey,
		wsURL:  wsURL,
		stopCh: make(chan struct{}),
	}
}

func (s *Socket) SetMessageHandler(handler func(map[string]interface{})) {
	s.onMsg = handler
}

func (s *Socket) Connect() error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.apiKey)
	header.Set("OpenAI-Beta", "realtime=v1")

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(s.wsURL, header)
	if err != nil {
		return err
	}

	s.conn = conn
	log.Println("Connected to WebSocket")

	go s.recvLoop()

	return nil
}

func (s *Socket) recvLoop() {
	defer log.Println("Receiver thread exiting")

	for {
		select {
		case <-s.stopCh:
			return
		default:
			_, message, err := s.conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("WebSocket error: %v", err)
				} else {
					log.Println("WebSocket closed by server")
				}
				return
			}

			if len(message) > 0 && s.onMsg != nil {
				var msg map[string]interface{}
				if err := json.Unmarshal(message, &msg); err != nil {
					log.Printf("Failed to parse message: %v", err)
					continue
				}
				s.onMsg(msg)
			}
		}
	}
}

func (s *Socket) Send(msg interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.conn == nil || s.conn.UnderlyingConn() == nil {
		return
	}

	if err := s.conn.WriteJSON(msg); err != nil {
		log.Printf("Send error: %v", err)
	}
}

func (s *Socket) Close() {
	s.stopOnce.Do(func() {
		close(s.stopCh)

		s.lock.Lock()
		defer s.lock.Unlock()

		if s.conn != nil {
			s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			s.conn.Close()
			log.Println("WebSocket fully closed")
		}
	})
}
