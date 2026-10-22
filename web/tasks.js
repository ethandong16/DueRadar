/* Shared task-list behavior; usable in the browser and Node regression tests. */
(function (root, factory) {
  const behavior = factory();
  if (typeof module === "object" && module.exports) module.exports = behavior;
  else root.DueRadarTasks = behavior;
})(globalThis, function () {
  "use strict";
  function deadline(task) {
    const value = task.due_at ?? task.end_at;
    const date = value ? Date.parse(value) : NaN;
    return Number.isFinite(date) ? date : null;
  }
  // Academic year starts in September; February through August still belong to
  // the spring term until the new academic year begins.
  function semester(now) {
    const date = new Date(now);
    const year = date.getFullYear();
    const month = date.getMonth();
    if (month === 0) return { start: new Date(year - 1, 8, 1).getTime(), key: (year - 1) * 2 };
    if (month >= 8) return { start: new Date(year, 8, 1).getTime(), key: year * 2 };
    return { start: new Date(year, 1, 15).getTime(), key: (year - 1) * 2 + 1 };
  }
  const termPattern = /(20\d{2})\s*-\s*(20\d{2})\s*-\s*([12])/;
  function courseKey(task) {
    return `${task.platform || ""}\u0000${task.course || `#${task.id || ""}`}`;
  }
  // A course belongs to an earlier semester when every dated task is before this
  // term, or, for courses without deadlines (videos), when its name carries an
  // older academic term. Undated, unlabelled courses stay visible.
  function scopeInfo(tasks, now = Date.now()) {
    const current = semester(now);
    const groups = new Map();
    for (const task of tasks) {
      const key = courseKey(task);
      let group = groups.get(key);
      if (!group) groups.set(key, group = { course: task.course || "", dates: [], historical: false });
      const due = deadline(task);
      if (due !== null) group.dates.push(due);
    }
    for (const group of groups.values()) {
      if (group.dates.some((value) => value >= current.start)) continue;
      const named = termPattern.exec(group.course);
      const namedKey = named ? Number(named[1]) * 2 + (Number(named[3]) - 1) : null;
      group.historical = group.dates.length > 0 || (namedKey !== null && namedKey < current.key);
    }
    return { start: current.start, key: current.key, groups };
  }
  function scopeTasks(tasks, options = {}) {
    const { scope = "current", now = Date.now() } = options;
    if (scope === "all") return tasks.slice();
    const info = scopeInfo(tasks, now);
    // The server classifies the semester once for both the list and reminders;
    // fall back to the local heuristic for snapshots that predate the flag.
    return tasks.filter((task) => typeof task.historical === "boolean" ? !task.historical : !info.groups.get(courseKey(task)).historical);
  }
  function timeGroup(task, now) {
    const due = deadline(task);
    return due === null ? "no_due" : due <= now ? "overdue" : "upcoming";
  }
  function rank(task, now) {
    if (task.status === "completed") return 4;
    if (task.status !== "pending") return 2;
    const group = timeGroup(task, now);
    return group === "upcoming" ? 0 : group === "no_due" ? 1 : 3;
  }
  function select(tasks, options = {}) {
    const { status = "all", platform = "all", kind = "all", timing = "all", query = "", scope = "current", now = Date.now(), platformLabel = (id) => id } = options;
    const search = query.trim().toLocaleLowerCase();
    return scopeTasks(tasks, { scope, now }).filter((task) =>
      (status === "all" || task.status === status) &&
      (platform === "all" || task.platform === platform) &&
      (kind === "all" || task.kind === kind) &&
      (timing === "all" || timeGroup(task, now) === timing) &&
      (!search || `${task.title || ""} ${task.course || ""} ${platformLabel(task.platform) || ""}`.toLocaleLowerCase().includes(search))
    ).sort((a, b) => {
      const priority = rank(a, now) - rank(b, now);
      if (priority) return priority;
      const left = deadline(a), right = deadline(b);
      if (left !== right) {
        if (left === null) return 1;
        if (right === null) return -1;
        return rank(a, now) >= 3 ? right - left : left - right;
      }
      return String(a.id).localeCompare(String(b.id));
    });
  }
  function paginate(tasks, page = 1, size = 30) {
    const pages = Math.max(1, Math.ceil(tasks.length / size));
    const current = Math.max(1, Math.min(pages, page));
    const start = (current - 1) * size;
    return { page: current, pages, total: tasks.length, start: tasks.length ? start + 1 : 0, end: Math.min(start + size, tasks.length), items: tasks.slice(start, start + size) };
  }
  function safeSource(value) {
    try {
      const url = new URL(value);
      return url.protocol === "https:" && !url.username && !url.password ? url.href : null;
    } catch { return null; }
  }
  return { deadline, timeGroup, select, paginate, safeSource, semester, scopeInfo, scopeTasks };
});
