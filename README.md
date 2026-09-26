# tvtrim

电视剧剧集**批量去头去尾**工具,Go 实现,基于 [ffmpeg-trim](https://github.com/totootao/ffmpeg-trim)。

砍掉每集固定的片头曲和片尾曲时长,**零重编码** —— 只做 stream copy,速度接近磁盘拷贝。

```console
$ tvtrim -head 90s -tail 60s ./某剧第一季/

tvtrim 1.0.0
  片头砍掉: 1m30.0s
  片尾砍掉: 1m00.0s
  ffmpeg  : /root/.cache/tvtrim/ffmpeg-linux-amd64
  待处理  : 24 个文件
  输出方式: 同目录生成,后缀 "-trim"

============================================================
完成: 24 成功, 耗时 1.2s
体积: 8.64 GB -> 7.06 GB

输出文件已按 "-trim" 后缀生成,确认无误后可自行删除原文件。
```

---

## 特性

- **零重编码**:`-c copy` 原样搬运数据,不损失画质,速度极快
- **批量处理**:给一个目录就自动扫出所有剧集,并发处理
- **集数识别**:自动解析 `S01E02` / `第02集` / `EP02` 等命名并按集排序
- **mp4 快速起播**:输出自动加 `-movflags +faststart`(moov 移到文件头)
- **安全默认**:默认生成新文件不碰原片,输出时长过短自动跳过,重复运行拒绝覆盖;
  空输出检测兜底,不产生"看似成功实为空文件"的垃圾
- **原地替换**:`-inplace` 先写临时文件再原子替换,中途失败不会损坏原片
- **ffmpeg 自动获取**:首次运行自动从 ffmpeg-trim 下载对应架构的二进制并校验
- **零第三方依赖**:只用 Go 标准库

## 安装

```sh
# 从源码编译(需要 Go 1.21+)
git clone https://github.com/totootao/tvtrim.git
cd tvtrim
go build -o tvtrim ./cmd/tvtrim
```

也可以直接用 `go install`:

```sh
go install github.com/totootao/tvtrim/cmd/tvtrim@latest
```

## 用法

```sh
tvtrim [选项] <文件或目录> [更多文件或目录...]
```

### 常用示例

```sh
# 砍掉 90 秒片头 + 60 秒片尾,输出 xxx-trim.mp4
tvtrim -head 90 -tail 60 ./某剧.S01/

# 先预览会做什么,不动文件
tvtrim -head 1m30s -tail 45s ./某剧.S01/ -dry-run

# 递归处理整个剧集目录
tvtrim -r -head 90s -tail 60s ./某剧全集/

# 直接替换原文件(自动开启 -overwrite,建议先备份)
tvtrim -head 90s -tail 60s -inplace ./某剧.S01/

# 指定输出到单独目录(仅单文件)
tvtrim -head 90s -tail 60s -o ./out/ep01.mp4 ./in/ep01.mkv

# 提高并发(机械硬盘建议 1-2,SSD 可用默认值)
tvtrim -head 90s -tail 60s -j 4 ./某剧.S01/
```

### 全部选项

| 选项 | 说明 | 默认值 |
|---|---|---|
| `-head <时长>` | 要砍掉的片头时长 | — |
| `-tail <时长>` | 要砍掉的片尾时长 | — |
| `-keep <时长>` | 结尾额外保留的安全余量,会让输出更短 | `0` |
| `-suffix <后缀>` | 输出文件名后缀,`a.mp4` → `a-trim.mp4` | `-trim` |
| `-inplace` | 原地替换原文件(先写临时文件再原子替换) | 关闭 |
| `-o <路径>` | 显式指定输出文件(仅单文件) | — |
| `-r` | 递归处理子目录 | 关闭 |
| `-j <数量>` | 并发数 | CPU 核数 |
| `-dry-run` | 只预览不执行 | 关闭 |
| `-overwrite` | 允许覆盖已存在的输出文件 | 关闭 |
| `-min <时长>` | 输出时长下限,低于此值则跳过 | `10s` |
| `-ffmpeg <路径>` | 指定 ffmpeg 可执行文件 | 自动查找 |
| `-no-download` | 禁止自动下载 ffmpeg-trim | 关闭 |
| `-v` | 输出详细信息 | 关闭 |

### 时长格式

`-head` / `-tail` / `-keep` / `-min` 都支持多种写法:

| 写法 | 含义 |
|---|---|
| `90` | 90 秒 |
| `90s` / `30秒` | 90 秒 / 30 秒 |
| `1m30s` / `1分30秒` | 1 分 30 秒 |
| `1:30` | 1 分 30 秒 |
| `0:01:30` | 1 分 30 秒(时:分:秒) |
| `1h2m3s` / `1小时` | 1 小时 2 分 3 秒 / 1 小时 |
| `1.5s` / `0.5m` | 1.5 秒 / 30 秒 |

### 支持的容器

`mp4` · `mkv` · `ts` · `avi` · `mp3`

这也正是 ffmpeg-trim 内置的全部容器。其他格式(webm、flv 等)会被拒绝。

## 工作原理

去头去尾本质是把 `[head, 总时长 - tail]` 这个时间窗口的数据搬运到新文件:

```
原文件:  |==== 片头 ====|======= 正片 =======|==== 片尾 ====|
              head                               tail
裁剪后:                |======= 正片 =======|
```

程序分三步:

1. **探测** —— 用 `ffmpeg -i <file>` 的 stderr 输出解析时长与流信息
   (ffmpeg-trim 裁掉了 ffprobe,只能这样读元数据)
2. **规划** —— 计算窗口 `start=head`、`end=总时长-tail`,并做合法性检查
3. **执行** —— 并发调用 `ffmpeg -i in -ss <start> -to <end> -map 0 -c copy -f <muxer> out`

### 关于切点精度

因为是 stream copy,切点会被对齐到**关键帧**:

- **音频(AAC/MP3)** 精确到音频帧(约 20ms)
- **视频** 只能从关键帧开始,实际切点可能比设定值早/晚一个 GOP(常见 1~5 秒)

对"砍掉固定时长片头曲"这类场景完全够用。如果你需要**帧精确**裁剪,那就必须重编码,
而 ffmpeg-trim 刻意删掉了所有编码器 —— 那属于另一个工具。

> **实测数据**:30 秒测试片砍 5 秒头 + 5 秒尾,输出 20.01 秒;
> 45 秒片砍 10 秒头 + 8 秒尾,输出 27.01 秒。误差在几十毫秒级。

### TS 格式特别注意

TS 没有关键帧索引,若起点落在非关键帧且窗口内没有关键帧,**视频轨会被丢弃**(音频不受影响)。
广播流通常 1~5 秒一个 GOP,一般无碍;拿不准就先输出 TS。

## ffmpeg-trim 的获取

程序按以下优先级查找 ffmpeg:

1. `-ffmpeg` 参数指定的路径
2. `TVTRIM_FFMPEG` 环境变量
3. 缓存目录中已下载的 ffmpeg-trim
4. 自动从 `totootao/ffmpeg-trim` 下载并校验
5. `PATH` 中的 `ffmpeg`(仅当禁用下载或下载失败时兜底)

PATH 排在自动下载之后是有意的:项目指定的后端是 ffmpeg-trim,PATH 里可能是
参数语义不一致的完整版 ffmpeg。自动下载会校验文件字节数,校验失败会报错而不会
留下半截文件。缓存位置在各平台的标准用户缓存目录下,例如 Linux 是 `~/.cache/tvtrim/`。

### 环境变量

| 变量 | 说明 |
|---|---|
| `TVTRIM_FFMPEG` | 指定 ffmpeg 路径 |
| `TVTRIM_PROXY` | GitHub 代理前缀,默认 `https://ghproxy.totootao.top`,设为 `none` 直连 |

## 开发

```sh
go test ./...        # 运行测试
go vet ./...         # 静态检查
gofmt -l .           # 格式检查(应无输出)
go build -o tvtrim ./cmd/tvtrim
```

### 项目结构

```
cmd/tvtrim/          CLI 入口:参数解析、时长格式解析、输出渲染
internal/ffmpeg/     二进制定位/缓存/自动下载;媒体探测(替代 ffprobe)
internal/scan/       目录扫描、扩展名过滤、集数识别与排序
internal/trim/       裁剪窗口计算、ffmpeg 参数组装、批量并发调度
```

### 一个值得记录的坑

ffmpeg-trim 是裁剪过的构建,它**会静默忽略 `-i` 之前的 `-ss`**。实测:

```sh
# 错误:片头根本没被砍掉,输出 25s
ffmpeg -ss 5 -i in.mp4 -to 25 -c copy out.mp4

# 正确:输出 20s
ffmpeg -i in.mp4 -ss 5 -to 25 -c copy out.mp4
```

完整版 ffmpeg 两种写法都正常(只是 seek 策略不同),所以这个问题只有在真机跑
ffmpeg-trim 时才会暴露。`TestBuildArgsPlacesSSAfterInput` 就是为此写的回归测试。

## 许可

MIT
