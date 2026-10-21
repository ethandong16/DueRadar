# 后续平台接入

以下记录 2026-10-22 对仍在提供服务的官方公开前端源码的检查结果。当前产品仍以 PLATFORMS.md 的实际能力为准；公开源码和模拟协议测试不代表账号登录或完整任务读取已经通过。

## 普通雨课堂账号密码登录

官方 `/web` 页面使用 `/pc/login/verify_pwd_login/`，手机号模式为 `PP`，邮箱模式为 `E`；密码字段通过 JSEncrypt RSAES-PKCS1-v1_5 加密，提交前还需要官方 TencentCaptcha 的 `ticket`／`randstr` 或备用 hCaptcha token。当前代码的字符串编码按 UTF-16 码元处理，非 BMP 字符与普通 UTF-8 不同；直接 Go 实现需要兼容性测试。

优先实现电脑辅助程序：在程序自建的临时浏览器上下文打开官方登录页，由用户输入账号密码并完成验证码；成功后只导出该上下文内雨课堂允许域的 Cookie。保留域、路径、HttpOnly、Secure 和绝对过期信息，不读取既有浏览器配置，不保存密码或验证码。随后通过短时、单次配对通道交给 DueRadar，设备独立验证身份并保存验证后的最新会话。

此流程需要实现配对、上传、过期、取消及扫码／密码流程相互替代；复用现有登录代次和凭据事务保护。配对密钥不进入 URL 或日志，目标实例须固定，传输使用 HTTPS 或明确受信的私网通道。普通导航成功不能代替独立身份验证；登录和任务采集分别报告。

不直接将验证码 SDK 加入管理网页：当前证据尚未证明官方验证码允许其他域名、跨设备 IP 或不同 Cookie 上下文复用。辅助程序把官方页面和验证留在同一浏览器会话，也保留现有管理页面 CSP。

## 知到共享课

已找到之前缺失的公开公共请求模块。课程请求会先将 `hike-teaching-center-gateway.polymas.com/gateway` 改写到 `cloudapi.polymas.com`，再进入签名判断；改写后的云端请求保留普通参数，使用现有 Polymas token。共享课课程列表为：

```text
GET https://cloudapi.polymas.com/teachingCenterAi/course/queryShareCourseInfo
status=0&pageNo=1&pageSize=999
```

成功响应使用 `code == 200` 和 `data.courseOpenDtos`。缺失数组、鉴权失败和明确空数组必须区分。官方此处仅示范第一页，不足以证明 999 条以内就是完整快照；课程时间也不能当成任务截止时间。

公开考试列表使用另一套带 Cookie 的表单请求。`studentexam-api.zhihuishu.com/studentExam/gateway/t/v1/` 下的 `exam/getSingleRecruit` 返回课程信息，`student/getStudentHomework` 使用 `{recruitId,courseId,flag,pageSize:100,pageNum:0}`；`flag=1`、`2` 分别对应未提交与已提交列表。响应字段为 `status`、`rt.studentHomeworkList` 和 `rt.next`，`next=-1` 是结束标志。非终止分页的含义仍需确认。

线上、机房和线下考试元数据在 `taurusexam-api.zhihuishu.com/taurusExam/gateway/t/v1/` 下分别读取。任务客户端通过 `labc(3)` 获得包装密钥，`yxyz(params,key)` 生成 `secretStr` 后表单编码；这一实现和考试服务会话仍需独立验证，不能直接套用课程列表的普通 token 请求。

下一步应先完成公共包装算法的本地复现和课程发现，再实现提交／未提交列表的完整合并。共享课章节分类、视频进度／截止、分页完整性、时区以及真实提交变化仍待样本。当前账号没有共享课，保持共享课能力未验收，不把空课程列表当作完整任务适配器。
