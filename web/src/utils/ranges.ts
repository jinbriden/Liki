// 已同步的 seq 区间（FR-17、FR-18）。
//
// 一个区间 [lo, hi] 表示"这个会话里 seq 在 lo..hi 之间的消息，本地已经和服务端对齐过"：
// 区间里本地有的消息就是全部；本地缺的 seq 只可能是被删除的消息（M6），不是没拉到。
// 不同区间之间的空白就是缺口，翻到那里时要去服务端补。
//
// 为什么不直接看 seq 是否连续：M6 加入逐条删除后，用户看到的 seq 本来就会跳号，
// 光看 seq 分不清"删掉了"和"没拉到"。区间记录的是"拉过哪一段"，和删没删无关。

export type Range = readonly [lo: number, hi: number] // 闭区间，lo <= hi

// addRange 加入一段区间，和重叠或相邻（如 [1,5] 与 [6,9]）的区间合并。返回新数组，按 lo 升序
export function addRange(ranges: readonly Range[], lo: number, hi: number): Range[] {
  if (lo > hi) return [...ranges]
  const out: Range[] = []
  let cur: [number, number] = [lo, hi]
  for (const r of ranges) {
    if (r[1] + 1 < cur[0] || cur[1] + 1 < r[0]) {
      out.push(r) // 不相交也不相邻
    } else {
      cur = [Math.min(cur[0], r[0]), Math.max(cur[1], r[1])]
    }
  }
  out.push(cur)
  return out.sort((a, b) => a[0] - b[0])
}

// findRange 包含 seq 的区间
export function findRange(ranges: readonly Range[], seq: number): Range | undefined {
  return ranges.find((r) => r[0] <= seq && seq <= r[1])
}

// topRange 最新的一段区间（hi 最大）
export function topRange(ranges: readonly Range[]): Range | undefined {
  let top: Range | undefined
  for (const r of ranges) if (!top || r[1] > top[1]) top = r
  return top
}
