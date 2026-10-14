# 本机完整后台演示

独立的 `cmd/localdemo` 复用 Go + PostgreSQL 的完整业务接口，解决 Worker
后台缺少收款设置、安全审计、商店、代理层级和高级售后接口的问题。
生产命令不导入 `internal/localdemo`，演示适配器不能由生产配置开关启用。

在仓库根目录执行（Docker 必须已启动）：

```sh
npm ci
npm run build
python3 scripts/local-demo.py setup
python3 scripts/local-demo.py start
# 在另一个终端打开本机专属入口，自动完成演示管理员登录：
python3 scripts/local-demo.py open
```

macOS 常驻安装：`python3 scripts/local-demo.py install`。运行程序、前端和私有配置会复制到
`~/Library/Application Support/GPTibo`，避免后台程序读取外置盘时等待系统访问授权。
模拟渠道及收件箱状态也保存在该目录，后续重新安装不会覆盖已有运行数据。
登录后自动启动，由 `~/Library/LaunchAgents/local.gptibo.plist` 管理；Docker 仍需运行。
旧 Worker 启动配置保留为 `local.gptibo.worker.plist.backup`，D1 数据保留在原仓库。

默认网页端口 8788、PostgreSQL 端口 5446，均只监听 127.0.0.1。
首次安装检查端口占用。已有其他服务时先选择空闲端口，修改私有演示配置
中的 `listen`、`origin`、`dbPort`，再启动；已有数据库容器不能直接改映射端口。
`setup` 编译 Go 原生程序，随后运行不依赖宿主机安装 Go。

## 使用方式

- 管理后台：`http://localhost:8788/admin`。
- 演示控制台和本地收件箱：`http://localhost:8788/local-demo`。
- 控制台可进入演示代理和下级代理；它们初始是两个独立账号，可在后台体验层级设置。
- 可创建 Plus 订单，模拟支付、提交假 Session、模拟成功或失败。失败后在管理后台
  查看演示凭据、执行原订单重试；通过模拟渠道执行售后退款。
- Gmail、Gemini、多邻国使用本地采购和邮件交付。
- 短信使用模拟支付和接码服务：取号 `+1 202-555-0147`，选择收到验证码或超时，
  约 5 秒返回 `123456` 或失败；超时可在原单重试。号码、验证码都不能用于真实验证。
- X Premium 3 / 6 个月使用进程内模拟供货，正常填写用户名后走资格检测、付款和自动交付。
  `demo_ok` 成功，`demo_fail` 失败可售后退款，`demo_retry` 首次失败后可从后台补发成功，
  `demo_missing` 资格检测不通过。结果约 5–15 秒更新，不会改变真实 X 账号。
- 控制台可直接选择上述商品和 X 场景。普通商城的付款二维码也指向本地控制台；
  模拟到账后回原订单刷新即可继续。所有订单均可通过原订单售后入口和后台审批体验退款。
- 域名绑定、DNS 和证书状态都由本机模拟；不修改公网解析、不申请证书。
- 收款设置和结算使用原始事务、版本检查、权限和审计；其中的款项、交易号都是演示数据。

页面顶部持续显示演示标识。所有模拟支付和供货适配器都在进程内响应，未知请求拒绝，
没有网络回退。普通邮件和验证码只写入本地收件箱，只有管理员可查看；无需真实邮箱、
支付密钥、供货账号或真实用户 Session。

`.secrets/local-demo.json` 保存随机生成的演示凭据及本机启动入口密钥，权限必须为 600。
模拟渠道及收件箱状态保存在 `.secrets/local-demo-state.json`，原子写入并保留幂等记录。
数据库使用独立容器 `gptibo-local-demo-db` 和卷 `gptibo-local-demo-pg`；程序启动时迁移，
API 运行使用受限数据库角色。重启不会重置套餐、代理资料、订单或设置。
原来的 `.wrangler` / D1 数据不会被修改或自动导入；这是一个独立的演示数据集。

`python3 scripts/local-demo.py build` 重编译后重启服务以应用代码修改；
macOS 常驻安装请再执行 `python3 scripts/local-demo.py install`。
`python3 scripts/local-demo.py status` 检查数据库及后台任务健康。

该命令明确限定数据库名 `gptibo_demo`、loopback 监听、匹配的 localhost 来源、
同源写请求和专属启动密钥，不可用于公网或生产。
