import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertTriangle,
  ArrowLeft,
  Check,
  ChevronDown,
  Copy,
  Mail,
  Send,
  Sparkles,
  UserCircle2,
} from 'lucide-react'
import PageHeader from './PageHeader'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '../api'
import type { AccountRow, InviteResult } from '../types'
import { getErrorMessage } from '../utils/error'
import { useToast } from '../hooks/useToast'

interface Props {
  accounts: AccountRow[]
  onClose: () => void
}

const MAX_EMAILS = 10
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const SPLIT_RE = /[,;\r\n\t ]+/

interface ParsedEmails {
  valid: string[]
  invalid: string[]
  duplicates: number
}

// 与后端 collectInviteEmails 保持一致：按分隔符切分、去重（忽略大小写）、正则校验。
function parseEmails(text: string): ParsedEmails {
  const tokens = text.split(SPLIT_RE).map((s) => s.trim()).filter(Boolean)
  const seen = new Set<string>()
  const valid: string[] = []
  const invalid: string[] = []
  let duplicates = 0
  for (const tk of tokens) {
    if (!EMAIL_RE.test(tk)) {
      invalid.push(tk)
      continue
    }
    const key = tk.toLowerCase()
    if (seen.has(key)) {
      duplicates++
      continue
    }
    seen.add(key)
    valid.push(tk)
  }
  return { valid, invalid, duplicates }
}

function accountDisplayName(account: AccountRow): string {
  return account.email || account.name || `#${account.id}`
}

function accountSearchText(account: AccountRow): string {
  return [
    String(account.id),
    `#${account.id}`,
    account.email,
    account.name,
    account.status,
    account.plan_type,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase()
}

function resolveAccountInput(accounts: AccountRow[], input: string): AccountRow | null {
  const normalized = input.trim().toLowerCase()
  if (!normalized) return null
  return accounts.find((account) => {
    const id = String(account.id)
    const name = account.name?.trim().toLowerCase()
    const normalizedNameWithID = normalized.replace(/\s+#(?=\d+$)/, '#')
    return (
      normalized === id ||
      normalized === `#${id}` ||
      normalized === account.email?.trim().toLowerCase() ||
      normalized === name ||
      (Boolean(name) && normalizedNameWithID === `${name}#${id}`)
    )
  }) ?? null
}

// CodexInviteView 是账号管理页内的「Codex 邀请」视图，入口与回收站一致。
export default function CodexInviteView({ accounts, onClose }: Props) {
  const { t } = useTranslation()
  const { showToast } = useToast()

  // 仅可用 Codex OAuth 账号发送邀请（中转 / AT-only 账号没有可用于 referral 的凭证）。
  const codexAccounts = useMemo(
    () => accounts.filter((a) => !a.openai_responses_api && !a.at_only),
    [accounts],
  )
  const firstAccount = codexAccounts[0] ?? null

  const [accountId, setAccountId] = useState<number | null>(firstAccount?.id ?? null)
  const [accountQuery, setAccountQuery] = useState(() => firstAccount ? accountDisplayName(firstAccount) : '')
  const [accountOpen, setAccountOpen] = useState(false)
  // accountTyping 区分「用户正在输入搜索」与「输入框只是回显已选账号」。仅在输入时
  // 才按文本过滤，否则展开下拉应显示全部账号（否则会被已选账号的邮箱过滤成只剩一条）。
  const [accountTyping, setAccountTyping] = useState(false)
  const [emailsText, setEmailsText] = useState('')
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [proxyUrl, setProxyUrl] = useState('')
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<InviteResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const accountPickerRef = useRef<HTMLDivElement>(null)

  const parsed = useMemo(() => parseEmails(emailsText), [emailsText])
  const selectedAccount = useMemo(
    () => codexAccounts.find((a) => a.id === accountId) ?? null,
    [codexAccounts, accountId],
  )
  const filteredAccounts = useMemo(() => {
    // 未在输入搜索时显示全部；正在输入才按文本过滤。
    if (!accountTyping) return codexAccounts
    const query = accountQuery.trim().toLowerCase()
    if (!query) return codexAccounts
    return codexAccounts.filter((account) => accountSearchText(account).includes(query))
  }, [accountTyping, accountQuery, codexAccounts])
  const overLimit = parsed.valid.length > MAX_EMAILS
  const canSend =
    !sending && accountQuery.trim() !== '' && parsed.valid.length > 0 && !overLimit

  useEffect(() => {
    if (accountId == null) return
    if (codexAccounts.some((a) => a.id === accountId)) return
    setAccountId(null)
    setAccountQuery('')
  }, [accountId, codexAccounts])

  useEffect(() => {
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target
      if (target instanceof Node && accountPickerRef.current?.contains(target)) return
      setAccountOpen(false)
    }
    document.addEventListener('pointerdown', handlePointerDown)
    return () => document.removeEventListener('pointerdown', handlePointerDown)
  }, [])

  const handleSend = async () => {
    const accountInput = accountQuery.trim()
    if (!accountInput) {
      setError(t('invite.noAccountSelected'))
      return
    }
    const account = selectedAccount ?? resolveAccountInput(codexAccounts, accountInput)
    if (!account) {
      setError(t('invite.accountNotFound'))
      showToast(t('invite.accountNotFound'), 'error')
      return
    }
    if (parsed.valid.length === 0) {
      setError(t('invite.noValidEmails'))
      return
    }
    setAccountId(account.id)
    setAccountQuery(accountDisplayName(account))
    setSending(true)
    setError(null)
    setResult(null)
    try {
      const res = await api.sendInvite(account.id, {
        emails: parsed.valid,
        proxy_url: proxyUrl.trim() || undefined,
      })
      setResult(res.result)
      if (res.ok) {
        showToast(t('invite.sendSuccess'), 'success')
      } else {
        showToast(t('invite.sendUpstreamFailed', { code: res.result.status_code }), 'error')
      }
    } catch (err) {
      setError(getErrorMessage(err))
      showToast(t('invite.sendFailed', { error: getErrorMessage(err) }), 'error')
    } finally {
      setSending(false)
    }
  }

  return (
    <div>
      <PageHeader
        title={t('invite.title')}
        description={t('invite.description')}
        actions={
          <div className="flex flex-wrap items-center justify-end gap-1.5">
            <Button variant="outline" onClick={onClose} className="max-sm:w-full">
              <ArrowLeft className="size-3.5" />
              {t('invite.back')}
            </Button>
          </div>
        }
      />

      <div className="mx-auto mt-4 max-w-2xl space-y-5">
        {codexAccounts.length === 0 ? (
          <EmptyState message={t('invite.noCodexAccounts')} />
        ) : (
          <div className="rounded-2xl border bg-card shadow-sm">
            {/* 账号选择 */}
            <div className="border-b p-5">
              <div className="mb-2 flex items-center gap-2">
                <UserCircle2 className="size-4 text-muted-foreground" />
                <label className="text-sm font-semibold">{t('invite.accountLabel')}</label>
              </div>
              <div ref={accountPickerRef} className="relative">
                <div className="relative">
                  <Input
                    value={accountQuery}
                    onFocus={() => { setAccountOpen(true); setAccountTyping(false) }}
                    onClick={() => { setAccountOpen(true); setAccountTyping(false) }}
                    onChange={(e) => {
                      const next = e.target.value
                      setAccountQuery(next)
                      setAccountOpen(true)
                      setAccountTyping(true)
                      setAccountId(resolveAccountInput(codexAccounts, next)?.id ?? null)
                      if (error === t('invite.accountNotFound')) setError(null)
                    }}
                    placeholder={t('invite.accountPlaceholder')}
                    role="combobox"
                    aria-expanded={accountOpen}
                    aria-controls="codex-invite-account-list"
                    className="h-10 pr-9"
                  />
                  <button
                    type="button"
                    onClick={() => { setAccountOpen((open) => !open); setAccountTyping(false) }}
                    className="absolute inset-y-0 right-0 inline-flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                    aria-label={t('invite.accountToggle')}
                  >
                    <ChevronDown className={`size-4 transition-transform ${accountOpen ? 'rotate-180' : ''}`} />
                  </button>
                </div>
                {accountOpen && (
                  <div
                    id="codex-invite-account-list"
                    role="listbox"
                    className="absolute z-30 mt-1.5 max-h-72 w-full overflow-auto rounded-lg border bg-popover p-1 text-popover-foreground shadow-lg"
                  >
                    {filteredAccounts.length > 0 ? (
                      filteredAccounts.map((account) => {
                        const active = account.id === accountId
                        return (
                          <button
                            key={account.id}
                            type="button"
                            role="option"
                            aria-selected={active}
                            onMouseDown={(event) => event.preventDefault()}
                            onClick={() => {
                              setAccountId(account.id)
                              setAccountQuery(accountDisplayName(account))
                              setAccountOpen(false)
                              setAccountTyping(false)
                              setError(null)
                            }}
                            className={`flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm transition-colors ${
                              active ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/70 hover:text-accent-foreground'
                            }`}
                          >
                            <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-muted text-[11px] font-semibold text-muted-foreground">
                              #{account.id}
                            </span>
                            <span className="min-w-0 flex-1">
                              <span className="block truncate font-medium">{accountDisplayName(account)}</span>
                              <span className="block truncate text-xs text-muted-foreground">
                                {[account.name && account.name !== account.email ? account.name : '', account.plan_type, account.status]
                                  .filter(Boolean)
                                  .join(' · ') || '-'}
                              </span>
                            </span>
                            {active && <Check className="size-4 shrink-0 text-primary" />}
                          </button>
                        )
                      })
                    ) : (
                      <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                        {t('invite.noAccountMatches')}
                      </div>
                    )}
                  </div>
                )}
              </div>
              {selectedAccount && (
                <div className="mt-2 flex flex-wrap items-center gap-1.5">
                  {selectedAccount.plan_type && (
                    <InfoPill label={t('invite.planLabel')} value={selectedAccount.plan_type} />
                  )}
                  <InfoPill label={t('invite.statusLabel')} value={selectedAccount.status || '-'} />
                </div>
              )}
              <p className="mt-2 text-xs text-muted-foreground">{t('invite.accountHint')}</p>
            </div>

            {/* 邮箱输入 */}
            <div className="p-5">
              <div className="mb-2 flex items-center gap-2">
                <Mail className="size-4 text-muted-foreground" />
                <label className="text-sm font-semibold">{t('invite.emailsLabel')}</label>
              </div>
              <textarea
                value={emailsText}
                onChange={(e) => setEmailsText(e.target.value)}
                rows={6}
                placeholder={t('invite.emailsPlaceholder')}
                className="w-full resize-y rounded-lg border bg-background px-3 py-2 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
              />

              {/* 实时解析统计 */}
              {(parsed.valid.length > 0 || parsed.invalid.length > 0 || parsed.duplicates > 0) && (
                <div className="mt-2 flex flex-wrap items-center gap-1.5">
                  <CountPill tone="success" text={t('invite.parsedValid', { count: parsed.valid.length })} />
                  {parsed.duplicates > 0 && (
                    <CountPill tone="muted" text={t('invite.parsedDuplicate', { count: parsed.duplicates })} />
                  )}
                  {parsed.invalid.length > 0 && (
                    <CountPill tone="danger" text={t('invite.parsedInvalid', { count: parsed.invalid.length })} />
                  )}
                </div>
              )}
              {parsed.invalid.length > 0 && (
                <p className="mt-1.5 break-all text-xs text-red-500">
                  {t('invite.invalidList')} {parsed.invalid.join(', ')}
                </p>
              )}
              {overLimit && (
                <p className="mt-1.5 flex items-center gap-1 text-xs text-amber-600">
                  <AlertTriangle className="size-3.5" />
                  {t('invite.overLimit', { max: MAX_EMAILS })}
                </p>
              )}
              {!overLimit && parsed.invalid.length === 0 && (
                <p className="mt-1.5 text-xs text-muted-foreground">{t('invite.emailsHint')}</p>
              )}

              {/* 高级选项 */}
              <button
                type="button"
                onClick={() => setShowAdvanced((v) => !v)}
                className="mt-4 inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
              >
                <ChevronDown className={`size-3.5 transition-transform ${showAdvanced ? 'rotate-180' : ''}`} />
                {t('invite.advanced')}
              </button>
              {showAdvanced && (
                <div className="mt-3 rounded-xl border bg-muted/30 p-3">
                  <label className="mb-1 block text-xs font-medium text-muted-foreground">
                    {t('invite.proxyLabel')}
                  </label>
                  <input
                    value={proxyUrl}
                    onChange={(e) => setProxyUrl(e.target.value)}
                    placeholder={t('invite.proxyPlaceholder')}
                    className="h-9 w-full rounded-lg border bg-background px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
                  />
                </div>
              )}

              {error && <div className="mt-3 text-sm text-red-500">{error}</div>}

              <div className="mt-4 flex justify-end">
                <Button disabled={!canSend} onClick={() => void handleSend()}>
                  <Send className="size-3.5" />
                  {sending
                    ? t('invite.sending')
                    : parsed.valid.length > 0
                      ? t('invite.sendCount', { count: parsed.valid.length })
                      : t('invite.send')}
                </Button>
              </div>
            </div>
          </div>
        )}

        {result && <InviteResultCard result={result} />}
      </div>
    </div>
  )
}

function InviteResultCard({ result }: { result: InviteResult }) {
  const { t } = useTranslation()
  const [showRaw, setShowRaw] = useState(false)
  const rawText =
    result.upstream != null
      ? JSON.stringify(result.upstream, null, 2)
      : result.upstream_raw || ''

  return (
    <div className="rounded-2xl border bg-card shadow-sm">
      <div className="flex items-center gap-2 border-b p-5">
        <div className={`flex size-9 items-center justify-center rounded-xl ${result.ok ? 'bg-emerald-500/10 text-emerald-600' : 'bg-red-500/10 text-red-600'}`}>
          {result.ok ? <Check className="size-4" /> : <AlertTriangle className="size-4" />}
        </div>
        <div className="min-w-0">
          <h4 className="text-base font-semibold">{t('invite.resultTitle')}</h4>
          <p className="text-xs text-muted-foreground">
            {result.ok
              ? t('invite.resultOkDesc', { count: result.emails.length })
              : t('invite.resultFailed', { code: result.status_code })}
          </p>
        </div>
        {result.request_id && (
          <span className="ml-auto hidden rounded-full bg-muted px-2.5 py-1 font-mono text-[11px] text-muted-foreground sm:inline">
            {result.request_id}
          </span>
        )}
      </div>

      <div className="space-y-3 p-5">
        {/* 无资格的友好提示 */}
        {!result.ok && result.status_code === 403 && (
          <div className="flex items-start gap-2 rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <Sparkles className="mt-0.5 size-4 shrink-0" />
            <span>{t('invite.eligibilityHint')}</span>
          </div>
        )}

        {/* 邀请明细 */}
        {result.invites && result.invites.length > 0 && (
          <div className="space-y-2">
            {result.invites.map((inv, i) => (
              <div
                key={inv.referral_id || inv.email || i}
                className="flex items-center justify-between gap-3 rounded-xl border bg-background px-3 py-2.5"
              >
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium text-foreground">{inv.email || '-'}</div>
                  {inv.invite_url && (
                    <a
                      href={inv.invite_url}
                      target="_blank"
                      rel="noreferrer"
                      className="block truncate text-xs text-primary hover:underline"
                    >
                      {inv.invite_url}
                    </a>
                  )}
                </div>
                {inv.invite_url && <CopyButton text={inv.invite_url} />}
              </div>
            ))}
          </div>
        )}

        {/* 原始响应（折叠） */}
        {rawText && (
          <div>
            <button
              type="button"
              onClick={() => setShowRaw((v) => !v)}
              className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
            >
              <ChevronDown className={`size-3.5 transition-transform ${showRaw ? 'rotate-180' : ''}`} />
              {t('invite.rawResponse')}
            </button>
            {showRaw && (
              <pre className="mt-2 max-h-64 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs">
                {rawText}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function CopyButton({ text }: { text: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      /* 忽略剪贴板权限错误 */
    }
  }
  return (
    <button
      type="button"
      onClick={() => void handleCopy()}
      title={copied ? t('invite.copied') : t('invite.copy')}
      className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg border bg-background text-muted-foreground transition-colors hover:text-foreground"
    >
      {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
    </button>
  )
}

function EmptyState({ message }: { message: string }) {
  return (
    <div className="flex flex-col items-center justify-center rounded-2xl border border-dashed bg-card py-16 text-center">
      <div className="mb-3 flex size-12 items-center justify-center rounded-2xl bg-muted text-muted-foreground">
        <Mail className="size-6" />
      </div>
      <p className="text-sm text-muted-foreground">{message}</p>
    </div>
  )
}

function InfoPill({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1 rounded-full bg-muted/60 px-2.5 py-1 text-xs text-muted-foreground">
      <span className="text-muted-foreground/70">{label}</span>
      <span className="font-medium text-foreground">{value}</span>
    </span>
  )
}

function CountPill({ tone, text }: { tone: 'success' | 'danger' | 'muted'; text: string }) {
  const cls =
    tone === 'success'
      ? 'bg-emerald-500/10 text-emerald-600'
      : tone === 'danger'
        ? 'bg-red-500/10 text-red-600'
        : 'bg-muted text-muted-foreground'
  return <span className={`inline-flex items-center rounded-full px-2.5 py-1 text-xs font-semibold ${cls}`}>{text}</span>
}
