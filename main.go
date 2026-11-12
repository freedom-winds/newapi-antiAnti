// S6 内容审核管理台 —— 独立小服务
// 直连 new-api 的 PostgreSQL，读写 moderation_settings / moderation_marks / moderation_hits。
// 网关侧（new-api）每 30s 轮询配置与标记集，管理台改完即自动生效。
// 监听 127.0.0.1:8295，由 nginx 反代挂 /moderation-admin/（自带登录 + IP 白名单可选）。
package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

//go:embed admin.html
var adminHTML []byte

// 与网关侧 service/moderation 保持一致的检测提示词（仅用于测试按钮）
const checkerSystemPrompt = `你是S6AI中转站的内容合规审核员。请判断最后的用户消息（结合简短上下文）是否明确属于以下违规类别之一：
- nsfw：露骨的色情/性描写内容
- cyberattack：攻击、入侵、破坏他人网站/系统/账号（爆破、DDoS、漏洞利用、恶意软件、盗号、钓鱼等）
- reverse_web：逆向、绕过或非法爬取未获授权的网站/平台/接口（绕过风控、模拟登录、破解接口签名等）
- reverse_commercial：逆向或破解商业软件（脱壳、绕过授权/付费、注册机等）

判定从宽，不要风吹草动就报警：
- 只有内容明确、蓄意指向上述目的时才判违规，否则一律放行。
- 学习提问、安全研究、CTF/授权靶场、开源软件或用户自有软件的分析、普通软件逆向、公开数据爬取、文学创作、转述新闻、只提到相关词汇但无实施意图，一律放行。
- 拿不准就放行。宁可漏判，不可误判。
只输出一行JSON，不要解释、不要markdown代码块：
{"violation":null}
或
{"violation":"类别代码","confidence":0.0~1.0,"reason":"一句话理由"}`

var (
	db          *sql.DB
	adminUser   string
	adminPass   string
	listenAddr  string
	allowedCIDR []*net.IPNet
	sessions    sync.Map // token -> expiry
)

func main() {
	dsn := os.Getenv("MOD_PG_DSN")
	if dsn == "" {
		log.Fatal("MOD_PG_DSN 未配置（例如 postgres://user:pass@127.0.0.1:5432/new-api?sslmode=disable）")
	}
	adminUser = getenv("MOD_ADMIN_USER", "admin")
	adminPass = os.Getenv("MOD_ADMIN_PASS")
	if adminPass == "" {
		log.Fatal("MOD_ADMIN_PASS 未配置")
	}
	listenAddr = getenv("MOD_LISTEN", "127.0.0.1:8295")
	for _, cidr := range strings.Split(os.Getenv("MOD_ALLOW_CIDR"), ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Fatalf("MOD_ALLOW_CIDR 无效: %s", cidr)
		}
		allowedCIDR = append(allowedCIDR, ipnet)
	}

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatal("数据库连接失败: ", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err = db.Ping(); err != nil {
		log.Fatal("数据库 ping 失败: ", err)
	}
	ensureSchema()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/overview", auth(handleOverview))
	mux.HandleFunc("/api/config", auth(handleConfig))
	mux.HandleFunc("/api/groups", auth(handleGroups))
	mux.HandleFunc("/api/hits", auth(handleHits))
	mux.HandleFunc("/api/hits/review", auth(handleReview))
	mux.HandleFunc("/api/marks", auth(handleMarks))
	mux.HandleFunc("/api/marks/delete", auth(handleMarkDelete))
	mux.HandleFunc("/api/memo", auth(handleMemo))
	mux.HandleFunc("/api/test", auth(handleTest))

	log.Printf("[moderation-console] listening on %s", listenAddr)
	// 兼容挂在 /moderation-admin/ 前缀下被反代：剥掉前缀再路由
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/moderation-admin" {
			http.Redirect(w, r, "/moderation-admin/", http.StatusMovedPermanently)
			return
		}
		if strings.HasPrefix(p, "/moderation-admin/") {
			r.URL.Path = strings.TrimPrefix(p, "/moderation-admin")
		}
		mux.ServeHTTP(w, r)
	})
	if err := http.ListenAndServe(listenAddr, handler); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func ensureSchema() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS moderation_settings(
			id int PRIMARY KEY, config text NOT NULL DEFAULT '', updated_at bigint DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS moderation_marks(
			id serial PRIMARY KEY, user_id bigint NOT NULL DEFAULT 0,
			content_hash varchar(64) NOT NULL DEFAULT '', category varchar(32) DEFAULT '',
			reason text, source varchar(16) DEFAULT 'auto', created_at bigint DEFAULT 0,
			expires_at bigint DEFAULT 0)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_mod_mark_unique ON moderation_marks(user_id, content_hash)`,
		`CREATE TABLE IF NOT EXISTS moderation_hits(
			id serial PRIMARY KEY, created_at bigint DEFAULT 0, request_id varchar(64) DEFAULT '',
			user_id bigint DEFAULT 0, username varchar(64) DEFAULT '', token_id bigint DEFAULT 0,
			token_name varchar(64) DEFAULT '', group_name varchar(64) DEFAULT '',
			model_name varchar(128) DEFAULT '', category varchar(32) DEFAULT '',
			confidence double precision DEFAULT 0, reason text, excerpt text,
			action varchar(16) DEFAULT '', reviewed int DEFAULT 0, note text, latency_ms int DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS moderation_memo(
			id int PRIMARY KEY, content text NOT NULL DEFAULT '', updated_at bigint DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_mod_hits_created ON moderation_hits(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_mod_hits_group ON moderation_hits(group_name)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			log.Fatal("建表失败: ", err, "\n", s)
		}
	}
}

// ==================== 通用 ====================

type apiResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, apiResp{OK: false, Error: msg})
}

func clientIP(r *http.Request) string {
	if x := r.Header.Get("cf-connecting-ip"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	if x := r.Header.Get("x-forwarded-for"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func ipAllowed(r *http.Request) bool {
	if len(allowedCIDR) == 0 {
		return true
	}
	ip := net.ParseIP(clientIP(r))
	if ip == nil {
		return false
	}
	for _, n := range allowedCIDR {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

type authedHandler func(http.ResponseWriter, *http.Request)

func auth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ipAllowed(r) {
			fail(w, 403, "IP not allowed")
			return
		}
		tok := ""
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			tok = strings.TrimSpace(a[7:])
		}
		if tok == "" {
			fail(w, 401, "unauthorized")
			return
		}
		if v, ok := sessions.Load(tok); !ok || v.(int64) < time.Now().Unix() {
			sessions.Delete(tok)
			fail(w, 401, "session expired")
			return
		}
		sessions.Store(tok, time.Now().Unix()+12*3600)
		h(w, r)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/moderation-admin") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(adminHTML)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if !ipAllowed(r) {
		fail(w, 403, "IP not allowed")
		return
	}
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&b); err != nil {
		fail(w, 400, "invalid json")
		return
	}
	if b.Username != adminUser || b.Password != adminPass {
		time.Sleep(500 * time.Millisecond)
		fail(w, 401, "用户名或密码错误")
		return
	}
	tok := fmt.Sprintf("mod-%d-%s", time.Now().UnixNano(), randHex(16))
	sessions.Store(tok, time.Now().Unix()+12*3600)
	writeJSON(w, map[string]any{"ok": true, "token": tok})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ==================== 概览 ====================

func handleOverview(w http.ResponseWriter, r *http.Request) {
	day := dayStart()
	data := map[string]any{}

	// 今日总请求（new-api 消费日志）
	var totalToday int64
	db.QueryRow(`SELECT COUNT(*) FROM logs WHERE type=2 AND created_at >= $1`, day).Scan(&totalToday)
	data["total_today"] = totalToday

	// 今日抽检情况
	var sampled, errs, viol, refused int64
	db.QueryRow(`SELECT COUNT(*) FROM moderation_hits WHERE created_at >= $1`, day).Scan(&sampled)
	db.QueryRow(`SELECT COUNT(*) FROM moderation_hits WHERE created_at >= $1 AND action='error'`, day).Scan(&errs)
	db.QueryRow(`SELECT COUNT(*) FROM moderation_hits WHERE created_at >= $1 AND category <> ''`, day).Scan(&viol)
	db.QueryRow(`SELECT COUNT(*) FROM moderation_hits WHERE created_at >= $1 AND action='refused'`, day).Scan(&refused)
	data["sampled_today"] = sampled
	data["detect_fail_today"] = errs
	data["violation_today"] = viol
	data["refused_today"] = refused
	if sampled > 0 {
		v := float64(sampled-errs) / float64(sampled) * 100
		data["success_rate"] = float64(int(v*10)) / 10
		v2 := float64(viol) / float64(sampled) * 100
		data["violation_ratio"] = float64(int(v2*10)) / 10
	}

	var marksActive int64
	db.QueryRow(`SELECT COUNT(*) FROM moderation_marks WHERE expires_at > $1`, time.Now().Unix()).Scan(&marksActive)
	data["marks_active"] = marksActive

	cfg := loadConfig()
	data["enabled"] = cfg["enabled"]
	data["mode"] = cfg["mode"]

	rows, err := db.Query(`SELECT COALESCE(TO_TIMESTAMP(created_at)::date::text,''), COALESCE(category,''), COUNT(*)
		FROM moderation_hits WHERE action NOT IN ('clean') AND created_at >= $1
		GROUP BY 1,2 ORDER BY 1 DESC LIMIT 60`, time.Now().Unix()-30*86400)
	if err == nil {
		defer rows.Close()
		type stat struct {
			Day      string `json:"day"`
			Category string `json:"category"`
			Count    int64  `json:"count"`
		}
		stats := []stat{}
		for rows.Next() {
			var s stat
			var dayS, cat sql.NullString
			var cnt int64
			if err := rows.Scan(&dayS, &cat, &cnt); err == nil {
				s.Day, s.Category, s.Count = dayS.String, cat.String, cnt
				stats = append(stats, s)
			}
		}
		data["stats"] = stats
	}
	writeJSON(w, apiResp{OK: true, Data: data})
}

func dayStart() int64 {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
}

// ==================== 配置 ====================

func loadConfig() map[string]any {
	var cfgStr string
	err := db.QueryRow(`SELECT config FROM moderation_settings WHERE id=1`).Scan(&cfgStr)
	if err != nil || cfgStr == "" {
		return map[string]any{"enabled": false}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(cfgStr), &m); err != nil {
		return map[string]any{"enabled": false}
	}
	return m
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, apiResp{OK: true, Data: loadConfig()})
	case http.MethodPost:
		var b struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&b); err != nil {
			fail(w, 400, "invalid json")
			return
		}
		var check map[string]any
		if err := json.Unmarshal(b.Config, &check); err != nil {
			fail(w, 400, "config 不是合法 JSON 对象")
			return
		}
		if m, ok := check["mode"].(string); ok && m != "off" && m != "observe" && m != "enforce" {
			fail(w, 400, "mode 只能是 off/observe/enforce")
			return
		}
		raw, _ := json.Marshal(check)
		_, err := db.Exec(`
			INSERT INTO moderation_settings(id, config, updated_at) VALUES(1, $1, $2)
			ON CONFLICT (id) DO UPDATE SET config = EXCLUDED.config, updated_at = EXCLUDED.updated_at`,
			string(raw), time.Now().Unix())
		if err != nil {
			fail(w, 500, "保存失败: "+err.Error())
			return
		}
		writeJSON(w, apiResp{OK: true})
	default:
		fail(w, 405, "method not allowed")
	}
}

func handleGroups(w http.ResponseWriter, r *http.Request) {
	set := map[string]bool{"default": true}
	if row := db.QueryRow(`SELECT value FROM options WHERE key='GroupRatio'`); row != nil {
		var v string
		if err := row.Scan(&v); err == nil {
			var m map[string]any
			if json.Unmarshal([]byte(v), &m) == nil {
				for g := range m {
					set[g] = true
				}
			}
		}
	}
	rows, err := db.Query(`SELECT DISTINCT group_name FROM moderation_hits WHERE group_name <> '' LIMIT 200`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var g string
			if rows.Scan(&g) == nil && g != "" {
				set[g] = true
			}
		}
	}
	if cfg := loadConfig(); cfg != nil {
		if groups, ok := cfg["groups"].(map[string]any); ok {
			for g := range groups {
				set[g] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	writeJSON(w, apiResp{OK: true, Data: map[string]any{"groups": out}})
}

// ==================== 命中记录 ====================

func handleHits(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where := []string{"1=1"}
	args := []any{}
	i := 1
	for _, cond := range []struct{ key, col string }{
		{"group", "group_name"}, {"category", "category"}, {"action", "action"},
	} {
		if v := q.Get(cond.key); v != "" {
			where = append(where, fmt.Sprintf("%s = $%d", cond.col, i))
			args = append(args, v)
			i++
		}
	}
	if v := q.Get("user_id"); v != "" {
		if uid, err := strconv.ParseInt(v, 10, 64); err == nil {
			where = append(where, fmt.Sprintf("user_id = $%d", i))
			args = append(args, uid)
			i++
		}
	}
	if v := q.Get("reviewed"); v != "" {
		if rv, err := strconv.Atoi(v); err == nil {
			where = append(where, fmt.Sprintf("reviewed = $%d", i))
			args = append(args, rv)
			i++
		}
	}
	limit, offset := 50, 0
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	var total int64
	db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM moderation_hits WHERE %s`, strings.Join(where, " AND ")), args...).Scan(&total)
	args = append(args, limit, offset)
	rows, err := db.Query(fmt.Sprintf(
		`SELECT id, created_at, request_id, user_id, username, token_name, group_name, model_name,
		        category, confidence, reason, excerpt, action, reviewed, note, latency_ms
		 FROM moderation_hits WHERE %s ORDER BY id DESC LIMIT $%d OFFSET $%d`,
		strings.Join(where, " AND "), i, i+1), args...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type hit struct {
		Id         int64   `json:"id"`
		CreatedAt  int64   `json:"created_at"`
		RequestId  string  `json:"request_id"`
		UserId     int64   `json:"user_id"`
		Username   string  `json:"username"`
		TokenName  string  `json:"token_name"`
		GroupName  string  `json:"group_name"`
		ModelName  string  `json:"model_name"`
		Category   string  `json:"category"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
		Excerpt    string  `json:"excerpt"`
		Action     string  `json:"action"`
		Reviewed   int     `json:"reviewed"`
		Note       string  `json:"note"`
		LatencyMs  int     `json:"latency_ms"`
	}
	hits := []hit{}
	for rows.Next() {
		var h hit
		var reqID, un, tn, gn, mn, cat, rs, ex, ac, note sql.NullString
		var conf sql.NullFloat64
		var rv, lat sql.NullInt64
		if err := rows.Scan(&h.Id, &h.CreatedAt, &reqID, &h.UserId, &un, &tn, &gn, &mn, &cat, &conf, &rs, &ex, &ac, &rv, &note, &lat); err == nil {
			h.RequestId, h.Username, h.TokenName, h.GroupName = reqID.String, un.String, tn.String, gn.String
			h.ModelName, h.Category, h.Reason, h.Excerpt = mn.String, cat.String, rs.String, ex.String
			h.Action, h.Note = ac.String, note.String
			h.Confidence, h.Reviewed, h.LatencyMs = conf.Float64, int(rv.Int64), int(lat.Int64)
			hits = append(hits, h)
		}
	}
	writeJSON(w, apiResp{OK: true, Data: map[string]any{"hits": hits, "total": total}})
}

func handleReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	var b struct {
		Id       int64  `json:"id"`
		Reviewed int    `json:"reviewed"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&b); err != nil || b.Id <= 0 {
		fail(w, 400, "invalid json")
		return
	}
	if b.Reviewed != 0 && b.Reviewed != 1 && b.Reviewed != 2 {
		fail(w, 400, "reviewed 只能是 0/1/2")
		return
	}
	_, err := db.Exec(`UPDATE moderation_hits SET reviewed=$1, note=$2 WHERE id=$3`, b.Reviewed, b.Note, b.Id)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, apiResp{OK: true})
}

// ==================== 标记管理 ====================

func handleMarks(w http.ResponseWriter, r *http.Request) {
	limit, offset := 100, 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 500 {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = v
	}
	var total int64
	db.QueryRow(`SELECT COUNT(*) FROM moderation_marks`).Scan(&total)
	rows, err := db.Query(`SELECT id, user_id, content_hash, category, reason, source, created_at, expires_at
		FROM moderation_marks ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type mark struct {
		Id          int64  `json:"id"`
		UserId      int64  `json:"user_id"`
		ContentHash string `json:"content_hash"`
		Category    string `json:"category"`
		Reason      string `json:"reason"`
		Source      string `json:"source"`
		CreatedAt   int64  `json:"created_at"`
		ExpiresAt   int64  `json:"expires_at"`
	}
	marks := []mark{}
	for rows.Next() {
		var m mark
		var ch, cat, rs, src sql.NullString
		if err := rows.Scan(&m.Id, &m.UserId, &ch, &cat, &rs, &src, &m.CreatedAt, &m.ExpiresAt); err == nil {
			m.ContentHash, m.Category, m.Reason, m.Source = ch.String, cat.String, rs.String, src.String
			marks = append(marks, m)
		}
	}
	writeJSON(w, apiResp{OK: true, Data: map[string]any{"marks": marks, "total": total}})
}

func handleMarkDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	var b struct {
		Id int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&b); err != nil || b.Id <= 0 {
		fail(w, 400, "invalid json")
		return
	}
	if _, err := db.Exec(`DELETE FROM moderation_marks WHERE id=$1`, b.Id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, apiResp{OK: true})
}

// ==================== 检测通道测试 ====================

func handleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	var b struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&b); err != nil || strings.TrimSpace(b.Text) == "" {
		fail(w, 400, "text 必填")
		return
	}
	cfg := loadConfig()
	ep, _ := cfg["endpoint"].(map[string]any)
	base, _ := ep["base_url"].(string)
	key, _ := ep["api_key"].(string)
	model, _ := ep["model"].(string)
	if base == "" || key == "" || model == "" {
		fail(w, 400, "检测通道未配置完整（base_url/api_key/model）")
		return
	}
	payload := map[string]any{
		"model": model, "temperature": 0, "max_tokens": 2048,
		"messages": []map[string]string{
			{"role": "system", "content": checkerSystemPrompt},
			{"role": "user", "content": b.Text},
		},
	}
	buf, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 90 * time.Second}
	req, _ := http.NewRequest("POST", strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, apiResp{OK: true, Data: map[string]any{"error": err.Error(), "latency_ms": time.Since(t0).Milliseconds()}})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(t0).Milliseconds()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(body, &out)
	content := ""
	if len(out.Choices) > 0 {
		content = out.Choices[0].Message.Content
	}
	writeJSON(w, apiResp{OK: true, Data: map[string]any{
		"status": resp.StatusCode, "latency_ms": latency,
		"content": content, "raw": string(body[:min(len(body), 2000)]),
	}})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ==================== 备忘录 ====================

func handleMemo(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var content string
		db.QueryRow(`SELECT content FROM moderation_memo WHERE id=1`).Scan(&content)
		writeJSON(w, apiResp{OK: true, Data: map[string]any{"content": content}})
	case http.MethodPost:
		var b struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&b); err != nil {
			fail(w, 400, "invalid json")
			return
		}
		_, err := db.Exec(`
			INSERT INTO moderation_memo(id, content, updated_at) VALUES(1, $1, $2)
			ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content, updated_at = EXCLUDED.updated_at`,
			b.Content, time.Now().Unix())
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, apiResp{OK: true})
	default:
		fail(w, 405, "method not allowed")
	}
}
