#!/usr/bin/env bash
# 交叉编译 linux/amd64（无 CGO 依赖）
set -e
cd "$(dirname "$0")"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o s6-moderation-console .
echo "built: s6-moderation-console"
