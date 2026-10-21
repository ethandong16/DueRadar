const { test } = require("node:test");
const assert = require("node:assert/strict");
const { select, paginate, safeSource, timeGroup } = require("./tasks.js");
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
