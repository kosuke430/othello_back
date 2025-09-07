func matchmakingWorker() {
	for {
		// Redisから2人分のセッションIDをZPOP
		pairs, err := rdb.ZPopMin(ctx, "mm:queue:othello", 2).Result()
		if err != nil || len(pairs) < 2 {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		sid1 := pairs[0].Member.(string)
		sid2 := pairs[1].Member.(string)

		// sid から userID を取得（HGET mm:sessid:<sid> userID）
		uid1, _ := rdb.HGet(ctx, "mm:sessid:"+sid1, "userID").Int64()
		uid2, _ := rdb.HGet(ctx, "mm:sessid:"+sid2, "userID").Int64()

		// 接続情報を Hub から取得
		conn1 := hub.Get(uid1, sid1)
		conn2 := hub.Get(uid2, sid2)

		if conn1 == nil || conn2 == nil {
			// どちらかが接続切れてたら中止（戻す処理なども可）
			continue
		}

		matchID := uuid.NewString()
		sides := []string{"black", "white"}
		rand.Shuffle(len(sides), func(i, j int) { sides[i], sides[j] = sides[j], sides[i] })

		// 双方にマッチング通知
		conn1.send <- toJSON(map[string]any{
			"type":     "match_found",
			"matchId":  matchID,
			"side":     sides[0],
			"opponent": map[string]any{"id": uid2},
		})
		conn2.send <- toJSON(map[string]any{
			"type":     "match_found",
			"matchId":  matchID,
			"side":     sides[1],
			"opponent": map[string]any{"id": uid1},
		})

		// 必要なら match 情報を Redis に保存
	}
}
