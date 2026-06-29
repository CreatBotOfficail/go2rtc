# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

`go2rtc` 是一个零依赖、超低延迟的摄像头/音视频流媒体服务器,同时充当多种协议的**协议转换器**和**代理**。它从一种或多种"源"协议(RTSP/RTMP/HTTP/ONVIF/FFmpeg/HomeKit 等)拉流,然后通过"消费"协议(WebRTC/RTSP/HLS/MP4/MJPEG/HomeKit)分发给客户端。

`main.go` 是一切的总入口 — 所有模块通过 `Init()` 函数顺序注册,顺序本身有含义(基础先于扩展,API 必须先于使用 API 的模块)。

## 常用命令

```bash
# 编译(与 CI 完全一致: -ldflags "-s -w" -trimpath)
go build -ldflags "-s -w" -trimpath

# 编译为可分发的二进制(在当前 OS/arch)
go build -ldflags "-s -w" -trimpath -o go2rtc

# 跨平台编译
GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -trimpath

# 运行单元测试
go test ./...

# 运行单个包的测试
go test ./pkg/core/...

# 运行单个测试用例
go test ./pkg/core/ -run TestReceiver -v

# 静态检查 / vet
go vet ./...

# 模块版本(项目要求 Go 1.21+,CI 使用 1.24)
go version

# 跨平台批量发布(Windows/macOS/Linux/FreeBSD 全部架构)
./scripts/build.sh    # 需要本机装 go、7z、upx
```

运行时:
- 默认 API 端口 `1984`(HTTP + WebSocket)
- 默认 RTSP 服务端口 `8554`
- 默认 WebRTC 端口 `8555`(TCP+UDP)
- 默认配置文件:`./go2rtc.yaml`(可通过 `-c path` 多次指定,支持原始 YAML/JSON 字符串如 `log.level=debug`)

## 核心架构

### 抽象层:`pkg/core`

整个项目围绕 `core.Producer` / `core.Consumer` 两个接口流转,数据以 `*rtp.Packet` 为单位,通过 `core.Receiver`(源端)→ `core.Node` 树 → `core.Sender`(消费端)连接。

- `core.Producer`:`GetMedias() / GetTrack() / Start() / Stop()`
- `core.Consumer`:`GetMedias() / AddTrack() / Stop()`
- `core.Connection` 是大部分实现的内嵌结构(已带 `Medias/Receivers/Senders/Transport/RemoteAddr/...`),`core.Receiver` / `core.Sender` 继承 `core.Node`,后者维护父/子节点链表,通过 `WithParent` / `Input` / `Output` 串联。
- `core.Packet` 是对 `*rtp.Packet` 的薄包装,`pkg/core/listener.go` 提供事件订阅。

### 流编排层:`internal/streams`

中心数据结构是 `Stream`(可对应 YAML 顶层 `streams: <name>: ...`),它持有多个 `*Producer` + `[]core.Consumer`。

关键文件:
- `streams.go` — 全局 `streams` map(`Get/New/Patch/Delete`),以及 `GetOrPatch(query)` 用 HTTP `?src=&name=` 动态建流。
- `stream.go` — `Stream` 类型,`AddConsumer` 通过 `add_consumer.go` 实现,`AddProducer` 支持外部推送(`stateExternal`)。
- `producer.go` — 单个 `Producer` 的状态机:`stateNone → stateMedias → stateTracks → stateStart`,带自动重连(`reconnect` 退避策略:`<5s:1s`,`<10s:5s`,`<20s:10s`,否则 `1min`)和"幽灵连接"修复(迁移 track 后再 `Stop` 老 conn,避免 exec/ffmpeg 卡死)。
- `add_consumer.go` — **核心匹配算法**:遍历 consumer 的 `Media`,对每个 producer 调 `Dial()`,再调 `prod.GetMedias()`,然后用 `Media.MatchMedia` 协商 codec,`recvonly` 走 `prod.GetTrack → cons.AddTrack`,`sendonly`(反向音频)走 `cons.GetTrack → prod.AddTrack`。这就是"多源 codec 协商"的入口。
- `handlers.go` — **scheme 注册表**:`HandleFunc(scheme, handler)` 把字符串 scheme(如 `rtsp`、`http`、`ffmpeg`)映射到 `Handler func(url) (core.Producer, error)`。`GetProducer(url)` 通过 `:` 前缀做协议分发;`RedirectFunc` 支持重定向(`onvif://...` → `rtsp://...`)。
- `publish.go` / `play.go` — `publish:` / `stream to camera` 配置的处理。

### 应用框架:`internal/app`

- `app.go` — `app.Version` / `app.Info` / `app.UserAgent`,处理 `-c/--config -d/--daemon -v/--version` 标志;守护模式(Linux/macOS):fork 后退出父进程。
- `config.go` — `LoadConfig(v any)` 把所有 yaml 源(嵌入的 `embedded_config.yaml` + `-c` 指定的文件或内联字符串)顺序 unmarshal 到同一结构体(后面的覆盖前面)。`parseConfString` 把 `a.b.c=val` 展开成 `{a:{b:{c:val}}}`。`PatchConfig` 支持运行时通过 API 修改配置文件。
- `log.go` — `initLogger` 支持 `log.level/format/output/time` + 按模块(模块名作为 logger name)独立设置 level;`app.GetLogger("streams")` 返回带正确级别的 logger;`app.MemoryLog` 是 16×64KB 环形 buffer,给 `/api/log` 用。
- `embedded_config.yaml` — 编译期嵌入的默认配置,包含每个模块的默认 `listen` 端口等。

### HTTP/WS API:`internal/api`

- `api.go` — `api.HandleFunc(pattern, handler)` 注册路由,`static.go` 嵌入 `www/` 静态资源(HTTP 控制台)。
- `api/ws/ws.go` — WebSocket 端点,客户端通过 WS 发送 offer/answer/candidate,服务端返回 SDP/ice。
- 协议约定:`?src=streamName&name=newName` 用于动态创建/查询流,`?dst=...` 用于推送。
- 大多数模块(HLS/MP4/MJPEG/webrtc/...)的 `Init()` 既注册 HTTP handler,也注册 WS handler。

### WebRTC:`internal/webrtc`

- `webrtc.go` — 服务器入口(`Init()` 监听 `webrtc.listen`),依赖 Pion (`pion/webrtc/v3`)。
- `server.go` / `candidates.go` — 处理 SDP 协商与 ICE candidate。
- 其它文件(`kinesis.go` / `milestone.go` / `openipc.go` / `creality.go` / `switchbot.go`)是**特定摄像头厂商的 WebRTC 客户端**:它们不监听,作为"source"主动连出去,适合走 WebRTC signaling 的私有云摄像头。

### RTSP / RTMP / HomeKit / Hass

- `internal/rtsp` — 客户端(通过 `streams.HandleFunc("rtsp", ...)` 注册) + 服务端(默认 `:8554`),支持 RTSP/RTSPS/RTSPX。
- `internal/rtmp` — 客户端(读 FLV)+ 服务端(写 FLV)。
- `pkg/hap` + `internal/homekit` — HomeKit Accessory Protocol 实现,`pkg/hap/camera` 复用 `streams.Stream` 包装成 HAP camera accessory。
- `internal/hass` — Home Assistant 集成(`hass://` 源、API 服务端)。

### GStreamer 分支(本仓库特有)

- `internal/gstreamer/gstreamer.go` — 高级 `gstreamer:NAME?param#flags` 解析,把"命名管线"展开成 `exec:gst-launch-1.0 ...` 或 `gstreamer:device?video=...`。
- `internal/gstreamer/args.go` / `bin.go` — 参数化模板(`#caps=#process=#output=#share=#postProcess=#bin=`)的实现。
- `pkg/gstreamer/producer.go` / `client.go` / `client_other.go` — `core.Producer` 实现,内部分两种 IO:**Unix socket 模式**(本仓库新增,见最近一次提交)和 **stdin/stdout pipe 模式**。`client_linux.go`(通过 `client_other.go` 的 build tag)在 Unix 域套接字上与本地 GStreamer 进程交换 RTP;`client_other.go`(Windows 等)走 exec pipe。
- **修改 GStreamer 模块时**:socket 协议约定见 `pkg/gstreamer/producer.go` 和 `internal/gstreamer/gstreamer.go`;任何 producer 改动都要确认 `internal/gstreamer/gstreamer_test.go` 仍然通过。

### pkg/ 下的协议/编解码库

- 流封装:`h264 / h265 / aac / opus / mp3 / pcm / flv / mp4 / hls / mpegts / mjpeg / mpjpeg / y4m / wav`
- 工具:`core / shell / yaml / mdns / xnet / magic / probe / mqtt / bits / ascii / iso / image`
- 这些包都是**纯实现细节**,不依赖 `internal/`,可单独被外部项目引用;`pkg/core` 是其它所有包的基础。

## 扩展新协议/源的约定

新增一个 source 类型(假设叫 `foo`):

1. 在 `pkg/foo/` 下实现 `core.Producer`(或嵌入 `core.Connection` 直接获得 `GetMedias/GetTrack/Stop`)。
2. 在 `internal/foo/foo.go` 写 `Init()`:
   - `app.LoadConfig(&cfg)` 读自己模块的 yaml 段
   - `app.Info["foo"] = cfg` 暴露给 `/api` 的信息接口
   - `log = app.GetLogger("foo")`
   - `streams.HandleFunc("foo", fooHandler)` 注册 scheme
   - 如需服务端,`net.Listen` + 启动 goroutine
3. 在 `main.go` 的"5. Other sources"或"4. Other sources and servers"区块添加 `foo.Init()`。

> 不要在 `internal/streams` 之外保存流,也不要绕开 `streams.HandleFunc` 直接生产 `core.Producer` — 这会破坏 `stream.stopProducers()` 的引用计数和 `add_consumer.go` 的 codec 协商。

## 关键设计要点

- **多源混合**:`streams.<name>: [rtsp://..., ffmpeg:..., ...]` 可以让 consumer 从不同 source 取不同 track,`add_consumer.go` 的 `consMedia.MatchAll()` 控制是一对一还是多对一。
- **零延迟目标**:尽量避免 transcode;只在不支持的 codec 上才用 FFmpeg fallback(`internal/ffmpeg`)或 `pkg/magic` 自动转 JPEG keyframe。
- **反向音频(backchannel)**:`media.Direction == sendonly` 的链路,`Producer` 充当 `Consumer`(`AddTrack`),`Consumer` 充当 `Producer`(`GetTrack`)— 见 `internal/streams/add_consumer.go:71-84`。
- **YAML 多源合并**:`embedded_config.yaml` 先于 `go2rtc.yaml` 解析,所以用户文件覆盖默认。

## 注意事项

- 本仓库是 upstream `AlexxIT/go2rtc` 的**内部分支**,`go2rtc.yaml` 包含 `creatbot` 设备相关的凭据和 `internal/gstreamer` 等定制;改动提交前请确认是否需要同步到 upstream。
- `.codegraph/` 已建立索引(本会话用 `mcp__codegraph__codegraph_explore` 探索);**新问题先查 codegraph**,再决定 Read。
- `go2rtc_linux_arm64` 是已经编译好的二进制,不要误提交改动。
- CGo 默认关闭(`build.yml` 设 `CGO_ENABLED=0`),如需 `pkg/gstreamer` 的 Unix socket 模式,本地 `go build` 不要带 `CGO_ENABLED=0` 之外的额外限制(具体见 `pkg/gstreamer/client_*.go` 的 build 约束)。
