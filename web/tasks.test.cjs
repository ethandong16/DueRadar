const { test } = require("node:test");
const assert = require("node:assert/strict");
const { select, paginate, safeSource, timeGroup, semester, scopeTasks, ageLabel, leadLabel, nextReminder, parseTime, deadline } = require("./tasks.js");
const now = Date.parse("2026-10-22T10:00:00+08:00");
const fixture = [
  { id:"old", status:"pending", kind:"homework", platform:"rain", title:"Old work", due_at:"2026-09-01T00:00:00+08:00" },
  { id:"future", status:"pending", kind:"exam", platform:"rain", title:"Exam", due_at:"2026-10-23T00:00:00+08:00" },
  { id:"video", status:"pending", kind:"video", platform:"zhidao_campus", title:"Practice", due_at:null },
  { id:"done", status:"completed", kind:"homework", platform:"rain", title:"Submitted", due_at:"2026-10-24T00:00:00+08:00" },
  { id:"unknown", status:"unknown", kind:"exam", platform:"rain", title:"Unknown", due_at:"invalid" },
];
test("overdue work comes first, then upcoming work without changing platform status", () => {
  assert.deepEqual(select(fixture, {now}).map(t=>t.id), ["old","future","video","unknown","done"]);
  assert.equal(fixture[0].status,"pending");
});
test("dismissed tasks stay hidden unless the explicit filter is selected", () => {
  const dismissed = [{...fixture[0], dismissed_at:"2026-10-01T00:00:00+08:00"}, ...fixture.slice(1)];
  assert.deepEqual(select(dismissed,{now}).map(t=>t.id), ["future","video","unknown","done"]);
  assert.deepEqual(select(dismissed,{now,dismissed:"only"}).map(t=>t.id), ["old"]);
  assert.deepEqual(select(dismissed,{now,dismissed:"include"}).map(t=>t.id), ["old","future","video","unknown","done"]);
});
test("all filters compose and missing or invalid deadlines remain unknown", () => {
  assert.deepEqual(select(fixture, {now,status:"pending",kind:"video",timing:"no_due",query:"智慧",platformLabel:()=>"智慧树"}).map(t=>t.id), ["video"]);
  assert.equal(timeGroup(fixture[4],now),"no_due");
  assert.equal(select(fixture,{now,platform:"rain",timing:"overdue"}).length,1);
});
test("paging covers all tasks once and clamps after a shrinking snapshot", () => {
  const tasks = Array.from({length:173},(_,id)=>({id}));
  const ids = Array.from({length:6},(_,i)=>paginate(tasks,i+1).items).flat().map(t=>t.id);
  assert.equal(ids.length,173);
  assert.equal(new Set(ids).size,173);
  assert.equal(paginate(tasks.slice(0,8),6).page,1);
  assert.deepEqual(paginate([],9),{page:1,pages:1,total:0,start:0,end:0,items:[]});
});
test("Go nanosecond timestamps parse in browsers that accept milliseconds only", () => {
  assert.equal(parseTime("2026-11-02T02:22:52.4902583+08:00"), Date.parse("2026-11-02T02:22:52.490+08:00"));
  assert.equal(parseTime("2026-11-02T02:22:52.4902583Z"), Date.parse("2026-11-02T02:22:52.490Z"));
  assert.equal(deadline({ due_at: "2026-11-02T02:22:52.4902583+08:00" }), Date.parse("2026-11-02T02:22:52.490+08:00"));
  assert.equal(parseTime("2026-11-02T02:22:52.49Z"), Date.parse("2026-11-02T02:22:52.49Z"));
  assert.ok(Number.isNaN(parseTime(null)));
});
test("sync freshness and reminder leads use readable units", () => {
  assert.equal(ageLabel(now - 20 * 1000, now), "刚刚");
  assert.equal(ageLabel(now - 5 * 60000, now), "5 分钟前");
  assert.equal(ageLabel(now - 3 * 3600000, now), "3 小时前");
  assert.equal(ageLabel(new Date(now - 2 * 86400000), now), "2 天前");
  assert.equal(ageLabel("invalid", now), null);
  assert.equal(leadLabel(1440), "1 天");
  assert.equal(leadLabel(4320), "3 天");
  assert.equal(leadLabel(180), "3 小时");
  assert.equal(leadLabel(30), "30 分钟");
  assert.equal(leadLabel(0), "");
});
test("the overview previews the earliest queued scheduled reminder only", () => {
  const events = [
    { task_id: "t1", kind: "new_task", status: "pending", due_at: "2026-10-22T09:55:00+08:00" },
    { task_id: "t1", kind: "deadline", status: "sent", due_at: "2026-10-22T09:56:00+08:00", minutes_before: 1440 },
    { task_id: "t1", kind: "deadline", status: "pending", due_at: "2026-10-23T09:56:00+08:00", minutes_before: 180 },
    { task_id: "t1", kind: "exam_start", status: "sending", due_at: "2026-10-23T09:00:00+08:00", minutes_before: 60 },
    { task_id: "t2", kind: "deadline", status: "pending", due_at: "2026-10-22T09:00:00+08:00", minutes_before: 30 },
  ];
  assert.deepEqual(nextReminder(events, "t1"), { at: Date.parse("2026-10-23T09:00:00+08:00"), minutes: 60, kind: "exam_start" });
  assert.equal(nextReminder(events, "missing"), null);
  assert.equal(nextReminder(null, "t1"), null);
});
test("task links allow HTTPS and reject script, file and credential URLs", () => {
  assert.equal(safeSource("https://www.yuketang.cn/v2/web/index"),"https://www.yuketang.cn/v2/web/index");
  for(const value of ["javascript:alert(1)","file:///secret","https://user:pass@example.com","/relative",null]) assert.equal(safeSource(value),null);
});
const termFixture = [
  { id:"old-hw", status:"completed", kind:"homework", platform:"zhidao_campus", course:"2025-2026-2高等数学", title:"Old homework", due_at:"2026-06-01T23:59:00+08:00" },
  { id:"old-video", status:"completed", kind:"video", platform:"zhidao_campus", course:"2025-2026-2高等数学", title:"Old video", due_at:null },
  { id:"new-hw", status:"pending", kind:"homework", platform:"zhidao_campus", course:"概率论与数理统计B-202609", title:"New homework", due_at:"2026-11-08T23:30:00+08:00" },
  { id:"new-video", status:"pending", kind:"video", platform:"zhidao_campus", course:"概率论与数理统计B-202609", title:"New video", due_at:null },
  { id:"undated", status:"pending", kind:"exam", platform:"rain", course:"毛泽东思想", title:"Undated exam", due_at:null },
  { id:"named-old", status:"pending", kind:"video", platform:"zhidao_campus", course:"2024-2025-1大学物理", title:"Old named video", due_at:null },
  { id:"retake", status:"pending", kind:"homework", platform:"zhidao_campus", course:"2025-2026-2线性代数", title:"Current retake", due_at:"2026-10-30T23:59:00+08:00" },
];
test("last semester courses and their undated videos are hidden by default", () => {
  assert.deepEqual(scopeTasks(termFixture,{now}).map(t=>t.id).sort(), ["new-hw","new-video","retake","undated"]);
  assert.deepEqual(select(termFixture,{now,kind:"video"}).map(t=>t.id), ["new-video"]);
  assert.equal(scopeTasks(termFixture,{now,scope:"all"}).length, termFixture.length);
});
test("dates outrank course names and unknown courses stay visible", () => {
  assert.ok(scopeTasks(termFixture,{now}).some(t=>t.id==="retake"));
  assert.ok(scopeTasks(termFixture,{now}).some(t=>t.id==="undated"));
  assert.equal(scopeTasks([{ id:"empty", platform:"rain", course:"无日期课程", status:"pending", kind:"video" }],{now}).length,1);
});
test("server semester flags take precedence over local heuristics", () => {
  const flagged = [
    { id:"server-old", platform:"rain", course:"临时课程", status:"pending", kind:"video", historical:true },
    { id:"server-new", platform:"rain", course:"2024-2025-1旧名课程", status:"pending", kind:"video", historical:false, due_at:"2026-12-01T00:00:00+08:00" },
  ];
  assert.deepEqual(scopeTasks(flagged,{now}).map(t=>t.id), ["server-new"]);
  assert.deepEqual(select(flagged,{now,kind:"video"}).map(t=>t.id), ["server-new"]);
});
test("academic terms roll over in September and February", () => {
  const start = (value) => new Date(semester(Date.parse(value), "Asia/Shanghai").start).toISOString();
  assert.equal(start("2026-09-01T12:00:00+08:00"), "2026-08-31T16:00:00.000Z");
  assert.equal(start("2026-08-31T12:00:00+08:00"), "2026-02-14T16:00:00.000Z");
  assert.equal(start("2027-01-10T12:00:00+08:00"), "2026-08-31T16:00:00.000Z");
  assert.equal(start("2027-02-20T12:00:00+08:00"), "2027-02-14T16:00:00.000Z");
});
test("semester boundaries follow the configured timezone, not the host clock", () => {
  const stamp = Date.parse("2026-10-22T10:00:00+08:00");
  assert.equal(new Date(semester(stamp, "Asia/Shanghai").start).toISOString(), "2026-08-31T16:00:00.000Z");
  assert.equal(new Date(semester(stamp, "UTC").start).toISOString(), "2026-09-01T00:00:00.000Z");
  // A deadline at the first minute of the term belongs to the current semester.
  const boundary = [{ id: "term", status: "pending", kind: "homework", platform: "rain", title: "Term start", due_at: "2026-09-01T00:00:00+08:00" }];
  assert.equal(scopeTasks(boundary, { now: stamp, timezone: "Asia/Shanghai" }).length, 1);
});
