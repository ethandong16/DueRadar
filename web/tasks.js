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
    const { status = "all", platform = "all", kind = "all", timing = "all", query = "", now = Date.now(), platformLabel = (id) => id } = options;
    const search = query.trim().toLocaleLowerCase();
    return tasks.filter((task) =>
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
  return { deadline, timeGroup, select, paginate, safeSource };
});
