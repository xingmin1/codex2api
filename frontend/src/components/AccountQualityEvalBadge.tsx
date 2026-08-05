import { useTranslation } from 'react-i18next'
import { FlaskConical } from 'lucide-react'
import type { AccountRow, QualityEvalStatus } from '../types'
import { cn } from '@/lib/utils'
import { accountSupportsQualityEval } from '../lib/accountCapabilities'

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
  const supported = accountSupportsQualityEval(account)
  const statusLabel = batch
    ? t(`accounts.qualityEvalStatus.${batch.status}`, { defaultValue: batch.status })
    : ''
  const candyScore = batch?.candy_requested
    ? `${batch.candy_correct}/${batch.candy_requested}`
    : ''
  const content = batch ? (
    <>
      <span className="size-1.5 rounded-full bg-current" />
      <span>{statusLabel}{candyScore ? ` ${candyScore}` : ''}</span>
    </>
  ) : supported ? (
    <>
      <FlaskConical className="size-3" />
      <span>{t('accounts.qualityEvalBadge')}</span>
    </>
  ) : (
    <>
      <FlaskConical className="size-3" />
      <span>{t('accounts.qualityEvalUnsupportedBadge')}</span>
    </>
  )
  const title = batch
    ? [
        `5.6 Max · ${statusLabel}${candyScore ? ` · Candy ${candyScore}` : ''}`,
        batch.latest_juice_value ? `Juice ${batch.latest_juice_value}` : '',
        t('accounts.qualityEvalReasoningSummary', {
          average: batch.reasoning_tokens_average.toFixed(1),
          maximum: batch.reasoning_tokens_maximum,
        }),
        `${batch.trigger_source === 'auto' ? t('accounts.qualityEvalSourceAuto') : t('accounts.qualityEvalSourceManual')} · ${new Date(batch.created_at).toLocaleString()}`,
        batch.error_message || '',
      ].filter(Boolean).join('\n')
    : supported
      ? t('accounts.qualityEvalNotRun')
      : t('accounts.qualityEvalUnsupported')
  const classes = cn(
    'inline-flex h-6 items-center gap-1 rounded-md px-1.5 text-[10px] font-semibold ring-1 ring-inset',
    batch
      ? statusClasses[batch.status] ?? statusClasses.incomplete
      : supported
        ? 'bg-violet-500/10 text-violet-700 ring-violet-500/20 hover:bg-violet-500/15 dark:text-violet-300'
        : 'bg-slate-500/10 text-slate-500 ring-slate-500/20 dark:text-slate-400',
    className,
  )

  if (onClick && (supported || batch)) {
    return <button type="button" className={classes} title={title} onClick={onClick}>{content}</button>
  }
  return <span className={classes} title={title}>{content}</span>
}
