# Fixtures

All fixtures are shaped like real responses; every identifier, token, e-mail
and account id is synthetic. No real credentials appear anywhere in this tree.

| file | provenance |
|---|---|
| `kiro_usage.json` | Real `GetUsageLimits` response captured 2026-09-24 (read-only), `userId` replaced with a synthetic value. |
| `kiro_usage_bonus_overage.json` | Derived from the real capture; bonus and overage-enabled fields are schema-derived (not yet observed live). Numbers are deliberately strings in places to exercise schema drift. |
| `kiro_access_denied.json` | AWS JSON 1.0 error shape. |
| `agy_usage.json` | Real `agy --print /usage --output-format json` capture on agy 1.2.1, from Hive (Apache-2.0, see NOTICE). Carries no account identifier. |
| `agy_login.txt` | Representative login prompt (synthetic). |
| `claude_usage.json` | Shape of `GET /api/oauth/usage` as observed by Hive's probes (top-level `five_hour` / `seven_day` / `seven_day_<model>` buckets plus the `limits[]` array with `scope.model`, plus `extra_usage` in cents). Values synthetic. |
| `claude_usage_toplevel_only.json` | Older top-level-only shape; `utilization` as a string to exercise drift. |
| `claude_rate_limited.json` | The 429 body. |
| `codex_wham_usage.json` | `GET /backend-api/wham/usage` shape; `credits.balance` as the string `"0"`, `planType prolite` and a 100 % weekly window as observed live 2026-09-24 via Hive's app-server capture. Ids synthetic. |
| `codex_wham_usage_numeric.json` | Same, with a numeric balance. |
| `copilot_user*.json` | `GET /copilot_internal/user` shapes (paid with `quota_snapshots`; free with `limited_user_quotas` / `monthly_quotas`). Schema-derived. |
| `deepseek_balance*.json` | `GET /user/balance` shape (string amounts). Schema-derived. |
