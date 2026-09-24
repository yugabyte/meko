/*
 * Repairs rows whose components/resources hold SQL NULL or the JSON literal `null`
 * where every reader expects an array. NOT written by the report CLOUDGA-36984
 * covers: ApplyStatus has always guarded both fields on nil. This covers rows from
 * earlier paths, and pairs with the jsonArray coercion now applied on the way in.
 *
 * Both columns in one statement, because one statement per file is what makes it
 * retryable -- see the batching rule in db/connect.go.
 */
UPDATE meko_system.environments
   SET components = CASE
           WHEN components IS NULL OR components = 'null'::jsonb
           THEN '[]'::jsonb ELSE components
       END,
       resources = CASE
           WHEN resources IS NULL OR resources = 'null'::jsonb
           THEN '[]'::jsonb ELSE resources
       END
 WHERE components IS NULL OR components = 'null'::jsonb
    OR resources IS NULL OR resources = 'null'::jsonb;
