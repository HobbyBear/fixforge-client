---
name: code-architecture-review
description: Analyze a repository or selected code change and produce an offline interactive HTML report that keeps business flow, code-module ownership, cross-module contracts, design intent, decisions, failures, and source evidence in one expandable compact view. Use when the user asks to understand how code works without reading it, connect business logic to current code design, map a feature across modules, generate a business or architecture flowchart, explain data flow, or turn code review findings into a browsable walkthrough.
---

# Code Architecture Review

生成一份可离线打开的 HTML 代码架构审核报告。报告的目标是帮助读者建立“从哪里进入、数据经过哪些模块、每个模块负责什么、结构体如何传递和变形”的可验证心智模型，而不是罗列文件或泛泛总结代码。

## 工作边界

- 先查 RepoMind，再查最小源码证据；需要跨文件调用链、影响面或关系补充时使用 Graphify。
- Git、AST、类型检查器、框架路由和实际源码是关系事实的来源。模型只负责命名、分组、解释和安排阅读顺序。
- **AI 与渲染器职责必须分离**：AI 只分析代码并输出一份符合 `references/report-contract.md` 的 JSON 数据；不要生成 HTML、CSS、JavaScript、React 组件，也不要修改模板样式来“修复”报告外观。页面布局、颜色、字体、Tab、折叠面板、星空图和交互全部由 `assets/architecture_report.py` 或宿主渲染器负责。
- 不把猜测写成调用边、服务依赖、表读写或字段含义。证据不足时明确标注“待确认/未发现证据”。
- 只读分析，不修改被分析仓库，不执行 `fetch`、`checkout`、`merge`、`commit`、`push`，不上传源码。
- HTML 默认自包含、无网络依赖；代码片段必须脱敏，禁止把 token、密码、私钥或真实业务数据嵌入报告。

## 分析流程

1. **确定范围**：识别用户关注的功能、目录、入口、分支或 commit。没有明确范围时，以仓库主入口、服务启动入口、路由注册和最近的业务模块为起点，并在报告中写明范围。
2. **查询知识库**：执行 `repomind kb-migrate`、`repomind kb-metadata`，按命中的 `concepts`、`modules`、`troubles` 读取 1-3 个最相关文档。使用模块文档提供的入口和关键词定位源码。
3. **建立证据表**：记录文件、符号、行号、定义/调用/读写关系和证据来源。先用 `rg -n -C 3` 取小上下文；只有关系问题无法由局部源码确认时才扩大读取。
4. **补充结构关系**：Go 优先使用 AST/类型定义和显式调用；TypeScript/JavaScript/Python/Java 等使用可用的语言解析器、路由注册、接口实现和导入关系。需要完整 callers/callees 或跨模块路径时，使用小预算的 `graphify query`、`graphify explain` 或 `graphify path`。
5. **划分业务功能**：先判断本次范围是否包含多个互不相关的业务能力。只有业务目标、入口、用户结果或领域边界明显不同，才拆成多个 `features`；同一功能中的 Prepare/Finalize、成功/失败/重试、Pub/Sub、去重、重排、Outbox 等是阶段、关系或 `scenarios`，不能误拆成功能。无法证明业务边界时保留为一个功能并标注待确认。必须读取用户语义分析重点；“新增的表/这次改动的类/聊天 server 相关类型”等表达要转成顶层 `analysis_focus`，并用于实体筛选，不能只作为报告标题。
6. **先建立设计主脊梁**：生成 `architecture_design`，明确每个代码模块在业务中代表什么、负责什么、为什么由它负责、接收和产出什么、明确不负责什么。再生成跨模块 `contracts`，说明调用方式、传输对象、语义、生命周期和源码证据。模块归属和契约不是附录，必须在业务流程节点、相邻契约桥或详情面板中可见。
   对事务、幂等和错误传播必须继续读到被调 helper：共享同一个 `tx` 不能自动证明原子性，必须确认错误是否上抛、每条失败路径是否 rollback、成功路径是否 commit，以及 helper 是否吞错后仍让领取/状态日志提交。设计意图和当前实现不一致时，主流程按实际行为描述，并在相关 lane/node 上添加风险。
7. **解释业务路径**：先把代码翻译成非技术读者能理解的业务流程，再绑定模块、契约和方法证据。使用 `flow_map` 表达真实执行顺序：根节点是用户动作，第一层是完整主链路，第二层是阶段内部的业务动作。每个节点必须有 `lane_id`，跨模块时使用 `contract_in_ids` / `contract_out_ids`；叶子节点通过 `source_node_ids` 绑定方法。不要用目录树、纯模块拓扑或孤立方法卡冒充架构说明。
   每层都必须具有明确入口、向下主线、判断节点、具名成功/失败分支和结束状态。停止、重复、并发超限、生成前拒绝、AI 错误、取消、空结果、投递失败、记录或结算失败等已由代码证明的出口必须放在发生它的节点旁边；不能把会提前结束的判断合并成一个普通顺序步骤。并行分支使用 `4A/4B`，不要制造虚假串行顺序。父层应概括子层的关键分支，使读者无需先展开也能知道哪里会提前结束。主流程标题和说明使用业务语言，实现术语只放在括号或证据区。若“用户已看到结果”和“业务真正完成”不是同一时刻，必须用 `status_change.user/system` 分开说明。叶子层先列 `business_rules` 和结果，再列方法证据。`reader_guide` 只解释术语和用户可见时序，不能替代 `flow_map`。
8. **生成 HTML**：canonical 模板固定放在本 Skill 的 `assets/architecture_report.py`，生成任意仓库的报告时必须读取并复用它；仓库内只保存报告 JSON 和生成后的 HTML，不复制或手改模板。用户只要求业务流程时，输出流程与代码设计工作台即可，不强制渲染类关系和表关系；用户明确要求完整架构报告时，再提供对应次级视图。旧的批注式 diff 页面不能作为本 Skill 的 HTML 输出。
   AI 在这一步只提交 JSON；不得把 HTML/CSS/JS 放进 JSON 的扩展字段。canonical renderer 的默认主视图必须把 `flow_map` 渲染成紧凑的纵向业务主线：节点按真实顺序排列，每张节点卡直接显示负责模块与代码边界，相邻跨模块流转显示契约名和 payload。`architecture_design.lanes` 的完整模块说明放在左侧导航和右侧详情中，不得再在画布顶部重复渲染一排模块卡，也不得为了模块归属把主线铺成固定多列泳道而制造大面积空白。点击节点必须在原位置展开 `children`，父节点、祖先和其它主流程阶段不能消失；再次点击收起。方法证据只进入右侧面板。禁止整页下钻、缩进目录树、忽略 `branches`、只显示方法卡或把模块关系放到另一个割裂 Tab。
   主流程沿单一视觉脊梁连接；失败、取消、正常提前返回等分支紧贴来源节点分栏展示，并在分支卡上标明目标模块。画布使用原生滚动容器：滚轮只滚动，不缩放；缩放只能由显式控件触发。鼠标平移必须要求主键持续按下并发生有效移动，松键、取消、失焦或 `buttons=0` 时立即结束；快速点击不得触发拖动或文本选择。
   canonical renderer 必须同时提供亮色和暗色主题：亮色为默认主题，显式主题按钮负责切换并在本地持久化用户选择。暗色主题使用近黑画布、炭灰分层面板、中性灰边框、高对比文字和克制的琥珀交互强调；模块、成功和失败颜色只做小面积语义标识，不能把整个暗色页面染成单一蓝紫色或覆盖原有业务状态含义。
   canonical renderer 的唯一调用方式是：`python <skill-root>/assets/architecture_report.py --input <report.json> --output <report.html>`。不得从仓库内的旧 HTML、`CodeChangeVisualizer` 或临时脚本复制页面。FixForge 原型参考源为 `/home/xch/goprojects/src/fixforge/web/src/react/pages/CodeArchitecturePrototypePage.jsx` 及同目录 CSS；该源码只用于维护 skill asset，不作为最终报告模板的替代品。
9. **检查报告**：先做数据校验，再做读者验收。确认根层顺序与源码一致，lane/contract/source/branch 引用全部可解析；每个核心模块都回答职责、设计理由、输入、输出和负向边界；每次跨模块流转都有契约或明确标记为待确认。默认页面必须让读者同时看见业务主线、模块分工和数据如何跨模块传递；展开任意节点后仍能看到其父节点和全局位置。桌面和窄屏布局不得重叠，页面无需网络即可打开。用真实浏览器分别验证亮色、暗色的桌面与窄屏布局、文字对比度、主题切换和刷新后持久化；同时验证滚轮保持缩放比例不变且只改变画布滚动位置、按住移动时才平移、松键后继续移动不会平移、快速点击不会留下文本选区，并确认画布没有重复模块卡、主流程卡之间的空白不超过单张流程卡高度、分支紧贴来源节点且模块标签可见。
10. **完成 RepoMind summary gate**：代码查找或生成报告后，执行 `repomind-summary` 规则。只有产生可复用的新模块入口、关键词、业务边界或排查经验时才写回知识库；最终答复前必须完成 gate。

## 报告必须回答的问题

- 这个功能解决什么问题，用户或上游调用方从哪里进入？
- 核心模块各自负责什么，边界在哪里，哪些逻辑不应放进该模块？
- 数据从请求/事件/命令进入后，经过哪些结构体、服务、仓储、外部接口和消息边界？
- 每个关键结构体/DTO/配置对象/表的用途是什么，字段分别代表什么，谁创建、谁读取、谁修改、何时失效？
- 结构体之间是包含、实现、输入输出转换、引用、持久化映射还是仅同名偶合？
- 哪些关系由源码直接证明，哪些只由 AST/Graphify 推断，哪些当前证据不足？
- 失败、重试、幂等、事务、异步和权限边界会怎样改变数据流？若当前范围没有证据，不能补写结论。
- 本次分析包含哪些互不相关的业务功能？当前选择的功能与其它功能的边界是什么？哪些节点是该功能专属，哪些是跨功能共享依赖？

## HTML 组织

业务流程模式按下面顺序组织页面，避免把所有内容堆在一个大图中：

1. **设计主张**：用 3-6 条短句说明代码为何这样拆分，避免读者自行从文件名猜设计意图。
2. **紧凑主线**：业务主流程按真实顺序沿单一纵向脊梁排列；节点卡用模块标签和颜色表达职责归属，避免固定多列造成空白。
3. **契约桥**：相邻跨模块流转展示调用方式、对象/payload 和语义；点击可查看来源与去向。
4. **原地展开**：点击阶段在原位置插入子流程，祖先和其它主流程阶段不消失；支持递归展开与逐层收起。
5. **右侧解释**：同步展示当前业务节点、所属模块、设计理由、输入输出、负向边界、业务规则和源码方法。
6. **风险与限制**：把当前实现缺口贴在相关模块或流程节点，而不是藏在报告末尾。

### 功能与场景边界

- `feature` 表示互不相关的业务能力，例如“聊天系统”和“项目初始化”；不要把同一能力的 Prepare、Finalize、Pub/Sub、去重、重排、Outbox 拆成多个功能。
- `scenario` 表示一个功能中的执行变体，例如成功、重复请求、幂等冲突、超时、取消和重试。
- 一个类、结构体、表或公共服务可以属于多个功能；在当前功能中标记为 `core`、`shared` 或 `context`，不要复制成多个事实节点。
- 自动功能聚类只能提出候选，依据应包括独立入口、业务结果、领域词、数据边界和模块社区；AI 可以命名、合并、拆分和排序，但不能创造关系。
- 用户选择一个功能后，架构图、业务流程图、类关系图和表关系图必须从同一个 feature projection 生成；切换 Tab 不能丢失功能选择。

详细字段契约见 [references/report-contract.md](references/report-contract.md)。

## 证据等级

- `direct_source`：当前锁定源码中直接出现的定义、调用、字段读写或路由注册。
- `ast_definition` / `ast_call`：语言 AST 解析得到的定义或调用，必须带文件和行号。
- `graph_relation`：Graphify 明确输出的结构关系；不可把少量源码片段替代完整调用图。
- `source_backed_walkthrough`：由多个直接证据串成的阅读步骤。
- `inferred`：合理但未被代码完整证明的解释，只能作为假设并显示“待确认”。
- `coverage_only`：仅表示 Git 变更覆盖顺序，不得称为业务调用链。

## FixForge 变更审核模式

当用户要求审核未提交改动或分支合并时，先锁定比较范围，再生成变更报告：

- `working_tree` 只分析已被 Git 跟踪的 staged/unstaged 文件；未跟踪文件不能伪装成已审核内容。
- `branch_compare` 使用锁定的 `base_ref`、`head_ref` 和 `merge_base`/`direct` 策略；不能用当前 checkout 源码冒充另一侧快照。
- Git 负责文件、hunk、unit ID 和新旧范围；模型只补充 `purpose`、`meaning`、`reason`、`impact` 和真实可映射的 `flows`。
- 页面必须保留 diff 证据、代码地图和结构体/数据流视图；批注导出要携带 comparison fingerprint，快照变化后不能沿用旧行号修改代码。

## 完成条件

业务流程报告只有同时满足以下条件才报告完成：

- HTML 已生成并可直接打开，且无外部网络请求。
- 至少有一个真实入口、一个核心模块职责说明和一条有源码证据的数据流。
- 主流程入口、关键判断、失败出口和成功终点均可见，且顺序与当前源码一致。
- 至少一个阶段可继续下钻到业务动作，至少一个叶子可下钻到具体方法和文件行号。
- 所有流程分支和方法绑定都有源码证据或明确标记为待确认。
- 已说明分析范围、未知项、快照/fingerprint 和 RepoMind summary gate 结果。

## 输出边界

AI 的最终分析产物必须是单个 JSON 对象，不能混入 Markdown 解释、HTML、CSS、JavaScript 或组件代码。最小流程是：

1. AI 读取代码证据并生成结构化报告 JSON。
2. 校验器检查 ID、行号、功能投影和证据引用。
3. canonical renderer 读取 JSON，生成离线 HTML。

同一份 JSON 必须可以被原型、正式页面和离线 HTML renderer 重复渲染；禁止让 AI 为不同页面分别生成不同的数据版本。
