#!/usr/bin/env python3
"""Print the per-pair summary of a run_corpus127.sh log (sizes, wall, RSS)."""
import re, sys
for path in sys.argv[1:]:
    print(f"## {path}")
    pair = None; step = None; rows = {}
    for line in open(path):
        line = line.rstrip()
        m = re.match(r'=== PAIR (\S+) -> (\S+)', line)
        if m:
            pair = (m.group(1).split('/')[-2] + '/' + m.group(1).split('/')[-1], m.group(2).split('/')[-1]); rows[pair] = {}; continue
        m = re.match(r'--- (.*)', line)
        if m: step = m.group(1); continue
        if pair is None: continue
        r = rows[pair]
        m = re.match(r'SIZE (\w+)=(\d+)', line)
        if m: r['size ' + m.group(1)] = int(m.group(2))
        m = re.match(r'PATCH \S+: (\d+) B = header (\d+) \+ layout (\d+) \+ stage1\(\w\) (\d+)(?: \(1a (\d+) \+ 1b (\d+)\))? \+ stage2\((\w)\) (\d+)', line)
        if m: r['patch ' + m.group(7)] = tuple(int(x) for x in m.groups()[:4]) + (int(m.group(8)),) + ((int(m.group(5)), int(m.group(6))) if m.group(5) else ())
        m = re.match(r'TIME_WALL=([\d.]+) TIME_USER=([\d.]+) TIME_SYS=([\d.]+) MAXRSS_KB=(\d+)', line)
        if m: r['time ' + step] = (float(m.group(1)), int(m.group(4)) // 1024)
        m = re.match(r'\s*stage-2 differing bytes by section: (.*)', line)
        if m and 'residual' not in r: r['residual'] = m.group(1)
        if line.endswith(' OK'): r.setdefault('ok', []).append(line)
    for pair, r in rows.items():
        print(f"### {pair[0]} -> {pair[1]}")
        for k in sorted(r): print(f"  {k}: {r[k]}")
