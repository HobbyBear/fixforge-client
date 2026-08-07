---
name: code-change-visualizer
description: 将本地未提交代码或两个 Git 分支之间的真实差异生成为离线交互式 HTML，按文件目录、方法/函数/逻辑区块解释修改，并展示数据库与配置、接口、关键日志影响以及可导出的区块批注。适用于理解 AI 本地改动、审核需求分支合并到主分支的变化、生成代码 walkthrough 或对变更区块添加修改意见。
---

# 代码变更图解

只解释 Git 能证明的代码差异，不修改代码，不直接提交批注。

## 选择比较模式

- `working_tree`：比较基线 commit（默认 `HEAD`）与当前已被 Git 跟踪的 staged、unstaged 内容；未进入 Git 跟踪的文件不属于分析范围。OpenSpec 实施时复用 `baseline.json`，原本 dirty 的文件只能标记为 `final_only`。
- `branch_compare`：比较 `base_ref` 与 `head_ref`。合并审核默认使用 `merge_base`，只展示 head 分支相对共同祖先引入的变化；只有明确需要分支快照差异时才使用 `direct`。

生成器会自动发现已跟踪文件的新增、修改、删除和重命名，并按真实 Git hunk 生成文件清单、语义区块 ID 和新旧行范围。模型声明只提供分析说明；遗漏、重复、虚构或范围不准的声明不会改变 Git 比较结构。

## 编写语义区块

读取 [input-format.md](references/input-format.md)，创建 v2 `walkthrough.json`。

1. 用 `changes` 提供理解有把握的文件级 `purpose` 和 `implementation`；文件可以遗漏，生成器会用 Git 事实补齐。
2. 可用 `changes[].units` 按 Git hunk 顺序补充 `title`、`meaning`、`reason` 和 `impact`，不要计算 `old_range` / `new_range`。条目数量不需要与 hunk 数量完全一致。
3. unit 的文件归属、ID、范围、完整覆盖和不重叠由生成器确定。模型给出的旧格式范围仅作为兼容提示，不参与验收。
4. 多文件或多区块变更优先产出 1-3 条 `flows`，每条包含 2-8 个按真实调用、数据流或状态变化顺序排列的步骤，用它把分散的 unit 串成可连续阅读的实现路径。只有证据不足或改动确实局部时才留空；不要按文件顺序硬凑链路。无法映射到真实 Git unit 的引用会被忽略。
5. `database_changes` 与 `log_points` 必须以真实 diff 为准：生成器扫描新增、删除和修改的 SQL、ORM/DAO/持久化模型代码及日志调用，再生成完整记录。人工条目只能补充已匹配 `code_targets` 的业务语义、发布影响和字段说明；没有代码证据的数据库或日志条目会被忽略，不会进入结果。
6. 日志只记录事件、级别、触发条件和非敏感字段，不嵌入凭据或真实业务数据。

## 校验并生成

```bash
python .codex/skills/code-change-visualizer/scripts/generate_visualization.py \
  --repo-root <repo> --change-dir <output-dir> --validate-only
```

校验通过后生成：

```bash
python .codex/skills/code-change-visualizer/scripts/generate_visualization.py \
  --repo-root <repo> --change-dir <output-dir>
```

默认读取 `<output-dir>/walkthrough.json`，可通过 `--input`、`--baseline`、`--output` 和 `--font` 覆盖。

## 检查页面

1. 有可信 `flows` 时页面默认进入变更导览，没有时直接进入代码差异。
2. 检查链路选择、步骤前后跳转、局部源码证据和“完整代码”跳转；代码视图继续支持目录树/文件列表、文件选择、unit 选择和 Diff 联动。
3. 检查右栏数据库、日志、接口变动三个标签，并确认条目能跳回对应代码区块。
4. 在 unit 中填写批注，确认“复制给 AI”和“导出批注”输出 `review-feedback.json`，目标包含比较指纹、文件、符号和新旧范围。
5. 检查桌面与移动布局、零网络请求、敏感信息过滤和路径边界。

## 边界

- 静态页面只保存和导出区块批注，不连接 AI、不修改仓库。
- 不用当前工作区源码冒充另一个分支的源码；分支模式必须从 commit 读取两侧内容。
- 不自动 fetch、checkout、merge、commit 或 push。
- 不接受绝对路径、父目录跳转、仓库外符号链接或无效 ref；模型语义声明不再负责差异覆盖校验。
- 已有 dirty 文件无法可靠拆分归属时，只展示明确标注的最终源码区块。
