# 知到课堂投票只读接入研究

## 结论与证据等级

新版知到校内课的官方学生 H5 前端包含独立的课堂投票读取链路，可以作为后续只读监测的接入依据。它不是普通作业／考试列表，也不依赖把数值任务类型推测成投票。

**当前状态（2026-11-10）：已在真实概统课堂完成只读读取验证；新投票的实时 Bark 到达仍待观察。** 生产实例成功读取进行中课次、投票发布信令和投票详情，四个只读端点均返回 200。当天 08:21 的课堂投票在修复后的首次完整快照前已由本人投过（`isFinish=1` 表示当前账号已投票，限时 300 秒），因此按基线规则静默记录，没有补发通知；这不能替代一条新投票从发布到 Bark 到达的完整验收。

关键未验证项：新投票在直播期间的检测与 Bark 到达时延，以及投票删除、延时、重新发布等边界。实现只读取官方学生 H5 文档中出现的四个只读端点，不调用入课、投票、签到或提交接口。由于尚未完成上述验收，界面与文档都不得宣称“已支持”或承诺检测延迟。实现失败时会显示明确错误，不会把失败当成“当前没有投票”。

## 实现（实验，默认开启）

- 设置项 `zhidao_in_class_enabled` 默认 `true`，用户可显式关闭且该选择会持久化；与雨课堂课上提醒相互独立，共用 `in_class_poll_seconds` 检查间隔。
- `internal/platform/zhidao_classroom.go` 只调用：`GET /student-course/lesson/live/list`、`GET /student-app/interaction/api/lesson/check/finish`、`POST /student-app/interaction/waterfall/list-all`、`GET /student-app/api/vote/getVoteDetail`。
- 鉴权只读取 `ai-poly` Cookie 并作为原值 `Authorization`；缺少该 Cookie 时返回明确错误，不回退到 `jt-cas`，也不修改现有任务的 token 优先级。真实会话已验证该凭据可用。
- 实测格式：`lessonId` 与 `voteId` 是 10 位字母数字串，不是纯数字；瀑布流同时提供 `createTime`（`YYYY-MM-DD HH:MM:SS`，中国时间）和 `msgTime`（epoch 秒），实现优先使用 `msgTime`。口述快速投票的 `optionContent` 为 `null`，仅返回 A/B/C 的 `optionCode`，实现回退显示编码。
- `internal/server/zhidao_inclass.go` 在绑定代次、原始会话字符串、开关和进程生命周期任一变化时丢弃本轮结果；失败或畸形快照不会建立基线。
- 每个课次第一次完整快照作为静默基线；只有 `createTime` 在最近 2 分钟内的投票会在首次快照中提醒，更早的历史投票静默记录。之后仅对未见过的 `lessonId + bizId` 拉取详情并提醒。
- `isFinish` 只表示当前账号是否已投票，不表示投票结束；已投票或已过结束时间（优先取 `endTime`，缺失时由 `startTime + limitSeconds` 推断）的投票静默记录。字段缺失或无法解析时状态标记为未知；结束时间未知时通知正文明确写“投票状态暂未确认”，不猜测已结束或未参与。
- 持久化去重键沿用 `core.PlatformZhidaoCampus`、`lessonId`、`vote:bizId`。不解析 `selectedStudents`、投票人数或任何参与者身份。
- 通知不显示截止时间：`endTime` 与 `startTime + limitSeconds` 只用于判断投票是否已结束，不写入通知正文；无法无歧义解析时保持未知并照常提醒。
- 通知只写“打开知到参与投票”，从未包含自动投票能力；全链路没有任何 `student/join`、`student/enter`、`vote/studentVote`、签到或提交调用。

## 1. 已找到的学生端读取链路

新版课程页面将学生导向 `classroom-h5.polymas.com/room-stu`，携带 `courseId`、`term` 和 `lessonId`。学生 H5 的接口基址为 `https://cloudapi.polymas.com`。

| 用途 | 方法与路径 | 前端可确认的参数／字段 |
| --- | --- | --- |
| 发现正在上课的课次 | GET `/student-course/lesson/live/list` | 调用点无显式参数，公共拦截器可能补 `courseId, term`；返回列表消费 `lessonId, courseId, term, courseName, endTime` |
| 学生课堂基础信息 | GET `/student-app/interaction/api/lesson/basic/info` | `lessonId` 及课程上下文；消费 `courseId, term, lessonId, groupId, lessonType, startTime` |
| 课堂互动快照 | POST `/student-app/interaction/waterfall/list-all` | JSON `{lessonId, queryType:"ALL"}`，公共层另补 `courseId, term`；消费 `{code:200,data:[...]}` |
| 投票详情 | GET `/student-app/api/vote/getVoteDetail` | `voteId` 及课程上下文；消费 `voteId, title, isQuickVote, checkType, isFinish, options`；实现另读取可选的 `startTime, endTime, limitSeconds` 判断是否已结束 |
| 课次结束状态 | GET `/student-app/interaction/api/lesson/check/finish` | `lessonId` 及课程上下文；前端在 `code===200` 时把 `data===false` 视为未结束 |

这些字段是前端消费的形状，不是完整服务端 schema，也不是脱敏真实响应。`lessonId` 表示一次课堂，不能用教学班 `classId` 代替。`live/list` 调用点无参数不等于已经证明裸 GET 会返回全账号全部课堂；需分别验证无上下文与按课程／学期查询的范围。

### 鉴权边界

H5 客户端启用 `withCredentials`，使用原值 `Authorization`，不自动添加 `Bearer `；默认 token 来自 `ai-poly` Cookie。公共拦截器向请求注入课程与学期上下文。

DueRadar 现有请求也使用原值 `Authorization`，但 `internal/platform/zhidao_session.go` 的 token 选择优先 `jt-cas`，其次才是 `ai-poly`。普通任务接口成功不能证明学生 H5 网关也接受同一 token。不要为了研究投票盲目修改全局 token 优先级；应在已有会话范围内独立验证，失败保持失败，不自动执行登录或入课写操作。

## 2. 投票识别、状态与去重

官方互动列表先解析 `row.message` 内的 JSON，再采用：

```js
const signal = envelope.signal || {};
const signalCode = signal.signalCode || row.signalCode;
const content = {...signal.content, ...row.bizObj};
```

学生映射明确把以下信令转为 `VOTE` 卡片：

```text
IN_CLASS::TOOL-VOTE::PUBLISH
```

其 ID 字段为合并后的 `content.bizId`，随后以 `voteId: bizId` 请求投票详情。因此不能假设原始记录有顶层 `voteId`，也不能用普通任务类型 `4`／`5` 识别投票。

建议稳定去重键为：

```text
platform = zhidao_campus
classroom = lessonId
activity = "vote:" + bizId
```

此处平台值须以实现中的 `core.PlatformZhidaoCampus` 为准，不另建不一致的平台枚举；标题、数组位置和生成的 UI 卡片 ID 都不适合去重。

前端还消费 `participated`、`createTime`、`isFinish` 与选项中的参与者记录：

- 组件以 `isFinish === 1` 标记当前账号已投票；它不是投票结束标记。已参与和已结束必须分别建模，不能互相反推。
- 缺少 `participated`／`isFinish` 不得自动解释为未参与；未知状态仍提醒并注明未确认。
- `isShowAnswer`／`isShowResult` 不代表投票是否开放。
- 快速投票与自定义投票均规范化为 `VOTE`；题型包括 `single`、`multi`、`judgment`。
- 组件对 `limitSeconds`、`endTime` 的计算不是普通的“截止减现在”；单位、时区、无限时编码和字段业务含义尚待实测，不能根据名字生成要展示的截止时间。实现仅在能无歧义解析时用它们判断是否已结束，且只用于抑制通知。
- 投票详情可能包含同学标识等信息。只读适配器应丢弃参与者姓名、头像、个体投票记录等无关数据，仅保留必要的自身状态及投票元数据，不写入任务快照、日志或通知。

## 3. 实时机制与最小方案

官方 H5 使用初始 HTTP 快照，加腾讯 TIM 风格 IM 自定义信令；返回前台时重新获取状态及快照，刷新有三秒节流。**三秒节流不是三秒轮询**，其他模块的在线人数计时器也不是投票刷新周期。

已找到 `PUBLISH`、`STUDENT_ADD`、`REFRESH`、`END`、`DELETE`、`DELAY` 等投票信令，但没有完成全部生命周期处理的运行验证，也没有确认可直接复用的 WebSocket 握手／心跳协议。

不建议第一版照搬整个官方课堂初始化：该流程还包含 `student/join`、`student/enter`、入课广播以及可能的已读回执，不能称为纯只读。额外 IM 登录还可能影响手机端多端会话，尚未验证。

最小方案应采用独立 HTTP 短周期读取，而非混入十五分钟任务同步：

1. 按已知课程／学期发现课次，校验归属；读取基础信息和结束状态。
2. 第一次成功的完整互动快照只建立基线，不把历史投票批量补发。
3. 仅在课期间读取互动，筛选发布信令，按需补拉详情；状态未知时不猜测开放或未参与。
4. 使用 `lessonId + bizId` 持久化去重；失败、畸形或不完整快照不当作空列表，不重建基线。
5. 可先评估“发现课堂 60 秒、在课互动 15—20 秒”，这是工程建议值，不是平台规定或已验证的延迟承诺；需限并发、失败退避和限流实测。
6. 发 Bark 前再次核验结束／有效性，不能直接套用普通课堂提醒的十五分钟有效窗口，让已结束短投票继续排队。
7. 绑定代次、原凭据比较与续期持久化沿用现有隔离原则；不读取或保存其他用户的资料。

明确不调用 `student/join`、`student/enter`、`/student-app/api/vote/studentVote`，也不调用签到、答题提交、发布、结束或删除接口。HTTP 方法为 GET 本身并不能证明一个端点没有副作用。

现有接入位置包括 `internal/platform/zhidao.go` 的请求／续期能力、`internal/server/zhidao.go` 的绑定代次保护，以及 `internal/core/inclass.go` 的持久化去重。知到投票监测使用独立循环 `runZhidaoInClass`，不会因雨课堂课上提醒开启而自动启用；两边的事件文案、失败策略和开关完全独立。

## 4. 待验证项与覆盖限制

以下项目在实现完成后仍然全部待验证。当前代码只是按公开客户端契约编写的只读适配器，测试全部使用合成响应，不能替代真实课堂验收。

- 现有会话及已有 `ai-poly` 对学生 H5 网关的鉴权兼容性。
- 不执行 join／enter 时，是否允许发现课堂、读取快照与详情。
- `live/list` 的查询范围及课程／学期上下文要求。
- 快速与自定义投票的真实发布、已投、结束、删除、延时、无限时状态与 `bizId` 对应关系。
- 计时字段单位／时区、历史快照截断、分页或增量能力、限流及可接受轮询频率。
- 重启基线、重新绑定、凭据过期、通知发送前已结束等边界。

新版 `hikeAiCourse` 有明确学生投票读取源码，仍未验收；旧版 `spocCourse` 投票链路本轮未完成。共享课及不同直播课堂类型也不能由新版校内课证据推导支持。

## 5. 公开官方源码索引

压缩资源以文件名和字符串锚点定位；构建 hash 变化后链接可能失效。以下仅证明客户端代码存在相应逻辑，不证明服务端运行行为或授权只读验收已通过。

| 证据 | 官方来源与锚点 |
| --- | --- |
| 课堂发现 | [classroom-DNv6AAnV.js](https://hike-teaching-center.polymas.com/agent_course_app/assets/classroom-DNv6AAnV.js)：`/student-course/lesson/live/list`；[课程首页](https://hike-teaching-center.polymas.com/agent_course_app/assets/index-DN_zd2Lc.js)：`OnGoing` |
| 新版学生入口 | [index-Bu83oCRy.js](https://hike-teaching-center.polymas.com/agent_course_app/assets/index-Bu83oCRy.js)：`/room-stu?courseId=` |
| 课程请求上下文 | [index-zeHjbZLB.js](https://hike-teaching-center.polymas.com/agent_course_app/assets/index-zeHjbZLB.js)：`function Ds()`、课程／学期注入 |
| H5 接口与鉴权 | [index-B0uxT_gM.js](https://classroom-h5.polymas.com/assets/shared/index-B0uxT_gM.js)：`J.interceptors.request.use`、`/waterfall/list-all`、`getVoteDetail` |
| 互动快照解析 | [useInteractions-aoHS2pwC.js](https://classroom-h5.polymas.com/assets/shared/useInteractions-aoHS2pwC.js)：`queryType:"ALL"`、`signalCode`、`getVoteDetail` |
| 学生投票映射 | [message-handlers-BotQAkyC.js](https://classroom-h5.polymas.com/assets/shared/message-handlers-BotQAkyC.js)：`IN_CLASS_TOOL_VOTE_PUBLISH`、`idField:"bizId"` |
| 信令名称 | [ai-signal-action-Xo3RRWXP.js](https://classroom-h5.polymas.com/assets/shared/ai-signal-action-Xo3RRWXP.js)：`IN_CLASS::TOOL-VOTE::PUBLISH` 等 |
| 课堂结束及前台刷新 | [DiscussionIcon-CtdS8NX2.js](https://classroom-h5.polymas.com/assets/shared/DiscussionIcon-CtdS8NX2.js)：`checkLessonStatus`、`visibilitychange` |
| 投票状态及计时 | [学生投票组件](https://classroom-h5.polymas.com/assets/modules/room-stu/index-BJIDjldV.js)：`isFinish===1` 表示本人已投票、`selectedStudents`、`limitSeconds` |
| IM 初始化边界 | [useStudentImSession--WPA7LEf.js](https://classroom-h5.polymas.com/assets/shared/useStudentImSession--WPA7LEf.js)：`businessType:"TX"`、`TIMCustomElem`；[IM wrapper](https://classroom-h5.polymas.com/assets/shared/im-monitor-DQuB511y.js)：`/signalling/user/v1/sig`、`onMessageReceived` |
| 另一条 PC 学生网关 | [index-BUeXDOKM.js](https://hike-teaching-center.polymas.com/classroom_hub/assets/index-BUeXDOKM.js)：`/student-course/lesson/live/waterfall/list-all`；尚未证明与 H5 投票覆盖等价 |
