-- Safe MTD accumulator backfill. Do NOT run automatically at application
-- startup and do NOT run against production without an approved ingestion
-- drain. This script intentionally fails closed if historical cost_usd is not
-- complete: rebuilding with NULL costs would silently undercount spend.
--
-- Run after every metering pod contains the accumulator writer.
--
-- REQUIRED OPERATOR SEQUENCE:
--   1. Drain/stop ALL metering event ingestion before starting this script.
--      Inference can remain available; this drain protects accounting while
--      the raw ledger snapshot is built.
--   2. Execute the script from line 11 through the COMMIT below. Do not start
--      ingestion during this period. The replacement snapshot is deliberately
--      taken after the drain, so there is no watermark/delta race and no
--      possibility of losing rows written between snapshot and swap.
--   3. Start metering ingestion again.
--   4. Validate totals, then enable MONTH_SPEND_ENABLED=true.

DO $$
DECLARE missing BIGINT;
BEGIN
  SELECT COUNT(*) INTO missing FROM usage_events WHERE cost_usd IS NULL;
  IF missing <> 0 THEN
    RAISE EXCEPTION 'refusing MTD backfill: % usage_events rows have NULL cost_usd', missing;
  END IF;
END $$;

DROP TABLE IF EXISTS user_month_spend_backfill;
CREATE TABLE user_month_spend_backfill (
  username TEXT NOT NULL,
  month DATE NOT NULL,
  spent_usd NUMERIC(20,8) NOT NULL DEFAULT 0,
  tokens BIGINT NOT NULL DEFAULT 0,
  requests BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (username, month)
);

INSERT INTO user_month_spend_backfill (username, month, spent_usd, tokens, requests)
SELECT username,
       date_trunc('month', timestamp)::date,
       COALESCE(SUM(cost_usd), 0),
       COALESCE(SUM(total_tokens), 0),
       COUNT(*)
FROM usage_events
GROUP BY username, date_trunc('month', timestamp)::date;

-- Validate the replacement before the controlled cutover.
DO $$
DECLARE bad BIGINT;
BEGIN
  SELECT COUNT(*) INTO bad
  FROM user_month_spend_backfill
  WHERE spent_usd IS NULL OR tokens < 0 OR requests < 0;
  IF bad <> 0 THEN
    RAISE EXCEPTION 'refusing MTD backfill: % invalid replacement rows', bad;
  END IF;
END $$;

-- Controlled cutover. Ingestion must still be drained from the operator
-- sequence above; this lock is only the short catalog/data-table swap.
BEGIN;
LOCK TABLE user_month_spend IN ACCESS EXCLUSIVE MODE;
ALTER TABLE user_month_spend RENAME TO user_month_spend_previous;
ALTER TABLE user_month_spend_backfill RENAME TO user_month_spend;
COMMIT;

-- Keep user_month_spend_previous until post-cutover totals and entitlement
-- checks are verified. Drop it separately after the rollback window:
-- DROP TABLE user_month_spend_previous;
