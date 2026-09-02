import { ETA_UNKNOWN } from './api/types'

export function bytes(n: number | undefined): string {
  if (n === undefined || n < 0) return '—'
  if (n < 1024) return `${n} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`
}

export function rate(bps: number | undefined): string {
  if (!bps || bps <= 0) return '—'
  return `${bytes(bps)}/s`
}

/** eta_sec is -1 when the engine cannot compute one yet; that is not zero. */
export function eta(seconds: number | undefined): string {
  if (seconds === undefined || seconds === ETA_UNKNOWN || seconds < 0) return 'estimating…'
  return duration(seconds)
}

export function duration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${s % 60}s`
  const h = Math.floor(m / 60)
  return `${h}h ${m % 60}m`
}

export function timestamp(iso: string | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

export function percent(done: number, total: number): number {
  if (total <= 0) return 0
  return Math.min(100, Math.round((done / total) * 100))
}
