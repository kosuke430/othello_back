package main

import (
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
)

type Conn struct {
	userID int64
	sid    string
	ws     *websocket.Conn
	send   chan []byte // 送信は writer goroutine 経由で1本化
}

type Hub struct {
	mu     sync.RWMutex
	byUser map[int64]map[string]*Conn // userID -> sid -> Conn
}

func NewHub() *Hub {
	return &Hub{byUser: make(map[int64]map[string]*Conn)}
}

// 追加（同一 (userID,sid) の旧接続があれば閉じて置換）
func (h *Hub) Add(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.byUser[c.userID] == nil {
		h.byUser[c.userID] = make(map[string]*Conn)
	}
	if old := h.byUser[c.userID][c.sid]; old != nil && old != c {
		// 古い接続を安全にクローズ（writer/read 側での defer 競合を避けるため send close 等は任意実装）
		_ = old.ws.Close()
	}
	h.byUser[c.userID][c.sid] = c
}

// 削除（“自分が現役”のときだけ消す：再接続レースに強い）
func (h *Hub) RemoveIfSame(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if m := h.byUser[c.userID]; m != nil {
		if cur, ok := m[c.sid]; ok && cur == c {
			delete(m, c.sid)
			if len(m) == 0 {
				delete(h.byUser, c.userID)
			}
		}
	}
}

// 送信（特定の (userID,sid) へ）
func (h *Hub) SendTo(userID int64, sid string, v any) {
	b, _ := json.Marshal(v)

	h.mu.RLock()
	c := h.byUser[userID][sid]
	h.mu.RUnlock()

	if c == nil {
		return
	}
	select {
	case c.send <- b:
	default:
		// バッファ満杯時は捨てる/切断する等ポリシーに応じて
	}
}

// 同一 user の全 sid へ送信（必要なら）
func (h *Hub) SendToUser(userID int64, v any) {
	b, _ := json.Marshal(v)

	h.mu.RLock()
	targets := h.byUser[userID]
	h.mu.RUnlock()

	for _, c := range targets {
		select {
		case c.send <- b:
		default:
		}
	}
}
