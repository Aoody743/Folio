"use client";

import { useEffect, useState } from "react";
import { Button } from "@astryxdesign/core/Button";
import { APIError, Capability, fullDate } from "@/lib/api";
import {
  parseSiteMigrationPlan,
  sameSiteMigrationPlan,
  siteMigrationGate,
  SiteMigrationPlan,
} from "@/lib/site-migration";
import { Site, SoftwareInstance } from "@/lib/resources";
import { Schema } from "@/lib/schema";
import { useResource } from "@/lib/resource-hook";
import { ActionForm, References } from "./actions";

function planGateMessage(reason: ReturnType<typeof siteMigrationGate>): string {
  switch (reason) {
    case "foreign_site":
      return "此计划属于另一网站，不能应用；请为当前网站重新规划。";
    case "site_changed":
      return "网站标识或版本与计划不一致；请重新读取网站并规划。";
    case "expired":
      return "迁移计划已过期；请重新规划后再应用。";
    case "invalid_plan":
      return "迁移计划内容无法验证；请重新规划。";
    default:
      return "";
  }
}

function formatMigrationError(error: APIError): string | undefined {
  if (error.code === "advanced_migration_choice_required")
    return "此网站使用手工配置。若确认放弃手工配置并重新生成，请勾选相应选项后重新规划。";
  if (error.code === "migration_plan_expired")
    return "迁移计划已过期，请重新规划。";
  if (error.code === "revision_conflict" || error.code === "conflict")
    return "网站版本已变化，请重新读取网站并重新规划。";
  return undefined;
}

function MigrationSummary({
  plan,
  webInstances,
}: {
  plan: SiteMigrationPlan;
  webInstances: SoftwareInstance[];
}) {
  const runtimeName = (id: string) => {
    const instance = webInstances.find((item) => item.id === id);
    return instance
      ? `${instance.plugin_id} · ${instance.instance_id}`
      : id;
  };
  return (
    <div className="detail-grid">
      <div>
        <small>网站</small>
        <strong>{plan.site_name}</strong>
        <code>{plan.site_id}</code>
      </div>
      <div>
        <small>网站版本</small>
        <strong>{plan.expected_revision}</strong>
      </div>
      <div>
        <small>运行位置</small>
        <strong>
          {runtimeName(plan.source_web_instance)} → {runtimeName(plan.target_web_instance)}
        </strong>
      </div>
      <div>
        <small>域名</small>
        <strong>{plan.domains.length ? plan.domains.join(" · ") : "未配置域名"}</strong>
      </div>
      <div>
        <small>默认网站</small>
        <strong>{plan.default ? "是" : "否"}</strong>
      </div>
      <div>
        <small>TLS</small>
        <strong>{plan.tls_enabled ? "已启用" : "未启用"}</strong>
        {plan.tls_certificate_id && (
          <code>
            {plan.tls_certificate_id} · 版本 {plan.tls_certificate_revision}
          </code>
        )}
      </div>
      <div>
        <small>PHP实例</small>
        <strong>
          {plan.php_instance ? runtimeName(plan.php_instance) : "未绑定"}
        </strong>
      </div>
      <div>
        <small>配置模式</small>
        <strong>{plan.configuration_mode === "advanced" ? "手工配置" : "托管配置"}</strong>
      </div>
      <div>
        <small>计划有效至</small>
        <strong>{fullDate(plan.expires_at)}</strong>
      </div>
    </div>
  );
}

export function SiteMigration({
  site,
  siteFresh,
  siteError,
  webInstances,
  webInstancesLoaded,
  webInstancesLoading,
  webInstancesError,
  capabilities,
  active,
  onExecuted,
}: {
  site: Site;
  siteFresh: boolean;
  siteError?: string;
  webInstances: SoftwareInstance[];
  webInstancesLoaded: boolean;
  webInstancesLoading: boolean;
  webInstancesError?: string;
  capabilities: Capability[];
  active: boolean;
  onExecuted: () => void;
}) {
  const planCapability = capabilities.find(
    (capability) => capability.name === "sites_migration_plan",
  );
  const getCapability = capabilities.find(
    (capability) => capability.name === "sites_migration_get",
  );
  const applyCapability = capabilities.find(
    (capability) => capability.name === "sites_migration_apply",
  );
  const canListInstances = capabilities.some(
    (capability) => capability.name === "plugins_list",
  );
  const [reviewedPlan, setReviewedPlan] = useState<SiteMigrationPlan>();
  const [planError, setPlanError] = useState("");
  const [planAttempt, setPlanAttempt] = useState(0);
  const [applyStarted, setApplyStarted] = useState(false);
  const [applySubmitted, setApplySubmitted] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const livePlanResource = useResource<unknown>(
    "sites_migration_get",
    { plan_id: reviewedPlan?.id ?? "" },
    !!reviewedPlan && !!getCapability && active,
  );
  const livePlan = parseSiteMigrationPlan(livePlanResource.data);
  const planReason = siteMigrationGate(reviewedPlan, site, now);
  const livePlanReason = siteMigrationGate(livePlan, site, now);
  const plansMatch =
    !!reviewedPlan && !!livePlan && sameSiteMigrationPlan(reviewedPlan, livePlan);
  const siteCanBeUsed = siteFresh && !siteError;
  const targetChoices = webInstances
    .filter(
      (instance) =>
        ["nginx", "openresty"].includes(instance.plugin_id) &&
        instance.id !== site.web_instance &&
        instance.status !== "uninstalled",
    )
    .map((instance) => ({
      value: instance.id,
          label: `${instance.plugin_id === "nginx" ? "Nginx" : "OpenResty"} · ${instance.instance_id}`,
    }));
  const canApply =
    !!reviewedPlan &&
    !!livePlan &&
    plansMatch &&
    !planReason &&
    !livePlanReason &&
    siteCanBeUsed &&
    !livePlanResource.error &&
    !!getCapability &&
    !!applyCapability;

  useEffect(() => {
    if (!reviewedPlan) return;
    const expiresAt = Date.parse(reviewedPlan.expires_at);
    if (!Number.isFinite(expiresAt)) return;
    const timer = window.setTimeout(
      () => setNow(Date.now()),
      Math.max(0, expiresAt - Date.now() + 1),
    );
    return () => window.clearTimeout(timer);
  }, [reviewedPlan]);

  const beginNewPlan = () => {
    setReviewedPlan(undefined);
    setPlanError("");
    setApplyStarted(false);
    setApplySubmitted(false);
    setPlanAttempt((attempt) => attempt + 1);
  };

  const references: References = {
    target_web_instance: targetChoices,
  };
  const rawPlanSchema = (planCapability?.input_schema ?? {}) as Schema;
  const planSchema: Schema = {
    ...rawPlanSchema,
    properties: {
      ...(rawPlanSchema.properties ?? {}),
      expected_revision: {
        ...rawPlanSchema.properties?.expected_revision,
        title: "规划时网站版本",
      },
      reset_advanced_config: {
        ...rawPlanSchema.properties?.reset_advanced_config,
        title: "放弃手工配置并重新生成",
        description:
          site.configuration_mode === "advanced"
            ? "此网站使用手工配置。勾选后迁移会转为托管配置，原配置会留存备份；不勾选则不能规划迁移。"
            : "仅当网站使用手工配置时才可勾选；勾选后原配置会留存备份。",
      },
    },
  };
  const localizedPlanCapability = planCapability
    ? { ...planCapability, input_schema: planSchema as Record<string, unknown> }
    : undefined;
  const staleReason = planReason ?? livePlanReason;

  return (
    <section className="stack site-migration">
      <div className="notice warning">
        <strong>迁移会将此网站的运行配置切换到另一个 Web 实例。</strong>
        <p>
          网站内容、域名、PHP绑定和TLS证书会随站点配置保留；应用前会检查目标实例并验证运行结果。迁移不会自动执行，必须在核对计划后再次确认。
        </p>
      </div>

      {siteError ? (
        <div className="notice danger" role="alert">
          无法读取当前网站版本，暂不能规划或应用迁移。
          <Button
            label="重新读取网站"
            variant="ghost"
            onClick={() => window.dispatchEvent(new Event("powerlinux:invalidate"))}
          />
        </div>
      ) : !siteFresh ? (
        <p className="muted" role="status">正在读取当前网站版本…</p>
      ) : null}

      {localizedPlanCapability && !reviewedPlan && !planError && (
        <div className="panel-card">
          <div className="card-heading">
            <h3>规划迁移</h3>
            <p className="muted">先选择目标 Web 实例并生成只读计划；此步骤不会修改网站。</p>
          </div>
          {!canListInstances ? (
            <div className="notice warning" role="status">
              当前账户没有读取软件实例列表的权限，无法选择迁移目标。
            </div>
          ) : !webInstancesLoaded || webInstancesLoading ? (
            <p className="muted" role="status">正在读取可用 Web 实例…</p>
          ) : webInstancesError ? (
            <div className="notice danger" role="alert">
              无法读取 Web 实例列表，暂不能规划迁移。
              <Button
                label="重新读取实例"
                variant="ghost"
                onClick={() => window.dispatchEvent(new Event("powerlinux:invalidate"))}
              />
            </div>
          ) : !targetChoices.length ? (
            <div className="notice warning" role="status">
              当前没有可选的其他 OpenResty 实例。
            </div>
          ) : !siteCanBeUsed ? null : (
            <ActionForm
              key={`${site.id}-${site.revision}-${planAttempt}`}
              capability={localizedPlanCapability}
              preset={{
                id: site.id,
                expected_revision: site.revision,
                reset_advanced_config: false,
              }}
              lockedPresetFields={["id", "expected_revision"]}
              references={references}
              cacheKeySuffix={`migration-plan-${site.id}-${site.revision}-${planAttempt}`}
              formatError={formatMigrationError}
              onExecuted={(result) => {
                const plan = parseSiteMigrationPlan(result);
                if (!plan) {
                  setPlanError("服务器返回的迁移计划无法验证，请重新规划。");
                  return;
                }
                setPlanError("");
                setReviewedPlan(plan);
                setApplyStarted(false);
                setApplySubmitted(false);
                setNow(Date.now());
              }}
            />
          )}
        </div>
      )}

      {planError && (
        <div className="notice danger" role="alert">
          {planError}
          <Button label="重新规划" variant="ghost" onClick={beginNewPlan} />
        </div>
      )}

      {reviewedPlan && (
        <div className="panel-card">
          <div className="card-heading">
            <h3>核对迁移计划</h3>
            <p className="muted">计划号 {reviewedPlan.id}</p>
          </div>
          <MigrationSummary plan={reviewedPlan} webInstances={webInstances} />

          {reviewedPlan.configuration_mode === "advanced" && (
            <div className="notice warning">
              {reviewedPlan.reset_advanced_config
                ? "此计划会放弃手工配置并重新生成托管配置；原手工配置会留存备份。"
                : "此网站使用手工配置。必须重新规划并明确选择放弃手工配置后，才能迁移。"}
            </div>
          )}

          {applySubmitted && applyCapability ? (
            <>
              <div className="notice warning" role="status">
                迁移任务已提交；请在下方查看任务状态。请勿重复提交。
              </div>
              {staleReason && (
                <div className="notice warning" role="status">
                  {planGateMessage(staleReason)}
                </div>
              )}
              <ActionForm
                key={`apply-${reviewedPlan.id}`}
                capability={applyCapability}
                preset={{
                  plan_id: reviewedPlan.id,
                  // Keep the generic "adopt latest revision" recovery control
                  // hidden: this revision is part of the reviewed plan and a
                  // conflict requires a new plan, never a revised apply.
                  expected_revision: String(reviewedPlan.expected_revision),
                }}
                lockedPresetFields={["plan_id", "expected_revision"]}
                cacheKeySuffix={`migration-apply-${reviewedPlan.id}`}
                formatError={formatMigrationError}
                onExecuted={() => {
                  setApplySubmitted(true);
                  onExecuted();
                }}
              />
            </>
          ) : !getCapability ? (
            <div className="notice warning" role="status">
              当前账户没有读取迁移计划的权限，无法验证计划；不能应用。
            </div>
          ) : livePlanResource.loading && !livePlan ? (
            <p className="muted" role="status">正在从服务器核对受保护的迁移计划…</p>
          ) : livePlanResource.error ? (
            <div className="notice danger" role="alert">
              无法确认此迁移计划（可能已过期）。请重新规划后再试。
              <Button
                label="重新规划"
                variant="ghost"
                onClick={beginNewPlan}
              />
            </div>
          ) : !livePlan ? (
            <p className="muted" role="status">正在核对计划内容…</p>
          ) : !plansMatch ? (
            <div className="notice danger" role="alert">
              服务器计划与当前核对内容不一致；请重新规划。
              <Button label="重新规划" variant="ghost" onClick={beginNewPlan} />
            </div>
          ) : staleReason ? (
            <div className="notice danger" role="alert">
              {planGateMessage(staleReason)}
              <Button label="重新规划" variant="ghost" onClick={beginNewPlan} />
            </div>
          ) : !siteCanBeUsed ? (
            <div className="notice warning" role="status">
              当前网站版本尚未成功读取，不能应用此计划。
            </div>
          ) : !applyCapability ? (
            <div className="notice warning" role="status">
              当前账户没有应用网站迁移的权限。
            </div>
          ) : canApply && !applyStarted ? (
            <div className="button-row">
              <Button
                label="确认计划并继续"
                variant="primary"
                onClick={() => setApplyStarted(true)}
              />
              <Button label="重新规划" variant="ghost" onClick={beginNewPlan} />
            </div>
          ) : applyStarted ? (
            <>
              <div className="notice warning" role="status">
                请确认要应用以上计划。下一步仍会要求再次确认提交。
              </div>
              <ActionForm
                key={`apply-${reviewedPlan.id}`}
                capability={applyCapability}
                preset={{
                  plan_id: reviewedPlan.id,
                  expected_revision: String(reviewedPlan.expected_revision),
                }}
                lockedPresetFields={["plan_id", "expected_revision"]}
                cacheKeySuffix={`migration-apply-${reviewedPlan.id}`}
                formatError={formatMigrationError}
                onExecuted={() => {
                  setApplySubmitted(true);
                  onExecuted();
                }}
              />
              <Button
                label="返回计划核对"
                variant="ghost"
                onClick={() => setApplyStarted(false)}
              />
            </>
          ) : (
            <div className="notice warning" role="status">
              当前计划不可应用，请重新规划。
              <Button label="重新规划" variant="ghost" onClick={beginNewPlan} />
            </div>
          )}
        </div>
      )}
    </section>
  );
}
