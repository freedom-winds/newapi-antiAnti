#!/usr/bin/env bash
# ============================================================================
# newapi-antiAnti 一键部署 —— 独立的内容审核管理面板（不修改 new-api 本体）
#
# 交互式：  sudo bash deploy.sh
# 非交互：  sudo bash deploy.sh --domain api.example.com \
#             --dsn "postgresql://user:pass@127.0.0.1:5432/new-api?sslmode=disable" \
#             [--user admin] [--password xxx] [--port 28906] [--path /anti-anti] \
#             [--conf-dir /etc/nginx/conf.d | --conf-file /path/to/xxx.conf]
#
# 行为：
#   1. 未检测到 nginx 直接取消
#   2. 默认端口 28906，被占用自动顺延
#   3. 编译面板（无 Go 时回退到下载 GitHub Release 二进制）
#   4. systemd 托管，只监听 127.0.0.1
#   5. nginx 自动注入 location（默认在 /etc/nginx/conf.d 与 sites-enabled 里
#      自动定位该域名的 server 块；也可用 --conf-dir/--conf-file 指定），
#      注入前备份、nginx -t 失败自动还原
# ============================================================================
set -euo pipefail

PORT_BASE=28906
INSTALL_DIR=/opt/newapi-antiAnti
SERVICE=newapi-antianti
BIN_NAME=antianti-console
REPO_RAW="https://github.com/freedom-winds/newapi-antiAnti/releases/latest/download"
URL_PATH="/anti-anti"

# ---------------- 参数 ----------------
DOMAIN=""; DSN=""; ADMIN_USER="admin"; ADMIN_PASS=""; PORT=""; SCOPE=""; CONF_TARGET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --domain)     DOMAIN="$2"; shift 2 ;;
    --dsn)        DSN="$2"; shift 2 ;;
    --user)       ADMIN_USER="$2"; shift 2 ;;
    --password)   ADMIN_PASS="$2"; shift 2 ;;
    --port)       PORT="$2"; shift 2 ;;
    --path)       URL_PATH="${2%/}"; shift 2 ;;
    --conf-dir)   SCOPE="dir"; CONF_TARGET="$2"; shift 2 ;;
    --conf-file)  SCOPE="file"; CONF_TARGET="$2"; shift 2 ;;
    *) echo "未知参数: $1"; exit 1 ;;
  esac
done

[ "$(id -u)" = "0" ] || { echo "请用 root 运行（需要写 nginx 配置和 systemd）"; exit 1; }

# ---------------- 1. nginx 检测 ----------------
if ! command -v nginx >/dev/null 2>&1; then
  echo "[x] 未检测到 nginx，已取消部署。请先安装 nginx 后重试。"
  exit 1
fi
echo "[ok] nginx: $(nginx -v 2>&1)"

# ---------------- 2. 交互补齐参数 ----------------
[ -n "$DOMAIN" ] || read -rp "请输入域名（已有 nginx server 配置的那个，如 api.example.com）: " DOMAIN
[ -n "$DOMAIN" ] || { echo "域名必填"; exit 1; }
[ -n "$DSN" ]    || read -rp "请输入 PostgreSQL 连接串（new-api 的库，如 postgresql://user:pass@127.0.0.1:5432/new-api?sslmode=disable）: " DSN
case "$DSN" in
  postgres://*|postgresql://*) ;;
  *) echo "DSN 必须以 postgres:// 或 postgresql:// 开头（面板目前只支持 PostgreSQL）"; exit 1 ;;
esac
[ -n "$ADMIN_PASS" ] || ADMIN_PASS="$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)"

# 端口：默认 28906，被占用顺延
if [ -z "$PORT" ]; then
  PORT=$PORT_BASE
  while : ; do
    if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${PORT}$"; then
      echo "[..] 端口 $PORT 被占用，顺延到 $((PORT+1))"
      PORT=$((PORT+1))
    else
      break
    fi
  done
fi
echo "[ok] 面板端口: $PORT"

# nginx 配置写入范围：默认 conf.d + sites-enabled 自动定位；可指定目录或文件
if [ -z "$SCOPE" ]; then
  echo
  echo "nginx 配置写入方式："
  echo "  [1] 默认：自动在 /etc/nginx/conf.d 和 /etc/nginx/sites-enabled 中定位该域名的 server 块"
  echo "  [2] 指定 nginx 配置目录或文件（你的 server 块所在位置）"
  read -rp "选择 [1/2]（回车=1）: " c
  case "$c" in
    2) read -rp "路径（目录或单个 .conf 文件）: " CONF_TARGET
       if [ -d "$CONF_TARGET" ]; then SCOPE="dir"; else SCOPE="file"; fi ;;
    *) SCOPE="dir"; CONF_TARGET="/etc/nginx/conf.d /etc/nginx/sites-enabled" ;;
  esac
fi

# ---------------- 3. 编译/获取二进制 ----------------
echo "[..] 准备面板二进制…"
mkdir -p "$INSTALL_DIR"
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
BUILT=0
if command -v go >/dev/null 2>&1; then
  echo "[..] 使用 $(go version) 从源码编译…"
  if (cd "$SRC_DIR" && CGO_ENABLED=0 go build -o "$INSTALL_DIR/$BIN_NAME" .); then
    BUILT=1
  else
    echo "[!] 源码编译失败，尝试回退到 Release 二进制"
  fi
fi
if [ "$BUILT" != "1" ]; then
  if [ -f "$SRC_DIR/antianti-console-linux-amd64" ]; then
    echo "[..] 使用仓库自带的预编译二进制"
    cp "$SRC_DIR/antianti-console-linux-amd64" "$INSTALL_DIR/$BIN_NAME"
    BUILT=1
  else
    echo "[..] 下载 Release 二进制…"
    curl -fsSL --retry 3 -o "$INSTALL_DIR/$BIN_NAME" "$REPO_RAW/antianti-console-linux-amd64" \
      || { echo "[x] 下载失败：请手动从 https://github.com/freedom-winds/newapi-antiAnti/releases 获取后放到 $INSTALL_DIR/$BIN_NAME"; exit 1; }
  fi
fi
chmod +x "$INSTALL_DIR/$BIN_NAME"
echo "[ok] 二进制: $INSTALL_DIR/$BIN_NAME"

# ---------------- 4. .env + systemd ----------------
umask 077
cat > "$INSTALL_DIR/.env" <<EOF
MOD_PG_DSN=$DSN
MOD_ADMIN_USER=$ADMIN_USER
MOD_ADMIN_PASS=$ADMIN_PASS
MOD_LISTEN=127.0.0.1:$PORT
EOF
umask 022

cat > /etc/systemd/system/${SERVICE}.service <<EOF
[Unit]
Description=newapi-antiAnti 内容审核管理面板
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$INSTALL_DIR/.env
ExecStart=$INSTALL_DIR/$BIN_NAME
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1
systemctl restart "$SERVICE"
sleep 2
if [ "$(systemctl is-active "$SERVICE")" != "active" ]; then
  echo "[x] 服务启动失败，日志：journalctl -u $SERVICE -n 20"; exit 1
fi
if ! curl -fsS -o /dev/null "http://127.0.0.1:$PORT/"; then
  echo "[x] 面板未在本机 $PORT 响应"; journalctl -u "$SERVICE" -n 10 --no-pager; exit 1
fi
echo "[ok] 服务已启动（仅监听 127.0.0.1:$PORT）"

# ---------------- 5. nginx 注入 location ----------------
AA_DOMAIN="$DOMAIN" AA_SCOPE="$SCOPE" AA_TARGET="$CONF_TARGET" AA_PATH="$URL_PATH" AA_PORT="$PORT" python3 - <<'PYEOF'
import os, sys, shutil, subprocess, time

domain   = os.environ["AA_DOMAIN"]
scope    = os.environ["AA_SCOPE"]
target   = os.environ["AA_TARGET"]
url_path = os.environ["AA_PATH"]
port     = os.environ["AA_PORT"]
marker   = "# newapi-antiAnti panel (auto-injected)"

snippet = f"""
    {marker}
    location = {url_path} {{ return 301 {url_path}/; }}
    location {url_path}/ {{
        proxy_pass http://127.0.0.1:{port}/;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        add_header Cache-Control "no-cache";
    }}
"""

candidates = []
if scope == "file":
    candidates = [target]
else:
    for d in target.split():
        if not os.path.isdir(d):
            continue
        for root, _, names in os.walk(d):
            for n in names:
                if n.endswith(".conf") or n.endswith(".conf.enabled") or n == "default":
                    candidates.append(os.path.join(root, n))

matches = []
for f in candidates:
    try:
        text = open(f, encoding="utf-8", errors="replace").read()
    except Exception:
        continue
    if marker in text:
        print("[..] %s 已注入过，跳过" % f)
        continue
    for line in text.splitlines():
        s = line.strip()
        if s.startswith("server_name") and domain in s:
            matches.append(f)
            break

def inject(path):
    lines = open(path, encoding="utf-8", errors="replace").read().splitlines(keepends=True)
    idx = next(i for i, l in enumerate(lines) if "server_name" in l and domain in l)
    s = idx
    while s >= 0 and "{" not in lines[s]:
        s -= 1
    depth, e = 0, len(lines) - 1
    for i in range(s, len(lines)):
        depth += lines[i].count("{") - lines[i].count("}")
        if depth == 0 and i > s:
            e = i
            break
    ins = e
    for i in range(s, e):
        if lines[i].strip().startswith("location "):
            ins = i
            break
    lines.insert(ins, snippet)
    return "".join(lines)

if not matches:
    print("[!] 没找到包含域名 %s 的 server 块。请手动在对应 server 块里加：" % domain)
    print(snippet)
    sys.exit(0)

for f in matches:
    bak = f + ".bak-antianti-" + time.strftime("%Y%m%d%H%M%S")
    shutil.copy2(f, bak)
    try:
        # 先算好新内容再写回：open("w") 会立刻截断文件，不能和 inject(f) 同表达式
        new_text = inject(f)
        with open(f, "w", encoding="utf-8") as fh:
            fh.write(new_text)
        r = subprocess.run(["nginx", "-t"], capture_output=True, text=True)
        if r.returncode != 0:
            shutil.copy2(bak, f)
            print("[x] nginx -t 失败已还原，请手动添加：\n" + snippet + r.stderr[-400:])
            sys.exit(1)
        print("[ok] 已注入 %s（备份 %s）" % (f, bak))
    except Exception as ex:
        shutil.copy2(bak, f)
        print("[x] 注入失败已还原: %s" % ex)
        raise

r = subprocess.run(["nginx", "-s", "reload"], capture_output=True, text=True)
print("[ok] nginx 已重载" if r.returncode == 0 else "[x] reload 失败: " + r.stderr[-200:])
PYEOF

# ---------------- 6. 完成摘要 ----------------
cat <<EOF

==================== 部署完成 ====================
面板地址 : https://$DOMAIN$URL_PATH/
登录账号 : $ADMIN_USER
登录密码 : $ADMIN_PASS
监听     : 127.0.0.1:$PORT（仅本机，公网走 nginx）
安装目录 : $INSTALL_DIR
服务管理 : systemctl {status|restart|stop} $SERVICE
日志     : journalctl -u $SERVICE -f

提示：
- 面板只管理审核配置/记录；new-api 侧还需要打审核补丁（见仓库 README）
- 忘记密码：编辑 $INSTALL_DIR/.env 里的 MOD_ADMIN_PASS 后 systemctl restart $SERVICE
==================================================
EOF
