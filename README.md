<p align="center"><img src="favicon.svg" width="128" height="128" alt="Phicomm S7"></p>

<p align="center">简体中文 · <a href="README.en.md">English</a></p>

# `phicomm-s7d`: Phicomm S7 本地服务器

`phicomm-s7d` 是一个面向斐讯 S7 体脂秤的本地服务器。它在局域网内模拟原云端
TCP 服务、显示和保存体重、体脂测量结果，并提供一个不依赖外部资源的网页仪表盘。

最终程序是一个静态链接的 ARM64 可执行文件，HTML、CSS、JavaScript 和 favicon
都编译在二进制中，适合运行在 ARMv8 的 OpenWrt/ImmortalWrt 路由器上。

> 本项目来自对自有设备和抓包数据的互操作性分析，不是斐讯官方软件。协议字段和
> 回包行为只在现有样本及设备上验证过。

Phicomm（斐讯）是其相应权利人的商标。**_本项目为独立的非官方项目，与斐讯及相关商标权利人不存在隶属、授权或背书关系。_**

## 功能

- 体重、体脂结果查询
- 趋势绘图
- 历史存储、CSV导出
- 夜间模式
- 多语言（中、英以外为机器翻译仅供参考，欢迎纠错）

### 截图

<table>
  <tr>
    <td><img src="readme_assets/screenshot-desktop-light.png"></td>
    <td><img src="readme_assets/screenshot-desktop-dark.png"></td>
  </tr>
  <tr>
    <td><img src="readme_assets/screenshot-mobile-light.png"></td>
    <td><img src="readme_assets/screenshot-mobile-dark.png"></td>
  </tr>
  <!-- 再写两行 -->
</table>

## 部署到 OpenWrt / ImmortalWrt

开始前，S7 必须已经通过原有方式完成 Wi-Fi 配网，并且能够正常接入局域网。本项目
只接管配网完成后的云端通信，不实现体脂秤的首次配网流程。建议在 DHCP 中为 S7
设置静态租约，避免地址变化后防火墙的源 IP 匹配失效。

### 一键安装脚本

在路由器的 SSH 终端运行：

```sh
wget -O /tmp/install-phicomm-s7.sh "https://raw.githubusercontent.com/custard-pi/phicomm-s7d/main/install-openwrt.sh" && ash /tmp/install-phicomm-s7.sh
```

指定版本可运行 `RELEASE_TAG=v1.0 ash /tmp/install-phicomm-s7.sh`。

防火墙备份保存在脚本输出的 `/tmp/firewall-phicomm-s7-*.backup` 路径，重启前可复制留存。

### 手动安装

以下示例假定：

- 路由器或运行服务的 OpenWrt 主机：`192.168.1.1`；
- S7：`192.168.1.2`；
- S7 原服务器：`106.14.93.199:30101`；
- 设备架构：ARMv8/AArch64；
- 网页端口：`8088`。

请按实际网络修改地址。执行防火墙操作前，最好保留一个 SSH 会话并备份配置：

```sh
uci export firewall > /tmp/firewall.backup
uname -m
```

`uname -m` 应显示 `aarch64`。其他架构需要重新交叉编译。

#### 1. 安装程序

在电脑上复制二进制：

```sh
scp -O phicomm-s7d-linux-arm64 root@192.168.1.1:/tmp/phicomm-s7d
```

在 OpenWrt 上安装：

```sh
mkdir -p /etc/phicom-s7
cp /tmp/phicomm-s7d /usr/bin/phicomm-s7d
chmod 0755 /usr/bin/phicomm-s7d
/usr/bin/phicomm-s7d --self-test
```

预期输出：

```text
self-test OK
```

#### 2. 创建配置

首次可以在 SSH 终端直接运行并回答交互问题：

```sh
/usr/bin/phicomm-s7d --configure
```

`--configure` 保存配置后退出。也可以直接创建 `/etc/phicom-s7/config.json`。
用于 procd 时应提前创建有效配置。

```json
{
  "tcp_listen": ":30101",
  "http_listen": ":8088",
  "data_file": "/etc/phicom-s7/measurements.jsonl",
  "height_cm": 180,
  "coef_set": "male",
  "strict_crc": true
}
```

- tcp_listen：体脂秤 TCP 服务的监听地址，默认 `:30101`。
- http_listen：Web 仪表盘的 HTTP 监听地址，默认 `:8088`。
- data_file：测量记录的保存路径。
- height_cm：以 cm 为单位的身高，用于体脂估算。
- coef_set：选择体脂估计使用的回归系数组，可选值包括`male`和`female`，对应[后文](#体脂估算)的两组系数。
- strict_crc：是否严格校验体脂秤数据包的 CRC。设为 true 时，CRC 校验失败的数据包将被拒绝。

替换为合适的值后，限制配置权限：

```sh
chmod 0600 /etc/phicom-s7/config.json
/usr/bin/phicomm-s7d --check-config
```

#### 3. 添加 procd 服务

启动脚本已放在仓库的 [init.d/phicomm-s7d](init.d/phicomm-s7d)。复制：

```sh
scp -O init.d/phicomm-s7d root@192.168.1.1:/tmp/phicomm-s7d.init
```

在 OpenWrt 上安装、启用并启动：

```sh
cp /tmp/phicomm-s7d.init /etc/init.d/phicomm-s7d
chmod 0755 /etc/init.d/phicomm-s7d
/etc/init.d/phicomm-s7d enable
/etc/init.d/phicomm-s7d start
```

修改 JSON 配置后执行 `/etc/init.d/phicomm-s7d reload`，procd 会检测配置变化并重启服务。
参见 [OpenWrt procd 文档](https://openwrt.org/docs/guide-developer/procd-init-scripts)。

#### 4. 劫持 S7 的云端连接

一键安装已自动完成此步骤。手动部署或单独调整规则时使用下面的脚本。

仓库中的 `openwrt-firewall-setup.sh` 兼容 OpenWrt 的 BusyBox `ash`。复制并运行：

```sh
scp -O openwrt-firewall-setup.sh root@192.168.1.1:/tmp/
ssh root@192.168.1.1
ash /tmp/openwrt-firewall-setup.sh
```

#### 5. sysupgrade 时保留程序、配置和数据

一键安装脚本会自动登记以下路径；手动安装时，将这些行加入 `/etc/sysupgrade.conf`
（保留文件原有内容）：

```text
/usr/bin/phicomm-s7d
/etc/init.d/phicomm-s7d
/etc/phicom-s7/
/etc/rc.d/S95phicomm-s7d
/etc/rc.d/K10phicomm-s7d
```

这些路径覆盖二进制、启动脚本、默认配置与测量数据，以及开机启动链接。升级固件时
必须选择保留配置；`sysupgrade -n` 不会保存这些文件。升级前检查实际备份列表：

```sh
sysupgrade -l | grep -E 'phicomm-s7d|phicom-s7/'
```

如果 `data_file` 指向其他内置存储路径，需要单独加入其绝对路径；USB 等外部存储上的
数据应另行备份，并确保升级后挂载正常。仅在目标固件仍兼容 AArch64 Linux 时沿用这个
二进制。参见 [OpenWrt sysupgrade 文档](https://openwrt.org/docs/techref/sysupgrade)。

## 使用

浏览器打开`http://192.168.1.1:8088/`即可访问面板。

检查日志：

```sh
logread -f -e phicomm-s7d
```

## 更新与卸载

可重新运行一键安装脚本更新到最新正式 Release，或用 `RELEASE_TAG` 指定版本。
下载、校验或配置检查失败时不会替换原程序；替换后若服务启动或防火墙配置失败，
会尝试恢复原程序、服务状态、本次修改的程序配置和防火墙配置。已有未提交的 UCI
防火墙修改需要先提交或撤销，再运行安装脚本。

更新二进制：

```sh
/etc/init.d/phicomm-s7d stop
cp /tmp/phicomm-s7d /usr/bin/phicomm-s7d
chmod 0755 /usr/bin/phicomm-s7d
/etc/init.d/phicomm-s7d start
```

移除劫持规则而保留数据：

```sh
uci -q delete firewall.hijack_phicomm_s7_dnat
uci -q delete firewall.hijack_phicomm_s7_snat
uci commit firewall
/etc/init.d/firewall restart
/etc/init.d/phicomm-s7d disable
/etc/init.d/phicomm-s7d stop
```

## 本地构建与测试

使用仓库中的 [Makefile](Makefile)：

```sh
make build    # 构建开发机原生二进制 phicomm-s7d
make test     # 构建原生二进制并运行内置自检
make arm64    # 构建静态 ARM64 二进制 phicomm-s7d-linux-arm64
make release  # 构建 ARM64、复制并命名发布附件、生成各自的校验文件
```

## 工作原理

S7 会主动通过 TCP 连接 `106.14.93.199:30101`。路由器利用 DNAT 把这条连接转发
到本地运行的 `phicomm-s7d`，程序再完成三件事：

1. 接收并解析体重、六路阻抗和状态字段；
2. 返回秤继续工作所需的确认包和当前时间；
3. 把原始测量写入 JSONL，并通过 HTTP API 和网页展示。

在当前抓包样本中，服务器回包没有体脂数据，主要是固定结构的确认、请求中的设备
标识字段和时间。因此体脂不是从回包中取得，而是在本地根据阻抗估算。

### 已实现的协议

线路上的帧经过逐字节 XOR `0x55`；解码后以 `55 aa` 开头，长度字段为大端，尾部
使用 Modbus CRC-16。当前程序认识并回复以下命令：

| 请求     | 回复     | 用途                  |
| -------- | -------- | --------------------- |
| `0x8000` | `0xc000` | 建立会话、同步时间    |
| `0x8062` | `0xc062` | 测量数据              |
| `0x8052` | `0xc052` | 已观察到的控制/确认帧 |
| `0x8090` | `0xc090` | 离线或历史流程中的帧  |

对 64 字节的 `0x8062` 测量帧：

- 字节 `43..44` 按大端整数读取并除以 100，得到 kg；
- 从字节 `45` 开始读取 6 个小端 16 位整数并除以 10，得到 Ω；
- 当前只用第六路阻抗估算体脂；
- 严格 CRC 模式下，校验失败的帧会被丢弃。

### 体脂估算

第六路阻抗在 `250–800 Ω` 范围内时，程序使用 Sun 等人发表的 BIA 无脂体重
回归方程。代码中使用的通用形式为：

```math
\text{去脂体重} = a + b \frac{\text{身高}^2}{\text{阻抗}} + c \cdot \text{体重} + d \cdot \text{阻抗}
```

```math
\text{体脂}_{\%} = \left(1 - \frac{\text{去脂体重}}{\text{体重}}\right) \times 100
```

其中身高单位为 cm，体重为 kg，阻抗为 $\Omega$。使用的系数如下：

| `coef_set` |    `a` |  `b` |  `c` |  `d` |
| ---------- | -----: | ---: | ---: | ---: |
| `male`     | -10.68 | 0.65 | 0.26 | 0.02 |
| `female`   |  -9.53 | 0.69 | 0.17 | 0.02 |

这里的阻抗是 S7 第六路数据，不是论文实验设备直接测得的 50 kHz
全身阻抗，所以**结果应视为趋势估算，不能替代医疗级身体成分检测**。公式和系数取自：Sun SS et al., _Development of bioelectrical impedance analysis
prediction equations for body composition with the use of a multicomponent model for use
in epidemiologic surveys_, Am J Clin Nutr. 2003;77(2):331–340,
[PMID 12540391](https://pubmed.ncbi.nlm.nih.gov/12540391/)，
[DOI 10.1093/ajcn/77.2.331](https://doi.org/10.1093/ajcn/77.2.331)。
