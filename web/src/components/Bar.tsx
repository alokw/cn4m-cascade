import { percent } from '../format'

/** A progress bar. Shared rather than retyped: the markup is trivial but the
 *  ARIA attributes are the part that quietly rots when there are two copies —
 *  a bar that renders correctly and announces nothing looks fine. */
export function Bar({ done, total }: { done: number; total: number }) {
  const pct = percent(done, total)
  return (
    <div className="bar" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
      <div className="bar-fill" style={{ width: `${pct}%` }} />
    </div>
  )
}
