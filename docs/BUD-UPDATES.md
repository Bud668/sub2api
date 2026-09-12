# Bud 网页更新

版本名为 `官方基线-Bud.序号`，例如 `0.2.4-Bud.14`；原 `cyberaudit` 标签不改写。相同基线按 Bud 数字序号比较，跨基线先比较官方版本。官方 v0.2.5 发布不会把本站直接更新成官方程序；审查合入后才能发布对应 Bud 版本。

## 管理员操作

点击左上版本号。Bud 区有新版时显示「更新 Bud 版」，依次显示下载、验签、备份与检查、短暂重启、验收。完成后点击刷新页面加载新界面。关闭浏览器、网关会话断线不会取消独立任务；再次打开可继续查状态。请求超时先看任务状态，切勿连续点更新。

官方区只包含版本和发布说明链接，没有安装按钮。管理员页面可见时约每 5 分钟检查两条来源，恢复页面可见性时检查缓存是否到期；手动刷新跳过缓存。两个来源独立，GitHub 不可用时保留最近结果并显示警告；缓存不是实时保证。后台页面关闭期间不额外推送通知。

失败前尚未切换的站点继续运行；只有新版尚未启动且旧进程干净退出、账务/schema/安全检查一致时，发布包内经过审查的恢复任务才可恢复原程序。新版开始启动后只向前验收，不盲目降级、恢复数据库或清用量。`blocked` 须由运营者核查主站和独立发布 journal，不能等同于「已恢复」。

## 一次性接入

目前仅支持本项目的受管 Linux amd64/systemd 安装。Docker、其它系统和没有受管备份/发布基线的安装不会尝试自更新。不要为了更新改成 root 运行应用、给予 sudo 权限或让应用可写安装目录。

运营者先审查并按自身路径/备份策略适配受控发布闸门，再在部署机 root 执行已核对的 `deploy/install-bud-updater.sh`。该脚本安装本机 Unix socket、独立安装器、公钥和 systemd 单元，不重启主应用。需要现有 `sub2api` 服务用户、受管清单、加密备份及已验收发布脚本；不是可随意套用到任意官方安装上的通用部署器。

- `/run/sub2api-update.sock`：root:sub2api，0660，无 TCP 端口；再次检查调用者 UID。只接受一个规范化 Bud 版本，拒绝命令、路径、仓库、URL及密钥参数。
- `/usr/local/libexec/sub2api-bud-update` 和 `/etc/sub2api/bud-release-public.pem`：root 所有。已有不同公钥时安装拒绝静默替换，轮换须另行审查。
- `/var/lib/sub2api-updater`：root:sub2api，0750；状态文件0640，其余下载包/详细日志 root 私有。应用只读取脱敏阶段、版本和时间。
- 安装器独立重新构造固定仓库的 HTTPS 地址，限制下载大小、重定向域和解压条目；Ed25519 验签覆盖整个发布包，验签成功前不解压或执行任何发布脚本。
- 应用无权写 `/opt/sub2api/sub2api` 或签名公钥；其 `ProtectSystem=strict`、`NoNewPrivileges=yes` 和既有写入目录保持不变。旧网页直接重启/降级接口明确拒绝，以免绕过账务收尾或清除进程内安全停转锁。

## 发布者流程

1. 官方更新受控合入修改主线，固定提交、测试，再用 `tools/build-managed-release.sh` 构建。数据库迁移与上一部署版本兼容范围必须人工审查；不因有更新按钮而省略。
2. 准备**新的**平铺受限 stage：二进制、accepted `managed-release.json`、`prepare-host.sh`、`deploy-reviewed-primary.sh`、迁移/配置/账务检查文件和 `ARTIFACT-SHA256SUMS`。准备脚本必须在线备份，部署脚本必须绑定已审查旧/新产物、检查安全停转锁、保留用量，并带独立 systemd 失败恢复。不得复用已完成归档或临时停用风控来通过检查。
3. 用 `tools/package-managed-update.sh <stage> <不存在的输出目录> <离线私钥> <已固定公钥>` 打包并签名。私钥不进 Git、不上传服务器应用目录、不进入发行附件；妥善离线备份，遗失后须另行轮换信任。
4. 发布到 `Bud668/sub2api` 的**非草稿、非预发布** Release `v官方版本-Bud.序号`。上传 `sub2api_版本_linux_amd64.update.tar.gz` 及对应 `.sig`，同时发布普通二进制包、源码包、发行清单、校验值。所有附件就绪后再标记最新，避免用户点击半成品。
5. 验签不替代代码和迁移审查；签名包里的 root 发布脚本是明确的信任边界。每版须重新声明并验证支持的前驱版本；未覆盖的跨版升级应在停止应用前拒绝，由运营者补充经过审查的升级路径。

依赖系统 OpenSSL、Python 标准库与 systemd，不新增应用依赖。为排障保留 root 私有包和加密备份，不自动删除历史档案；空间不足应在备份/切换前处理。备机不自动同步。

## 本地检查

```bash
python3 -m unittest deploy/test_bud_updater.py
# backend 目录
go test -race -tags=unit ./internal/service ./internal/handler/admin -run 'BudUpdate|SystemHandler'
# frontend 已构建，并指定现有 Playwright 模块时
node tests/bud-update.browser.mjs
```

浏览器检查使用合成 API，并拒绝非 loopback 请求；覆盖桌面/手机、明暗及原地主题切换、官方只读、更新过程/失败反馈和缓存定时过期。生产验收仍须额外核对真实静态资源、版本/hash、权限、安全状态、自然请求和账务。
