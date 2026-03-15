# github.com/soulteary/gorge-task-queue

Go 实现的 Phorge 任务队列 HTTP 服务，替换原有 PHP `PhabricatorWorkerLeaseQuery` 的 MySQL 轮询模式。

本服务操作 Phorge 原有的 `worker_activetask` / `worker_archivetask` / `worker_taskdata` 三张表，与 PHP 端数据完全兼容。

## 功能

- **入队** (`POST /api/queue/enqueue`) — 替代 `PhabricatorWorker::scheduleTask()`
- **租用** (`POST /api/queue/lease`) — 替代 `PhabricatorWorkerLeaseQuery::execute()`
- **完成** (`POST /api/queue/complete`) — 替代 `archiveTask(RESULT_SUCCESS)`
- **失败** (`POST /api/queue/fail`) — 支持永久/可重试两种模式
- **让步** (`POST /api/queue/yield`) — 替代 `PhabricatorWorkerYieldException`
- **取消** (`POST /api/queue/cancel`) — 替代 `archiveTask(RESULT_CANCELLED)`
- **唤醒** (`POST /api/queue/awaken`) — 替代 `PhabricatorWorker::awakenTaskIDs()`
- **统计** (`GET /api/queue/stats`) — 队列状态概览
- **任务列表** (`GET /api/queue/tasks`) — 查看活跃任务
- **任务详情** (`GET /api/queue/tasks/:id`) — 查看单个任务

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MYSQL_HOST` | `127.0.0.1` | MySQL 地址 |
| `MYSQL_PORT` | `3306` | MySQL 端口 |
| `MYSQL_USER` | `phorge` | MySQL 用户 |
| `MYSQL_PASS` | (空) | MySQL 密码 |
| `STORAGE_NAMESPACE` | `phorge` | 数据库名前缀 |
| `LISTEN_ADDR` | `:8090` | HTTP 监听地址 |
| `SERVICE_TOKEN` | (空) | API 鉴权 token |
| `LEASE_DURATION` | `7200` | 默认租约时长（秒） |
| `RETRY_WAIT` | `300` | 默认重试等待（秒） |

## 开发

```bash
# 运行
MYSQL_HOST=127.0.0.1 MYSQL_PASS=secret go run ./cmd/server

# 测试
go test ./...

# 构建
CGO_ENABLED=0 go build -o github.com/soulteary/gorge-task-queue ./cmd/server
```

## Docker

```bash
docker build -t github.com/soulteary/gorge-task-queue .
docker run -e MYSQL_HOST=db -e MYSQL_PASS=secret github.com/soulteary/gorge-task-queue
```
