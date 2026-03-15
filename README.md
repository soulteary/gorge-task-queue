# gorge-task-queue

Gorge 平台中的任务队列微服务，以 Go 实现，替代 Phorge 原有的 PHP `PhabricatorWorkerLeaseQuery` MySQL 轮询模式。

本服务操作 Phorge 原有的 `worker_activetask` / `worker_archivetask` / `worker_taskdata` 三张表，通过基于 MySQL 的租约模型实现任务的并发安全分配。与 PHP 端数据完全兼容，切换时无需迁移数据。

## 架构

| 层次 | 路径 | 职责 |
|------|------|------|
| 入口 | `cmd/server/` | 加载配置、连接 MySQL、注册 Echo 路由并启动 HTTP 服务 |
| API | `internal/httpapi/` | HTTP 端点定义、Token 认证中间件、请求解析与响应封装 |
| 存储 | `internal/queue/` | 数据模型定义、MySQL CRUD 操作、租约与归档逻辑 |
| 配置 | `internal/config/` | 环境变量加载、DSN 生成 |

## API

所有 `/api/queue/*` 端点通过 Token 认证中间件保护（`X-Service-Token` 请求头或 `token` 查询参数）。

| 端点 | 方法 | 对应 PHP 方法 | 说明 |
|------|------|---------------|------|
| `/api/queue/enqueue` | POST | `PhabricatorWorker::scheduleTask()` | 入队，支持延迟执行 |
| `/api/queue/lease` | POST | `PhabricatorWorkerLeaseQuery::execute()` | 租用任务，两阶段选择 |
| `/api/queue/complete` | POST | `archiveTask(RESULT_SUCCESS)` | 标记完成并归档 |
| `/api/queue/fail` | POST | — | 失败处理（永久/可重试） |
| `/api/queue/yield` | POST | `PhabricatorWorkerYieldException` | 让步，暂时释放执行权 |
| `/api/queue/cancel` | POST | `archiveTask(RESULT_CANCELLED)` | 取消并归档 |
| `/api/queue/awaken` | POST | `PhabricatorWorker::awakenTaskIDs()` | 唤醒被 Yield 的任务 |
| `/api/queue/stats` | GET | — | 队列状态统计 |
| `/api/queue/tasks` | GET | — | 活跃任务列表（分页） |
| `/api/queue/tasks/:id` | GET | — | 单个任务详情 |

### 请求示例

```bash
# 入队
curl -X POST http://localhost:8090/api/queue/enqueue \
  -H 'Content-Type: application/json' \
  -H 'X-Service-Token: your-token' \
  -d '{"taskClass":"PhabricatorRepositoryCommitHeraldWorker","data":"{\"id\":42}","priority":2000}'

# 租用
curl -X POST http://localhost:8090/api/queue/lease \
  -H 'X-Service-Token: your-token' \
  -d '{"limit":5}'

# 完成
curl -X POST http://localhost:8090/api/queue/complete \
  -H 'X-Service-Token: your-token' \
  -d '{"taskID":1,"duration":1500}'

# 统计
curl http://localhost:8090/api/queue/stats?token=your-token
```

### 响应格式

成功响应：

```json
{"data": { ... }}
```

错误响应：

```json
{"error": {"code": "ERR_BAD_REQUEST", "message": "taskClass is required"}}
```

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MYSQL_HOST` | `127.0.0.1` | MySQL 地址 |
| `MYSQL_PORT` | `3306` | MySQL 端口 |
| `MYSQL_USER` | `phorge` | MySQL 用户 |
| `MYSQL_PASS` | (空) | MySQL 密码 |
| `STORAGE_NAMESPACE` | `phorge` | 数据库名前缀，连接 `{namespace}_worker` 库 |
| `LISTEN_ADDR` | `:8090` | HTTP 监听地址 |
| `SERVICE_TOKEN` | (空) | API 鉴权 Token，为空时不启用认证 |
| `LEASE_DURATION` | `7200` | 默认租约时长（秒） |
| `RETRY_WAIT` | `300` | 失败后默认重试等待（秒） |
| `POLL_INTERVAL` | `1000` | 轮询间隔（毫秒） |
| `MAX_WORKERS` | `4` | 并发租约处理器数 |

## 构建 & 运行

```bash
# 运行
MYSQL_HOST=127.0.0.1 MYSQL_PASS=secret go run ./cmd/server

# 测试
go test ./...

# 构建
CGO_ENABLED=0 go build -o gorge-task-queue ./cmd/server
```

## Docker

```bash
docker build -t gorge-task-queue .
docker run -e MYSQL_HOST=db -e MYSQL_PASS=secret gorge-task-queue
```

## 项目结构

```
gorge-task-queue/
├── cmd/server/
│   └── main.go                  # 服务入口，配置加载与 Echo 启动
├── internal/
│   ├── config/
│   │   ├── config.go            # 环境变量配置加载、DSN 生成
│   │   └── config_test.go       # 配置单元测试
│   ├── httpapi/
│   │   └── handlers.go          # HTTP API 路由、认证中间件、请求处理
│   └── queue/
│       ├── model.go             # 数据模型、常量、请求/响应结构体
│       ├── store.go             # MySQL 存储层，租约/归档/统计等核心逻辑
│       └── store_test.go        # 存储层单元测试
├── Dockerfile                   # 多阶段 Docker 构建
├── go.mod
└── go.sum
```

## 技术栈

- **语言**：Go 1.22
- **HTTP 框架**：[Echo](https://github.com/labstack/echo) v4.12
- **数据库驱动**：[go-sql-driver/mysql](https://github.com/go-sql-driver/mysql) v1.8
- **许可证**：Apache License 2.0
