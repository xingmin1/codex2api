import { useTranslation } from "react-i18next";
import type {
  AccountFirstTokenStats,
  AccountFirstTokenWindowStats,
} from "../types";
import { formatBeijingTime } from "../utils/time";

interface AccountFirstTokenStatsViewProps {
  stats?: AccountFirstTokenStats;
  variant?: "table" | "detail";
}

function formatFirstTokenDuration(milliseconds: number): string {
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) return "—";
  if (milliseconds < 1000) return `${Math.round(milliseconds)}ms`;
  return `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 1 : 0)}s`;
}

/** 紧凑显示一个首字统计窗口。 */
function FirstTokenWindowLine({
  label,
  stats,
  detailed,
}: {
  label: string;
  stats?: AccountFirstTokenWindowStats;
  detailed: boolean;
}) {
  const { t } = useTranslation();
  const title = stats?.last_sample_at
    ? t("accounts.firstTokenLastSample", {
        time: formatBeijingTime(stats.last_sample_at),
      })
    : t("accounts.firstTokenNoSamples");

  if (!stats || stats.sample_count <= 0) {
    return (
      <div
        className="flex items-center justify-between gap-3 text-muted-foreground"
        title={title}
      >
        <span className="font-medium">{label}</span>
        <span>—</span>
      </div>
    );
  }

  if (detailed) {
    return (
      <div
        className="grid grid-cols-[auto_1fr] items-center gap-x-3 gap-y-0.5 border-b border-border/60 py-2 last:border-b-0"
        title={title}
      >
        <span className="text-[11px] font-semibold text-muted-foreground">
          {label}
        </span>
        <div className="grid grid-cols-3 gap-2 text-right text-[12px] tabular-nums">
          <span>
            <span className="text-muted-foreground">{t("accounts.firstTokenAverageShort")} </span>
            {formatFirstTokenDuration(stats.average_ms)}
          </span>
          <span>
            <span className="text-muted-foreground">{t("accounts.firstTokenMaximumShort")} </span>
            {formatFirstTokenDuration(stats.maximum_ms)}
          </span>
          <span className="text-muted-foreground">
            {t("accounts.firstTokenSampleCount", {
              count: stats.sample_count,
            })}
          </span>
        </div>
      </div>
    );
  }

  return (
    <div className="whitespace-nowrap tabular-nums" title={title}>
      <span className="mr-1 font-medium text-muted-foreground">{label}</span>
      <span>{t("accounts.firstTokenAverageShort")} {formatFirstTokenDuration(stats.average_ms)}</span>
      <span className="text-muted-foreground"> · </span>
      <span>{t("accounts.firstTokenMaximumShort")} {formatFirstTokenDuration(stats.maximum_ms)}</span>
      <span className="text-muted-foreground"> · {stats.sample_count}</span>
    </div>
  );
}

/** 按表格或详情样式展示账号的 10 分钟和 1 小时首字统计。 */
export default function AccountFirstTokenStatsView({
  stats,
  variant = "table",
}: AccountFirstTokenStatsViewProps) {
  const { t } = useTranslation();
  const detailed = variant === "detail";

  return (
    <div
      className={
        detailed
          ? "rounded-xl border border-border bg-card px-3"
          : "space-y-0.5 text-[11px] leading-tight"
      }
    >
      <FirstTokenWindowLine
        label={t("accounts.firstTokenShortWindow")}
        stats={stats?.short}
        detailed={detailed}
      />
      <FirstTokenWindowLine
        label={t("accounts.firstTokenLongWindow")}
        stats={stats?.long}
        detailed={detailed}
      />
    </div>
  );
}
