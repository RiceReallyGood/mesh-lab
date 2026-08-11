#!/usr/bin/env python3
"""把 bench-matrix.sh 的逐格输出汇成跨格对照表。

用法：  matrix-report.py <矩阵输出目录>

为什么单独写一个而不是让 merge 干：merge 的职责是**单次运行**的归因，
它连「多次运行」这个概念都没有，也不该有 —— 跨运行比较涉及参数是否可比、
链路是否饱和这类判断，那是分析层的事，塞进 merge 会把时钟纪律那套逻辑搞脏。
"""
import os
import re
import sys

# summary.txt 里端到端分解的行：  标签  avg  占比  p50  p90  p99
ROW = re.compile(r'^\s{2}(\S.*?)\s{2,}(-?[\d.]+[a-zµm]+)\s+(-?[\d.]+)%\s+(-?[\d.]+[a-zµm]+)')
E2E = re.compile(r'^\s{2}端到端（client 总时长）\s+(-?[\d.]+[a-zµm]+)\s+(-?[\d.]+[a-zµm]+)')
# detail.txt 里的段：  标签  avg=..  p50=..
DET = re.compile(r'^\s+(\S.*?)\s+avg=(-?[\d.]+[a-zµmn]+)\s+p50=(-?[\d.]+[a-zµmn]+).*?\(n=(\d+)\)')

UNIT = {'ns': 1e-3, 'µs': 1.0, 'us': 1.0, 'ms': 1e3, 's': 1e6}


def us(s):
    """把 '3.2µs' 这类读数统一成微秒浮点。"""
    m = re.match(r'(-?[\d.]+)\s*([a-zµ]+)', s)
    if not m:
        return None
    return float(m.group(1)) * UNIT.get(m.group(2), 1.0)


def load_cells(d):
    cells = {}
    for f in sorted(os.listdir(d)):
        if not f.endswith('.summary.txt'):
            continue
        tag = f[:-len('.summary.txt')]
        s = {'rows': {}, 'detail': {}}
        for line in open(os.path.join(d, f), encoding='utf-8', errors='replace'):
            m = E2E.match(line)
            if m:
                s['e2e_avg'], s['e2e_p50'] = us(m.group(1)), us(m.group(2))
                continue
            m = ROW.match(line)
            if m and '端到端' not in m.group(1) and '合计' not in m.group(1):
                s['rows'][m.group(1).strip()] = (us(m.group(2)), float(m.group(3)))
        dp = os.path.join(d, tag + '.detail.txt')
        if os.path.exists(dp):
            node = ''
            for line in open(dp, encoding='utf-8', errors='replace'):
                if line.startswith('── '):
                    node = line.split()[1]
                    continue
                m = DET.match(line)
                if m:
                    # 段名带树形缩进，剥掉再做键
                    name = m.group(1).strip(' │├└─')
                    s['detail'][f'{node}|{name}'] = (us(m.group(2)), us(m.group(3)), int(m.group(4)))
        cells[tag] = s
    return cells


def order(tags):
    def key(t):
        m = re.match(r'c(\d+)-(\d+)([KM]?)', t)
        if not m:
            return (99, 99)
        mult = {'': 1, 'K': 1024, 'M': 1024 * 1024}[m.group(3)]
        return (int(m.group(1)), int(m.group(2)) * mult)
    return sorted(tags, key=key)


def table(title, headers, rows):
    print(f'\n### {title}\n')
    print('| ' + ' | '.join(headers) + ' |')
    print('|' + '|'.join(['---'] * len(headers)) + '|')
    for r in rows:
        print('| ' + ' | '.join(r) + ' |')


def fmt(v):
    if v is None:
        return '—'
    neg = '-' if v < 0 else ''
    v = abs(v)
    if v >= 1000:
        return f'{neg}{v/1000:.2f}ms'
    if v >= 10:
        return f'{neg}{v:.1f}µs'
    if v >= 1:
        return f'{neg}{v:.2f}µs'
    return f'{neg}{v*1000:.0f}ns'


def main():
    d = sys.argv[1]
    cells = load_cells(d)
    tags = order(cells)
    if not tags:
        print('没有找到任何 *.summary.txt', file=sys.stderr)
        return 1

    seg = ['kitex-client 自身', 'UDS往返 kitex-client↔envoy-out', 'envoy-out 自身',
           '跨机往返 envoy-out↔envoy-in', 'envoy-in 自身',
           'UDS往返 envoy-in↔kitex-server', 'kitex-server 自身']

    table('端到端分解（avg，各段相加 = 端到端）',
          ['段'] + tags,
          [[s] + [fmt(cells[t]['rows'].get(s, (None,))[0]) for t in tags] for s in seg]
          + [['**端到端**'] + [f"**{fmt(cells[t].get('e2e_avg'))}**" for t in tags]])

    table('各段占比（%）',
          ['段'] + tags,
          [[s] + [(f"{cells[t]['rows'][s][1]:.1f}" if s in cells[t]['rows'] else '—') for t in tags]
           for s in seg])

    keys = [('kitex-client|★writev系统调用', 'client writev'),
            ('kitex-client|★goroutine调度延迟', 'client 调度延迟'),
            ('envoy-out|★上游writev系统调用', 'envoy-out 上游 writev'),
            ('envoy-out|下游readv系统调用', 'envoy-out 下游 readv'),
            ('envoy-out|★等待上游(对端+网络)', 'envoy-out 纯等待'),
            ('envoy-out|★事件循环排队', 'envoy-out 上游事件循环排队'),
            ('envoy-in|★上游writev系统调用', 'envoy-in 上游 writev'),
            ('envoy-in|★事件循环排队', 'envoy-in 上游事件循环排队'),
            ('kitex-server|★writev系统调用', 'server writev'),
            ('kitex-server|★goroutine调度延迟', 'server 调度延迟'),
            ('kitex-server|业务handler', 'server 业务 handler')]
    table('关键分段（avg）',
          ['段'] + tags,
          [[label] + [fmt(cells[t]['detail'].get(k, (None,))[0]) for t in tags] for k, label in keys])

    print('\n### 样本数（n）\n')
    print('| 格 | ' + ' | '.join(tags) + ' |')
    print('|' + '|'.join(['---'] * (len(tags) + 1)) + '|')
    ns = []
    for t in tags:
        vals = {v[2] for v in cells[t]['detail'].values()}
        ns.append(str(max(vals)) if vals else '—')
    print('| trace 数 | ' + ' | '.join(ns) + ' |')
    return 0


if __name__ == '__main__':
    sys.exit(main())
