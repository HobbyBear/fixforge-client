# 架构报告数据契约

该契约用于指导 AI 产出结构化数据，再交给现有 HTML 生成器或自包含渲染器。字段可以扩展，但不能删除证据和来源字段。

## AI 输出边界

AI 只生成符合本契约的 JSON 数据，不生成 HTML、CSS、JavaScript、React 组件或页面布局配置。字体、颜色、Tab、面板折叠、星空图布局和交互由 Skill 的 `assets/architecture_report.py` 或宿主 renderer 统一负责。页面出现样式问题时，修 renderer/template，不改变同一份分析 JSON 来迎合页面。

推荐流水线：`源码证据 -> AI JSON -> 校验/功能投影 -> canonical renderer -> 离线 HTML`。

## 顶层

```json
{
  "schema": "code-architecture-report/v1",
  "source_format": "code-change-walkthrough/v2",
  "title": "功能架构审核",
  "summary": "一句话说明功能和阅读入口",
  "analysis_focus": {"query": "用户指定的分析重点", "entity": "all", "policy": "all", "include_ids": [], "exclude_ids": []},
  "reader_guide": {"title": "一轮角色聊天：从发送消息到回复落库", "subtitle": "用用户能理解的语言说明入口、主路径、失败出口和最终状态", "default_scenario_id": "", "terms": [], "user_flow": [], "state_flow": [], "user_timeline": [], "failure_branches": []},
  "architecture_design": {"title":"代码设计主脊梁","summary":"业务流程如何落到模块和契约","principles":[],"lanes":[],"contracts":[],"risks":[]},
  "flow_map": {"id": "root", "title": "业务流程", "summary": "可逐层展开的业务流程树", "children": []},
  "scope": {"paths": [], "entrypoints": [], "comparison": {}},
  "features": [],
  "services": [],
  "nodes": [],
  "edges": [],
  "scenarios": [],
  "data_structures": [],
  "tables": [],
  "evidence": [],
  "unknowns": []
}
```

`reader_guide` 面向第一次接触功能的读者。`default_scenario_id` 必须指向最主要的成功场景；`terms[]` 用一句话解释 Core、Edge、Prepare、Finalize、SSE、Delivery 等术语；`user_flow[]` 只描述主线，`state_flow[]` 必须描述成功状态和失败出口，`user_timeline[]` 描述客户端可见事件，`failure_branches[]` 描述失败原因、客户端结果、是否进入 Finalize、持久化结果和重试策略。没有这些证据时标记为待确认，不要让读者从技术函数名猜业务流程。

## 代码设计主脊梁

`architecture_design` 强制把业务流程和当前代码组织连接起来：

```json
{
  "principles": [{"title":"业务规则归中心服务","statement":"连接层不直接决定权益","source_node_ids":["core.prepare"]}],
  "lanes": [{
    "id":"core", "name":"业务中心", "code_label":"regional_chat",
    "represents":"权威业务决策", "responsibilities":["校验权益"],
    "why_here":"需要访问中心业务数据", "receives":["PrepareRequest"],
    "produces":["ModelPlan"], "not_responsible":["持有用户连接"],
    "source_node_ids":["core.prepare"]
  }],
  "contracts": [{
    "id":"prepare-contract", "name":"生成前决策契约",
    "source_lane_id":"edge", "target_lane_id":"core", "kind":"HTTP",
    "payload":"PrepareRequest -> PrepareResponse", "meaning":"连接参数换取业务决策",
    "lifecycle":"单次请求", "source_node_ids":["edge.handle","core.prepare"]
  }],
  "risks": [{"title":"当前限制","meaning":"证据支持的设计缺口","lane_ids":["core"],"source_node_ids":["core.prepare"]}]
}
```

- `lanes[]` 是业务职责边界，不是目录分组；同一目录承担不同职责时可拆泳道，多个目录实现同一职责时可合并泳道。
- 每个核心 lane 必须提供 `represents`、`responsibilities`、`why_here`、`receives`、`produces`、`not_responsible` 和源码节点。
- `lanes[]` 是语义映射，不要求 renderer 使用固定列坐标。默认流程页应在节点卡、左侧导航和右侧详情中消费 lane 信息；不要在画布顶部重复渲染与侧边栏相同的模块卡。
- `contracts[]` 只记录有源码/AST/Graphify 证据的跨模块流转。必须写明两端泳道、调用/事件/数据方式、payload、业务语义和生命周期。
- `principles[]` 解释整体设计选择；`risks[]` 只写当前实现证据支持的限制或缺口。
- 契约涉及事务、幂等或领取状态时，`lifecycle` 必须描述当前实现而不是理想语义。只有验证被调 helper 会向上传播错误、失败路径回滚且成功路径提交后，才能写“原子提交”；helper 吞错、部分成功或状态日志可能单独提交时必须进入 `risks[]` 并绑定相关节点。

`flow_map` 是流程图的首要数据，不是架构节点的别名列表。它必须是一棵有方向的业务流程树：第一层按真实执行顺序放用户能理解的完整主链路；子层逐步展开到服务职责、业务判断和最小动作；叶子通过 `source_node_ids` 绑定实际方法。节点至少包含 `id`、`title`、`summary`、`kind`、`lane_id`、`children`、`source_node_ids`，跨模块节点还要用 `contract_in_ids` / `contract_out_ids` 引用 `architecture_design.contracts`。节点可增加 `status_change` 和 `business_rules`；只要用户可见完成与后台业务完成不是同一时刻，就必须提供 `status_change`。

有分支的任意节点都可以声明 `branches`，不只限于 `decision`。每个分支包含：

```json
{"label":"检查失败","target_id":"chat.prepare.failed","meaning":"直接返回错误，不进入生成","outcome":"failure"}
```

- `target_id` 必须指向同一 `flow_map` 中的节点。
- `outcome` 取 `continue`、`success`、`failure`、`cancel` 或 `terminal`；`terminal` 表示复用已有结果等正常提前返回，用于区分继续主线与提前结束，不能让 renderer 依赖标题猜测。
- 父阶段必须重复最关键的成功/失败出口，使根层就能看懂分支；子层再给出具体判断。
- 失败目标可以位于父节点的 `children` 中，但 renderer 不得把它同时排入成功主线。
- 根层和每个子层都按 `children` 数组顺序渲染；不要用模块坐标替代执行顺序。
- 同级步骤存在真实并行关系时，父节点使用 `"execution":"parallel"`，子节点编号使用 `A/B/C`；renderer 必须明确显示“并行、不代表先后”，不能用普通向下箭头制造虚假串行。
- renderer 默认显示全部第一层主流程。点击有 `children` 的节点时，在该节点原位置展开下一层；父节点和其它第一层节点必须保留。再次点击收起，允许递归展开多个分支。
- renderer 必须消费 `lane_id`、contracts、branches、source ids 和源码位置；方法只在详情面板出现，不能替换主画布。
- renderer 默认使用紧凑纵向主线，`lane_id` 通过节点标签、颜色和详情表达，而不是把节点强制放进固定多列坐标。相邻跨模块节点之间用契约桥表达来源、去向和 payload；分支紧贴声明 `branches` 的节点分栏展示，并在目标卡上显示目标模块。禁止因泳道列制造大面积空白，也禁止让分支卡脱离来源节点悬空。
- 流程节点的 `title`、`summary`、`branches[].label` 和 `meaning` 必须使用业务语言。Core、Edge、Prepare、Finalize、SSE、RESP、SYNC、token、msgid 等实现术语只能作为括号内代码名或证据区内容，不能承担主流程解释。
- 叶子层必须先展示 `business_rules`、`status_change` 和业务说明，再展示方法证据；禁止让非技术读者从函数名、参数名或 DTO 推断业务规则。

`analysis_focus` 是用户语义的硬约束，不是页面备注。`query` 保存用户原话，`entity` 取 `tables`、`structures`、`nodes` 或 `all`，`policy` 至少支持 `all`、`changed_only`、`related_to_changed`。例如用户说“这次代码新增的表”，必须使用 `entity: "tables"`、`policy: "changed_only"`，只输出 HEAD 相对 base 新增或被修改且有证据的表；旧表只能作为明确的关联上下文出现，并标记为 `context`，不能混入主表设计。用户没有指定范围时，默认 `related_to_changed`，优先变更实体及其最小关系闭包。

## 功能与场景

`features[]` 只表示互不相关的业务能力，不表示技术阶段。一个功能可以包含多个场景；例如聊天功能可以同时包含请求受理、模型流、消息投递和结果提交，这些应继续属于同一个功能。

```json
{
  "id": "project-init",
  "name": "项目初始化",
  "summary": "创建本地项目、扫描仓库并建立初始知识库",
  "entrypoints": ["api.project.init"],
  "node_ids": ["api.project.init", "repo.scanner", "repomind.writer"],
  "edge_ids": ["init.scan", "init.write"],
  "scenario_ids": ["init.success", "init.failed"],
  "role_by_node": {
    "api.project.init": "core",
    "repo.scanner": "core",
    "repomind.writer": "shared"
  },
  "evidence": "source_backed_feature_cluster",
  "evidence_ids": ["source.init.entry", "source.init.scan"],
  "confidence": "medium"
}
```

`features[]` 的候选依据包括独立入口、业务结果、领域词、数据边界和模块社区。无法确认是独立业务能力时，不要强行拆分；可以保留一个功能并在 `unknowns[]` 记录待确认边界。

节点和边可以被多个功能引用：

```json
{
  "id": "shared.auth-service",
  "kind": "service",
  "feature_ids": ["chat", "project-init"],
  "feature_roles": {
    "chat": "shared",
    "project-init": "shared"
  }
}
```

页面必须提供 `all features` 和单功能选择器。选择一个功能后，架构、场景、类关系、表关系、搜索结果、统计数字、证据列表和 unknowns 都使用同一个 projection；无关节点隐藏，共享节点保留但标记为 `shared`，上下文节点标记为 `context`。

所有可筛选实体都必须具备 `feature_ids` 和可选的 `feature_roles`：`features`、`services`、`nodes`、`edges`、`scenarios`、`data_structures`、`tables`、`evidence`。不能依赖从端点、场景或目录名反推功能归属。

`feature_role` 是功能内角色，取值为 `core`、`shared`、`context`；`change` 是代码变化状态，取值为 `changed`、`unchanged`；`kind` 才表示 `external` 等节点类型。这三个维度不能混用。

单功能 projection 规则固定为：保留该功能的 `core` 节点、该功能引用的 `shared` 节点，以及为连接当前入口和结果所需的最小 `context` 闭包；只保留两端都在 projection 内的边，孤立边必须丢弃；被隐藏功能独占的节点、边、证据、表和统计不进入结果。

## 模块/代码节点

`nodes[]` 表示服务、模块、函数、方法、接口、结构体或表。代码节点至少包含：

```json
{
  "id": "api.handler.ask",
  "kind": "function",
  "label": "handleQAAsk",
  "service": "internal/api",
  "module": "QA API",
  "file": "internal/api/handler.go",
  "line": 120,
  "end_line": 190,
  "responsibility": "鉴权、组装上下文并把执行事件转为 SSE",
  "inputs": ["QARequest"],
  "outputs": ["QAEvent/SSE"],
  "feature_ids": ["qa"],
  "feature_roles": {"qa": "core"},
  "change": "changed|unchanged",
  "source_kind": "repository|database|external|runtime",
  "evidence": "direct_source",
  "confidence": "high"
}
```

`responsibility` 必须描述职责和边界，不要复述函数名。`inputs`/`outputs` 只填源码可确认的类型或协议。

## 边

```json
{
  "id": "edge-1",
  "source": "api.handler.ask",
  "target": "qa.executor.run",
  "feature_ids": ["qa"],
  "feature_roles": {"qa": "core"},
  "scenario_ids": ["qa/success"],
  "number": "2",
  "kind": "call|data|http|websocket|event|sql|transaction|implements|contains|maps_to|reads|writes",
  "label": "调用执行器",
  "payload": "QARequest -> 本地执行参数",
  "meaning": "鉴权后的请求被转换为执行器输入",
  "evidence_kind": "direct_source",
  "file": "internal/api/handler.go",
  "line": 156,
  "confidence": "high"
}
```

`contains` 表示结构体/模块包含关系，`implements` 表示接口实现，`maps_to` 表示 DTO/持久化模型变形，`reads`/`writes` 表示字段或表的实际读写。仅有相似字段名时不能创建 `maps_to`。

## 结构体、DTO 和字段

```json
{
  "id": "runner.QARequest",
  "name": "QARequest",
  "kind": "struct|class|interface|dto|config|entity",
  "role": "runner 到服务端的执行请求",
  "file": "internal/runner/types.go",
  "line": 40,
  "fields": [
    {
      "name": "RunnerConnID",
      "type": "string",
      "tags": "json:\"runner_conn_id\"",
      "required": false,
      "nullable": false,
      "default": "",
      "meaning": "指定执行请求发送到哪条在线 runner 连接",
      "source": "前端请求",
      "consumers": ["runner hub 路由"],
      "evidence": "direct_source",
      "file": "internal/runner/types.go",
      "line": 44
    }
  ],
  "feature_ids": ["qa"],
  "feature_roles": {"qa": "core"},
  "evidence": "ast_definition",
  "confidence": "high"
}
```

字段含义必须优先从命名、注释、标签、校验、构造、读取和写入共同确认；只有声明没有消费证据时，写“声明用途待确认”。`required`、`nullable`、`default` 不确定时用 `null`，不能猜 `false` 或空字符串的业务含义。

聊天/服务端分析不能只收集跨服务 DTO。必须同时收集变更入口直接拥有或消费的业务 struct/class，包括控制器状态、Prepare/Finalize 业务快照、错误对象、模型执行上下文、连接/投递封装和持久化映射；每个类型至少有定义文件、职责、字段和与节点/其他类型的 `contains`、`maps_to` 或 `consumes` 关系。只有独立声明但未被聊天入口调用的通用模型可以降为 context。

## 场景

```json
{
  "id": "qa/success",
  "feature_id": "qa",
  "title": "正常成功",
  "description": "请求经过鉴权、执行、保存并流式返回",
  "steps": [
    {"number": "1", "node_ids": ["api.handler.ask"], "edge_ids": ["qa/edge/receive"], "explanation": "接收请求"},
    {"number": "2", "node_ids": ["qa.executor.run"], "edge_ids": ["qa/edge/execute"], "explanation": "执行模型"},
    {"number": "3", "node_ids": ["storage.qa.save"], "edge_ids": ["qa/edge/save"], "explanation": "保存结果"}
  ]
}
```

场景 ID 必须全局唯一，建议使用 `feature_id/scenario_name` 命名空间。每个步骤必须引用至少一个真实 `node_id` 和零个或多个真实 `edge_id`；并行步骤使用同一个 `number` 的多个步骤，分支使用 `4A/4B`。无法证明顺序时拆成独立关系或标记并行，不要按文件顺序硬排。没有足够证据时使用 `coverage_only`，并明确它不是完整调用链。

每个成功场景必须补充 `outcome`（客户端结果、持久化变化、失败/重试结果）和 `lanes`（Client、Core、Edge、LLM、Storage、Delivery），用于渲染泳道或阶段卡；步骤还应标记 `mode`（sync、async、parallel、branch）和 `next`。不要把同步响应、异步投递和最终状态写入混在同一条无方向的关系线上。重复参与同一流程的节点必须使用 `step_node_id` 或步骤实例，不得仅靠去重后的 node_id 表示完整时序。

## 表和外部对象

表节点应说明业务职责、核心字段、索引/唯一约束、事务边界、权威状态和缓存/队列关系，并包含 `feature_ids`、`feature_roles`、`change`、`evidence` 和源码/DDL锚点。表之间以及表与业务节点之间的真实读写、事务提交、Outbox 触发、投递关系必须写入顶层 `table_relations[]`，关系至少包含 `source`、`target`、`kind`、`label`、`meaning` 和证据。外部 API、消息 topic、WebSocket 和数据库连接都要记录协议与 payload，但不要泄露 endpoint 中的 token、真实用户数据或凭据。

数据库表、缓存/队列、外部执行接口必须有 `storage_kind`，取 `authoritative_persistence`、`online_delivery`、`external_execution` 之一；还必须有 `evidence_status`，取 `current_implementation`、`openspec_design`、`not_implemented`、`inferred` 之一。结构体应补充 `lifecycle`（created_in、consumed_by、persisted、cross_boundary、replayable），以便页面按业务边界而不是按字段列表展示。

## 渲染和验收

- 页面可点击：服务 -> 模块 -> 节点 -> 结构体/字段 -> 源码证据。
- 节点视觉至少分别表达 `kind`、`change` 和 `feature_role`：不能用 `external` 代替 feature 角色，也不能用 `context` 代替未修改状态；边视觉上区分调用、数据、持久化和异步。
- 支持按场景、模块、结构体名称搜索；窄屏下图可横向滚动，详情不覆盖图和导航。
- 默认画布不得重复左侧模块导航；首屏不应再出现一排相同模块卡。主流程相邻步骤的垂直间距不得大于一张主流程卡的高度，除非中间正在展示契约桥、分支或用户展开的子流程。
- 画布滚轮执行原生纵向/横向滚动，不改变缩放比例；缩放只响应显式控件。鼠标平移只在主键持续按下且移动超过阈值时生效，并在 `pointerup`、`pointercancel`、`lostpointercapture`、窗口失焦或 `buttons=0` 时结束。
- 画布内禁止意外文本选择和浏览器原生拖图；快速点击仍需正常触发节点、泳道和契约选择，完成过拖动的手势不得继续触发点击。
- canonical renderer 默认使用亮色主题，并提供显式的亮色/暗色切换且本地持久化选择。暗色主题使用近黑画布、炭灰面板、中性灰边框、高对比文字和克制琥珀强调；lane、成功、失败颜色仅作为局部语义标识。桌面与窄屏验收必须覆盖两种主题，主题控件不能挤压标题或越出视口。
- 所有代码片段来自锁定快照并做敏感信息过滤；报告中保存 comparison fingerprint。
- 报告末尾显示“已确认关系”和“当前未知项”，防止读者把架构图误解为完整静态调用图。
- 校验所有 `feature_ids`、`node_ids`、`edge_ids`、`scenario_ids`、`evidence_ids` 引用存在且唯一；孤立边、无效引用和无法闭包的上下文节点必须进入 unknowns 或被 projection 丢弃。
