# aliyun-cdt-keepalive

阿里云 CDT 流量监控 + 抢占式实例保活工具（Go 单二进制实现）。

> 适用场景：阿里云（国内/国际站）抢占式实例 + CDT 免费流量玩法。监控每月 CDT 出向流量，超限自动关机止损，月度重置/抢占回收后自动开机保活，全程飞书通知。

## 功能

- **流量熔断**：定时查询 CDT 出向流量，超过阈值自动 `StopInstance` 止损
- **自动恢复**：月度流量重置后自动 `StartInstance` 恢复业务
- **抢占保活**：检测到实例被非预期停止（抢占式回收）时自动开机
- **飞书通知**：关机/开机/保活事件实时推送，每日报表（含流量百分比、实例状态、账户余额）
- **多实例**：单配置文件支持多台实例（可跨账号/跨地域）
- **零依赖**：单静态二进制，阿里云签名自行实现（RPC V1 / HMAC-SHA1），无第三方库

## 构建与交叉编译

```bash
# 本机平台
go build -o aliyun-cdt-keepalive .

# 交叉编译 Linux amd64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o aliyun-cdt-keepalive .
```

## 部署

### 1. RAM 权限（强烈建议最小授权，勿用主账号 AK）

```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ecs:DescribeInstances",
        "ecs:DescribeInstanceStatus",
        "ecs:StartInstance",
        "ecs:StopInstance"
      ],
      "Resource": "acs:ecs:*:*:instance/<你的实例ID>"
    },
    {
      "Effect": "Allow",
      "Action": "bssopenapi:QueryInstanceBill",
      "Resource": "*"
    }
  ]
}
```

### 2. 配置

参考 `config.example.json` 写 `config.json`：

```json
{
  "feishu_webhook": "https://open.feishu.cn/open-apis/bot/v2/hook/xxxx",
  "instances": [
    {
      "name": "HK-01",
      "region": "cn-hongkong",
      "instance_id": "i-xxxx",
      "ak": "LTAI5t...",
      "sk": "...",
      "traffic_limit_gb": 180
    }
  ]
}
```

> `feishu_webhook`：飞书群 → 设置 → 群机器人 → 添加「自定义机器人」获得。
> `traffic_limit_gb` 建议低于免费额度（如额度 200G 就设 180），留出余量。

### 3. systemd 定时（每 5 分钟监控 + 每日 9 点报表）

```bash
sudo mkdir -p /opt/aliyun-keepalive
sudo cp aliyun-cdt-keepalive /opt/aliyun-keepalive/
sudo cp config.json /opt/aliyun-keepalive/ && sudo chmod 600 /opt/aliyun-keepalive/config.json
sudo cp deploy/*.service deploy/*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now aliyun-keepalive.timer aliyun-report.timer
```

### 4. 手动验证

```bash
# 只检查并打印将执行的动作，不实际开关机
/opt/aliyun-keepalive/aliyun-cdt-keepalive -dry-run

# 立即发一次日报（验证飞书）
/opt/aliyun-keepalive/aliyun-cdt-keepalive -report
```

## 参数

| 参数 | 说明 |
|---|---|
| `-config` | 配置文件路径 |
| `-state` | 状态文件路径（记录止损标记，防止重复动作） |
| `-dry-run` | 只打印动作不执行 |
| `-report` | 发送每日报表 |
| `-probe <实例ID>` | 探测实例 ID 所在地域（不记得实例建在哪个 region 时用） |

## 通知策略

- 流量超限 → 关机 + 推送 🚨 告警
- 月度重置 → 开机 + 推送 ✅ 恢复通知
- 实例被外部停止 → 开机 + 推送 🔄 保活通知
- 每日 09:00 → 推送 📊 流量/状态/余额日报

## 常见错误排查

| 错误 | 原因 | 解决 |
|---|---|---|
| `API错误 NoPermission` | RAM 策略缺少 CDT 权限 | 添加 `cdt:ListCdtInternetTraffic`（Resource 只能 `*`） |
| `Forbidden.RAM`（调 DescribeInstances） | 列表型 API 不支持按单个实例 ID 限权，或策略未授权给 AK 所属用户 | Describe 类动作 Resource 用 `acs:ecs:*:*:instance/*`；并确认策略已授权到 AK 所属 RAM 用户 |
| `InvalidInstanceId.NotFound` | 实例 ID 或地域不对。注意 `d-` 开头是**云盘** ID，实例 ID 是 **`i-` 开头** | 用 `-probe <实例ID>` 全地域扫描定位 |
| 飞书收不到消息 | webhook 失效或被群机器人安全设置拦截 | 重新生成 webhook；检查群里机器人是否被移除 |

> ⚠️ 提醒：ECS 控制台里 **`d-` 开头是云盘、`i-` 开头才是实例**，别复制错。

## 说明

- `config.json` 权限 600，不入 git（`.gitignore` 已覆盖）
- 同时在阿里云费用中心设置「预算告警」作为最后防线

## License

MIT
