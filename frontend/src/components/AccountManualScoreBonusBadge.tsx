import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import type { AccountRow } from "../types";

interface AccountManualScoreBonusBadgeProps {
  account: AccountRow;
  onClick: () => void;
  showEmpty?: boolean;
  className?: string;
}

/** 显示会随绝对到期时间自动消失的账号临时调度分徽标。 */
export default function AccountManualScoreBonusBadge({
  account,
  onClick,
  showEmpty = false,
  className = "",
}: AccountManualScoreBonusBadgeProps) {
  const { t } = useTranslation();
  const expiresAt = useMemo(() => {
    const parsed = Date.parse(account.manual_score_bonus_until ?? "");
    return Number.isFinite(parsed) ? parsed : 0;
  }, [account.manual_score_bonus_until]);
  const [now, setNow] = useState(Date.now());
  const bonus = account.manual_score_bonus ?? 0;
  const active = bonus !== 0 && expiresAt > now;

  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, [active]);

  if (!active && !showEmpty) return null;
  const remainingMinutes = Math.max(1, Math.ceil((expiresAt - now) / 60_000));

  return (
    <button
      type="button"
      onClick={(event) => {
        event.stopPropagation();
        onClick();
      }}
      className={`inline-flex items-center rounded-md bg-violet-500/10 px-1.5 py-0.5 text-[11px] font-semibold tabular-nums text-violet-700 transition-colors hover:bg-violet-500/15 dark:text-violet-300 ${className}`}
      title={t("accounts.manualScoreBonusTitle")}
    >
      {active
        ? t("accounts.manualScoreBonusActive", {
            bonus: bonus > 0 ? `+${bonus}` : String(bonus),
            minutes: remainingMinutes,
          })
        : t("accounts.manualScoreBonus")}
    </button>
  );
}
