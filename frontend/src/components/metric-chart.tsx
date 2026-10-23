"use client";
import { useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { bytes } from "@/lib/api";
import { getLocale, t as translate } from "@/lib/i18n";
export type MetricSample = {
  timestamp: string;
  cpu_percent: number | null;
  memory: { percent: number } | null;
  disk: { percent: number } | null;
  load_average?: number[];
  network?: { interface: string; receive_bytes_per_second: number | null; transmit_bytes_per_second: number | null }[];
};
const percent = (value: unknown) => typeof value === "number" && Number.isFinite(value) ? `${value.toFixed(1)}%` : "—";
const sampleDate = (value?: string) => value && Number.isFinite(Date.parse(value)) ? new Intl.DateTimeFormat(getLocale(), { dateStyle: "medium", timeStyle: "medium" }).format(new Date(value)) : "—";
const rate = (value: unknown) => typeof value === "number" && Number.isFinite(value) && value >= 0 ? `${bytes(value)}/s` : "—";
export function MetricChart({ samples, interval }: { samples: MetricSample[]; interval: number }) {
  const [selected, setSelected] = useState<string | null>(null);
  const found = selected === null ? -1 : samples.findIndex(sample => sample.timestamp === selected);
  const currentIndex = found < 0 ? samples.length - 1 : found;
  const current = samples[currentIndex];
  const times = samples.map(sample => Date.parse(sample.timestamp));
  const validTimes = times.filter(Number.isFinite);
  const start = validTimes[0] ?? NaN, end = validTimes.at(-1) ?? NaN;
  const span = Math.max(1, end - start);
  const intervals = times.slice(1).map((time, index) => time - times[index]).filter(value => Number.isFinite(value) && value > 0).sort((a, b) => a - b);
  const gap = Math.max(interval * 1000, intervals[Math.floor(intervals.length / 2)] ?? 0) * 3;
  const series = [
    { label: "CPU", color: "var(--accent)", values: samples.map(sample => sample.cpu_percent) },
    { label: translate("内存"), color: "var(--success)", values: samples.map(sample => sample.memory?.percent ?? null) },
    { label: translate("磁盘"), color: "var(--warning)", values: samples.map(sample => sample.disk?.percent ?? null) },
  ];
  const timeLabel = (stamp: number) => Number.isFinite(stamp) ? new Intl.DateTimeFormat(getLocale(), span > 86400000 ? { month: "short", day: "numeric", hour: "2-digit" } : { hour: "2-digit", minute: "2-digit", ...(span < 300000 ? { second: "2-digit" as const } : {}) }).format(new Date(stamp)) : "—";
  return (
    <section className="metric-chart" aria-label={translate("主机历史指标")}>
      <div className="chart-plot">
        <div className="chart-y" aria-hidden="true">{[100, 75, 50, 25, 0].map(value => <span key={value}>{value}%</span>)}</div>
        <div className="metric-history">
          <svg viewBox="0 0 700 140" preserveAspectRatio="none" aria-hidden="true" onPointerMove={event => {
            const rect = event.currentTarget.getBoundingClientRect();
            const target = start + Math.max(0, Math.min(1, (event.clientX - rect.left) / Math.max(1, rect.width))) * span;
            let closest = currentIndex;
            times.forEach((time, index) => { if (Number.isFinite(time) && (!Number.isFinite(times[closest]) || Math.abs(time - target) < Math.abs(times[closest] - target))) closest = index; });
            setSelected(samples[closest]?.timestamp ?? null);
          }}>
            {[0, 25, 50, 75, 100].map(value => <line key={value} x1="0" x2="700" y1={140 - value * 1.4} y2={140 - value * 1.4} stroke="var(--border)" strokeDasharray="4 4" />)}
            {series.map(item => {
              let path = "", segment = false;
              item.values.forEach((value, index) => {
                if (typeof value !== "number" || !Number.isFinite(value) || !Number.isFinite(times[index])) { segment = false; return; }
                if (index > 0 && times[index] - times[index - 1] > gap) segment = false;
                path += `${segment ? "L" : "M"}${(times[index] - start) / span * 700},${140 - Math.min(100, Math.max(0, value)) * 1.4} `;
                segment = true;
              });
              const value = item.values[currentIndex];
              return <g key={item.label}>
                <path d={path} fill="none" stroke={item.color} strokeWidth="2" />
                {typeof value === "number" && Number.isFinite(value) && Number.isFinite(times[currentIndex]) && <circle cx={(times[currentIndex] - start) / span * 700} cy={140 - Math.max(0, Math.min(100, value)) * 1.4} r="3" fill={item.color} />}
              </g>;
            })}
            {Number.isFinite(times[currentIndex]) && <line x1={(times[currentIndex] - start) / span * 700} x2={(times[currentIndex] - start) / span * 700} y1="0" y2="140" stroke="var(--muted)" strokeDasharray="3 4" />}
          </svg>
        </div>
        <div className="chart-x" aria-hidden="true"><span>{timeLabel(start)}</span><span>{timeLabel(start + span / 2)}</span><span>{timeLabel(end)}</span></div>
      </div>
      <label className="chart-scrubber">{translate("查看时间点")}
        <input type="range" min={0} max={Math.max(0, samples.length - 1)} value={currentIndex} onChange={event => setSelected(samples[Number(event.target.value)]?.timestamp ?? null)} aria-valuetext={[sampleDate(current?.timestamp), ...series.map(item => `${item.label} ${percent(item.values[currentIndex])}`)].join(", ")} />
      </label>
      <div className="chart-readout">
        <time dateTime={current?.timestamp}>{sampleDate(current?.timestamp)}</time>
        {series.map(item => <span key={item.label} style={{ color: item.color }}>{item.label} <strong>{percent(item.values[currentIndex])}</strong></span>)}
        {selected !== null && <Button label={translate("最新数据")} variant="ghost" onClick={() => setSelected(null)} />}
      </div>
      {!!current?.load_average?.length && <p className="muted">{translate("负载（1 / 5 / 15 分钟）")}：{current.load_average.map(value => Number.isFinite(value) ? value.toFixed(2) : "—").join(" / ")}</p>}
      {!!current?.network?.length && <div className="network-readout">{current.network.map(network => <div key={network.interface}><strong>{network.interface}</strong><span>{translate("接收")} {rate(network.receive_bytes_per_second)}</span><span>{translate("发送")} {rate(network.transmit_bytes_per_second)}</span></div>)}</div>}
      <details className="technical-details"><summary>{translate("采样详情")}</summary><p>{translate("{0} 个样本，采样间隔 {1} 秒；缺失时段不补造数据。", [samples.length, interval])}</p></details>
    </section>
  );
}
