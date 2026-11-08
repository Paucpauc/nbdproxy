# nbdproxy

[English](README.md) | [Русский](README_RU.md) | 简体中文 | [Español](README_ES.md) | [Français](README_FR.md)

一个小型、无外部依赖的 NBD 代理，可在重新连接已重启的后端时保持**本地客户端套接字连接不断开**。两端均支持 TCP 和 Unix 套接字。Linux `nbd-client` 无需 `-persist` 或 netlink 重连支持，包括 3.26.1 等版本。

```text
Linux /dev/nbd0 ── stable local socket ── nbdproxy ── reconnect ── NBD server
```

适用于 s3nbd 等独立于代理重启的本地服务器。每个进程将一个配置好的导出提供给**一个客户端连接**。所有请求按线路接收顺序执行，最多只有一个尚未完成的后端请求。这是以吞吐量换取简单的重放顺序。

## 构建与运行

需要 Go 1.24 或更新版本；无需外部模块或 C 库。

```sh
go build -trimpath -o nbdproxy .
./nbdproxy -version
sudo install -d -m 0755 /run/nbdproxy
sudo ./nbdproxy \
  -listen unix:/run/nbdproxy/nbd.sock \
  -upstream tcp:127.0.0.1:10809 \
  -export disk1
```

在 Linux 的另一个终端中运行：

```sh
sudo modprobe nbd
sudo nbd-client -unix /run/nbdproxy/nbd.sock /dev/nbd0 -N disk1 -b 4096
```

使用**单个连接**，不要使用 `-C 2`。后端重启无需 `-persist`。如果中断可能超过内核 NBD I/O 超时时间，应避免设置有限的超时；即使本地套接字仍保持连接，内核请求超时也可能导致 I/O 失败。对于已有设备，请检查 `/sys/block/nbd0/queue/io_timeout`（单位为毫秒，取决于内核是否提供）。`nbd-client -timeout` 为零时会保留内核的现有或默认设置，不能可靠地清除先前为重复使用的设备设置的超时。请确认内核的实际设置。

TCP 前端和 Unix 后端示例：

```sh
./nbdproxy -listen tcp:127.0.0.1:10810 -upstream unix:/run/s3nbd/nbd.sock -export disk1
sudo nbd-client 127.0.0.1 10810 /dev/nbd0 -N disk1 -b 4096
```

两端均不使用身份验证或 TLS。TCP 监听应限制在回环地址或可信的受限网络。前端 Unix 套接字以 0600 权限创建，父目录必须已存在。启动时不会直接删除已有套接字路径；如有必要，请先检查，再手动删除残留套接字。正常关闭时，Go 会删除 Unix 监听器的套接字。

## 重启时的行为

1. 代理先与后端协商配置的导出，再完成客户端协商。首次连接时后端不可用会延迟协商，因此最初的 `nbd-client` 连接仍可能超时，需要重试。
2. 在传输阶段，每个请求及其 WRITE 数据保留在内存中，直到收到完整的后端响应。不完整的 READ 响应会被丢弃。
3. 出现拨号、协商、传输或后端 I/O 超时错误时，代理会等待并无限重连。本地客户端套接字保持连接。后端 NBD 错误响应正常转发，不重试。
4. 重连时，后端大小和传输标志必须与原始值完全相同。不匹配会终止客户端会话，避免向不兼容的导出重放请求。配置的地址和名称必须仍指向**同一存储**：NBD 没有强制的持久导出 UUID，因此大小、标志和名称检查无法检测替换为另一个同样大小的磁盘。
5. 待处理请求在任何后续请求提交之前重放。

重放保证是**至少一次**，而非恰好一次。WRITE 可能在响应丢失前已完成。只有旧后端操作不可能在较新操作之后再次执行时，重复才安全。已停止的后端进程满足此条件；任意网络分区、仍在运行或挂起的服务器超时以及并发写入，都需要后端提供隔离（fencing）或串行化保证。不要将其用作通用分布式故障转移系统，也不要与其他写入者同时使用。

没有日志或持久写缓存。后端若确认了易失性写入，崩溃时可能丢失这些数据；代理无法恢复已向内核确认的请求。后端 FLUSH/FUA 的持久性保证仍然重要。重放还要求后端 WRITE/TRIM 实现可安全重复执行。重启或终止**代理本身**会断开客户端连接，不在后端重连机制的保障范围内。

## 协议与资源限制

- Fixed-newstyle NBD；前端支持 EXPORT_NAME、INFO、GO、LIST、ABORT。
- 支持 READ、WRITE、FLUSH、TRIM 和协商后的 FUA；仅支持简单响应。
- 后端协商使用配置名称的 EXPORT_NAME。后端必须接受该模式及其隐含的块大小限制（最小 1 字节）。
- 不支持的选项（包括结构化响应和 TLS）返回 ERR_UNSUP；不支持的命令或标志返回 EINVAL。只读导出拒绝写入。
- 即使后端声明支持多连接，该能力也会被屏蔽。第二个前端连接会直接关闭，不进行协商。
- READ/WRITE 上限为每个请求 32 MiB。一个正在执行的请求加一个预读请求；TCP/套接字缓冲提供背压。应用数据缓冲区每个活跃会话约不超过 96 MiB（包含读取响应），与中断时长无关。TRIM 范围不携带数据。
- 无效魔数、过大的选项或 WRITE 数据、客户端断开都会结束会话。永不完成请求发送的客户端可以占用唯一的会话；这是可信本地服务，不是公开的多租户端点。
- 后端故障通过 Go 结构化日志记录器写入 stderr。持续无效的后端握手或响应会被重试；请查看日志诊断。

使用 `./nbdproxy -help` 查看选项：

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-listen` | `unix:/run/nbdproxy/nbd.sock` | 本地监听地址 |
| `-upstream` | `tcp:127.0.0.1:10809` | 后端地址 |
| `-export` | 空 | 两端的导出名称 |
| `-retry` | `1s` | 重连尝试之间的固定延迟 |
| `-connect-timeout` | `5s` | 后端连接超时 |
| `-handshake-timeout` | `30s` | 每个连接的协商超时 |
| `-io-timeout` | `1m` | 后端请求期限和向前端写入响应的期限 |

后端请求超时触发重连，不限制总恢复时间。传输阶段代理不设置前端读取期限，因此空闲客户端保持连接。SIGINT/SIGTERM 会关闭监听器和活跃会话，取消重试并退出。停止代理前，请卸载文件系统并断开 NBD。

## systemd

安装二进制文件，修改附带单元中的 upstream/export，然后启用：

```sh
sudo install -m 0755 nbdproxy /usr/local/bin/nbdproxy
sudo install -m 0644 packaging/nbdproxy.service /etc/systemd/system/nbdproxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now nbdproxy
sudo journalctl -u nbdproxy -f
```

该单元使用 DynamicUser，创建 `/run/nbdproxy`，无需 root 权限。Root 可以将 `nbd-client` 连接到权限为 0600 的套接字。Unix 后端必须允许服务用户访问；这种部署应将 DynamicUser 替换为固定用户/组，并设置适当的套接字权限。保持该单元与后端服务独立：不要添加在后端重启时停止代理的 PartOf/BindsTo。Restart=on-failure 会重启进程，但无法保留已有 NBD 会话。

## 验证

```sh
go vet ./...
go test -race ./...
go test -run='^$' -fuzz=FuzzExportName -fuzztime=10s
```

测试覆盖已完成写入但响应丢失、不完整读取响应、flush 顺序、导出变化、取消、异常协商、请求验证和解析器模糊测试。CI 在 Linux 和 macOS 上运行测试，并构建所有发布目标。Linux 作业还通过发行版提供的 nbd-client 连接真实 `/dev/nbd0`，执行直接 I/O，停止后端，验证 I/O 保持等待，重启后端，再验证数据以及后续写入和 flush。

要手动运行这个会改写设备的集成测试，请使用**具有未使用的 `/dev/nbd0` 的一次性 Linux 主机**，并安装 nbd-client：

```sh
sudo modprobe nbd nbds_max=4 max_part=0
go test -c -o /tmp/nbdproxy.test
sudo env NBDPROXY_KERNEL_TEST=1 /tmp/nbdproxy.test \
  -test.v -test.run '^TestLinuxKernelRestart$' -test.timeout 60s
```

测试会写入 `/dev/nbd0`，必须显式启用，普通测试运行会跳过。真实内核测试无法在 macOS 上运行。

## 发布

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.0
./scripts/release.sh v0.1.0
```

在 `dist/` 中生成 Linux amd64/arm64 和 macOS amd64/arm64 归档，每个归档包含独立二进制文件、文档、许可证和 systemd 单元，同时生成 Linux amd64/arm64 的 DEB 和 RPM 软件包以及所有产物的 SHA-256 校验和。

将仓库推送到 GitHub 后，推送 `v*` 标签会触发发布工作流。它执行 vet 和测试，构建归档和软件包，并立即发布带自动生成说明的 GitHub Release。CI 和发布工作流仅具有读取权限，发布作业的 contents:write 除外。

协议参考：[NBD 规范](https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md)。
许可证：MIT。
