const { test } = require("node:test");
const assert = require("node:assert/strict");
const { select, paginate, safeSource, timeGroup, semester, scopeTasks } = require("./tasks.js");
const now = Date.parse("2026-10-22T10:00:00+08:00");
const fixture = [
  { id:"old", status:"pending", kind:"homework", platform:"rain", title:"Old work", due_at:"2026-09-01T00:00:00+08:00" },
  { id:"future", status:"pending", kind:"exam", platform:"rain", title:"Exam", due_at:"2026-10-23T00:00:00+08:00" },
  { id:"video", status:"pending", kind:"video", platform:"zhidao_campus", title:"Practice", due_at:null },
  { id:"done", status:"completed", kind:"homework", platform:"rain", title:"Submitted", due_at:"2026-10-24T00:00:00+08:00" },
  { id:"unknown", status:"unknown", kind:"exam", platform:"rain", title:"Unknown", due_at:"invalid" },
];
test("active upcoming work precedes old overdue work without changing platform status", () => {
  assert.deepEqual(select(fixture, {now}).map(t=>t.id), ["future","video","unknown","old","done"]);
  assert.equal(fixture[0].status,"pending");
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
  const start = (value) => semester(Date.parse(value)).start;
  assert.equal(start("2026-09-01T12:00:00+08:00"), new Date(2026,8,1).getTime());
  assert.equal(start("2026-08-31T12:00:00+08:00"), new Date(2026,1,15).getTime());
  assert.equal(start("2027-01-10T12:00:00+08:00"), new Date(2026,8,1).getTime());
  assert.equal(start("2027-02-20T12:00:00+08:00"), new Date(2027,1,15).getTime());
});
