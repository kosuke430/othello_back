package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

var jwtSecret = []byte("CHANGE_ME_DEV")

type Claims struct {
	UserID int64 `json:"userId"`
	jwt.RegisteredClaims
}

func buildJWT(userID int64) string {
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, &Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	})
	s, _ := t.SignedString(jwtSecret)
	return s
}

func parseJWT(tok string) (*Claims, error) {
	t, err := jwt.ParseWithClaims(tok, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		return jwtSecret, nil
	})
	if err != nil {
		return nil, err
	}
	if c, ok := t.Claims.(*Claims); ok && t.Valid {
		return c, nil
	}
	return nil, err
}

// ===== WS Hub =====

type Message struct {
	Type string          `json:"type"`
	Mode string          `json:"mode,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Conn struct {
	userID int64
	id     string
	ws     *websocket.Conn
	send   chan []byte
}

type Hub struct {
	mu    sync.RWMutex
	conns map[int64]*Conn
	reg   chan *Conn
	unreg chan *Conn
}

func NewHub() *Hub {
	return &Hub{
		conns: map[int64]*Conn{},
		reg:   make(chan *Conn),
		unreg: make(chan *Conn),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case c := <-h.reg:
			h.mu.Lock()
			h.conns[c.userID] = c
			h.mu.Unlock()
		case c := <-h.unreg:
			h.mu.Lock()
			if cur, ok := h.conns[c.userID]; ok && cur.id == c.id {
				delete(h.conns, c.userID)
			}
			h.mu.Unlock()
			close(c.send)
		}
	}
}

func (h *Hub) SendJSON(userID int64, v any) {
	b, _ := json.Marshal(v)
	h.mu.RLock()
	c := h.conns[userID]
	h.mu.RUnlock()
	if c != nil {
		select {
		case c.send <- b:
		default:
		}
	}
}

// ===== Redis ベース実装 =====

var (
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	hub      = NewHub()

	rdb *redis.Client
	ctx = context.Background()

	// Redis Keys
	keyQueue     = "mm:queue:othello" // ZSET: 待機ユーザー（score=enqueue時刻）
	keySessionFn = func(uid int64) string {
		return "mm:session:" + fmt.Sprintf("%d", uid) // HASH: 接続セッション
	}
)

func main() {
	// Redis 接続
	rdb = redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
		// Password: "", DB: 0,
	})

	// ハブ/マッチング開始
	go hub.Run()
	go matchmakingWorker()

	// 簡易CORS
	withCORS := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}

	http.HandleFunc("/api/login", withCORS(loginHandler))
	http.HandleFunc("/ws", wsHandler)

	log.Println("listening :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	// 毎回ユニークなIDを割り当て（開発用）
	uid := time.Now().UnixNano() // int64
	token := buildJWT(uid)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":  token,
		"userId": uid,
	})
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	claims, err := parseJWT(token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &Conn{
		userID: claims.UserID,
		id:     uuid.NewString(),
		ws:     ws,
		send:   make(chan []byte, 16),
	}
	hub.reg <- c

	// セッション初期化 & TTL
	_ = rdb.HSet(ctx, keySessionFn(c.userID), map[string]any{
		"status":     "idle",
		"connId":     c.id,
		"updated_at": time.Now().Unix(),
	}).Err()
	_ = rdb.Expire(ctx, keySessionFn(c.userID), 60*time.Second).Err()

	go writer(c)
	reader(c)
}

func reader(c *Conn) {
	defer func() {
		// 切断時のクリーンアップ：待機から除外 & セッション状態更新
		_ = rdb.ZRem(ctx, keyQueue, fmt.Sprintf("%d", c.userID)).Err()
		_ = rdb.HSet(ctx, keySessionFn(c.userID), "status", "disconnected").Err()
		hub.unreg <- c
		_ = c.ws.Close()
	}()

	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg Message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "enqueue":
			now := float64(time.Now().UnixMilli())
			// ZSET: score=enqueue時刻, member=userID
			if err := rdb.ZAdd(ctx, keyQueue, redis.Z{Score: now, Member: fmt.Sprintf("%d", c.userID)}).Err(); err == nil {
				// セッション更新
				_ = rdb.HSet(ctx, keySessionFn(c.userID), map[string]any{
					"status":     "waiting",
					"connId":     c.id,
					"updated_at": time.Now().Unix(),
				}).Err()
				_ = rdb.Expire(ctx, keySessionFn(c.userID), 60*time.Second).Err()

				// 待機人数/自分の順位（任意）
				// rank, _ := rdb.ZRank(ctx, keyQueue, c.userID).Result() // 0-based
				rank, _ := rdb.ZRank(ctx, keyQueue, fmt.Sprintf("%d", c.userID)).Result()
				card, _ := rdb.ZCard(ctx, keyQueue).Result()

				hub.SendJSON(c.userID, map[string]any{
					"type":     "enqueued",
					"position": rank + 1,
					"waiting":  card,
				})
			}

		case "cancel":
			_ = rdb.ZRem(ctx, keyQueue, fmt.Sprintf("%d", c.userID)).Err()
			_ = rdb.HSet(ctx, keySessionFn(c.userID), "status", "idle").Err()
			_ = rdb.Expire(ctx, keySessionFn(c.userID), 60*time.Second).Err()
			hub.SendJSON(c.userID, map[string]any{"type": "canceled"})

		case "heartbeat":
			// セッション延命（status は簡易に 'idle'、厳密にやるなら現値維持でもOK）
			_ = rdb.HSet(ctx, keySessionFn(c.userID), map[string]any{
				"status":     "idle",
				"connId":     c.id,
				"updated_at": time.Now().Unix(),
			}).Err()
			_ = rdb.Expire(ctx, keySessionFn(c.userID), 60*time.Second).Err()
			hub.SendJSON(c.userID, map[string]any{"type": "pong"})
		}
	}
}

func writer(c *Conn) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			_ = c.ws.WriteMessage(websocket.TextMessage, msg)
		case <-ticker.C:
			_ = c.ws.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
		}
	}
}

func matchmakingWorker() {
	rand.Seed(time.Now().UnixNano())
	for {
		// 最も待機時間が長い2人を原子的にポップ
		pairs, err := rdb.ZPopMin(ctx, keyQueue, 2).Result()
		if err == nil && len(pairs) == 2 {
			u1 := toInt64(pairs[0].Member)
			u2 := toInt64(pairs[1].Member)

			matchId := uuid.NewString()
			colors := []string{"black", "white"}
			rand.Shuffle(2, func(i, j int) { colors[i], colors[j] = colors[j], colors[i] })

			hub.SendJSON(u1, map[string]any{
				"type": "match_found", "matchId": matchId,
				"opponent": map[string]any{"id": u2}, "side": colors[0],
			})
			hub.SendJSON(u2, map[string]any{
				"type": "match_found", "matchId": matchId,
				"opponent": map[string]any{"id": u1}, "side": colors[1],
			})
			// ここで必要なら Redis に mm:match:<matchId> を記録/EXPIRE、MySQLにもINSERTする

			time.Sleep(50 * time.Millisecond)
			continue
		}
		// 1人だけ取り出してしまった場合は元に戻す
		if err == nil && len(pairs) == 1 {
			_ = rdb.ZAdd(ctx, keyQueue, redis.Z{Score: pairs[0].Score, Member: pairs[0].Member}).Err()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func toInt64(m any) int64 {
	switch v := m.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		var id int64
		_, _ = fmt.Sscanf(v, "%d", &id)
		return id
	default:
		return 0
	}
}
