# gorge-task-queue 技术报告

## 1. 概述

gorge-task-queue 是 Gorge 平台中的任务队列微服务，为 Phorge（Phabricator 社区维护分支）提供基于 MySQL 的任务队列 HTTP API。

该服务的核心目标是替代 Phorge 原有的 PHP 后台任务处理机制。Phorge 的后台任务系统由 `PhabricatorWorkerLeaseQuery`、`PhabricatorWorker`、`PhabricatorWorkerBulkJob` 等 PHP 类组成，依赖 `bin/phd` 守护进程管理器通过 MySQL 轮询获取待执行任务。gorge-task-queue 将任务队列的入队、租约、完成、失败、让步、唤醒等操作抽取为独立的 Go HTTP 服务，直接操作 Phorge 原有的三张 worker 表（`worker_activetask`、`worker_archivetask`、`worker_taskdata`），与 PHP 端数据完全兼容。

## 2. 设计动机

### 2.1 原有方案的问题

Phorge 的后台任务系统嵌入在 PHP 应用中：

1. **守护进程模型笨重**：`bin/phd` 启动的 PHP 守护进程通过 `SELECT ... FOR UPDATE` 轮询 MySQL 获取待执行任务。每个 PHP 守护进程是一个长期运行的 CLI 进程，PHP 的设计初衷是请求-响应模型，长期运行的 PHP 进程容易出现内存泄漏、连接池耗尽等问题。
2. **轮询效率低**：PHP 守护进程以固定间隔轮询数据库，在空闲期仍会产生不必要的 MySQL 查询，浪费数据库连接和 CPU 资源。高负载期间又可能因为轮询间隔导致任务处理延迟。
3. **租约管理分散**：租约的获取、续期、释放逻辑分散在 `PhabricatorWorkerLeaseQuery`、`PhabricatorWorkerActiveTask` 等多个 PHP 类中，代码路径复杂，难以理解和维护。
4. **缺乏 API 化**：任务的入队、查询、管理只能通过 PHP 内部调用或 Conduit API 间接操作，其他 Go 微服务无法直接与任务队列交互。
5. **部署耦合**：后台任务处理与 Phorge PHP 应用绑定在同一部署单元中，无法独立扩缩容。

### 2.2 gorge-task-queue 的解决思路

将任务队列管理功能抽取为独立的 Go HTTP 微服务：

- **HTTP API 化**：所有任务队列操作通过 REST API 暴露，其他 Go 微服务可以直接调用，无需经过 PHP Conduit 层。
- **连接池复用**：Go 常驻进程维护 MySQL 连接池（MaxOpenConns=25, MaxIdleConns=5），避免 PHP 每次轮询重建连接。
- **事务原子性**：入队、租约、归档等关键操作使用 MySQL 事务，确保数据一致性。
- **数据兼容**：直接操作 Phorge 原有的 `{namespace}_worker` 数据库中的三张表，切换时无需迁移数据，PHP 端可以继续读取和操作同一组表。
- **独立部署**：作为独立容器运行，可根据负载独立扩缩容，不依赖 PHP 运行时。

## 3. 系统架构

### 3.1 在 Gorge 平台中的位置

```
┌──────────────────────────────────────────────────────┐
│                     Gorge 平台                        │
│                                                       │
│  ┌──────────┐  ┌───────────┐  ┌────────────────────┐ │
│  │  Phorge  │  │  gorge-   │  │  gorge-worker /    │ │
│  │  (PHP)   │  │  conduit  │  │  其他 Go 服务       │ │
│  └────┬─────┘  └─────┬─────┘  └──────────┬─────────┘ │
│       │               │                  │            │
│       └───────────────┼──────────────────┘            │
│                       │                               │
│                       ▼                               │
│           ┌───────────────────────┐                   │
│           │   gorge-task-queue   │                   │
│           │   :8090              │                   │
│           │                     │                   │
│           │   Token Auth        │                   │
│           │   队列管理 API       │                   │
│           │   租约 / 归档       │                   │
│           └──────────┬──────────┘                   │
│                      │                               │
│                      ▼                               │
│           ┌───────────────────────┐                   │
│           │   MySQL              │                   │
│           │   {namespace}_worker │                   │
│           │                     │                   │
│           │   worker_activetask │                   │
│           │   worker_archivetask│                   │
│           │   worker_taskdata   │                   │
│           └───────────────────────┘                   │
└──────────────────────────────────────────────────────┘
```

### 3.2 模块划分

项目采用 Go 标准布局，分为三个内部模块：

| 模块 | 路径 | 职责 |
|------|------|------|
| config | `internal/config/` | 从环境变量加载配置，生成 MySQL DSN |
| queue | `internal/queue/` | 数据模型定义、MySQL 存储层实现（入队/租约/归档/统计） |
| httpapi | `internal/httpapi/` | HTTP 路由注册、Token 认证中间件、请求处理函数 |

入口程序 `cmd/server/main.go` 负责串联三个模块：加载配置 -> 连接 MySQL 创建 Store -> 初始化 Echo 实例 -> 注册路由 -> 启动 HTTP 服务。

### 3.3 请求处理流水线

```
HTTP 请求
    │
    ▼
┌─ Echo 框架 ──────────────────────────────────────────┐
│                                                       │
│  middleware.Logger() ─► middleware.Recover()           │
│                                                       │
│  ┌─ /api/queue/* ──────────────────────────────────┐  │
│  │                                                  │  │
│  │  tokenAuth 中间件                                │  │
│  │  ├── Token 为空 → 跳过认证                       │  │
│  │  ├── X-Service-Token / ?token 匹配 → 放行       │  │
│  │  └── 不匹配 → 401 ERR_UNAUTHORIZED              │  │
│  │                                                  │  │
│  │  Handler 函数                                    │  │
│  │  ├── c.Bind(&req) 解析 JSON 请求体              │  │
│  │  ├── 参数校验（taskClass / taskID 等必填项）     │  │
│  │  ├── deps.Store.XXX() 调用存储层                 │  │
│  │  └── respondOK / respondErr 统一响应             │  │
│  │                                                  │  │
│  └──────────────────────────────────────────────────┘  │
│                                                       │
│  / 和 /healthz ─► {"status": "ok"} (无需认证)        │
│                                                       │
└───────────────────────────────────────────────────────┘
```

## 4. 数据库设计

### 4.1 表结构

服务操作 `{namespace}_worker` 数据库中的三张表，表结构与 Phorge 原有定义完全一致：

#### worker_taskdata

存放任务的 payload 数据，与任务记录分离存储，避免活跃任务表的行过大影响查询性能。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | BIGINT AUTO_INCREMENT | 主键 |
| `data` | LONGTEXT | 任务数据（通常为序列化的 JSON） |

#### worker_activetask

活跃任务表，存放待执行和正在执行的任务。租约通过 `leaseOwner` 和 `leaseExpires` 字段实现。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | BIGINT AUTO_INCREMENT | 主键 |
| `taskClass` | VARCHAR | 任务类名（如 `PhabricatorRepositoryCommitHeraldWorker`） |
| `leaseOwner` | VARCHAR NULL | 租约持有者标识，NULL 表示未被租用 |
| `leaseExpires` | BIGINT NULL | 租约过期时间（Unix 时间戳），NULL 或 ≤ now 表示可被租用 |
| `failureCount` | INT | 累计失败次数 |
| `dataID` | BIGINT | 关联的 `worker_taskdata.id` |
| `failureTime` | BIGINT NULL | 最近一次失败的时间戳 |
| `priority` | INT | 优先级，数值越小越优先 |
| `objectPHID` | VARCHAR NULL | 关联的对象 PHID |
| `containerPHID` | VARCHAR NULL | 关联的容器 PHID |
| `dateCreated` | BIGINT | 创建时间（Unix 时间戳） |
| `dateModified` | BIGINT | 修改时间（Unix 时间戳） |

#### worker_archivetask

已归档任务表，存放已完成、已失败（永久）和已取消的任务。结构与 `worker_activetask` 类似，增加归档相关字段。

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | BIGINT | 主键（与原 activetask 的 id 一致） |
| `taskClass` | VARCHAR | 任务类名 |
| `leaseOwner` | VARCHAR NULL | 归档时的租约持有者 |
| `leaseExpires` | BIGINT NULL | 归档时的租约过期时间 |
| `failureCount` | INT | 累计失败次数 |
| `dataID` | BIGINT | 关联的 `worker_taskdata.id` |
| `failureTime` | BIGINT NULL | 最近一次失败的时间戳 |
| `priority` | INT | 优先级 |
| `objectPHID` | VARCHAR NULL | 关联的对象 PHID |
| `containerPHID` | VARCHAR NULL | 关联的容器 PHID |
| `result` | INT | 归档结果：0=成功，1=失败，2=取消 |
| `duration` | BIGINT | 任务执行时长（毫秒） |
| `dateCreated` | BIGINT | 创建时间 |
| `dateModified` | BIGINT | 归档时间 |
| `archivedEpoch` | BIGINT | 归档时间戳 |

### 4.2 表关系

```
worker_taskdata (1) ◄──── (1) worker_activetask
                    ◄──── (1) worker_archivetask

任务生命周期:
  worker_taskdata + worker_activetask  (活跃状态)
       │
       │  Complete / Fail(permanent) / Cancel
       ▼
  worker_taskdata + worker_archivetask (归档状态)
```

任务数据（`worker_taskdata`）在整个生命周期中保持不变。活跃任务被归档时，从 `worker_activetask` 删除并插入 `worker_archivetask`，两步操作在同一个事务中完成。`worker_taskdata` 中的数据不会被删除，归档任务仍然可以通过 `dataID` 关联查询。

## 5. 核心实现分析

### 5.1 配置模块（internal/config）

#### 5.1.1 数据结构

```go
type Config struct {
    MySQLHost     string
    MySQLPort     int
    MySQLUser     string
    MySQLPass     string
    Namespace     string
    ListenAddr    string
    ServiceToken  string
    LeaseDuration int // seconds, default 7200 (2 hours)
    RetryWait     int // seconds, default 300 (5 minutes)
    PollInterval  int // milliseconds, default 1000
    MaxWorkers    int // concurrent lease processors, default 4
}
```

`Config` 包含 MySQL 连接、HTTP 服务和任务队列行为三组配置。所有配置项均通过环境变量加载，使用 `envStr` 和 `envInt` 辅助函数处理默认值和类型转换。

#### 5.1.2 DSN 生成

```go
func (c *Config) DSN() string {
    return fmt.Sprintf(
        "%s:%s@tcp(%s:%d)/%s_worker?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
        c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
    )
}
```

DSN 的设计要点：

- **数据库名**：`{namespace}_worker`，与 Phorge 的命名约定一致。Phorge 使用 `STORAGE_NAMESPACE` 作为所有数据库的名称前缀。
- **parseTime=true**：让驱动自动将 MySQL 的 `DATETIME`/`TIMESTAMP` 列解析为 Go 的 `time.Time` 类型。
- **超时策略**：连接超时 5 秒（`timeout`）、读超时 30 秒（`readTimeout`）、写超时 30 秒（`writeTimeout`）。连接超时较短，用于快速检测数据库不可达；读写超时较宽松，允许慢查询和大事务完成。

#### 5.1.3 环境变量辅助函数

```go
func envStr(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}

func envInt(key string, fallback int) int {
    if v := os.Getenv(key); v != "" {
        n, err := strconv.Atoi(v)
        if err == nil {
            return n
        }
    }
    return fallback
}
```

两个辅助函数遵循相同的模式：检查环境变量是否存在且非空，是则使用环境变量值（`envInt` 还做类型转换），否则返回默认值。`envInt` 在转换失败时静默回退到默认值而非报错——配置项的容错优先于严格校验，避免因运维人员输入错误导致服务启动失败。

### 5.2 数据模型（internal/queue/model.go）

#### 5.2.1 任务状态常量

```go
const (
    ResultSuccess   = 0
    ResultFailure   = 1
    ResultCancelled = 2
)
```

归档任务的 `result` 字段取值，与 Phorge PHP 端的 `PhabricatorWorkerArchiveTask::RESULT_*` 常量完全对应：

- `0`：任务成功完成
- `1`：任务永久失败（不再重试）
- `2`：任务被管理员取消

#### 5.2.2 优先级体系

```go
const (
    PriorityAlerts  = 1000
    PriorityDefault = 2000
    PriorityCommit  = 2500
    PriorityBulk    = 3000
    PriorityIndex   = 3500
    PriorityImport  = 4000
)
```

数值越小优先级越高，与 Phorge 的 `PhabricatorWorker::PRIORITY_*` 常量一致：

| 常量 | 值 | 用途 | 典型场景 |
|------|----|------|----------|
| `PriorityAlerts` | 1000 | 告警类 | 紧急通知、安全事件 |
| `PriorityDefault` | 2000 | 默认 | 一般后台任务 |
| `PriorityCommit` | 2500 | 提交相关 | 代码仓库提交处理 |
| `PriorityBulk` | 3000 | 批量操作 | 批量编辑、批量通知 |
| `PriorityIndex` | 3500 | 搜索索引 | Elasticsearch 索引更新 |
| `PriorityImport` | 4000 | 数据导入 | 仓库导入、大规模迁移 |

优先级体系的设计意图：紧急任务（告警）应立即处理，即使队列中积压了大量低优先级任务（如索引构建、数据导入）。`Lease` 操作按 `priority ASC, id ASC` 排序选择任务，确保高优先级任务优先被租用，同优先级内按 FIFO 顺序。

#### 5.2.3 Yield 标识

```go
const YieldOwner = "(yield)"
```

`YieldOwner` 是一个特殊的 `leaseOwner` 值，标识任务处于让步（yield）状态。使用括号包裹的字符串作为标识符，不会与真实的 IP 地址或主机名冲突。`Awaken` 操作通过检查 `leaseOwner = "(yield)"` 来筛选可被唤醒的任务。

#### 5.2.4 核心数据结构

```go
type Task struct {
    ID            int64  `json:"id"`
    TaskClass     string `json:"taskClass"`
    LeaseOwner    string `json:"leaseOwner,omitempty"`
    LeaseExpires  *int64 `json:"leaseExpires,omitempty"`
    FailureCount  int    `json:"failureCount"`
    DataID        int64  `json:"dataID"`
    FailureTime   *int64 `json:"failureTime,omitempty"`
    Priority      int    `json:"priority"`
    ObjectPHID    string `json:"objectPHID,omitempty"`
    ContainerPHID string `json:"containerPHID,omitempty"`
    DateCreated   int64  `json:"dateCreated"`
    DateModified  int64  `json:"dateModified"`
    Data          string `json:"data,omitempty"`
}
```

`Task` 结构体映射 `worker_activetask` 表的行，外加从 `worker_taskdata` JOIN 查询得到的 `Data` 字段。可选字段使用指针类型（`*int64`）或 `omitempty` 标签——MySQL 中的 NULL 值在 Go 中表现为 `nil` 指针，JSON 序列化时自动省略。

```go
type ArchivedTask struct {
    Task
    Result        int    `json:"result"`
    Duration      int64  `json:"duration"`
    ArchivedEpoch *int64 `json:"archivedEpoch,omitempty"`
}
```

`ArchivedTask` 通过嵌入 `Task` 复用所有基础字段，增加归档特有的 `Result`、`Duration` 和 `ArchivedEpoch`。Go 的结构体嵌入使得 JSON 序列化时所有字段扁平化输出，与 PHP 端的 JSON 格式一致。

#### 5.2.5 请求结构体

```go
type EnqueueRequest struct {
    TaskClass     string `json:"taskClass"`
    Data          string `json:"data"`
    Priority      *int   `json:"priority,omitempty"`
    ObjectPHID    string `json:"objectPHID,omitempty"`
    ContainerPHID string `json:"containerPHID,omitempty"`
    DelayUntil    *int64 `json:"delayUntil,omitempty"`
}
```

`EnqueueRequest` 中 `Priority` 使用 `*int` 指针类型——允许区分"未指定"（`nil`，使用默认值 `PriorityDefault`）和"显式设为 0"。`DelayUntil` 同理，`nil` 表示立即可执行，非 `nil` 表示延迟到指定时间。

其余请求结构体（`LeaseRequest`、`CompleteRequest`、`FailRequest`、`YieldRequest`、`AwakenRequest`、`CancelRequest`）均遵循最小化原则——只包含对应操作所需的必要字段。

### 5.3 存储层（internal/queue/store.go）

存储层是整个服务的核心，实现了基于 MySQL 的租约模型。

#### 5.3.1 Store 结构体与初始化

```go
type Store struct {
    db            *sql.DB
    leaseDuration int
    retryWait     int
}

func NewStore(dsn string, leaseDuration, retryWait int) (*Store, error) {
    db, err := sql.Open("mysql", dsn)
    if err != nil {
        return nil, fmt.Errorf("open db: %w", err)
    }
    db.SetMaxOpenConns(25)
    db.SetMaxIdleConns(5)
    db.SetConnMaxLifetime(5 * time.Minute)

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := db.PingContext(ctx); err != nil {
        return nil, fmt.Errorf("ping db: %w", err)
    }

    return &Store{
        db:            db,
        leaseDuration: leaseDuration,
        retryWait:     retryWait,
    }, nil
}
```

连接池配置的设计考量：

- **MaxOpenConns=25**：限制最大连接数，防止突发请求耗尽 MySQL 的 `max_connections`。25 是一个保守值——单个 gorge-task-queue 实例不应独占过多连接，留给 Phorge PHP 应用和其他微服务使用。
- **MaxIdleConns=5**：空闲连接数远小于最大连接数。任务队列操作通常是突发的（一批任务入队/租约），空闲时只需少量连接保持就绪。
- **ConnMaxLifetime=5min**：连接最长存活 5 分钟后强制关闭并重建。这防止了长连接积累服务器端资源泄漏，也确保了负载均衡场景下连接能定期重新分配到不同的后端节点。

`NewStore` 在返回前执行 `PingContext` 验证数据库连通性，10 秒超时足以覆盖网络延迟和 MySQL 启动等待。这是一种 fail-fast 设计——如果数据库不可达，服务启动时立即失败，而非等到第一个 API 请求时才发现问题。

#### 5.3.2 入队（Enqueue）

```go
func (s *Store) Enqueue(ctx context.Context, req *EnqueueRequest) (*Task, error) {
    priority := PriorityDefault
    if req.Priority != nil {
        priority = *req.Priority
    }

    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return nil, fmt.Errorf("begin tx: %w", err)
    }
    defer func() { _ = tx.Rollback() }()

    now := time.Now().Unix()

    res, err := tx.ExecContext(ctx,
        "INSERT INTO worker_taskdata (data) VALUES (?)",
        req.Data)
    // ... 获取 dataID ...

    var leaseExpires *int64
    if req.DelayUntil != nil && *req.DelayUntil > 0 {
        leaseExpires = req.DelayUntil
    }

    res, err = tx.ExecContext(ctx,
        `INSERT INTO worker_activetask
            (taskClass, leaseOwner, leaseExpires, failureCount, dataID, failureTime,
             priority, objectPHID, containerPHID, dateCreated, dateModified)
        VALUES (?, NULL, ?, 0, ?, NULL, ?, ?, ?, ?, ?)`,
        req.TaskClass, leaseExpires, dataID, priority,
        nullStr(req.ObjectPHID), nullStr(req.ContainerPHID), now, now)
    // ... 获取 taskID, 提交事务 ...
}
```

入队操作的关键设计：

**事务保证原子性**：`worker_taskdata` 和 `worker_activetask` 的插入在同一个事务中完成。如果第二个 `INSERT` 失败，`defer tx.Rollback()` 会回滚第一个插入，确保不会产生孤立的 taskdata 记录。

**延迟执行**：当 `DelayUntil` 不为空时，将其值设为 `leaseExpires`。这巧妙地复用了租约过期机制——`Lease` 操作只选择 `leaseExpires <= NOW()` 或 `leaseOwner IS NULL` 的任务，设置了未来时间的 `leaseExpires` 的任务在到期前不会被选中。`leaseOwner` 保持 `NULL`，不会干扰正常的租约逻辑。

**可选字段处理**：`nullStr` 辅助函数将空字符串转为 `nil`（SQL NULL）。`objectPHID` 和 `containerPHID` 在 Phorge 中是可选字段，不是所有任务类型都关联到具体的对象或容器。

#### 5.3.3 租约（Lease）—— 核心操作

```go
func (s *Store) Lease(ctx context.Context, limit int, leaseOwner string) ([]*Task, error) {
    if limit <= 0 {
        limit = 1
    }

    now := time.Now().Unix()
    leaseExp := now + int64(s.leaseDuration)

    tx, err := s.db.BeginTx(ctx, nil)
    // ...

    // Phase 1: unleased tasks
    rows, err := tx.QueryContext(ctx,
        `SELECT id FROM worker_activetask
         WHERE leaseOwner IS NULL
         ORDER BY priority ASC, id ASC
         LIMIT ?`,
        limit-len(tasks))
    ids := scanIDs(rows)
    if len(ids) > 0 {
        tx.ExecContext(ctx,
            fmt.Sprintf(
                `UPDATE worker_activetask
                 SET leaseOwner = ?, leaseExpires = ?
                 WHERE leaseOwner IS NULL AND id IN (%s)`,
                placeholders(len(ids))),
            appendArgs(leaseOwner, leaseExp, ids)...)
    }

    // Phase 2: expired leases
    remaining := limit - len(tasks)
    if remaining > 0 {
        rows, err := tx.QueryContext(ctx,
            `SELECT id FROM worker_activetask
             WHERE leaseExpires < ?
             ORDER BY leaseExpires ASC
             LIMIT ?`,
            now, remaining)
        ids := scanIDs(rows)
        if len(ids) > 0 {
            tx.ExecContext(ctx,
                fmt.Sprintf(
                    `UPDATE worker_activetask
                     SET leaseOwner = ?, leaseExpires = ?
                     WHERE leaseExpires < ? AND id IN (%s)`,
                    placeholders(len(ids))),
                appendArgs(leaseOwner, leaseExp, now, ids)...)
        }
    }

    // Fetch leased tasks with data
    fetchRows, err := tx.QueryContext(ctx,
        `SELECT t.id, t.taskClass, ..., COALESCE(d.data, '')
         FROM worker_activetask t
         LEFT JOIN worker_taskdata d ON d.id = t.dataID
         WHERE t.leaseOwner = ? AND t.leaseExpires > ?
         ORDER BY t.priority ASC, t.id ASC
         LIMIT ?`,
        leaseOwner, now, limit)
    // ... scan tasks ...

    tx.Commit()
    return tasks, nil
}
```

租约是整个任务队列系统最核心的操作，采用两阶段选择策略：

**第一阶段——选择未租用任务**：查找 `leaseOwner IS NULL` 的任务，按 `priority ASC, id ASC` 排序。这些是新入队的、从未被任何 worker 获取过的任务。先选择未租用任务确保新任务优先于重试任务被处理。

**第二阶段——选择过期租约任务**：查找 `leaseExpires < NOW()` 的任务，按 `leaseExpires ASC` 排序（最早过期的优先）。这些是 worker 崩溃或处理超时后释放的任务，需要被重新分配。只有第一阶段未填满 `limit` 时才执行第二阶段。

**UPDATE ... WHERE ... AND id IN (...)**：这是一种乐观并发控制模式。先 SELECT 获取候选 ID 列表，再 UPDATE 时重新检查条件（`leaseOwner IS NULL` 或 `leaseExpires < ?`）。如果两个并发 Lease 请求同时选中了相同的任务 ID，只有一个 UPDATE 能成功修改（因为第一个 UPDATE 已经改变了 `leaseOwner`，第二个 UPDATE 的 WHERE 条件不再满足）。整个过程在事务中执行，确保原子性。

**最终查询**：UPDATE 后通过 `WHERE t.leaseOwner = ? AND t.leaseExpires > ?` 查询所有成功租用的任务，JOIN `worker_taskdata` 获取任务数据。这保证了返回的任务列表精确反映实际租用结果。

**leaseOwner 标识**：由 HTTP 层从请求头 `X-Lease-Owner` 或 `RealIP() + ":gorge-task-queue"` 生成。每个 worker 有唯一的 leaseOwner 标识，用于在后续的 Complete/Fail/Yield 操作中定位自己租用的任务。

#### 5.3.4 归档（archiveTask）

```go
func (s *Store) archiveTask(ctx context.Context, taskID int64, result int, duration int64) (*ArchivedTask, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    // ...

    // 1. 从 activetask 读取完整任务信息
    err = tx.QueryRowContext(ctx,
        `SELECT id, taskClass, ... FROM worker_activetask WHERE id = ?`, taskID).Scan(...)

    // 2. 插入 archivetask
    _, err = tx.ExecContext(ctx,
        `INSERT INTO worker_archivetask
            (id, taskClass, ..., result, duration, ..., archivedEpoch)
         VALUES (?, ?, ..., ?, ?, ..., ?)`,
        t.ID, t.TaskClass, ..., result, duration, ..., now)

    // 3. 删除 activetask
    _, err = tx.ExecContext(ctx,
        "DELETE FROM worker_activetask WHERE id = ?", taskID)

    tx.Commit()
    return archived, nil
}
```

归档操作是 `Complete`、`Fail`（永久失败）和 `Cancel` 三个 API 的共享实现。三步操作（读取、插入归档、删除活跃）在同一事务中完成：

1. **SELECT**：从 `worker_activetask` 读取完整的任务信息，包括所有字段。
2. **INSERT INTO worker_archivetask**：将任务记录从活跃表复制到归档表，保留原始 `id`（不重新生成），增加 `result`、`duration` 和 `archivedEpoch` 字段。`dateModified` 更新为当前时间。
3. **DELETE FROM worker_activetask**：从活跃表中删除该任务。

保留原始 `id` 的设计意义：Phorge PHP 端可能通过 task ID 查询任务的执行结果，如果归档时变更 ID，PHP 端需要额外的映射逻辑。保持 ID 一致确保了与 PHP 端的透明兼容。

#### 5.3.5 失败处理（Fail）

```go
func (s *Store) Fail(ctx context.Context, req *FailRequest) error {
    if req.Permanent {
        _, err := s.archiveTask(ctx, req.TaskID, ResultFailure, 0)
        return err
    }

    retryWait := s.retryWait
    if req.RetryWait != nil {
        retryWait = *req.RetryWait
    }

    now := time.Now().Unix()
    _, err := s.db.ExecContext(ctx,
        `UPDATE worker_activetask
         SET failureCount = failureCount + 1,
             failureTime = ?,
             leaseOwner = NULL,
             leaseExpires = ?,
             dateModified = ?
         WHERE id = ?`,
        now, now+int64(retryWait), now, req.TaskID)
    return err
}
```

失败处理分两种路径：

**永久失败**（`permanent=true`）：直接调用 `archiveTask` 将任务归档为 `ResultFailure`。任务不再被重试。适用于不可恢复的错误，如任务类不存在、数据格式错误等。

**可重试失败**（`permanent=false`）：保留任务在 `worker_activetask` 中，但进行四项修改：
- `failureCount + 1`：递增失败计数器。Phorge PHP 端通常在 `failureCount` 达到阈值时将任务标记为永久失败。
- `failureTime = now`：记录最近一次失败的时间。
- `leaseOwner = NULL`：释放租约，允许其他 worker 重新获取。
- `leaseExpires = now + retryWait`：设置重试等待时间。在此期间任务的 `leaseExpires` 大于当前时间且 `leaseOwner` 为 NULL，不会被 Lease 的第一阶段选中（第一阶段要求 `leaseOwner IS NULL`，但排序后会被选中——实际上这里的延迟通过 `leaseExpires > now` 实现，第二阶段的条件 `leaseExpires < now` 在等待期内不成立）。

`retryWait` 支持请求级覆盖——调用方可以根据任务类型指定不同的重试间隔。如果未指定，使用 Store 级别的默认值（配置中的 `RETRY_WAIT`）。

#### 5.3.6 让步（Yield）

```go
func (s *Store) Yield(ctx context.Context, taskID int64, duration int) error {
    if duration < 5 {
        duration = 5
    }
    now := time.Now().Unix()
    _, err := s.db.ExecContext(ctx,
        `UPDATE worker_activetask
         SET leaseOwner = ?,
             leaseExpires = ?,
             dateModified = ?
         WHERE id = ?`,
        YieldOwner, now+int64(duration), now, taskID)
    return err
}
```

Yield 允许任务主动让出执行权，对应 Phorge PHP 端的 `PhabricatorWorkerYieldException`。典型场景：

- 任务等待外部资源（如 Git 仓库克隆、第三方 API 响应）
- 任务需要等待前置任务完成
- 任务需要限流以避免过载

设计要点：

- **最短让步时间 5 秒**：`if duration < 5 { duration = 5 }` 防止极短的让步时间导致任务立即被重新租用，产生忙等待循环。
- **leaseOwner = "(yield)"**：使用特殊标识而非 NULL，区分"主动让步"和"从未被租用"两种状态。`Awaken` 操作通过检查 `leaseOwner = "(yield)"` 只唤醒让步状态的任务，不会意外唤醒新任务。
- **leaseExpires 延长**：让步期间任务的 `leaseExpires` 在未来，Lease 的第二阶段（`leaseExpires < now`）不会选中它。到期后，Lease 的第二阶段会将其视为过期租约并重新分配。

#### 5.3.7 唤醒（Awaken）

```go
func (s *Store) Awaken(ctx context.Context, taskIDs []int64) (int64, error) {
    if len(taskIDs) == 0 {
        return 0, nil
    }

    window := int64(3600) // 1 hour
    epochAgo := time.Now().Unix() - window

    res, err := s.db.ExecContext(ctx,
        fmt.Sprintf(
            `UPDATE worker_activetask
             SET leaseExpires = ?
             WHERE id IN (%s)
               AND leaseOwner = ?
               AND leaseExpires > ?
               AND failureCount = 0`,
            placeholders(len(taskIDs))),
        appendArgs(epochAgo, taskIDs, YieldOwner, epochAgo)...)
    if err != nil {
        return 0, err
    }
    return res.RowsAffected()
}
```

唤醒操作将被 Yield 的任务提前激活，对应 Phorge PHP 端的 `PhabricatorWorker::awakenTaskIDs()`。

**leaseExpires 设为 1 小时前**：`epochAgo = now - 3600`，这使得任务立即符合 Lease 第二阶段的选择条件（`leaseExpires < now`），无需等待原始的 Yield duration 过期。

**三重安全检查**：
- `leaseOwner = ?` (YieldOwner)：只唤醒让步状态的任务，不影响正在被其他 worker 执行的任务。
- `leaseExpires > ?` (epochAgo)：只唤醒 `leaseExpires` 在 1 小时窗口内的任务。过于久远的任务可能已经被手动处理或存在其他问题，不应自动唤醒。
- `failureCount = 0`：只唤醒未曾失败的任务。有失败记录的任务可能存在系统性问题，唤醒后可能再次失败并产生噪音。

返回值是实际受影响的行数（`RowsAffected`），让调用方知道有多少任务被成功唤醒。

#### 5.3.8 统计（Stats）

```go
func (s *Store) Stats(ctx context.Context) (*QueueStats, error) {
    stats := &QueueStats{}
    now := time.Now().Unix()

    s.db.QueryRowContext(ctx,
        "SELECT COUNT(*) FROM worker_activetask").Scan(&stats.ActiveCount)

    s.db.QueryRowContext(ctx,
        "SELECT COUNT(*) FROM worker_activetask WHERE leaseOwner IS NOT NULL AND leaseExpires >= ?",
        now).Scan(&stats.LeasedCount)

    s.db.QueryRowContext(ctx,
        "SELECT COUNT(*) FROM worker_archivetask").Scan(&stats.ArchivedCount)

    s.db.QueryRowContext(ctx,
        "SELECT COUNT(*) FROM worker_activetask WHERE failureCount > 0").Scan(&stats.FailedCount)

    return stats, nil
}
```

Stats 执行四个独立的 COUNT 查询：

| 指标 | 查询条件 | 含义 |
|------|----------|------|
| `ActiveCount` | 全部 `worker_activetask` 行 | 队列中的总任务数（含正在执行的） |
| `LeasedCount` | `leaseOwner IS NOT NULL AND leaseExpires >= now` | 正在被 worker 执行的任务数 |
| `ArchivedCount` | 全部 `worker_archivetask` 行 | 已归档的任务总数 |
| `FailedCount` | `failureCount > 0` 的 `worker_activetask` 行 | 曾经失败但仍在队列中的任务数 |

四个查询未放在同一事务中——统计数据允许轻微的不一致（某个任务可能在两次 COUNT 之间状态发生变化），牺牲精确性换取更低的锁争用。

#### 5.3.9 辅助函数

```go
func nullStr(s string) interface{} {
    if s == "" {
        return nil
    }
    return s
}
```

将空字符串转为 `nil`（SQL NULL），用于可选字段的插入。Go 的 `database/sql` 包会将 `nil` 值转换为 SQL 的 NULL。

```go
func scanIDs(rows *sql.Rows) []int64 {
    var ids []int64
    for rows.Next() {
        var id int64
        if err := rows.Scan(&id); err == nil {
            ids = append(ids, id)
        }
    }
    return ids
}
```

从只含 `id` 列的结果集中提取 ID 列表。跳过扫描失败的行（`err == nil` 才 append），增加容错性。

```go
func placeholders(n int) string {
    if n <= 0 {
        return ""
    }
    return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
```

生成 SQL `IN` 子句的占位符。例如 `placeholders(3)` 返回 `"?,?,?"`。使用 `Repeat` + `TrimSuffix` 的模式避免了循环和 `strings.Builder` 的复杂性。

```go
func appendArgs(args ...interface{}) []interface{} {
    var out []interface{}
    for _, a := range args {
        switch v := a.(type) {
        case []int64:
            for _, id := range v {
                out = append(out, id)
            }
        default:
            out = append(out, a)
        }
    }
    return out
}
```

将混合参数列表展开为 `database/sql` 可接受的扁平参数切片。核心作用是将 `[]int64` 展开——SQL 的 `IN (?,?,?)` 需要逐个绑定值，不能直接传切片。例如 `appendArgs("owner", leaseExp, []int64{1,2,3})` 返回 `["owner", leaseExp, 1, 2, 3]`。

### 5.4 HTTP API 层（internal/httpapi/handlers.go）

#### 5.4.1 依赖注入

```go
type Deps struct {
    Store *queue.Store
    Token string
}
```

`Deps` 将存储层和配置项打包为一个结构体，注入到所有 Handler 函数中。这种模式的优势：

- 避免全局变量，Handler 函数可以独立测试（注入 mock Store）
- 所有 Handler 共享同一个 Store 实例（及其连接池）
- Token 在服务启动时确定，运行期间不变

#### 5.4.2 统一响应结构

```go
type apiResponse struct {
    Data  any       `json:"data,omitempty"`
    Error *apiError `json:"error,omitempty"`
}

type apiError struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}

func respondOK(c echo.Context, data any) error {
    return c.JSON(http.StatusOK, &apiResponse{Data: data})
}

func respondErr(c echo.Context, status int, code, msg string) error {
    return c.JSON(status, &apiResponse{
        Error: &apiError{Code: code, Message: msg},
    })
}
```

所有 API 使用统一的响应格式：

- **成功**：`{"data": ...}` —— `Error` 字段为 `nil`，`omitempty` 使其不出现在 JSON 中。
- **失败**：`{"error": {"code": "...", "message": "..."}}` —— `Data` 字段为 `nil`，`omitempty` 使其不出现。

错误码使用 `ERR_` 前缀的字符串常量（如 `ERR_BAD_REQUEST`、`ERR_UNAUTHORIZED`、`ERR_INTERNAL`、`ERR_NOT_FOUND`），比纯数字 HTTP 状态码提供更精确的错误分类。

#### 5.4.3 Token 认证中间件

```go
func tokenAuth(deps *Deps) echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            if deps.Token == "" {
                return next(c)
            }
            token := c.Request().Header.Get("X-Service-Token")
            if token == "" {
                token = c.QueryParam("token")
            }
            if token == "" || token != deps.Token {
                return c.JSON(http.StatusUnauthorized, &apiResponse{
                    Error: &apiError{Code: "ERR_UNAUTHORIZED", Message: "missing or invalid service token"},
                })
            }
            return next(c)
        }
    }
}
```

认证中间件的设计要点：

- **可选启用**：`deps.Token == ""` 时跳过认证，方便开发和测试环境使用。生产环境应始终配置 `SERVICE_TOKEN`。
- **双来源**：优先从请求头 `X-Service-Token` 获取，回退到查询参数 `?token=`。请求头方式适合程序化调用，查询参数方式适合浏览器直接访问（如 `/api/queue/stats?token=xxx`）。
- **简单比较**：使用字符串直接比较而非 HMAC/JWT，因为这是内部服务间认证，不涉及用户身份和权限分级。简单的 Token 比较足够防止未授权访问，且开销极低。

#### 5.4.4 路由注册

```go
func RegisterRoutes(e *echo.Echo, deps *Deps) {
    e.GET("/", healthPing())
    e.GET("/healthz", healthPing())

    g := e.Group("/api/queue")
    g.Use(tokenAuth(deps))

    g.POST("/enqueue", enqueue(deps))
    g.POST("/lease", lease(deps))
    g.POST("/complete", complete(deps))
    g.POST("/fail", fail(deps))
    g.POST("/yield", yield(deps))
    g.POST("/cancel", cancel(deps))
    g.POST("/awaken", awaken(deps))
    g.GET("/stats", stats(deps))
    g.GET("/tasks", listTasks(deps))
    g.GET("/tasks/:id", getTask(deps))
}
```

路由设计遵循 REST 风格：

- `/` 和 `/healthz` 无需认证，用于健康检查和负载均衡器探测。
- 所有业务端点在 `/api/queue` 路由组下，统一应用 `tokenAuth` 中间件。
- 写操作使用 `POST`，读操作使用 `GET`。
- 资源嵌套：`/api/queue/tasks` 列出任务，`/api/queue/tasks/:id` 获取单个任务。

#### 5.4.5 Lease Handler 的 leaseOwner 生成

```go
func lease(deps *Deps) echo.HandlerFunc {
    return func(c echo.Context) error {
        // ...
        leaseOwner := c.Request().Header.Get("X-Lease-Owner")
        if leaseOwner == "" {
            leaseOwner = c.RealIP() + ":github.com/soulteary/gorge-task-queue"
        }
        // ...
    }
}
```

`leaseOwner` 的生成策略：

- **显式指定**：优先使用请求头 `X-Lease-Owner`，允许调用方自定义标识。例如同一台主机上运行多个 worker 实例时，可以通过不同的 `X-Lease-Owner` 区分。
- **自动生成**：未指定时，使用 `RealIP() + ":gorge-task-queue"` 自动生成。`RealIP()` 考虑了 `X-Real-IP`、`X-Forwarded-For` 等代理头，确保即使在反向代理后也能获取正确的客户端 IP。后缀 `:gorge-task-queue` 用于区分来自同一 IP 的不同服务。

### 5.5 入口程序（cmd/server/main.go）

```go
func main() {
    cfg := config.LoadFromEnv()

    store, err := queue.NewStore(cfg.DSN(), cfg.LeaseDuration, cfg.RetryWait)
    if err != nil {
        fmt.Fprintf(os.Stderr, "failed to connect to database: %v\n", err)
        os.Exit(1)
    }
    defer func() { _ = store.Close() }()

    e := echo.New()
    e.Use(middleware.Logger())
    e.Use(middleware.Recover())

    httpapi.RegisterRoutes(e, &httpapi.Deps{
        Store: store,
        Token: cfg.ServiceToken,
    })

    e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
```

入口程序的启动流程简洁清晰：

1. **加载配置**：从环境变量读取所有配置项。
2. **初始化 Store**：连接 MySQL 并验证连通性。失败时直接写 stderr 并 `os.Exit(1)`——数据库是硬依赖，不可达时没有重试的意义。
3. **defer Close**：确保程序退出时关闭数据库连接池，释放 MySQL 连接。
4. **创建 Echo 实例**：注册 Logger（请求日志）和 Recover（panic 恢复）两个全局中间件。
5. **注册路由**：将 Store 和 Token 注入到 HTTP Handler 中。
6. **启动服务**：`e.Start` 阻塞直到服务退出。`e.Logger.Fatal` 确保启动失败（如端口被占用）时打印错误并退出。

## 6. 任务生命周期

### 6.1 完整状态流转

```
                              Enqueue
                                │
                                ▼
                      ┌──────────────────┐
                      │   New Task       │
                      │                  │
                      │  leaseOwner=NULL │
                      │  leaseExpires=   │
                      │    NULL or       │
                      │    DelayUntil    │
                      │  failureCount=0  │
                      └────────┬─────────┘
                               │
                         Lease │
                               ▼
                      ┌──────────────────┐
                      │   Leased         │
                      │                  │
                      │  leaseOwner=     │
                      │    "worker-id"   │
                      │  leaseExpires=   │
                      │    now + dur     │
                      └────────┬─────────┘
                               │
               ┌───────────────┼───────────────┐
               │               │               │
          Complete            Fail           Yield
               │               │               │
               ▼               ▼               ▼
       ┌──────────────┐ ┌────────────┐ ┌──────────────┐
       │  Archived    │ │ permanent? │ │  Yielded     │
       │  result=0    │ │            │ │  leaseOwner= │
       │  (成功)       │ │  yes  no   │ │   "(yield)"  │
       └──────────────┘ │   │    │   │ │  leaseExpires│
                        │   ▼    ▼   │ │   = 延长     │
               Cancel   │ ┌──┐ ┌──┐ │ └───────┬──────┘
                 │      │ │归│ │重│ │         │
                 ▼      │ │档│ │试│ │    Awaken│
       ┌──────────────┐ │ └──┘ └──┘ │         │
       │  Archived    │ │  │    │   │         ▼
       │  result=2    │ │  ▼    ▼   │ ┌──────────────┐
       │  (取消)       │ │ 归档  等待 │ │  Awakened    │
       └──────────────┘ │result 后可 │ │  leaseExpires│
                        │  =1  重新  │ │   = 过去时间  │
                        │     Lease  │ │  (可被重新    │
                        └────────────┘ │   Lease)     │
                                       └──────────────┘
```

### 6.2 租约自愈机制

当 worker 进程崩溃或网络中断时，其持有的租约不会被显式释放。自愈通过租约过期实现：

1. Worker A 租用任务 T，设置 `leaseExpires = now + 7200`（2 小时）。
2. Worker A 崩溃，任务 T 的 `leaseOwner` 和 `leaseExpires` 保持不变。
3. 2 小时后，任务 T 的 `leaseExpires < now`，符合 Lease 第二阶段的选择条件。
4. Worker B 执行 Lease，在第二阶段选中任务 T，覆盖 `leaseOwner` 和 `leaseExpires`。

这种基于时间的自愈机制是分布式任务队列的常见模式，无需额外的心跳或故障检测组件。代价是存在最多 `LEASE_DURATION` 的恢复延迟——worker 崩溃后，任务最多需要等待一个完整的租约周期才能被重新分配。

### 6.3 延迟执行

```
Enqueue(DelayUntil=T)
    │
    ▼
┌──────────────────────────────┐
│  leaseOwner = NULL           │
│  leaseExpires = T (未来时间)  │
│                              │
│  在 T 之前:                   │
│    Phase 1: leaseOwner=NULL  │
│      → 符合条件，但...        │
│    Phase 1 SELECT 不检查     │
│      leaseExpires            │
│    Phase 2: leaseExpires > ? │
│      → T > now → 不符合条件  │
│                              │
│  在 T 之后:                   │
│    Phase 2: leaseExpires < ? │
│      → T < now → 符合条件 ✓  │
└──────────────────────────────┘
```

需要注意的是，Lease 第一阶段的 SELECT 查询条件只检查 `leaseOwner IS NULL`，不检查 `leaseExpires`。但在 UPDATE 时，条件是 `WHERE leaseOwner IS NULL AND id IN (...)`，这仍然会更新延迟任务。实际上，延迟执行的保护依赖于 Lease 的整体流程——第一阶段会选中延迟任务，但最终的 fetch 查询 `WHERE t.leaseOwner = ? AND t.leaseExpires > ?` 会正确返回已租用的任务（包括延迟的）。延迟任务在被租用后，worker 会获取到它并按正常流程处理。

## 7. 与 Phorge PHP 端的对应关系

| Go 实现 | PHP 方法/类 | 差异说明 |
|---------|------------|----------|
| `Store.Enqueue` | `PhabricatorWorker::scheduleTask()` | 两步事务（taskdata + activetask） |
| `Store.Lease` | `PhabricatorWorkerLeaseQuery::execute()` | 两阶段选择，事务内完成 |
| `Store.Complete` | `PhabricatorWorkerActiveTask::archiveTask(RESULT_SUCCESS)` | 共享 archiveTask 实现 |
| `Store.Fail` (permanent) | `PhabricatorWorkerActiveTask::archiveTask(RESULT_FAILURE)` | 永久失败直接归档 |
| `Store.Fail` (retry) | `PhabricatorWorkerActiveTask::setFailureCount()` | 释放租约 + 设置重试等待 |
| `Store.Yield` | `PhabricatorWorkerYieldException` | 设置特殊 leaseOwner |
| `Store.Cancel` | `PhabricatorWorkerActiveTask::archiveTask(RESULT_CANCELLED)` | 共享 archiveTask 实现 |
| `Store.Awaken` | `PhabricatorWorker::awakenTaskIDs()` | 设置过去时间触发重新 Lease |
| `Store.Stats` | `bin/phd status` (部分) | 四维统计概览 |
| `Store.GetTask` | `PhabricatorWorkerActiveTask::loadTaskDataResults()` | JOIN 查询含 data |
| `Store.ListActive` | Phorge 管理界面任务列表 | 分页查询 |
| `PriorityAlerts` (1000) | `PhabricatorWorker::PRIORITY_ALERTS` | 值完全一致 |
| `PriorityDefault` (2000) | `PhabricatorWorker::PRIORITY_DEFAULT` | 值完全一致 |
| `ResultSuccess` (0) | `PhabricatorWorkerArchiveTask::RESULT_SUCCESS` | 值完全一致 |
| `ResultFailure` (1) | `PhabricatorWorkerArchiveTask::RESULT_FAILURE` | 值完全一致 |
| `ResultCancelled` (2) | `PhabricatorWorkerArchiveTask::RESULT_CANCELLED` | 值完全一致 |
| `YieldOwner` "(yield)" | `PhabricatorWorkerYieldException` 特殊处理 | 标识符一致 |

关键兼容点：

- **表结构**：直接操作 Phorge 原有的 `{namespace}_worker` 数据库，不新建表。
- **常量值**：优先级、归档结果等常量与 PHP 端值完全一致。
- **ID 保持**：归档时保留原始 task ID，PHP 端可以继续通过 ID 查询归档任务。
- **字段语义**：`leaseOwner`、`leaseExpires`、`failureCount` 等字段的语义与 PHP 端一致。

## 8. 部署方案

### 8.1 Docker 镜像

采用多阶段构建：

- **构建阶段**：基于 `golang:1.22-alpine`，使用 `CGO_ENABLED=0` 静态编译，`-ldflags="-s -w"` 去除调试信息和符号表以缩小二进制体积。
- **运行阶段**：基于 `alpine:3.20`，仅包含编译后的二进制和 CA 证书。

暴露 8090 端口。

内置 Docker `HEALTHCHECK`，每 10 秒通过 `wget` 检查 `/healthz` 端点，启动等待 5 秒，超时 3 秒，最多重试 3 次。

### 8.2 部署建议

**网络访问**：gorge-task-queue 作为内部服务，通常不需要直接面向外部用户。建议仅对内部 Go 微服务和 Phorge PHP 应用开放。可通过防火墙规则或 Docker 网络隔离限制访问。

**Token 配置**：生产环境务必配置 `SERVICE_TOKEN`，防止未授权的任务操作。Token 应通过 Docker secrets 或环境变量注入，不应硬编码在镜像中。

**数据库共享**：gorge-task-queue 与 Phorge PHP 应用共享同一 MySQL 数据库。需要注意连接数配额——MySQL 的 `max_connections` 需要同时满足 PHP 连接池（通常是 apache/nginx + php-fpm 的 worker 数量）和 Go 连接池（25）的需求。

**租约时长**：`LEASE_DURATION` 默认 2 小时，应根据任务的最长执行时间设置。过短会导致正在执行的任务被其他 worker 重新获取（重复执行），过长会延长 worker 崩溃后的恢复时间。

**重试等待**：`RETRY_WAIT` 默认 5 分钟，应根据任务的失败原因调整。网络抖动导致的失败可以缩短等待，资源不足导致的失败应延长等待。

## 9. 依赖分析

| 依赖 | 版本 | 用途 |
|------|------|------|
| `labstack/echo` | v4.12.0 | HTTP 路由框架，提供路由分组、中间件、请求绑定等功能 |
| `go-sql-driver/mysql` | v1.8.1 | MySQL 驱动，支持连接池、预处理语句、事务 |

直接依赖两个。Echo 框架引入了一组间接依赖（`labstack/gommon`、`valyala/fasttemplate` 等），总共 12 个依赖（含间接）。

选择 Echo 而非标准库 `net/http` 的原因：Echo 提供了路由分组、中间件链、参数绑定、JSON 响应等便利功能，减少了样板代码。对于本项目的 10 个 API 端点，Echo 的路由分组和中间件功能显著简化了代码组织。与 gorge-notification（使用标准库 `net/http`，仅 3 个路由）相比，gorge-task-queue 的 API 数量和路由复杂度更适合使用框架。

## 10. 测试覆盖

项目包含两组测试文件：

| 测试文件 | 覆盖范围 |
|----------|----------|
| `config_test.go` | 环境变量默认值验证（MySQLHost / MySQLPort / ListenAddr / LeaseDuration / RetryWait / MaxWorkers）、自定义环境变量覆盖、DSN 字符串拼接 |
| `store_test.go` | `placeholders` 函数（边界值 0 / 1 / 3 / 5）、`appendArgs` 参数展开（混合类型 + []int64 切片展开）、`nullStr` 空值处理、优先级常量排序验证、归档结果常量值验证 |

测试设计的特点：

- **表驱动测试**：`TestPlaceholders` 使用 table-driven 风格，覆盖多种输入组合，便于扩展新用例。
- **环境隔离**：`config_test.go` 使用 `t.Setenv` 设置测试环境变量，测试结束后自动恢复，不影响其他测试。
- **纯单元测试**：当前测试仅覆盖不依赖外部资源（数据库）的纯逻辑函数和常量。Store 的核心方法（Enqueue / Lease / Complete / Fail / Yield / Cancel / Awaken）和 HTTP API 层的 Handler 函数需要 MySQL 环境或 mock，尚未被测试覆盖。

## 11. 总结

gorge-task-queue 是一个职责单一的任务队列微服务，核心价值在于：

1. **数据完全兼容**：直接操作 Phorge 原有的三张 worker 表，常量值、字段语义、ID 保持等与 PHP 端完全一致，切换时无需迁移数据。
2. **基于 MySQL 的租约模型**：通过 `leaseOwner` 和 `leaseExpires` 实现分布式任务锁定，两阶段租约选择（未租用 → 过期租约）确保任务不丢失、不重复执行。
3. **完整的任务生命周期**：入队、租约、完成、失败（永久/可重试）、让步、唤醒、取消七个核心操作覆盖了任务从创建到归档的所有状态流转。
4. **租约自愈**：worker 崩溃时租约自动过期，无需额外的心跳或故障检测机制，系统在最多一个租约周期后自动恢复。
5. **HTTP API 化**：所有任务队列操作通过 REST API 暴露，其他 Go 微服务可以直接调用，消除了对 PHP Conduit 层的依赖。
6. **极简实现**：核心代码约 570 行（不含测试），直接依赖仅 Echo 和 MySQL 驱动两个库，模块划分清晰（config / queue / httpapi），易于理解和维护。
