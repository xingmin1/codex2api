import { useTranslation } from 'react-i18next'
import type { AccountRow, QualityEvalStatus } from '../types'
import { cn } from '@/lib/utils'

const statusClasses: Record<QualityEvalStatus, string> = {
  running: 'bg-blue-500/10 text-blue-700 ring-blue-500/20 dark:text-blue-300',
  normal: 'bg-emerald-500/10 text-emerald-700 ring-emerald-500/20 dark:text-emerald-300',
  suspected: 'bg-amber-500/10 text-amber-700 ring-amber-500/20 dark:text-amber-300',
  degraded: 'bg-red-500/10 text-red-700 ring-red-500/20 dark:text-red-300',
  incomplete: 'bg-slate-500/10 text-slate-700 ring-slate-500/20 dark:text-slate-300',
}

interface AccountQualityEvalBadgeProps {
  account: AccountRow
  className?: string
  onClick?: () => void
}

export default function AccountQualityEvalBadge({ account, className, onClick }: AccountQualityEvalBadgeProps) {
  const { t } = useTranslation()
  const batch = account.latest_quality_eval
  if (!batch) return null

  const label = batch.candy_requested > 0
    ? `${batch.candy_correct}/${batch.candy_requested}`
    : `${batch.juice_correct}/${batch.juice_requested}`
  const content = (
    <>
      <span className="size-1.5 rounded-full bg-current" />
      <span>5.6 Max {label}</span>
    </>
  )
  const title = t(`accounts.qualityEvalStatus.${batch.status}`, {
    defaultValue: batch.status,
  })
  const classes = cn(
    'inline-flex h-6 items-center gap-1 rounded-md px-1.5 text-[10px] font-semibold ring-1 ring-inset',
    statusClasses[batch.status] ?? statusClasses.incomplete,
    className,
  )

  if (onClick) {
    return <button type="button" className={classes} title={title} onClick={onClick}>{content}</button>
  }
  return <span className={classes} title={title}>{content}</span>
}
