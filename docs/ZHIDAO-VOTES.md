# 知到课堂投票只读接入研究

## 结论与证据等级

新版知到校内课的官方学生 H5 前端包含独立的课堂投票读取链路，可以作为后续只读监测的接入依据。它不是普通作业／考试列表，也不依赖把数值任务类型推测成投票。

**目前完成的是公开客户端静态源码研究，不是实时监测实现或真实课堂验收。** 未调用真实课堂业务接口、未读取真实会话或个人任务快照，也未进行投票、答题、签到、入课或部署。下载的客户端 JavaScript 只作静态阅读，没有执行。

关键阻塞是：已有会话能否在不调用入课接口的情况下读取，以及怎样可靠确认投票仍开放。未解决这两项前，不应在界面宣称已经支持实时投票。

## 1. 已找到的学生端读取链路

新版课程页面将学生导向 `classroom-h5.polymas.com/room-stu`，携带 `courseId`、`term` 和 `lessonId`。学生 H5 的接口基址为 `https://cloudapi.polymas.com`。

| 用途 | 方法与路径 | 前端可确认的参数／字段 |
| --- | --- | --- |
| 发现正在上课的课次 | GET `/student-course/lesson/live/list` | 调用点无显式参数，公共拦截器可能补 `courseId, term`；返回列表消费 `lessonId, courseId, term, courseName, endTime` |
| 学生课堂基础信息 | GET `/student-app/interaction/api/lesson/basic/info` | `lessonId` 及课程上下文；消费 `courseId, term, lessonId, groupId, lessonType, startTime` |
| 课堂互动快照 | POST `/student-app/interaction/waterfall/list-all` | JSON `{lessonId, queryType:"ALL"}`，公共层另补 `courseId, term`；消费 `{code:200,data:[...]}` |
| 投票详情 | GET `/student-app/api/vote/getVoteDetail` | `voteId` 及课程上下文；消费 `voteId, title, isQuickVote, checkType, isFinish, options` |
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

- 组件以 `isFinish === 1` 判断已结束；不能反推缺少该字段就表示仍开放。
- 缺少 `participated` 不得自动解释为未参与；已参与和已结束应分别建模。
- `isShowAnswer`／`isShowResult` 不代表投票是否开放。
- 快速投票与自定义投票均规范化为 `VOTE`；题型包括 `single`、`multi`、`judgment`。
- 组件对 `limitSeconds`、`endTime` 的计算不是普通的“截止减现在”；单位、时区、无限时编码和字段业务含义尚待实测，不能根据名字生成截止时间。
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

现有接入位置包括 `internal/platform/zhidao.go` 的请求／续期能力、`internal/server/zhidao.go` 的绑定代次保护，以及 `internal/core/inclass.go` 的持久化去重。服务器当前课上循环专用于雨课堂，给知到增加读取方法不会自动启用投票监测；事件文案和结束取消策略也需独立设计。

## 4. 待验证项与覆盖限制

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
| 投票状态及计时 | [学生投票组件](https://classroom-h5.polymas.com/assets/modules/room-stu/index-BJIDjldV.js)：`isFinish===1`、`selectedStudents`、`limitSeconds` |
| IM 初始化边界 | [useStudentImSession--WPA7LEf.js](https://classroom-h5.polymas.com/assets/shared/useStudentImSession--WPA7LEf.js)：`businessType:"TX"`、`TIMCustomElem`；[IM wrapper](https://classroom-h5.polymas.com/assets/shared/im-monitor-DQuB511y.js)：`/signalling/user/v1/sig`、`onMessageReceived` |
| 另一条 PC 学生网关 | [index-BUeXDOKM.js](https://hike-teaching-center.polymas.com/classroom_hub/assets/index-BUeXDOKM.js)：`/student-course/lesson/live/waterfall/list-all`；尚未证明与 H5 投票覆盖等价 |
