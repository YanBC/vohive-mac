# vohive-mac

[English](README.md) | **简体中文**

一个面向 **macOS** 的极简 SIM 卡管理平台，灵感来自
[VoHive](https://github.com/iniwex5/vohive)（后者面向 Linux）。它**完全在用户态
通过原始 USB**（libusb）驱动百旺 / 移远（Quectel）系列的 4G USB 上网卡——不需要内核
扩展，也不需要串口驱动——并提供一个 Web 控制台：

1. **实时模块状态** —— 运营商、接入制式（RAT）、信号、SIM 状态、WAN IP
2. **短信** —— 发送短信（完整 Unicode/UCS2，超长自动分条）并浏览短信归档（PDU
   解码、长短信分片重组）。短信会持续从模块 / SIM 存储归档进 SQLite，并且默认在
   归档后从硬件中删除——硬件的存储槽位很少（SIM 上约 50 条，模块 Flash 中约 23
   条），这样才不会存满并把新到的短信退回。用 `-archive-delete=false` 可以关闭删除。
3. **蜂窝流量统计** —— 实时速率图、本次会话 / 今日用量、按天保存且重启不丢的历史
4. **SIM PIN** —— 解锁被 PIN 保护的卡（或用 PUK 解除锁死），开关开机 PIN 校验、
   修改 PIN。在你消耗一次尝试之前就会显示剩余次数；任何密码都不会被写进日志
5. **低数据模式** —— 把上网卡的链路标记为计费链路，让 macOS 不再把有流量上限的
   LTE 套餐当成免费以太网（见下文）

单个 Go 二进制文件，UI 已内嵌。短信通过 USB 批量传输走上网卡的 AT 端口；流量则是
从 macOS 上该设备 ECM 网络接口的字节计数器采样得到。所有持久化状态都存放在一个
SQLite 数据库 `data/vohive.db` 中（旧版的 `data/usage.json` 会被导入一次并改名）。

## 前置条件

- macOS 11 及以上（Apple Silicon 或 Intel）
- `brew install libusb go`
- 一个大疆4g一代模块（百旺 QDC507 上网卡，USB ID `2ca3:4006`），并且**已切换到 ECM USB 模式**——
  这是一次性操作，见 **[docs/dongle-setup.md](docs/dongle-setup.md)**。
  其他兼容 Quectel 的设备可以通过 `BAIWANG_VID`/`BAIWANG_PID`/`BAIWANG_IFACE`
  这几个环境变量覆盖默认值来尝试。

## 编译与运行

```sh
go build -o vohive-mac ./cmd/vohive-mac
./vohive-mac                # Web 控制台： http://127.0.0.1:7676
./vohive-mac -addr :7676    # 暴露到局域网（没有任何鉴权——请谨慎）
```

## AT 命令控制台

这个二进制同时是一个临时 AT 终端，走的是同一条原始 USB 通道（请先停掉服务——
AT 接口同一时间只能被一个进程占用）：

```sh
./vohive-mac at 'AT+CSQ' 'AT+COPS?'
```

安装文档里那次一次性的 ECM 模式切换，用的也是这个工具。

## 低数据模式

对 iPhone 的个人热点，macOS 会自动打开「低数据模式」；但处于 ECM 模式的上网卡在
系统看来就是一条普通的有线以太网：系统认为这是免费链路，于是 iCloud 同步、照片
上传、App Store 和系统更新下载统统走了这张实际上按量计费的 SIM 卡。

流量面板里的 **low data** 开关就是用来解决这件事的。它会设置「低数据模式」背后的
两个接口标志位——`IFEF_EXPENSIVE` 和 `IFXF_CONSTRAINED`，等价于执行
`ifconfig en6 expensive constrained`——此后 macOS 会把这条链路以「昂贵、受限」的
身份报告给每一个 App。系统服务和行为良好的 App 会自觉降低用量。

**该开关默认是开启的。** 在你明确说明之前，程序一律假定这张卡是付费套餐：这个方向
猜错了，代价只是同步慢一点；猜错另一个方向，花掉的是你的流量。如果是不限量套餐，
可以针对该卡单独关掉——这个选择会在重启后保留，并且不会被重新改回默认值。

有三点需要知道：

- **它需要 root 权限。** 这两个标志位既没有对应的 `networksetup` 命令，也没有可写的
  偏好设置文件，所以服务必须以 root 身份运行才能设置：

  ```sh
  sudo ./vohive-mac
  ```

  如果没有 root，设置本身仍然会按卡保存，开关会显示 *pending*（待生效），日志里会
  写明缺了什么。数据库文件会被交还给执行 `sudo` 的那个用户，所以之后再以普通权限
  运行也不会出问题。

- **这个设置属于 SIM 卡，而不属于上网卡设备。** 同一个接口，插入有流量上限的旅行卡
  时是计费链路，插入不限量卡时就是免费链路；所以每张 SIM 各自带着自己的标志位，
  换卡时会重新应用。新卡默认按计费处理，那个保留的「未知 SIM」（读不出卡时的兜底
  归属）同样如此。

- **这是一个提示，而不是硬性限制。** 系统不会强制执行任何东西：无视这两个标志位的
  App 照样会全速跑。要真正切断，请用旁边的 **data** 开关，它会直接关掉模块的蜂窝
  数据。

这两个标志位存在于接口实例上，所以每次重新插拔、以及守护逻辑在休眠 / 唤醒后执行的
每一次 USB 复位，都会让它们消失。服务会在几秒内重新设置回去；而在退出时会刻意保留
它们不清除——因为服务停止之后，上网卡还在继续承载流量。

## HTTP API

| 路由 | 方法 | 说明 |
|---|---|---|
| `/api/status` | GET | 模块 / SIM / 网络状态（缓存 5 秒） |
| `/api/traffic` | GET | 计数器、速率、历史、每日用量 |
| `/api/metered` | GET / POST | 某张 SIM 的低数据模式 / 用 `{"enabled": true}` 把链路标记为 expensive + constrained |
| `/api/sim/lock` | GET / POST | PIN 状态与剩余尝试次数 / 用 `{"enabled": true, "pin": "1234"}` 开关开机 PIN 校验 |
| `/api/sim/unlock` | POST | `{"code": "1234"}`——卡被锁死时则用 `{"code": "<8 位 puk>", "new_pin": "1234"}` |
| `/api/sim/pin` | POST | `{"pin": "1234", "new_pin": "5678"}`——修改该卡的 PIN |
| `/api/sms/inbox` | GET | 归档的短信（收 + 发），最新在前；数据过期时会触发一次模块同步 |
| `/api/sms/send` | POST | `{"to": "+86138...", "text": "..."}`——同时记入归档 |

## 代码结构

```
cmd/vohive-mac/    程序入口：组装各组件 + `at` 子命令
internal/
  pdu/             SMS-DELIVER PDU 解码器（纯函数，不做任何 I/O）
  sim/             SIM 身份（ICCID/IMSI/号码/运营商）及其标签规则
  modem/           原始 USB AT 通道：发短信（UCS2）、读写存储、状态查询，
                   以及 SIM PIN（解锁 / PUK 解锁、开关锁、改 PIN）。
                   唯一直接操作 USB 的包。
  store/           SQLite：sims、messages、usage 三张表以及数据库迁移
  sims/            SIM 注册表：当前插在上网卡里的是哪张卡
  netif/           ECM 网络接口：链路状态、低数据标志位，以及流量统计
                   背后的字节计数器采样器
  metered/         低数据模式：让链路的 expensive/constrained 标志位
                   与当前 SIM 的设置保持一致（需要 root）
  archive/         短信归档器：模块 / SIM 存储 → SQLite（归档后删除）
  recovery/        ECM 链路守护：链路在休眠 / 唤醒后一直不起来时复位 USB，
                   DHCP 不再响应时重启模块
  server/          HTTP API + 内嵌的 Web 控制台（static/）
docs/              上网卡安装手册（USB 模式切换、故障排查）
```

依赖关系是单向的：`pdu` 和 `sim` 不依赖任何内部包，`store` 只依赖 `sim`（因此它的
测试既不需要上网卡也不需要 libusb），而它们之上的所有组件只在 `cmd/vohive-mac`
里被组装到一起。

## 限制与注意事项

- 没有任何鉴权——它默认只监听 `127.0.0.1`，这是有意为之。
- 流量是在 Mac 的网络接口上统计的，所以只统计这台 Mac 经由上网卡产生的流量
  （不包括其他设备通过该上网卡热点产生的流量，如果有的话）。
- AT 接口是独占的：Web 服务和 `vohive-mac at` 不能同时运行。
- 低数据模式需要服务以 root 运行，而且只是建议性的：macOS 和行为良好的 App 会
  遵守这两个标志位，但没有任何东西会强制执行。

## 许可证

[MIT](LICENSE)。

本项目通过 [gousb](https://github.com/google/gousb)（Apache-2.0）**动态链接**
**libusb**（LGPL-2.1）；SQLite 访问使用
[go-sqlite3](https://github.com/mattn/go-sqlite3)（MIT）。正是动态链接让 libusb
的许可条款不会波及你自己的代码——静态链接 libusb 的构建必须提供重新链接的能力，
所以请保持用 `brew install libusb` 的方式来提供它。

vohive-mac 是独立实现，而不是移植：[VoHive](https://github.com/iniwex5/vohive)
没有发布任何许可证，因此本项目没有取用它的任何代码。
