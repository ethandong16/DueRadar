# 长期运行观察

`scripts/observe.py` 在 Linux 设备上独立记录应用进程的内存、CPU 累计时间、重启次数、两平台最近同步时间和任务／事件数量。它只读取状态，不修改任务，不发送通知，也不记录课程名称、任务标题、账号身份或凭据。需要 Python 3 和 systemd。

默认 97 次采样、间隔 900 秒，覆盖约 24 小时。启动后可以关闭辅助电脑，观察进程继续在设备上运行。输出以独占方式创建，已有报告不会被覆盖；失败样本会记录错误类型并继续下一次采样。

```sh
sudo install -m 755 scripts/observe.py /opt/dueradar/observe.py
sudo systemd-run --unit=dueradar-observe \
  --property=MemoryMax=32M --property=Nice=10 \
  /usr/bin/python3 /opt/dueradar/observe.py \
  --output /var/log/dueradar-observation.jsonl

systemctl status dueradar-observe
sudo python3 /opt/dueradar/observe.py \
  --output /var/log/dueradar-observation.jsonl --summarize
```

每次新观察使用新的单元名与输出路径。采样器自身的资源不计入应用 RSS；`cpu_seconds` 是应用进程累计 CPU 时间，需除以相邻样本的实际间隔才能计算平均 CPU 占用。进程 PID 或启动标记任一变化时视为新进程。

验收时检查实际覆盖时长、失败样本、进程身份变化、两平台 `last_success_at` 是否持续推进，以及 RSS 是否持续增长。`systemctl active` 只证明服务进程运行，不能替代同步或手机到达验证。观察期间升级、重启或设备重启均应记录；设备重启会终止此临时观察服务，需要另开一段观察，不拼接成未经说明的连续 24 小时。

汇总中的 `covers_24_hours` 只表示首尾样本跨度达到 24 小时；还要核对 `max_sample_gap_seconds`、`errors` 和 `process_identities`。短时测试即使采够 97 次也不算一天。失败采样不会被误计为另一个进程；`rss_change_kib` 仅在观察到同一个进程时提供，表示末次与首次 RSS 之差，不代表期间峰值或内存泄漏结论。

`platform_sync` 按平台统计同步情况：`success_timestamps` 是观察到的不同成功时间数量，`failed_samples` 是发现同步失败或登录失效的样本数，`stale_samples` 表示成功数据已超过 30 分钟。`max_success_age_seconds` 给出最久未更新时长；`missing_success_samples` 和 `revalidation_samples` 分别记录缺少成功时间与等待重新核验。尚未绑定的平台单独记入 `unbound_samples`。这些统计来自离散采样，不等同于平台请求成功率。

汇总可以在开发电脑上对复制的 JSONL 运行；无需替换或重启正在设备上采样的脚本。合成报告回归测试使用 `python -m unittest discover -s scripts -p '*_test.py'`。

停止观察不会停止 DueRadar：

```sh
sudo systemctl stop dueradar-observe
```

保留 JSONL 报告即可继续汇总。短时冒烟测试可以使用 `--samples 2 --interval 2` 和单独的输出路径。
