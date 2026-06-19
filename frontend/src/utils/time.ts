import i18n from '../i18n'

export interface RelativeTimeOptions {
  variant?: 'long' | 'compact'
  includeSeconds?: boolean
  fallback?: string
}

/** 按当前语言输出相对时间，例如“3 分钟前”。 */
export function formatRelativeTime(dateStr?: string | null, options: RelativeTimeOptions = {}): string {
  const {
    variant = 'long',
    includeSeconds = false,
    fallback = '-',
  } = options

  if (!dateStr) {
    return fallback
  }

  const timestamp = new Date(dateStr).getTime()
  if (Number.isNaN(timestamp)) {
    return fallback
  }

  const diff = Math.max(0, Date.now() - timestamp)
  const seconds = Math.floor(diff / 1000)

  if (includeSeconds && seconds < 60) {
    return variant === 'compact'
      ? i18n.t('common.secondsAgoCompact', { count: seconds })
      : i18n.t('common.secondsAgoLong', { count: seconds })
  }

  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) {
    return i18n.t('common.justNow')
  }

  if (minutes < 60) {
    return variant === 'compact'
      ? i18n.t('common.minutesAgoCompact', { count: minutes })
      : i18n.t('common.minutesAgoLong', { count: minutes })
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return variant === 'compact'
      ? i18n.t('common.hoursAgoCompact', { count: hours })
      : i18n.t('common.hoursAgoLong', { count: hours })
  }

  const days = Math.floor(hours / 24)
  return variant === 'compact'
    ? i18n.t('common.daysAgoCompact', { count: days })
    : i18n.t('common.daysAgoLong', { count: days })
}

const TIMEZONE_STORAGE_KEY = 'codex2api_timezone'
const DEFAULT_TIMEZONE = 'Asia/Shanghai'

type DateInput = string | number | Date | null | undefined

/** 用户选择时区下的日期时间部件，所有字段都已补齐到两位数。 */
export interface ZonedDateTimeParts {
  year: string
  month: string
  day: string
  hour: string
  minute: string
  second: string
}

/** 获取用户选择的时区，默认 Asia/Shanghai */
export function getTimezone(): string {
  try {
    return localStorage.getItem(TIMEZONE_STORAGE_KEY) || DEFAULT_TIMEZONE
  } catch {
    return DEFAULT_TIMEZONE
  }
}

/** 设置时区并持久化到 localStorage */
export function setTimezone(tz: string): void {
  try {
    localStorage.setItem(TIMEZONE_STORAGE_KEY, tz)
  } catch { /* 忽略 */ }
}

/** 按用户选择的时区拆分日期时间，避免图表标签落回浏览器本地时区。 */
export function getZonedDateTimeParts(value: DateInput): ZonedDateTimeParts | null {
  const date = parseDateInput(value)
  if (!date) return null

  const formatter = new Intl.DateTimeFormat('en-CA', {
    timeZone: getTimezone(),
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  })
  const values = Object.fromEntries(formatter.formatToParts(date).map((part) => [part.type, part.value]))
  const year = values.year
  const month = values.month
  const day = values.day
  const hour = values.hour === '24' ? '00' : values.hour
  const minute = values.minute
  const second = values.second

  if (!year || !month || !day || !hour || !minute || !second) return null
  return { year, month, day, hour, minute, second }
}

/** 按用户选择的时区格式化为 HH:mm:ss。 */
export function formatClockTime(value: DateInput, fallback = '--:--:--'): string {
  const parts = getZonedDateTimeParts(value)
  if (!parts) return fallback
  return `${parts.hour}:${parts.minute}:${parts.second}`
}

/** 按用户选择的时区格式化为 HH:mm。 */
export function formatHourMinute(value: DateInput, fallback = '--:--'): string {
  const parts = getZonedDateTimeParts(value)
  if (!parts) return fallback
  return `${parts.hour}:${parts.minute}`
}

/** 返回用户选择时区在指定时刻的 systemd/JS 风格偏移毫秒值。 */
export function getZonedTimezoneOffsetMs(value: DateInput): number | null {
  const date = parseDateInput(value)
  const parts = getZonedDateTimeParts(date)
  if (!date || !parts) return null

  const zonedAsUtc = Date.UTC(
    Number(parts.year),
    Number(parts.month) - 1,
    Number(parts.day),
    Number(parts.hour),
    Number(parts.minute),
    Number(parts.second),
  )
  return date.getTime() - zonedAsUtc
}

/** 按用户选择的时区格式化为本地化日期。 */
export function formatDateOnly(value: DateInput, locale?: string, fallback = '-'): string {
  const date = parseDateInput(value)
  if (!date) return fallback

  return new Intl.DateTimeFormat(locale, {
    timeZone: getTimezone(),
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(date)
}

/**
 * Format a date string with the configured timezone.
 * Output format: YYYY-MM-DD HH:mm:ss
 *
 * 使用 Intl.DateTimeFormat 以用户选择的时区格式化，
 * 无论后端返回的是 UTC（带 Z）还是带时区偏移（+08:00），都能正确显示，
 * 避免手动加减偏移导致的重复转换问题。
 */
export function formatBeijingTime(dateStr?: string | null, fallback = '-'): string {
  const parts = getZonedDateTimeParts(dateStr)
  if (!parts) return fallback
  return `${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}`
}

function parseDateInput(value: DateInput): Date | null {
  if (value === null || value === undefined || value === '') return null
  const date = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(date.getTime())) return null
  return date
}
