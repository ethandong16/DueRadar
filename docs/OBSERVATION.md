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

每次新观察使用新的单元名与输出路径。采样器自身的资源不计入应用 RSS；`cpu_seconds` 是应用进程累计 CPU 时间，需除以相邻样本的实际间隔才能计算平均 CPU 占用。进程 PID 和启动标记同时变化时视为新进程。

验收时检查实际覆盖时长、失败样本、进程身份变化、两平台 `last_success_at` 是否持续推进，以及 RSS 是否持续增长。`systemctl active` 只证明服务进程运行，不能替代同步或手机到达验证。观察期间升级、重启或设备重启均应记录；设备重启会终止此临时观察服务，需要另开一段观察，不拼接成未经说明的连续 24 小时。

停止观察不会停止 DueRadar：

```sh
sudo systemctl stop dueradar-observe
```

保留 JSONL 报告即可继续汇总。短时冒烟测试可以使用 `--samples 2 --interval 2` 和单独的输出路径。
