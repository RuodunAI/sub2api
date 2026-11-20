-- plugin_monitor_receipts deduplicated the outcome of a Monitor claim by
-- (claim id, digest), from when a plugin could report one claim's result
-- more than once through a host callback. Monitor now returns one result,
-- applied once per claim, and every claim id is new (a fresh UUID for an
-- entry, the task id with the claim's token for a task), so no receipt was
-- ever found again: the table only cost a lock, a read and an insert per
-- outcome. Nothing reads it any more (CONTRACTS §112.9).
DROP TABLE IF EXISTS plugin_monitor_receipts;
