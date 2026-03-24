REVOKE CONNECT ON DATABASE yugabyte FROM PUBLIC;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;

-- Superusers bypass privilege checks, but grant explicitly for clarity.
GRANT CONNECT ON DATABASE yugabyte TO yugabyte;
