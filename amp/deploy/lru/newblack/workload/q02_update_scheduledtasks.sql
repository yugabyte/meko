UPDATE backgroundjobs
SET NextExecutionTime = $1,
    LastExecutionTime = $2,
    LastSuccessTime = $2,
    ExecutionCount = COALESCE(ExecutionCount, $4) + $5,
    SuccessCount = COALESCE(SuccessCount, $6) + $7
where ID = $3;
