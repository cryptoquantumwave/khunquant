import { useQuery, useQueryClient } from "@tanstack/react-query"
import { useTranslation } from "react-i18next"

import {
  type AutonomyResponse,
  type DialSetting,
  getAutonomy,
  putAutonomy,
} from "@/api/proactive-dashboard"
import {
  ManageQuery,
  WebOwnerNotice,
} from "@/components/proactive/manage-shared"
import { useManageAction } from "@/components/proactive/manage-utils"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"

// The dial only tightens what the assistant does on its own. There is no setting that
// does more than today, so there is no "act" here, and the gateway refuses it too.
const SETTINGS: DialSetting[] = ["default", "ask", "notify", "off"]
const RANK: Record<string, number> = { default: 0, ask: 1, notify: 2, off: 3 }
const TIERS = ["T1", "T2", "T3"]
const AUTONOMY_KEY = ["proactive", "autonomy"] as const

export function AutonomySection() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const query = useQuery({
    queryKey: AUTONOMY_KEY,
    queryFn: getAutonomy,
    refetchInterval: 10_000,
  })
  const save = useManageAction(
    ({ tier, setting }: { tier: string; setting: DialSetting }) =>
      putAutonomy({ [tier]: setting }),
  )

  return (
    <Card className="border-border/60">
      <CardHeader>
        <CardTitle className="text-base">
          {t("pages.agent.proactive.autonomy.title")}
        </CardTitle>
        <CardDescription>
          {t("pages.agent.proactive.autonomy.description")}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <ManageQuery query={query}>
          {(data) => (
            <div className="space-y-4">
              <PolicyNote data={data} />
              {!data.web_owner_active && (
                <WebOwnerNotice missing={data.web_owner_missing ?? ""} />
              )}
              {(data.tiers ?? [])
                .filter((tier) => TIERS.includes(tier.tier))
                .map((tier) => (
                  <div
                    key={tier.tier}
                    className="space-y-2 rounded-lg border p-3"
                  >
                    <div>
                      <p className="text-sm font-medium">
                        {t(`pages.agent.proactive.autonomy.tiers.${tier.tier}`)}
                      </p>
                      <p className="text-muted-foreground text-xs">
                        {t(
                          `pages.agent.proactive.autonomy.tier_about.${tier.tier}`,
                        )}
                      </p>
                    </div>
                    <ul className="text-muted-foreground list-disc pl-5 text-xs">
                      {tier.columns.map((c) => (
                        <li key={c.column}>
                          {t("pages.agent.proactive.autonomy.default_for", {
                            who: t(
                              `pages.agent.proactive.autonomy.columns.${c.column}`,
                              {
                                defaultValue: c.label,
                              },
                            ),
                            what: t(
                              `pages.agent.proactive.autonomy.modes.${c.default}`,
                              {
                                defaultValue: c.default_text,
                              },
                            ),
                          })}
                        </li>
                      ))}
                    </ul>
                    <div
                      className="grid gap-2 sm:grid-cols-2"
                      role="radiogroup"
                    >
                      {SETTINGS.map((s) => {
                        const current = tier.setting === s
                        const loosens =
                          (RANK[s] ?? 0) < (RANK[tier.setting] ?? 0)
                        const blocked = loosens && !data.web_owner_active
                        return (
                          <Button
                            key={s}
                            role="radio"
                            aria-checked={current}
                            variant={current ? "secondary" : "outline"}
                            className="h-auto flex-col items-start gap-0.5 py-2 text-left whitespace-normal"
                            disabled={blocked || save.isPending}
                            title={blocked ? data.web_owner_missing : undefined}
                            onClick={() => {
                              if (!current) {
                                save.mutate(
                                  { tier: tier.tier, setting: s },
                                  {
                                    onSettled: () =>
                                      void queryClient.invalidateQueries({
                                        queryKey: AUTONOMY_KEY,
                                      }),
                                  },
                                )
                              }
                            }}
                          >
                            <span className="text-sm font-medium">
                              {t(
                                `pages.agent.proactive.autonomy.settings.${s}`,
                              )}
                            </span>
                            <span className="text-muted-foreground text-xs font-normal">
                              {t(
                                `pages.agent.proactive.autonomy.setting_about.${s}`,
                              )}
                            </span>
                            {blocked && (
                              <span className="text-xs font-normal text-amber-700 dark:text-amber-400">
                                {t(
                                  "pages.agent.proactive.autonomy.needs_owner",
                                  {
                                    reason: data.web_owner_missing ?? "",
                                  },
                                )}
                              </span>
                            )}
                          </Button>
                        )
                      })}
                    </div>
                  </div>
                ))}
            </div>
          )}
        </ManageQuery>
      </CardContent>
    </Card>
  )
}

// PolicyNote says when the dial takes effect: in the policy's live mode it applies to
// tool calls; mandated buys (T2) obey it always.
function PolicyNote({ data }: { data: AutonomyResponse }) {
  const { t } = useTranslation()
  const mode = data.policy_mode ?? "off"
  return (
    <p className="text-muted-foreground text-xs">
      {t("pages.agent.proactive.autonomy.policy_note", {
        mode: t(`pages.agent.proactive.autonomy.policy_modes.${mode}`, {
          defaultValue: mode,
        }),
      })}
    </p>
  )
}
