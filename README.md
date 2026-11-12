# newapi-antiAnti

new-api 内容审核管理面板 —— **独立服务**，不修改 new-api 任何代码。

一个单二进制小服务（Go），直连 new-api 的 PostgreSQL 数据库，为「内容审核补丁」提供 Web 管理界面：

- **概览**：今日总请求 / 抽检请求数 / 检测成功率 / 检出违规数与比例 / 已标记对话数
- **分组设置**：分组自动从 new-api 同步，卡片式配置（模式 只记录/拦截、抽检比例、违规类别、预检）
- **抽检记录**：每次抽检结果，点行看完整内容，人工复核（误报/确认）
- **对话标记**：被标记对话列表，一键解除（误报申诉处理）
- **高级设置**：检测通道（任意 OpenAI 兼容地址 + key + 模型，如 glm-5.3-flash）在线测试、提示语模板、判定参数
- **备忘录**：存在数据库里的私人便签

面板本身不含审核逻辑——审核逻辑在 new-api 侧的补丁里，见
[freedom-winds/new-api](https://github.com/freedom-winds/new-api)（`patches/` 目录 + `S6-MODERATION.md` 使用文档）。

## 一键部署（推荐）

要求：Linux root、已装 nginx、new-api 使用 PostgreSQL。

```bash
git clone https://github.com/freedom-winds/newapi-antiAnti.git
cd newapi-antiAnti
sudo bash deploy.sh
```

脚本会依次：检测 nginx（没有就取消）→ 问域名 → 问数据库 DSN → 端口默认 28906（被占自动顺延）→
编译面板（无 Go 时自动下载 Release 二进制）→ systemd 托管（只监听 127.0.0.1）→
问 nginx 写入方式（默认在 `/etc/nginx/conf.d` 和 `sites-enabled` 自动定位该域名的 server 块注入 location；
也可指定配置目录/文件；注入前自动备份，`nginx -t` 失败自动还原）。

非交互式（复制粘贴用）：

```bash
sudo bash deploy.sh \
  --domain api.example.com \
  --dsn "postgresql://user:pass@127.0.0.1:5432/new-api?sslmode=disable" \
  --user admin \
  --conf-dir /etc/nginx/conf.d
```

全部参数：`--domain --dsn --user --password --port --path`（URL 路径，默认 `/anti-anti`）`--conf-dir | --conf-file`。

部署完成后访问 `https://你的域名/anti-anti/`，用脚本打印的账密登录。

## 手动部署

```bash
CGO_ENABLED=0 go build -o antianti-console .
cat > /opt/newapi-antiAnti/.env <<EOF
MOD_PG_DSN=postgresql://user:pass@127.0.0.1:5432/new-api?sslmode=disable
MOD_ADMIN_USER=admin
MOD_ADMIN_PASS=改成你自己的
MOD_LISTEN=127.0.0.1:28906
EOF
# systemd 单元见 systemd/newapi-antianti.service
# nginx: location /anti-anti/ { proxy_pass http://127.0.0.1:28906/; }
```

面板在任意 URL 前缀下都能跑（内部自动剥前缀），路径随意。

## 说明与注意

- **数据库**：目前只支持 PostgreSQL 直连（new-api 的 PG 库）。MySQL/SQLite 站点可先直接 SQL 管理
  `moderation_settings` 配置，或提 issue 催加驱动。
- **安全**：面板只监听 127.0.0.1，公网入口必须走 nginx；登录账密在 `/opt/newapi-antiAnti/.env`，
  请务必改掉随机生成的默认密码；如需加固可给 nginx location 再套 basic auth 或 IP 白名单。
- **审核功能本体**：抽检、对话标记、拦截、预检等逻辑全部在 new-api 侧补丁里，
  安装/更新方法见 [freedom-winds/new-api 的 S6-MODERATION.md](https://github.com/freedom-winds/new-api/blob/main/S6-MODERATION.md)。
  面板单独跑起来也不会有问题——没有补丁时三张表是空的而已。
- 卸载：`systemctl disable --now newapi-antianti && rm -rf /opt/newapi-antiAnti /etc/systemd/system/newapi-antianti.service`，
  并删掉 nginx 配置里 `# newapi-antiAnti panel` 标记的 location 段后 reload。
