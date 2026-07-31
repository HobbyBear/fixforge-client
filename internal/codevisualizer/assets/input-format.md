# 图解输入格式 v2

## 顶层结构

```json
{
  "version": 2,
  "title": "订单状态变更审核",
  "summary": "需求分支增加状态跳转保护。",
  "comparison": {},
  "flows": [],
  "changes": [],
  "database_changes": [],
  "config_changes": [],
  "api_changes": [],
  "log_points": []
}
```

验收和风险仍属于上游工作流质量门槛，不进入图解输入或页面。

## 比较模式

本地未提交代码：

```json
{
  "mode": "working_tree",
  "base_ref": "HEAD",
  "scope_paths": ["service", "controller"],
  "exclude_paths": ["generated"]
}
```

若同目录存在 `baseline.json`，省略 `base_ref` 时优先使用其中的 `base_commit` 和 `scope_paths`。

分支合并审核：

```json
{
  "mode": "branch_compare",
  "base_ref": "origin/master",
  "head_ref": "origin/feature/order-status",
  "strategy": "merge_base"
}
```

- `merge_base`：比较共同祖先与 head，适合 MR/合并审核。
- `direct`：直接比较 base 与 head 两个快照。
- 生成器只读取现有 ref，不执行 fetch 或 checkout。

## 文件分析和语义说明

Git 是文件清单、hunk、unit ID 和新旧行范围的唯一事实来源。模型只提供分析；`changes`、`units` 以及其中任意字段缺失时，生成器都会从真实差异补齐可渲染结构。

普通修改可使用 `file` 简写：

```json
{
  "file": "service/order.go",
  "purpose": "阻止非法状态跳转。",
  "implementation": "在 updateStatus 保存前校验允许矩阵。",
  "units": [
    {
      "id": "analysis.update-status",
      "title": "校验订单状态跳转",
      "meaning": "在保存前校验状态并处理失败。",
      "reason": "旧实现只检查空值。",
      "impact": "所有订单状态写入经过统一校验。"
    }
  ]
}
```

新增、删除和重命名使用明确路径：

```json
{"old_file": null, "new_file": "migrations/20260722.sql"}
{"old_file": "legacy.go", "new_file": null}
{"old_file": "old/order.go", "new_file": "service/order.go"}
```

unit 是按 Git hunk 顺序提供的可选语义说明：

```json
{
  "id": "analysis.update-status",
  "title": "校验订单状态跳转",
  "meaning": "在保存前校验状态并处理失败。",
  "reason": "旧实现只检查空值。",
  "impact": "所有订单状态写入经过统一校验。"
}
```

`id` 只是模型在 `flows` / `code_targets` 中连接自己分析内容的临时别名，输出时会被替换为 Git 派生 ID。不要填写 `kind`、`symbol`、`old_range` 或 `new_range`；生成器会从 Git 差异确定这些结构字段。旧格式仍可输入，但这些字段只作语义映射提示，不能覆盖真实 Git 结果。二进制文件和基线前已经 dirty 的文件也由生成器自动选择安全展示方式。

## 改动链路

```json
{
  "title": "订单状态更新",
  "description": "请求进入服务后校验并保存。",
  "steps": [
    {
      "label": "校验状态",
      "unit_id": "analysis.update-status",
      "explanation": "拒绝未配置的状态跳转。"
    }
  ]
}
```

`flows` 可以为空。由于 unit ID 由生成器确定，模型生成的无法映射引用会被忽略；不应为了构造引用而猜测代码范围。

## 数据库、配置、接口和日志

四类数组中的条目使用适合项目的中文字段，并通过 `code_targets` 关联 unit：

```json
{
  "对象": "orders(status, updated_at)",
  "类型": "新增联合索引",
  "迁移文件": "migrations/20260722.sql",
  "数据影响": "不改历史数据",
  "code_targets": ["database.order-status-index"]
}
```

建议内容：

- `database_changes`：表、字段、索引、SQL/迁移文件、历史数据和上线影响；可用 `SQL` 补充从代码还原的准确 SQL，生成器会为缺失内容生成有明确占位符的参数化等价 SQL。
- `config_changes`：配置路径、旧值、新值、默认值和发布要求。
- `api_changes`：接口 URL、请求参数、响应变化和错误码；页面不会展示兼容性、数据流或其他扩展属性。
- `log_points`：事件作为可直接搜索的稳定关键词，并填写级别、触发条件、字段、脱敏要求和调试用途；页面将触发条件和用途合并为“使用场景与证实问题”。

页面固定保留数据库、日志、接口变动三个标签；`config_changes` 显示在数据库标签的“配置写入”分区，数组为空时在对应分区显示未发现变更的空状态。`database_changes` 与 `log_points` 以真实 diff 为唯一事实来源：生成器扫描新增、删除和修改的 SQL、ORM/DAO/持久化模型代码和日志调用，并拒绝无对应代码区块或事件的人工条目。数据库结果会自动补充表名、字段以及新建表/新增字段分类；人工内容只能补充已识别条目的业务含义，不能覆盖这些代码事实。

## 批注输出

页面按 unit 导出 `review-feedback.json`：

```json
{
  "version": 1,
  "comparison": {
    "mode": "branch_compare",
    "base_sha": "...",
    "head_sha": "...",
    "compare_sha": "...",
    "fingerprint": "..."
  },
  "comments": [
    {
      "id": "service.update-status.validation",
      "file": "service/order.go",
      "symbol": "updateStatus",
      "old_range": [5, 10],
      "new_range": [13, 23],
      "comment": "错误需要使用项目统一错误码。"
    }
  ]
}
```

只有填写了 `comment` 的区块会进入批注数量、本地保存和导出结果。AI 重新修改前必须核对 fingerprint；不一致时重新生成图解，不能按过期范围直接修改。
