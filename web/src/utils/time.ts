// 后端返回的时间都是 UTC（...Z），new Date() 解析后按浏览器本地时区显示

const pad = (n: number) => String(n).padStart(2, '0')

function isSameDay(a: Date, b: Date) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate()
}

// formatTime 列表里用的相对时间：今天 14:05 / 昨天 14:05 / 3月8日 14:05 / 2025年3月8日
export function formatTime(iso: string): string {
  const d = new Date(iso)
  const now = new Date()
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`
  if (isSameDay(d, now)) return hm

  const yesterday = new Date(now)
  yesterday.setDate(now.getDate() - 1)
  if (isSameDay(d, yesterday)) return `昨天 ${hm}`

  if (d.getFullYear() === now.getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日 ${hm}`
  return formatDate(iso)
}

// formatDate 只显示日期：2026年11月21日
export function formatDate(iso: string): string {
  const d = new Date(iso)
  return `${d.getFullYear()}年${d.getMonth() + 1}月${d.getDate()}日`
}

// formatClock 只显示时刻：14:05（消息悬停时显示）
export function formatClock(iso: string): string {
  const d = new Date(iso)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}
