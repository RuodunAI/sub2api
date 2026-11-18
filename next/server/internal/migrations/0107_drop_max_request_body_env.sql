-- The request body limit is the gateway setting max_request_body_mb
-- (settings row "gateway", CONTRACTS §110): the one limit on a gateway
-- request, set in the console under system settings, gateway. It used to be
-- the environment variable of the same name (CONTRACTS §74.3) with a global
-- layer (settings row "environment") and a group layer (groups.environment).
-- The variable is no longer declared, and a layer holding a key that is not
-- declared is refused when it is saved again, so the stored values go. The
-- account layer never accepted the variable; a value written into it some
-- other way goes too.
--
-- The old values are not carried over: the limit starts from its default
-- (128 MB) and an administrator sets it again. One way. Safe to run again.

SET LOCAL lock_timeout = '10s';

UPDATE settings SET value = value - 'max_request_body_mb'
 WHERE key = 'environment' AND value ? 'max_request_body_mb';
UPDATE groups SET environment = environment - 'max_request_body_mb'
 WHERE environment ? 'max_request_body_mb';
UPDATE accounts SET environment = environment - 'max_request_body_mb'
 WHERE environment ? 'max_request_body_mb';
