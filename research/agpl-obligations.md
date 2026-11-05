# AGPLv3 SaaS 义务与商业授权调研

**问题**：以对外 SaaS 形式运营 `OS-Lihua/2api`（New API fork）时，AGPLv3 第 13 条等要求履行哪些义务（源码提供方式、范围、受保护品牌信息）？上游 New API 是否提供商业授权或其他许可选项？

**结论**：可以作为 SaaS 运营，但必须向通过网络访问该服务的用户提供运行版本的完整对应源码；同时保留许可证、版权与项目署名/上游链接，并保留前端设计署名及第三方许可声明。上游 New API 公开说明可联系 `support@quantumnous.com` 洽谈商业授权或许可安排，但具体条款未在仓库中公开。

## 结论

1. **SaaS 使用本身不被 AGPLv3 禁止，但触发网络交互源码义务。**  
   本仓库采用 AGPLv3，并特别说明其目标是让网络服务器上运行的修改版本源码可用。AGPLv3 第 13 条要求：如果修改后的程序支持通过计算机网络远程交互，必须以免费、标准或惯用的复制方式，向所有远程用户提供该修改版本的对应源码。  
   来源：`LICENSE:540` 至 `LICENSE:547`；`README.md:321-323`；`NOTICE:6`。

2. **源码提供方式：可直接从网络服务器提供访问。**  
   第 13 条允许通过“网络服务器”提供对应源码访问，不要求必须邮寄介质；对 SaaS 来说，常见做法是在公开源码仓库、下载页或服务内显著位置提供与线上版本一致的源码。  
   来源：`LICENSE:540-546`。

3. **源码范围是“完整对应源码”，不只是补丁。**  
   对象代码形式的对应源码包括生成、安装、运行和修改该软件所需的全部源码及控制脚本，但不包括系统库、通用工具、未经修改且可独立获得的自由软件，也不包括可由其他对应源码自动再生的内容。对本仓库而言，至少应覆盖 Go 后端与前端构建源码、构建脚本、依赖声明与第三方许可信息。  
   来源：`LICENSE:122-140`；`go.mod`、`go.sum`、`web/package.json`、`web/bun.lock`；`THIRD-PARTY-LICENSES.md:1-11`。

4. **如果以二进制、Docker 镜像或桌面安装包形式分发，还需按第 6 条提供源码。**  
   第 6 条要求对象代码交付时同时提供机器可读的对应源码；通过网络服务器提供时，源码可放在另一服务器，但必须保持清晰指引，并在需要满足义务的期间内持续可用。  
   来源：`LICENSE:237-267`。

5. **必须保留版权、许可证与“适当法律声明”。**  
   AGPLv3 要求交互式用户界面在适当法律声明位置展示版权和许可声明；本仓库的许可证与 NOTICE 也明确要求保留这些信息。  
   来源：`LICENSE:85-93`、`LICENSE:219-220`；`NOTICE:1-7`。

6. **上游品牌、署名与原项目链接受项目附加条款保护。**  
   NOTICE 规定修改版本必须在适当的法律声明和用户界面显著的关于、法律、页脚或署名位置保留：  
   - `Frontend design and development by New API contributors.`  
   - 指向原项目 `https://github.com/QuantumNous/new-api` 的可见链接；  
   - 不得歪曲软件来源，并需按 AGPLv3 第 7(c) 条标注自己的修改。  
   当前前端关于页已经展示上游仓库、QuantumNous、One API、JustSong、AGPLv3 许可证等信息；修改版不得删除这些显著署名和来源信息。  
   来源：`NOTICE:9-25`；`README.md:325-337`；`web/src/features/about/index.tsx:50-108`。

7. **公司名称、Logo 等自身品牌可以替换，但不能抹去上游版权和项目来源标识。**  
   系统名称和页脚文案存在运行时可配置能力，例如后端 `SystemName` 默认值为 `New API`，前端也支持读取系统配置中的 `systemName`、Logo 和自定义页脚 HTML。因此“自己的品牌”通常可以体现在产品名称、Logo、页脚内容或独立商业外观上；但这不等同于删除 NOTICE 中要求保留的上游版权、设计署名、原项目链接或法律声明。具体边界应以最终商业授权协议和当地法律判断为准。  
   来源：`common/constants.go:15-16`；`web/src/components/layout/components/footer.tsx:150-167`；`NOTICE:9-25`；`README.md:325-337`。

8. **上游 New API 提供商业授权联系方式。**  
   README 明确说明：如果组织政策不允许使用 AGPLv3 软件，或希望规避 AGPLv3 开源义务，可联系 `support@quantumnous.com`。仓库内多个前端文件头部也写有 “For commercial licensing, please contact support@quantumnous.com”。  
   这说明上游存在商业授权/许可安排的渠道，但仓库没有公开商业授权价格、范围或替代许可证条款；若需要闭源 SaaS、免除网络源码公开义务或获得商标/品牌安排，应直接与上游确认书面协议。  
   来源：`README.md:333-335`；`README.zh_CN.md:327-329`；`web/src/main.tsx:17`。

9. **fork 自身可以自由对外提供服务，但商业化方案需要区分两层。**  
   - 对使用 AGPLv3 软件的 SaaS：可以运营，但必须履行源码提供和署名义务。  
   - 对希望免除公开源码义务、获得商业许可或使用上游品牌/商业渠道：需要取得上游商业授权，目前只能通过 README 提供的联系邮箱洽谈。  
   来源：`LICENSE:540-547`；`README.md:333-335`。

## 对本 fork 的落地清单

- 公开发布或 SaaS 运营时，保留 `LICENSE`、`NOTICE`、版权声明和第三方许可文件。  
- 向用户显著提供与线上版本一致的完整源码，建议使用公开 Git 仓库或源码包，并明确构建方式。  
- 在管理界面或站点显著位置保留上游项目链接和前端设计署名。  
- 不要声称 fork 是原版 New API；应说明基于 New API/AGPLv3，并标注自己的修改。  
- 如计划更换品牌、闭源商业分发或规避 AGPL 源码义务，联系 `support@quantumnous.com` 获取商业授权。  
- 最终商业合作、商标使用和责任划分需以书面授权协议为准。

## 参考来源

- `LICENSE`：AGPLv3 原文，第 0、4-6、13 条。  
- `NOTICE`：New API 附加条款、署名要求、第三方与 Electron 通知。  
- `README.md` / `README.zh_CN.md`：项目许可证与商业联系说明。  
- `web/src/features/about/index.tsx`：前端关于页的上游链接与 AGPL 声明。  
- `web/src/components/layout/components/footer.tsx`：系统名称、Logo 与页脚配置能力。  
- `THIRD-PARTY-LICENSES.md`：第三方依赖许可声明。
