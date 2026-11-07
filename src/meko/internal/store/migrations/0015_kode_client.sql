-- Kode's own calls carry X-Meko-Client: kode, and agentk before, when Meko called it agentk: the
-- call log names them all by the new name, so the Calls page's filter finds the old ones too.
UPDATE call_log SET client = 'kode' WHERE client = 'agentk';
