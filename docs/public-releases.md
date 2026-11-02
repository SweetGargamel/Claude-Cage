# 公开发行与主页部署

本页供维护者发布 Cage 安装包并维护项目主页。公开下载源为 [GitHub Releases](https://github.com/Lee-xeo/Claude-Cage/releases)，项目主页为 <https://lee-xeo.github.io/Claude-Cage/>。macOS 构建、签名与公证步骤见[发布 macOS 安装包](releasing.md)，Windows 安装器、签名及验收要求见 [Windows 指南](windows.md#维护者构建与验收)。

## 发布安装包

`VERSION` 表示正在构建的应用版本；主页下载入口只指向附件完整的公开 Release。发布新版本时，先完成相关测试、提交源码并推送对应的 `v<版本>` 标签，再按平台发行文档生成已签名且通过公证的 macOS DMG，以及完成签名和实机验收的 Windows 安装器。

以 macOS 为主的公开 Release 包含：

- `Cage-<版本>-macos-arm64.dmg`。
- 与安装包中代理核心版本一致的 `mihomo-v<版本>-source.tar.gz`。
- `SHA256SUMS`，摘要在最终签名和公证票据附加后生成。

同批发布 Windows 时，增加 `Cage-<版本>-windows-amd64-setup.exe`、该构建的 QEMU 主程序源码和包含随附库/固件的完整对应源码归档。两个平台共用一份 `SHA256SUMS`；不要用一个平台的清单覆盖另一个平台的清单。未签名开发包不得作为正式平台附件。

使用本地合并工具核对两个平台目录中的摘要并生成统一发布目录：

```sh
node scripts/prepare-release-assets.mjs \
  --output _output/publish-vVERSION \
  _output/releases/VERSION _output/releases/VERSION/windows
```

把 `VERSION` 替换为实际版本。输出目录必须不存在；工具拒绝摘要错误、不同内容的同名文件和开发安装器，相同源码附件只保留一份。该工具只准备文件，签名、公证与实机检查仍由对应平台流程完成。

先创建草稿并上传全部附件，再发布草稿。Alpha 版本标为预发行；所有 `gh` 命令显式指定 `--repo Lee-xeo/Claude-Cage`，避免 fork 的默认仓库指向上游。已发布的安装包不原地覆盖，修正二进制时使用新版本号。

发布或编辑 Release 会运行 `Website and public releases`：匿名下载附件，核对 SHA-256、应用版本、Developer ID 签名、公证票据与 Gatekeeper；通过后重建并部署主页。草稿不会进入公开下载列表。验收失败时，该轮主页部署停止，维护者应检查 Actions 日志并修正发行物。

版本标签对应的源码须包含发布工作流。需要复验时，使用下文带 `release_tag` 的手动命令。

GitHub Release 一旦公开，应用内更新即可读到它，早于这项事后检查完成。因此，本地打包验收必须在发布草稿前通过，不能依赖 Actions 作为安装包公开前的审批门槛。CI 中若用默认 `GITHUB_TOKEN` 发布 Release，不会触发后续工作流，须另外显式调用网站工作流；手工网页发布或 `gh` 用户凭据发布会正常触发。

## 应用内更新

### Windows 未签名预览版

未签名测试包可以作为独立预发行提供手动下载，标签使用 `windows-preview-YYYYMMDD`，标记为预发行且不设为 latest。附件保留 `-unsigned.exe` 后缀，Release 说明实际应用版本、系统验收范围、手动安装步骤及来源材料。它不进入正式安装器合并工具，也不进入应用自动更新或官网正式下载版本的选择逻辑。

此类预览版随附安装器摘要、已验证的 Cage 源码快照和第三方来源材料。来源核验中尚未完成的对应关系必须在材料清单中明确说明。发布前创建草稿并上传全部附件；公开后匿名核对下载内容。网站工作流跳过 `windows-preview-` 发布事件，继续从正式平台 Release 获取下载版本。

### 正式更新通道

支持应用内更新的客户端直接读取公开 Release，经所选受管代理下载，没有主机直连回退。正式客户端忽略预发行；预发行客户端可接收更新的预发行或正式版本。客户端仍独立核对摘要、应用身份、平台签名：macOS 检查同一 Developer ID Team 和 Apple 验证结果，Windows 检查 Authenticode 信任链及相同发布者公钥。

首个内测版本为 `0.1.0-alpha.1`，macOS 首次安装使用 DMG，Windows 使用对应 EXE；后续版本可通过应用内入口升级。公开源校验与单元测试不能证明真实用户环境下的替换、重启和回滚已经验收；每次发布均须在 Release 中说明实际验证范围。

可在不安装、不启动 App 的情况下验证公开附件：

```sh
node www/scripts/sync-release.mjs --tag v0.1.0-alpha.1 \
  --output /tmp/cage-release-check/release.json \
  --archive-directory /tmp/cage-release-check/assets
bash scripts/verify-macos-release.sh /tmp/cage-release-check/assets
```

## 主页与措辞同步

GitHub Pages 的发布源设为 GitHub Actions。`main` 的网站文件变更会自动部署；手动重新部署使用：

```sh
gh workflow run www.yml --repo Lee-xeo/Claude-Cage --ref main
```

需要同时重验某个公开安装包时，在命令后增加 `-f release_tag=v0.1.0-alpha.1`，替换为实际版本。

主页默认选择具有完整 macOS 资产的公开 Release。同一 Release 的 Windows 正式安装器及有效摘要存在时，下载区同时提供 Windows 10/11 x64 入口；未签名或缺失附件不显示。Windows-only Release 仍可被 Windows 应用更新器选择，但不会成为主页的默认发布版本。

主页构建从公开 Release 刷新下载链接，避免源码版本领先安装包时出现失效下载。图片、样式、404 返回链接与兼容重定向均使用 `/Claude-Cage/` 子路径。

产品正文由维护者确认的 README 驱动人工同步。README 修改完成后，再更新网页措辞及多语言版本，并分别核对功能范围、安装条件与验收限制。网站部署本身不会自动翻译 README。
